package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 使用没有业务 service 的服务验证诊断与参数错误都不访问浏览器或登录状态。
func diagnosticRPC(t *testing.T, handler http.Handler, method string, params any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload), response.Body.String())
	require.NotContains(t, payload, "error", response.Body.String())
	result, ok := payload["result"].(map[string]any)
	require.True(t, ok, response.Body.String())
	return result
}

func TestDiagnosticsVersionConsistentAndNoBrowser(t *testing.T) {
	previous := version
	version = "v-test-injected"
	t.Cleanup(func() { version = previous })
	router := setupRoutes(NewAppServer(nil, ""))
	initial := diagnosticRPC(t, router, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "diagnostics-test", "version": "1.0"},
	})
	assert.Equal(t, version, initial["serverInfo"].(map[string]any)["version"])
	result := diagnosticRPC(t, router, "tools/call", map[string]any{"name": "get_diagnostics", "arguments": map[string]any{}})
	assert.NotEqual(t, true, result["isError"])
	structured := result["structuredContent"].(map[string]any)
	assert.Equal(t, version, structured["version"])
	assert.Equal(t, version, structured["build"].(map[string]any)["version"])
	assert.Equal(t, "alive", structured["status"])
	assert.Equal(t, "process_liveness", structured["scope"])
	assert.Equal(t, "not_checked", structured["browser_status"])
	assert.Equal(t, "not_checked", structured["login_status"])
	assert.NotEmpty(t, structured["request_id"])
	_, err := time.Parse(time.RFC3339Nano, structured["timestamp"].(string))
	require.NoError(t, err)
	limits := structured["limits"].(map[string]any)
	assert.Equal(t, float64(feedBatchMax), limits["batch_max"])

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var health struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &health))
	assert.Equal(t, "healthy", health.Data["status"])
	assert.Equal(t, "process_liveness", health.Data["scope"])
	assert.Equal(t, structured["version"], health.Data["version"])
	assert.Equal(t, structured["build"], health.Data["build"])
}

func TestMCPDiagnosticToolContracts(t *testing.T) {
	router := setupRoutes(NewAppServer(nil, ""))
	result := diagnosticRPC(t, router, "tools/list", map[string]any{})
	tools := make(map[string]map[string]any)
	for _, value := range result["tools"].([]any) {
		tool := value.(map[string]any)
		tools[tool["name"].(string)] = tool
	}
	require.Contains(t, tools, "get_diagnostics")
	assert.Equal(t, true, tools["get_diagnostics"]["annotations"].(map[string]any)["readOnlyHint"])
	assert.NotEqual(t, true, tools["list_notifications"]["annotations"].(map[string]any)["readOnlyHint"])
	for _, name := range []string{"get_feed_detail", "get_feed_details"} {
		assert.NotContains(t, tools[name]["description"], "前10条")
	}
	assert.NotContains(t, tools["get_feed_detail"]["description"], "subtitleText")
	assert.NotContains(t, tools["get_feed_detail"]["description"], "video.subtitle")
}

func TestMCPReferenceArgumentErrorCodes(t *testing.T) {
	router := setupRoutes(NewAppServer(nil, ""))
	for _, name := range []string{"get_feed_detail", "get_feed_image", "user_profile", "like_feed", "favorite_feed", "post_comment_to_feed", "reply_comment_in_feed"} {
		for _, ref := range []string{"", "missing-ref"} {
			args := map[string]any{}
			if name == "post_comment_to_feed" || name == "reply_comment_in_feed" {
				args["content"] = "test"
			}
			if ref != "" {
				args["ref"] = ref
			}
			result := diagnosticRPC(t, router, "tools/call", map[string]any{"name": name, "arguments": args})
			require.Equal(t, true, result["isError"], name)
			structured := result["structuredContent"].(map[string]any)
			info := structured["error"].(map[string]any)
			want := "INVALID_ARGUMENT"
			if ref != "" {
				want = "REF_EXPIRED"
			}
			assert.Equal(t, want, info["code"], name)
			assert.Equal(t, structured["request_id"], info["request_id"], name)
		}
	}
}

func TestMCPBatchLimitErrorCode(t *testing.T) {
	router := setupRoutes(NewAppServer(nil, ""))
	result := diagnosticRPC(t, router, "tools/call", map[string]any{"name": "get_feed_details", "arguments": map[string]any{"refs": []string{"a", "b", "c", "d", "e", "f", "g"}}})
	require.Equal(t, true, result["isError"])
	structured := result["structuredContent"].(map[string]any)
	info := structured["error"].(map[string]any)
	assert.Equal(t, "INVALID_ARGUMENT", info["code"])
	assert.Equal(t, structured["request_id"], info["request_id"])
}
