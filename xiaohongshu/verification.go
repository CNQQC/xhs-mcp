package xiaohongshu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

// verifyProbeKeyword 触发风控用的关键词。这次导航只为让服务端决定要不要弹验证，
// 词本身不参与任何业务，取一个最普通的。
const verifyProbeKeyword = "美食"

// verifyQrcodeSelector 验证页上的二维码。
//
// 必须用 #captcha-div 限定：登录弹窗的二维码是 .login-container .qrcode-img，
// 跟这张同名，不限定就会在未登录时取回一张登录码——用户扫完验证照样过不了。
const verifyQrcodeSelector = `#captcha-div .qrcode-container img.qrcode-img`

// VerificationQrcode 一次取码的结果。Blocked 为 false 时只有 PageURL 有值——
// 「没被拦」也要带回落点：这个分支同时盖着登录墙和任何没被识别出来的拦截形态，
// 不带页面信息就等于对用户断言「风控没问题」，而那可能是错的。
type VerificationQrcode struct {
	Blocked    bool   // 当前是否真的被安全验证拦着
	Img        string // 二维码图片的 src，实测是 data-URI
	PageURL    string // 页面最终落点，诊断用；被拦时就是验证页 URL
	VerifyType string
	VerifyBiz  string
}

// VerificationAction 把服务端 headless 会话里的安全验证二维码原样搬给账号本人扫。
// 只做搬运：不解码、不识别、不提交、不代替用户完成验证——码是小红书发的，
// 扫的是账号本人的 App，这边只负责让浏览器活着等结果。
type VerificationAction struct {
	page *rod.Page
}

// NewVerificationAction 不在这里包 Timeout：WaitVerified 要在请求返回后继续用这个
// page，页面级 deadline 会连它一起烧掉。各方法自己按需要绑 ctx。
func NewVerificationAction(page *rod.Page) *VerificationAction {
	return &VerificationAction{page: page}
}

// FetchQrcode 走一次搜索页，被拦就把验证二维码取回来。
func (a *VerificationAction) FetchQrcode(ctx context.Context) (*VerificationQrcode, error) {
	page := a.page.Context(ctx).Timeout(pageTimeout)

	// 同 search.go：Navigate 不等重定向落地，不等 DOMContentLoaded 就读 URL
	// 只会读到跳转前的搜索页地址，于是永远判成「没被拦」。
	waitLanded := page.Timeout(probeTimeout).WaitNavigation(proto.PageLifecycleEventNameDOMContentLoaded)
	if err := page.Navigate(makeSearchURL(verifyProbeKeyword)); err != nil {
		return nil, fmt.Errorf("打开搜索页失败: %w", err)
	}
	waitLanded()

	info, err := waitVerifyLanding(page)
	if err != nil {
		return nil, err
	}

	if !isCaptchaURL(info.URL) {
		// 「没被拦」是本工具两种返回之一，而它同时盖着登录墙和任何没被识别出来的
		// 拦截形态——一个字不留的话，运维分不清这次是真没被拦还是漏判了。
		warnPageState(page, "取验证码时未被安全验证拦截")

		return &VerificationQrcode{PageURL: info.URL}, nil
	}

	verifyType, verifyBiz := verifyParams(info.URL)

	img, err := readVerifyQrcode(page, info.URL, verifyType, verifyBiz)
	if err != nil {
		return nil, err
	}

	logrus.Infof("取到安全验证二维码：%s（verifyType=%s verifyBiz=%s）", info.URL, verifyType, verifyBiz)

	return &VerificationQrcode{
		Blocked:    true,
		Img:        img,
		PageURL:    info.URL,
		VerifyType: verifyType,
		VerifyBiz:  verifyBiz,
	}, nil
}

// verifyProbeInterval 与 waitSearchFeeds 的注水轮询同一个节奏。
const verifyProbeInterval = 300 * time.Millisecond

// waitVerifyLanding 在 probeTimeout 内反复看这次访问有没有落到验证页。
//
// 只判一次不够：DOMContentLoaded 只对服务端 302 保证 URL 已是终态（实测 11~29ms），
// 客户端 location.href 跳转要晚几百毫秒才落地，判一次必然读到跳转前的搜索页地址。
// 而 Search 那边是在 waitSearchFeeds 里每 300ms 判一次的——作为「被拦之后唯一的
// 出路」，本工具的识别窗口不能比它窄，否则就是 search_feeds 说被拦、取码工具说
// 没被拦，用户拿不到码也没有下一步。
//
// 提前收工的出口和 Search 一致：搜索结果注水了，说明这是一个正常工作的搜索页，
// 不必把整个预算白等完（实测正常页面 waitLanded 之后 12ms 就能读到 feeds）。
func waitVerifyLanding(page *rod.Page) (*proto.TargetTargetInfo, error) {
	deadline := time.Now().Add(probeTimeout)

	var (
		last    *proto.TargetTargetInfo
		lastErr error
	)
	for {
		info, err := pageInfo(page)
		switch {
		case err != nil:
			// 一次 CDP 失败不当结论，下一拍再看
			lastErr = err
		case isCaptchaURL(info.URL):
			return info, nil
		default:
			last = info
			if feeds, ferr := readSearchFeeds(page); ferr == nil && feeds != "" {
				return info, nil
			}
		}

		if time.Now().After(deadline) {
			if last == nil {
				return nil, fmt.Errorf("读取页面信息失败: %w", lastErr)
			}

			return last, nil
		}

		time.Sleep(verifyProbeInterval)
	}
}

