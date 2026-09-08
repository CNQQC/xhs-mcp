package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

type imageRoundTrip func(*http.Request) (*http.Response, error)

func (f imageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeedImageResult(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nimage")
	images := []xiaohongshu.DetailImageInfo{{URLDefault: "http://sns-img-bd.xhscdn.com/first"}, {URLPre: "https://sns-img-bd.xhscdn.com/second"}}
	for _, tc := range []struct {
		name          string
		index, status int
		body          []byte
		fail          bool
	}{
		{"selected image and preview fallback", 2, 200, png, false},
		{"out of range", 3, 200, png, true},
		{"zero index", 0, 200, png, true},
		{"HTTP error", 1, 403, png, true},
		{"HTML instead of image", 1, 200, []byte("<html>blocked</html>"), true},
		{"too large", 1, 200, bytes.Repeat([]byte("x"), feedImageMaxBytes+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "https", r.URL.Scheme)
				if tc.index == 2 {
					require.Equal(t, "/second", r.URL.Path)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			result := feedImageResult(context.Background(), client, images, tc.index)
			require.Equal(t, tc.fail, result.IsError)
			if tc.index < 1 || tc.index > len(images) {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
			if !tc.fail {
				converted := convertToMCPResult(result)
				require.Len(t, converted.Content, 2)
				img, ok := converted.Content[1].(*mcp.ImageContent)
				require.True(t, ok)
				require.Equal(t, "image/png", img.MIMEType)
				require.Equal(t, png, img.Data)
			}
		})
	}
}

func TestFeedImageURLAndRedirectPolicy(t *testing.T) {
	for _, raw := range []string{"https://localhost/a", "https://xhscdn.com.evil.test/a", "file:///tmp/a", "https://a.xhscdn.com:123/a", "https://user@a.xhscdn.com/a"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		require.Error(t, validateFeedImageURL(u))
		require.Error(t, feedImageClient().CheckRedirect(&http.Request{URL: u}, nil))
	}
	u, _ := url.Parse("https://sns-img-bd.xhscdn.com/a")
	require.NoError(t, validateFeedImageURL(u))
	require.Error(t, feedImageClient().CheckRedirect(&http.Request{URL: u}, make([]*http.Request, 5)))
}

func TestGetFeedImageRejectsInvalidInputBeforeBrowser(t *testing.T) {
	s := &AppServer{refs: newRefTable()}
	require.True(t, s.handleGetFeedImage(context.Background(), FeedImageArgs{Ref: "expired"}).IsError)
	result := s.handleGetFeedImage(context.Background(), FeedImageArgs{FeedID: "id", XsecToken: "token", ImageIndex: -1})
	require.True(t, result.IsError)
	require.True(t, strings.Contains(result.Content[0].Text, "image_index"))
}

func TestFeedImageToolRegistered(t *testing.T) {
	router := setupRoutes(NewAppServer(NewXiaohongshuService(), ""))
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
				Description string
				InputSchema struct{ Properties map[string]json.RawMessage }
			}
		}
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	for _, tool := range result.Result.Tools {
		if tool.Name != "get_feed_image" {
			continue
		}
		require.Contains(t, tool.Description, "明确视觉需求")
		require.Contains(t, tool.Description, "视觉 token")
		require.Contains(t, tool.InputSchema.Properties, "ref")
		require.Contains(t, tool.InputSchema.Properties, "image_index")
		return
	}
	t.Fatal("get_feed_image 未注册")
}
