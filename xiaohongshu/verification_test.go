package xiaohongshu

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 限流判定要求「频次」和「等会儿再来」两类文案共现，这条不能退化成一个 OR 列表：
// OR 列表里最松的那个词会决定整体判定，而中文风控页解释「为什么弹验证」时几乎必用
// 「操作过于频繁」——滑块验证的开场白就是这一句。误判成限流，本该如实上报「这次
// 不是扫码类验证」的场合就变成让用户干等一分钟再撞一次。
func TestIsThrottleText(t *testing.T) {
	t.Run("两类共现才算限流", func(t *testing.T) {
		for _, text := range []string{
			// 实测原文：标题仍是「安全验证」，正文换成这一句，页面上根本没有二维码
			"请求太频繁，请一分钟后再试 刷新 问题反馈",
			"操作过于频繁，请稍后重试",
			// 英文文案大小写混写也要认得出
			"Too Many requests, try again later",
			"RATE LIMIT exceeded. Try Again Later.",
		} {
			assert.True(t, isThrottleText(text), "%q 应判成限流", text)
		}
	})

	t.Run("只命中一类的都不算", func(t *testing.T) {
		for _, text := range []string{
			"",
			// 只命中「频繁」：滑块挑战，是一次如实上报「不支持这种验证形态」的机会。
			// 判成限流就是让用户白等一分钟，回来还是同一个滑块。
			"您的操作过于频繁，请拖动下方滑块完成验证",
			// 只命中「后再试」类：通用错误提示，跟验证页限流没关系
			"网络异常，请稍后重试",
			"系统繁忙，请稍后再试",
			// 正常的扫码验证页：一类都不命中
			"为保护账号安全，请使用已登录该账号的「小红书APP」扫码验证身份 二维码1分钟失效",
		} {
			assert.False(t, isThrottleText(text), "%q 不该判成限流", text)
		}
	})
}