// readVerifyQrcode 读二维码的 src。全程非 Must：验证页是活的，码 1 分钟一失效、
// DOM 跟着换，MustElement 撞上就是 panic，而这一步浏览器还没移交给后台，
// 上层 recover 成一句「内部错误」，用户既拿不到码也拿不到下一步。
func readVerifyQrcode(page *rod.Page, pageURL, verifyType, verifyBiz string) (string, error) {
	el, err := page.Timeout(probeTimeout).Element(verifyQrcodeSelector)
	if err != nil {
		// 这是这条路径上最可能发生的失败，而超时本身分不出是站点改了 DOM 还是这次
		// 根本不是扫码类验证。URL / verifyType / verifyBiz 上面已经拿在手上了，
		// 连同页面现场一起交出去，运维才判断得了该改选择器还是该支持另一种形态。
		warnPageState(page, "验证页上没找到扫码二维码")

		return "", fmt.Errorf("验证页（verifyType=%s verifyBiz=%s，%s）上没有扫码二维码 %s"+
			"——可能是站点改了 DOM，也可能这次不是扫码类验证: %w",
			verifyType, verifyBiz, pageURL, verifyQrcodeSelector, err)
	}

	src, err := el.Attribute("src")
	if err != nil {
		return "", fmt.Errorf("读取二维码 src 失败（%s）: %w", pageURL, err)
	}
	if src == nil || *src == "" {
		return "", fmt.Errorf("验证页二维码 src 为空（%s）", pageURL)
	}

	return *src, nil
}

// verifyPollInterval 扫码到跳转是人操作的量级，1s 足够，再密只是白发 CDP 请求。
const verifyPollInterval = time.Second

// WaitVerified 等账号本人扫码通过：验证一过页面自己跳回 redirectPath 指向的业务页。
//
// 判据用 URL 不用元素——验证页上业务选择器一个都不存在，等元素只能等到超时。
// 但不能只判「不是验证页」：扫码失败被弹回登录墙同样满足，会被当成通过，进而把一份
// 没通过验证的 cookie 覆盖上去。要求离开 /website-login/ 这一整族页面才算数。
//
// 读 URL 走 pageInfo（browser 的 ctx），页面 deadline 烧穿后依然读得到；ctx 是后台那条
// 独立超时，跟发起请求的 ctx 无关——请求早就返回了。
func (a *VerificationAction) WaitVerified(ctx context.Context) bool {
	ticker := time.NewTicker(verifyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			info, err := pageInfo(a.page)
			if err != nil {
				// 一次 CDP 失败不当结论，下一拍再看
				continue
			}
			if isLoginWallURL(info.URL) {
				// 还在验证页，或者被弹回了登录墙，两种都还没通过
				continue
			}

			logrus.Infof("安全验证已通过，页面回到 %s", info.URL)

			return true
		}
	}
}

// verifyExpiredText 二维码过期后 #captcha-div 里出现的文案关键词。
//
// 只能靠读文案：实测这张码自己永远不会换（连续采样 2.5 分钟，src 的 sha256 一直
// 不变），2~3 分钟后文案从「二维码1分钟失效」变成「已过期，点击二维码区域刷新」——
// 站点把换码这件事整个交给了点击，不读文案就不知道该点了。
const verifyExpiredText = "已过期"

const (
	verifyCaptchaDivSelector    = `#captcha-div`
	verifyQrcodeContainerSelect = `#captcha-div .qrcode-container`
)

// qrcodeRefreshTimeout 点完刷新等新码落地的上限。
const qrcodeRefreshTimeout = 3 * time.Second

// LiveQrcode 就地读一次验证页的结果。三种形态互斥：验证已通过 / 还被验证页拦着 /
// 两者都不是（被弹回登录墙，页面上已经没有码可扫）。
type LiveQrcode struct {
	Verified  bool   // 已离开 /website-login/ 这一族页面，验证通过
	Blocked   bool   // 还停在安全验证页上，Img 有效
	Refreshed bool   // 本次是否点过刷新
	Img       string // 二维码 src，实测是 data-URI
	PageURL   string // 页面当前落点
}

