package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestBatchResearchPartialSuccessAndDedup(t *testing.T) {
	s := &AppServer{refs: newRefTable()}
	target := refTarget{FeedID: "0123456789abcdef01234567", XsecToken: "actual-token"}
	a := s.refs.put(target)
	duplicate := s.refs.put(target)
	b := s.refs.put(refTarget{FeedID: "1123456789abcdef01234567", XsecToken: "token-b"})
	// 旧句柄接近到期时，详情返回不能把到期时间伪装成新注册的一小时。
	expiry := time.Now().Add(time.Minute).UTC()
	entry := s.refs.items[a]
	entry.expiresAt = expiry
	s.refs.items[a] = entry
	calls := 0
	result := s.handleGetFeedDetailsWithFetch(context.Background(), FeedDetailsArgs{Refs: []string{a, "expired", b, duplicate}},
		func(ctx context.Context, targets []refTarget, config xiaohongshu.FeedDetailConfig) []feedDetailResult {
			calls++
			require.Len(t, targets, 2)
			assert.Equal(t, target, targets[0])
			return []feedDetailResult{
				{detail: &xiaohongshu.FeedDetailResponse{Note: xiaohongshu.FeedDetail{Title: "success"}}},
				{err: context.DeadlineExceeded},
			}
		})
	require.False(t, result.IsError)
	assert.Equal(t, 1, calls)
	var view noteBatchView
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &view))
	assert.Equal(t, 4, view.Count)
	assert.Equal(t, 2, view.Failed)
	assert.Equal(t, []string{a, "expired", b, duplicate}, []string{view.Notes[0].Ref, view.Notes[1].Ref, view.Notes[2].Ref, view.Notes[3].Ref})
	assert.Equal(t, "success", view.Notes[3].Title)
	assert.Equal(t, target.FeedID, view.Notes[0].NoteID)
	require.NotNil(t, view.Notes[1].ErrorInfo)
	assert.Equal(t, "REF_EXPIRED", view.Notes[1].ErrorInfo.Code)
	require.NotNil(t, view.Notes[2].ErrorInfo)
	assert.NotEmpty(t, view.Notes[2].ErrorInfo.NextAction)
	assert.True(t, view.Notes[2].ErrorInfo.Retryable)
	actualExpiry, err := time.Parse(time.RFC3339Nano, view.Notes[0].RefExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, expiry, actualExpiry, time.Second)
}

func TestBatchResearchInvalidFetchResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []feedDetailResult
	}{
		{"missing", nil}, {"nil detail", []feedDetailResult{{}}},
		{"failure", []feedDetailResult{{err: errors.New("temporary failure")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &AppServer{refs: newRefTable()}
			ref := s.refs.put(refTarget{FeedID: "note", XsecToken: "token"})
			result := s.handleGetFeedDetailsWithFetch(context.Background(), FeedDetailsArgs{Refs: []string{ref}},
				func(context.Context, []refTarget, xiaohongshu.FeedDetailConfig) []feedDetailResult { return tc.results })
			require.True(t, result.IsError)
			var view noteBatchView
			require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &view))
			assert.Equal(t, 1, view.Failed)
			assert.NotEmpty(t, view.Notes[0].Error)
		})
	}
}
