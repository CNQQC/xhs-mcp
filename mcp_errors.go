package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// MCPError 是供调用方判断恢复动作的稳定错误契约；不复制原始异常或凭证。
type MCPError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action"`
	RequestID  string `json:"request_id,omitempty"`
}

func (e *MCPError) Error() string { return e.Message }

// classifyMCPError 优先保留包装错误的类型，只为旧式参数校验保留文字兼容。
func classifyMCPError(err error, legacyText string) *MCPError {
	var existing *MCPError
	if errors.As(err, &existing) && existing != nil {
		copy := *existing
		return &copy
	}
	newError := func(code, message string, retryable bool, next string) *MCPError {
		return &MCPError{Code: code, Message: message, Retryable: retryable, NextAction: next}
	}
	var timeout net.Error
	switch {
	case errors.Is(err, xhserrors.ErrRiskVerification):
		return newError("RISK_VERIFICATION_REQUIRED", "小红书要求账号本人完成安全验证", false, "调用 get_verification_qrcode，由账号本人扫码验证后再重试")
	case errors.Is(err, xhserrors.ErrVerifyThrottled):
		return newError("RATE_LIMITED", "小红书暂时限制请求频率", true, "等待至少一分钟再重试，避免连续请求")
	case errors.Is(err, xhserrors.ErrLoginRequired):
		return newError("LOGIN_REQUIRED", "需要登录小红书", false, "调用 get_login_qrcode，由账号本人扫码登录后再重试")
	case errors.Is(err, xhserrors.ErrRefExpired):
		return newError("REF_EXPIRED", "ref 无效或已过期", false, "重新调用 search_feeds 或 list_feeds，使用返回的新 ref")
	case errors.Is(err, xhserrors.ErrInvalidArgument):
		return newError("INVALID_ARGUMENT", "请求参数缺失或无效", false, "根据工具参数说明修正参数后重新调用")
	case errors.Is(err, context.Canceled):
		return newError("CANCELLED", "请求已取消", false, "如仍需结果，发起新的请求；写操作应先检查是否已生效")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, xhserrors.ErrSearchResultTimeout), errors.As(err, &timeout) && timeout.Timeout():
		return newError("TIMEOUT", "请求超时", true, "稍后重试；发布、评论等写操作应先检查是否已生效，避免重复")
	}
	text := strings.ToLower(legacyText)
	if err != nil {
		text += " " + strings.ToLower(err.Error())
	}
	switch {
	case strings.Contains(text, "ref") && (strings.Contains(text, "过期") || strings.Contains(text, "无效")):
		return classifyMCPError(xhserrors.ErrRefExpired, "")
	case strings.Contains(text, "缺少"), strings.Contains(text, "不能为空"), strings.Contains(text, "必须"), strings.Contains(text, "超出范围"), strings.Contains(text, "最多6条"):
		return classifyMCPError(xhserrors.ErrInvalidArgument, "")
	case !strings.Contains(text, "可能") && (strings.Contains(text, "未登录") || strings.Contains(text, "请先登录") || strings.Contains(text, "登录已失效")):
		return classifyMCPError(xhserrors.ErrLoginRequired, "")
	case strings.Contains(text, "浏览器繁忙"):
		return newError("BROWSER_BUSY", "浏览器处理资源繁忙", true, "稍后重试，避免同时发起大量请求")
	case strings.Contains(text, "context deadline exceeded"), strings.Contains(text, "timeout"), strings.Contains(text, "超时"):
		return classifyMCPError(context.DeadlineExceeded, "")
	case strings.Contains(text, "内部错误"), strings.Contains(text, "序列化失败"):
		return newError("INTERNAL_ERROR", "服务内部错误", false, "使用 request_id 联系服务维护者检查日志；写操作应先检查是否已生效")
	default:
		return newError("UPSTREAM_ERROR", "操作未能完成", false, "检查原始错误提示；若持续失败，使用 request_id 联系服务维护者")
	}
}

func newMCPErrorResult(err error, legacyText string) *MCPToolResult {
	return &MCPToolResult{
		Content:           []MCPContent{{Type: "text", Text: legacyText}},
		IsError:           true,
		StructuredContent: map[string]any{"error": classifyMCPError(err, legacyText)},
	}
}

// refArgumentError 区分没给参数与给了已经失效的 ref，避免误导客户端反复搜索。
func refArgumentError(ref, idField string) *MCPToolResult {
	if ref != "" {
		return newMCPErrorResult(xhserrors.ErrRefExpired, errRefExpired)
	}
	return newMCPErrorResult(xhserrors.ErrInvalidArgument,
		fmt.Sprintf("缺少 ref，或完整的 %s 和 xsec_token；请提供其中一种参数组合", idField))
}

type mcpRequestIDKey struct{}

var requestSequence atomic.Uint64

func newMCPRequestID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), requestSequence.Add(1))
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(mcpRequestIDKey{}).(string)
	return id
}

// decorateMCPResult 仅增加结构化元数据，保留既有文字及图像内容。
func decorateMCPResult(result *mcp.CallToolResult, requestID string) *mcp.CallToolResult {
	if result == nil {
		result = &mcp.CallToolResult{}
	}
	structured := make(map[string]any)
	if existing, ok := result.StructuredContent.(map[string]any); ok {
		for key, value := range existing {
			structured[key] = value
		}
	} else if result.StructuredContent != nil {
		// 保留结构体或 RawMessage 的对象字段，不改动调用方的原始对象。
		data, err := json.Marshal(result.StructuredContent)
		if err != nil || json.Unmarshal(data, &structured) != nil || structured == nil {
			structured = map[string]any{"data": result.StructuredContent}
		}
	}
	structured["request_id"] = requestID
	if result.IsError {
		var info *MCPError
		if existing, ok := structured["error"].(*MCPError); ok && existing != nil {
			copy := *existing
			info = &copy
		} else {
			var messages []string
			for _, content := range result.Content {
				if value, ok := content.(*mcp.TextContent); ok {
					messages = append(messages, value.Text)
				}
			}
			info = classifyMCPError(nil, strings.Join(messages, "\n"))
		}
		info.RequestID = requestID
		structured["error"] = info
	}
	result.StructuredContent = structured
	return result
}

// 非幂等写操作超时后可能已经生效，不能建议客户端直接自动重试。
func guardMutationRetry(toolName string, result *mcp.CallToolResult) {
	switch toolName {
	case "publish_content", "publish_with_video", "post_comment_to_feed", "reply_comment_in_feed", "reply_notification":
	default:
		return
	}
	if !result.IsError {
		return
	}
	structured, _ := result.StructuredContent.(map[string]any)
	info, _ := structured["error"].(*MCPError)
	if info == nil {
		return
	}
	switch info.Code {
	case "TIMEOUT", "CANCELLED", "INTERNAL_ERROR", "UPSTREAM_ERROR":
		info.Retryable = false
		info.NextAction = "先检查发布或评论是否已经生效，确认未生效后再重试，避免重复；不确定时联系服务维护者并提供 request_id"
	}
}