// Live 就地读一次当前状态：过期了先点一下换新码，然后把码带回来。
//
// 全程不导航，这是本方法最重要的约束。实测点击二维码区域换码时，页面 URL 和
// verifyUuid 一个字都不变，同一个挑战会话、同一个 redirectPath 被完整保住；而重新
// 导航会换掉 verifyUuid 和 redirectPath，用户手机上正在扫的那张码当场作废，
// 扫完了服务端状态零变化。
func (a *VerificationAction) Live(ctx context.Context) (*LiveQrcode, error) {
	info, err := pageInfo(a.page)
	if err != nil {
		return nil, fmt.Errorf("读取页面信息失败: %w", err)
	}

	if !isLoginWallURL(info.URL) {
		// 判据同 WaitVerified：必须离开 /website-login/ 这一整族页面才算通过。
		// 只判「不是验证页」的话，扫码失败被弹回登录墙同样满足，会被当成通过，
		// 进而把一份没通过验证的 cookie 覆盖上去。
		return &LiveQrcode{Verified: true, PageURL: info.URL}, nil
	}

	if !isCaptchaURL(info.URL) {
		// 还在这一族页面里但已经不是验证页：被弹回了登录墙。这时页面上没有验证码
		// 可读，接着读只会白等到超时，交给上层去说人话。
		return &LiveQrcode{PageURL: info.URL}, nil
	}

	page := a.page.Context(ctx).Timeout(pageTimeout)

	refreshed := refreshExpiredQrcode(page)

	verifyType, verifyBiz := verifyParams(info.URL)

	img, err := readVerifyQrcode(page, info.URL, verifyType, verifyBiz)
	if err != nil {
		return nil, err
	}

	return &LiveQrcode{Blocked: true, Refreshed: refreshed, Img: img, PageURL: info.URL}, nil
}

// Verified 单次判断验证有没有通过，判据同 WaitVerified。
//
// 给「拆浏览器之前最后看一眼」用：用户可能刚扫完就把页面关了，最后那一拍心跳没
// 上来，而验证结果只写在这个会话的 cookie jar 里，浏览器一关就没了，白扫一次。
func (a *VerificationAction) Verified() bool {
	info, err := pageInfo(a.page)
	if err != nil {
		// 读不到就当没过：宁可漏存一次 cookie，也不能把一份没通过验证的 cookie 存上去。
		return false
	}

	return !isLoginWallURL(info.URL)
}

// refreshExpiredQrcode 码过期了就照站点自己的提示点一下二维码区域换一张，返回是否点过。
//
// 用 go-rod 原生的 Element/Text/Click，不注入 JS：这里要的就是一次真实的左键点击，
// 站点的换码逻辑挂在它的事件上。
//
// 出错一律当「这次没刷成」而不是报错：刷新是尽力而为的保鲜，这一拍失败下一拍还会
// 再来；升级成错误只会让一次偶发的 CDP 失败毁掉整条链接。
func refreshExpiredQrcode(page *rod.Page) bool {
	div, err := page.Timeout(probeTimeout).Element(verifyCaptchaDivSelector)
	if err != nil {
		return false
	}

	text, err := div.Text()
	if err != nil || !strings.Contains(text, verifyExpiredText) {
		// 没过期就别点。实测未过期时点击是无害的空操作（src 一字不变），
		// 但每拍都点等于白发一次 CDP 请求。
		return false
	}

	container, err := page.Timeout(probeTimeout).Element(verifyQrcodeContainerSelect)
	if err != nil {
		return false
	}

	if err := container.Click(proto.InputMouseButtonLeft, 1); err != nil {
		logrus.Warnf("点击刷新验证二维码失败: %v", err)

		return false
	}

	waitQrcodeRefreshed(page)

	return true
}

// waitQrcodeRefreshed 等新码落地。
//
// 站点是异步换 src 的，点完马上读会读回那张已经过期的旧码，等于让用户白扫一次。
// 判据用文案复位而不是比对 src：比 src 得先把旧值拿在手上，而这一步唯一的目的
// 就是别把旧值发出去。等不到就算了，下一拍还会再判一次过期。
func waitQrcodeRefreshed(page *rod.Page) {
	deadline := time.Now().Add(qrcodeRefreshTimeout)

	for time.Now().Before(deadline) {
		time.Sleep(verifyProbeInterval)

		// 每拍重新取元素：换码时这一块会重渲染，攥着旧 handle 读只会拿到
		// detached node 的错误。单次预算也要短，否则元素一时不在就会挂满
		// 整个页面 deadline，远远超过这里的 3 秒。
		div, err := page.Timeout(verifyProbeInterval).Element(verifyCaptchaDivSelector)
		if err != nil {
			continue
		}
		if text, err := div.Text(); err == nil && !strings.Contains(text, verifyExpiredText) {
			return
		}
	}
}
