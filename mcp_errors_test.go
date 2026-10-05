package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

func TestClassifyMCPError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		text      string
		code      string
		retryable bool
	}{
		{"risk", fmt.Errorf("wrapped: %w token=secret", xhserrors.ErrRiskVerification), "", "RISK_VERIFICATION_REQUIRED", false},
		{"throttled", fmt.Errorf("wrapped: %w", xhserrors.ErrVerifyThrottled), "", "RATE_LIMITED", true},
		{"login", fmt.Errorf("wrapped: %w", xhserrors.ErrLoginRequired), "", "LOGIN_REQUIRED", false},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "", "TIMEOUT", true},
		{"cancelled", fmt.Errorf("wrapped: %w", context.Canceled), "", "CANCELLED", false},
		{"search timeout", xhserrors.ErrSearchResultTimeout, "", "TIMEOUT", true},
		{"invalid", xhserrors.ErrInvalidArgument, "", "INVALID_ARGUMENT", false},
		{"ref", xhserrors.ErrRefExpired, "", "REF_EXPIRED", false},
		{"missing legacy", nil, "缺少 comment_id 或 user_id", "INVALID_ARGUMENT", false},
		{"expired legacy", nil, errRefExpired, "REF_EXPIRED", false},
		{"uncertain login", errors.New("页面状态里没有分区，可能未登录或页面结构已变化"), "", "UPSTREAM_ERROR", false},
		{"unrelated risk text", errors.New("could not fetch 安全验证 title"), "", "UPSTREAM_ERROR", false},
		{"secret", errors.New("api-key=secret session=private"), "", "UPSTREAM_ERROR", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyMCPError(tc.err, tc.text)
			assert.Equal(t, tc.code, got.Code)
			assert.Equal(t, tc.retryable, got.Retryable)
			assert.NotEmpty(t, got.Message)
			assert.NotEmpty(t, got.NextAction)
			assert.NotContains(t, got.Message, "secret")
		})
	}
}

func TestMCPRecoveryPreservesLegacyAndRequestID(t *testing.T) {
	var seenID string
	handler := withPanicRecovery("search_feeds", func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		seenID = requestIDFromContext(ctx)
		return convertToMCPResult(newMCPErrorResult(xhserrors.ErrRiskVerification, "legacy readable error")), nil, nil
	})
	result, response, err := handler(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, response)
	require.True(t, result.IsError)
	assert.Equal(t, "legacy readable error", result.Content[0].(*mcp.TextContent).Text)
	structured := result.StructuredContent.(map[string]any)
	require.NotEmpty(t, seenID)
	assert.Equal(t, seenID, structured["request_id"])
	info := structured["error"].(*MCPError)
	assert.Equal(t, seenID, info.RequestID)
	assert.Equal(t, "RISK_VERIFICATION_REQUIRED", info.Code)
}

func TestMCPRecoveryDoesNotLeakPanic(t *testing.T) {
	for _, panicValue := range []any{"secret token password", errors.New("secret token password"), context.DeadlineExceeded} {
		handler := withPanicRecovery("get_feed_detail", func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			panic(panicValue)
		})
		result, _, err := handler(context.Background(), nil, nil)
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "secret")
		structured := result.StructuredContent.(map[string]any)
		assert.NotEmpty(t, structured["request_id"])
		info := structured["error"].(*MCPError)
		if panicValue == context.DeadlineExceeded {
			assert.Equal(t, "TIMEOUT", info.Code)
		} else {
			assert.Equal(t, "INTERNAL_ERROR", info.Code)
		}
	}
}

func TestMCPRecoveryDecoratesSuccessAndReturnedErrors(t *testing.T) {
	var ids []string
	for _, fail := range []bool{false, false, true} {
		handler := withPanicRecovery("search_feeds", func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			if fail {
				return nil, nil, errors.New("private URL secret")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "legacy success"}}, StructuredContent: map[string]any{"value": 42}}, nil, nil
		})
		result, _, err := handler(context.Background(), nil, nil)
		require.NoError(t, err)
		structured := result.StructuredContent.(map[string]any)
		ids = append(ids, structured["request_id"].(string))
		if fail {
			require.True(t, result.IsError)
			assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "secret")
		} else {
			assert.Equal(t, "legacy success", result.Content[0].(*mcp.TextContent).Text)
			assert.Equal(t, 42, structured["value"])
			assert.NotContains(t, structured, "error")
		}
	}
	assert.NotEqual(t, ids[0], ids[1])
}

func TestMutationTimeoutIsNotAutomaticallyRetryable(t *testing.T) {
	for _, name := range []string{"publish_content", "publish_with_video", "post_comment_to_feed", "reply_comment_in_feed", "reply_notification", "search_feeds"} {
		handler := withPanicRecovery(name, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return convertToMCPResult(newMCPErrorResult(context.DeadlineExceeded, "请求超时")), nil, nil
		})
		result, _, err := handler(context.Background(), nil, nil)
		require.NoError(t, err)
		info := result.StructuredContent.(map[string]any)["error"].(*MCPError)
		assert.Equal(t, name == "search_feeds", info.Retryable, name)
		if name != "search_feeds" {
			assert.Contains(t, info.NextAction, "先检查")
		}
	}
}

func TestMCPRecoveryClassifiesBrowserSaturationPanic(t *testing.T) {
	handler := withPanicRecovery("search_feeds", func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		panic("浏览器繁忙：等待 1m0s 仍未取得名额（并发上限 2），请稍后重试 secret")
	})
	result, _, err := handler(context.Background(), nil, nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	info := result.StructuredContent.(map[string]any)["error"].(*MCPError)
	assert.Equal(t, "BROWSER_BUSY", info.Code)
	assert.True(t, info.Retryable)
	assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "secret")
}
