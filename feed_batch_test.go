package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// 结果要按下标对齐，并发数不能超过上限，而且真的要并发起来。
func TestFetchConcurrentlyOrderAndLimit(t *testing.T) {
	const n, limit = 4, 2
	var inFlight, maxInFlight atomic.Int32

	results := fetchConcurrently(context.Background(), n, limit,
		func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error) {
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				old := maxInFlight.Load()
				if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
					break
				}
			}
			// 比开页错峰的上限（1s）长，保证前两条一定重叠
			time.Sleep(1100 * time.Millisecond)

			return &xiaohongshu.FeedDetailResponse{Note: xiaohongshu.FeedDetail{NoteID: string(rune('a' + i))}}, nil
		})

	require.Len(t, results, n)
	for i, r := range results {
		require.NoError(t, r.err)
		assert.Equal(t, string(rune('a'+i)), r.detail.Note.NoteID, "第 %d 条错位", i)
	}
	assert.Equal(t, int32(limit), maxInFlight.Load())
}

// 子 goroutine 里的 panic 必须接住，只算那一条失败，否则整个进程被带崩。
func TestFetchConcurrentlyRecoversPanic(t *testing.T) {
	results := fetchConcurrently(context.Background(), 2, 2,
		func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error) {
			if i == 0 {
				panic("boom")
			}
			return &xiaohongshu.FeedDetailResponse{}, nil
		})

	require.Error(t, results[0].err)
	assert.Contains(t, results[0].err.Error(), "boom")
	assert.NoError(t, results[1].err)
	assert.NotNil(t, results[1].detail)
}

// ctx 已取消就一条都不开，全部记为 ctx 错误。
func TestFetchConcurrentlyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var called atomic.Int32
	results := fetchConcurrently(ctx, 3, 2,
		func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error) {
			called.Add(1)
			return nil, nil
		})

	assert.Zero(t, called.Load())
	for _, r := range results {
		assert.ErrorIs(t, r.err, context.Canceled)
	}
}

// 参数不对、ref 全失效时，都不该起浏览器（service 为 nil，真起了会直接 panic）。
func TestGetFeedDetailsRejectsBeforeBrowser(t *testing.T) {
	s := &AppServer{refs: newRefTable()}
	ctx := context.Background()

	t.Run("refs 为空", func(t *testing.T) {
		assert.True(t, s.handleGetFeedDetails(ctx, FeedDetailsArgs{}).IsError)
	})

	t.Run("超过上限", func(t *testing.T) {
		refs := make([]string, feedBatchMax+1)
		result := s.handleGetFeedDetails(ctx, FeedDetailsArgs{Refs: refs})
		require.True(t, result.IsError)
		assert.Contains(t, result.Content[0].Text, "最多")
	})

	t.Run("ref 全部失效", func(t *testing.T) {
		result := s.handleGetFeedDetails(ctx, FeedDetailsArgs{Refs: []string{"x1", "x2"}})
		require.True(t, result.IsError)

		var view noteBatchView
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &view))
		assert.Equal(t, 2, view.Count)
		assert.Equal(t, 2, view.Failed)
		assert.Equal(t, "x2", view.Notes[1].Ref, "失败的条目要带回原 ref，模型才知道是哪条")
		assert.NotEmpty(t, view.Notes[1].Error)
	})
}

// 成功项展开成与 get_feed_detail 相同的字段；失败项只有 ref 和 error。
func TestNoteBatchItemJSON(t *testing.T) {
	ok, err := json.Marshal(noteBatchItem{noteDetailView: noteDetailView{Ref: "r1", Title: "标题"}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ref":"r1","title":"标题"}`, string(ok))

	failed, err := json.Marshal(noteBatchItem{noteDetailView: noteDetailView{Ref: "r2"}, Error: "笔记不存在"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ref":"r2","error":"笔记不存在"}`, string(failed))
}

func TestToolStringListSchemas(t *testing.T) {
	router := setupRoutes(NewAppServer(nil, ""))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var result struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema *jsonschema.Schema
			}
		}
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	schemas := make(map[string]*jsonschema.Schema, len(result.Result.Tools))
	for _, tool := range result.Result.Tools {
		schemas[tool.Name] = tool.InputSchema
	}

	require.NotNil(t, schemas["get_feed_details"])
	assert.Contains(t, schemas["get_feed_details"].Properties, "include_images")
	require.NotNil(t, schemas["get_feed_details"].Properties["skip_comments"])
	assert.Equal(t, "boolean", schemas["get_feed_details"].Properties["skip_comments"].Type)
	assert.NotContains(t, schemas["get_feed_details"].Required, "skip_comments")
	wantFields := map[string]map[string]bool{
		"get_feed_details":   {"refs": true},
		"publish_content":    {"images": true, "tags": false, "products": false},
		"publish_with_video": {"tags": false, "products": false},
	}
	for toolName, fields := range wantFields {
		require.NotNil(t, schemas[toolName], toolName)
		for fieldName, required := range fields {
			t.Run(toolName+"/"+fieldName, func(t *testing.T) {
				input := schemas[toolName]
				field := input.Properties[fieldName]
				require.NotNil(t, field)
				assert.Equal(t, required, slices.Contains(input.Required, fieldName))
				assert.NotEmpty(t, field.Description)
				assert.Empty(t, field.Type)
				assert.Empty(t, field.Types)
				assert.Nil(t, field.Items)
				require.Len(t, field.AnyOf, 2)
				assert.Equal(t, "array", field.AnyOf[0].Type)
				require.NotNil(t, field.AnyOf[0].Items)
				assert.Equal(t, "string", field.AnyOf[0].Items.Type)
				assert.Equal(t, "null", field.AnyOf[1].Type)
			})
		}
	}
}
