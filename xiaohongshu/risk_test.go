package xiaohongshu

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

// 本地实测被拦时的跳转目标，参数原样保留。
const capturedCaptchaURL = "https://www.xiaohongshu.com/website-login/captcha?" +
	"redirectPath=https%3A%2F%2Fwww.xiaohongshu.com%2Fsearch_result%3Fkeyword%3D%25E6%25B7%25B1%25E6%2588%25B7" +
	"&verifyUuid=XHSXXXXcf8b1e0f4a1d4e0a9e2b&verifyType=124&verifyBiz=461"

// 判定只认路径这一段：verifyUuid 每次都变、redirectPath 随来源页而变，
// 把匹配放宽到整条 URL 或收窄到某个参数都会漏判。
//
// 反向同样是硬要求：WaitVerified 靠「URL 不再是验证页」判断扫码成功，
// 认宽了就永远等不到，用户扫完了也不会存 cookie。
func TestIsCaptchaURL(t *testing.T) {
	require.True(t, isCaptchaURL(capturedCaptchaURL))
	require.False(t, isCaptchaURL("https://www.xiaohongshu.com/search_result?keyword=x"))
	require.False(t, isCaptchaURL(""))
}

// 登录弹窗的二维码也叫 .qrcode-img，取码必须限定在 #captcha-div 内，
// 否则未登录时会取回一张登录码——用户扫完，验证照样没过。
func TestVerifyQrcodeSelectorScopedToCaptchaDiv(t *testing.T) {
	require.True(t, strings.HasPrefix(verifyQrcodeSelector, "#captcha-div "))
}

func TestFormatVerifyParams(t *testing.T) {
	t.Run("实测拦截 URL 能取到两个参数", func(t *testing.T) {
		require.Equal(t, "（verifyType=124 verifyBiz=461）", formatVerifyParams(capturedCaptchaURL))
	})

	// 参数只是错误消息里的细节，缺了或解析失败都不能影响「被拦了」这个结论，
	// 更不能让判定本身出错。
	t.Run("没有参数或 URL 不可解析时返回空串", func(t *testing.T) {
		require.Empty(t, formatVerifyParams("https://www.xiaohongshu.com/website-login/captcha"))
		require.Empty(t, formatVerifyParams("://%%%"))
	})
}

// 错误文案是这条路径唯一交付给用户的东西：它必须指向本工具真做得到的动作，
// 而不是让用户拿 URL 去自己浏览器里打开——那是另一个会话，扫了也不算数。
func TestRiskVerificationError(t *testing.T) {
	err := riskVerificationError(capturedCaptchaURL)

	require.True(t, stderrors.Is(err, errors.ErrRiskVerification))

	msg := err.Error()
	require.Contains(t, msg, "get_verification_qrcode")
	require.Contains(t, msg, "仅供诊断")
	require.Contains(t, msg, "（verifyType=124 verifyBiz=461）")
	require.Contains(t, msg, capturedCaptchaURL)
}
