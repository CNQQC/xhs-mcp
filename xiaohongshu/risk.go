package xiaohongshu

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

// captchaPath 安全验证页的路径特征，判定只认这一段：verifyUuid 每次访问都不同、
// redirectPath 随来源页而变；页面标题「安全验证」这类文案是站点随时能改的本地化
// 字符串，只配写进错误消息，不配当触发条件。
const captchaPath = "/website-login/captcha"

func isCaptchaURL(rawURL string) bool {
	return strings.Contains(rawURL, captchaPath)
}

// loginWallPath 登录与验证这一族页面的公共前缀，captchaPath 是它下面的一支。
const loginWallPath = "/website-login/"

// isLoginWallURL 页面还停在登录/验证这一族页面上。
//
// WaitVerified 用它而不是 isCaptchaURL 判成功：只要求「不是验证页」的话，扫码失败
// 被弹回登录墙同样满足，会被当成验证通过，然后把一份并没有通过验证的 cookie 覆盖
// 到 cookies.json 上，还在日志里留下一句「已通过」。判据必须是「离开了这一整族页面」。
func isLoginWallURL(rawURL string) bool {
	return strings.Contains(rawURL, loginWallPath)
}

// infoTimeout 读页面 URL/标题的独立预算。
const infoTimeout = 5 * time.Second

// pageInfo 读当前页面的 URL 和标题。
//
// 走 browser 的 ctx，所以页面 deadline 烧穿之后依然可用——那时 Eval/Element 只会返回
// context 错误。代价是 browser ctx 就是 context.Background()，必须自带超时：CDP 不回
// 就永久阻塞，调用方 defer 的 Close 执行不到，浏览器名额票会永久丢失。
func pageInfo(page *rod.Page) (*proto.TargetTargetInfo, error) {
	res, err := proto.TargetGetTargetInfo{TargetID: page.TargetID}.
		Call(page.Browser().Timeout(infoTimeout))
	if err != nil {
		return nil, err
	}
	if res.TargetInfo == nil {
		return nil, fmt.Errorf("Target.getTargetInfo 没有返回 targetInfo")
	}

	return res.TargetInfo, nil
}

// checkRiskVerification 看一眼当前页面有没有被 302 到安全验证页，没被拦返回 nil。
//
// 调用点要紧跟导航、排在所有等待之前：验证页上 __INITIAL_STATE__ 恒为 undefined、
// 业务选择器一个都不存在，WaitStable / Wait / Element 都只会等到 deadline 才收场。
// 只识别、不绕过：被拦时唯一的出路是账号本人用小红书 App 扫码。
func checkRiskVerification(page *rod.Page) error {
	info, err := pageInfo(page)
	if err != nil || !isCaptchaURL(info.URL) {
		// 读不到页面信息就当没被拦：一次 CDP 调用失败不能反过来编造风控结论。
		return nil
	}

	logrus.Warnf("被小红书安全验证拦截：%s（页面标题：%s）", info.URL, info.Title)

	return riskVerificationError(info.URL)
}

// riskVerificationError 组织返回给客户端的错误。验证页跟本次请求的浏览器会话绑定，
// 请求一返回会话就销毁，用户拿这条 URL 自己打开只会得到另一个 verifyUuid 的新挑战，
// 扫完服务端状态零变化——所以 URL 只当诊断信息，动作指向 get_verification_qrcode。
func riskVerificationError(pageURL string) error {
	// 两个入口都要写：这条错误也会被 handlers_api.go 原样透传给 REST 调用方，
	// 而 REST 侧没有叫 get_verification_qrcode 的东西。
	return fmt.Errorf("%w%s：请调用 get_verification_qrcode（REST：GET /api/v1/verification/qrcode）取回验证二维码，"+
		"用已登录该账号的小红书 App 扫码完成验证后重试；本工具只做识别，不会自动绕过验证。"+
		"下面这条验证页 URL 仅供诊断——页面随本次请求的浏览器会话一起销毁了，"+
		"自己打开只会拿到另一个会话的新验证码，扫了也不算数：%s",
		errors.ErrRiskVerification, formatVerifyParams(pageURL), pageURL)
}

// formatVerifyParams 把 URL 里的 verifyType/verifyBiz 格式化成一段附注，便于人工核对
// 是哪一类验证（实测扫码验证身份是 124/461）。取不到返回空串，不影响判定。
func formatVerifyParams(rawURL string) string {
	verifyType, verifyBiz := verifyParams(rawURL)
	if verifyType == "" && verifyBiz == "" {
		return ""
	}

	return fmt.Sprintf("（verifyType=%s verifyBiz=%s）", verifyType, verifyBiz)
}

// verifyParams 取原值，取不到就是空串——只是给人核对的附注，缺了不影响判定。
func verifyParams(rawURL string) (verifyType, verifyBiz string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}

	q := u.Query()

	return q.Get("verifyType"), q.Get("verifyBiz")
}

// bodyTextJS 取页面可见文本的开头一小段，失败留痕用。
const bodyTextJS = `() => (document.body ? document.body.innerText : "").slice(0, 200)`

// warnPageState 把当前页面现场打进日志，并返回最终 URL（读不到就是空串）。
//
// 拦截形态不止安全验证一种，例如搜索页登录墙的 URL 仍然是 search_result。判定不为此
// 放宽——猜一张关键词表出来只会带来误判——但失败时必须留下现场。
func warnPageState(page *rod.Page, reason string) string {
	var pageURL, title string
	if info, err := pageInfo(page); err == nil {
		pageURL, title = info.URL, info.Title
	}

	var text string
	if res, err := page.Eval(bodyTextJS); err == nil {
		text = strings.Join(strings.Fields(res.Value.Str()), " ")
	}

	logrus.Warnf("%s：URL=%s 标题=%q 正文=%q", reason, pageURL, title, text)

	return pageURL
}
