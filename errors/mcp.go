package errors

import "errors"

// 这些哨兵让 MCP 调用方通过 errors.Is 判断恢复动作，避免依赖页面文案。
var (
	ErrLoginRequired   = errors.New("需要登录小红书")
	ErrInvalidArgument = errors.New("请求参数缺失或无效")
	ErrRefExpired      = errors.New("ref 无效或已过期")
)
