package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// Live 的就地保鲜是这条路径上第二容易坏的东西（第一是选择器）：站点把换码整个交给
// 了点击，而「点错地方」和「不点」在日志里长得一模一样——用户只会看到一张永远扫不动
// 的过期码。所以要真起一次浏览器验它。
//
// 需要真 Chromium，默认跳过：XHS_BROWSER_PROBE=1 go test ./xiaohongshu -run TestLive -v
func liveProbeEnabled(t *testing.T) {
	if os.Getenv("XHS_BROWSER_PROBE") != "1" {
		t.Skip("set XHS_BROWSER_PROBE=1")
	}
}

// 过期形态：文案是「已过期」，点一下容器才换码，且换码是异步的（实测站点如此）。
// 200ms 那个延迟就是用来卡 waitQrcodeRefreshed 的——点完立刻读会读回旧码。
const expiredCaptchaHTML = `<html><head><title>安全验证</title></head><body>
<div id="captcha-div"><div class="qrcode-container" onclick="swap()">
<img class="qrcode-img" src="data:image/png;base64,T0xE">
<div class="qrcode-desc">已过期，点击二维码区域刷新</div>
</div></div>
<script>
function swap(){setTimeout(function(){
  document.querySelector("#captcha-div img.qrcode-img").src="data:image/png;base64,TkVX";
  document.querySelector("#captcha-div .qrcode-desc").textContent="二维码1分钟失效";
},200);}
</script></body></html>`

// 未过期形态：点了也不该换（站点实测是无害空操作），这里干脆连 onclick 都不给，
// 真点了就会因为码没变而被下面的断言抓住。
const freshCaptchaHTML = `<html><head><title>安全验证</title></head><body>
<div id="captcha-div"><div class="qrcode-container">
<img class="qrcode-img" src="data:image/png;base64,T0xE">
<div class="qrcode-desc">二维码1分钟失效</div>
</div></div></body></html>`

// newLivePage 起一个本地站点和一个真浏览器。
//
// 不需要 hijack 到 www.xiaohongshu.com：isCaptchaURL / isLoginWallURL 判的都只是
// 路径那一段，本机 httptest 上的 /website-login/captcha 一样满足。
func newLivePage(t *testing.T, captchaBody string) (*rod.Page, string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/website-login/captcha", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, captchaBody)
	})
	mux.HandleFunc("/website-login/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>登录</title></head><body>登录墙</body></html>`)
	})
	mux.HandleFunc("/search_result", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>搜索</title></head><body>结果</body></html>`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctrl := launcher.New().Headless(true).MustLaunch()
	b := rod.New().ControlURL(ctrl).MustConnect()
	t.Cleanup(func() { b.MustClose() })

	return b.MustPage(), srv.URL
}

const captchaQuery = "?redirectPath=%2Fsearch_result&verifyUuid=U1&verifyType=124&verifyBiz=461"

// 过期了要就地点一下换新码，而且换码后 URL 和 verifyUuid 必须一个字不变——
// 重新导航会换掉挑战会话，用户手机上正扫的那张码当场作废。
func TestLiveRefreshesExpiredQrcodeInPlace(t *testing.T) {
	liveProbeEnabled(t)

	page, base := newLivePage(t, expiredCaptchaHTML)
	page.MustNavigate(base + captchaPath + captchaQuery).MustWaitLoad()

	before := page.MustInfo().URL

	live, err := NewVerificationAction(page).Live(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	if !live.Blocked || !live.Refreshed {
		t.Fatalf("过期码应被就地刷新: blocked=%v refreshed=%v", live.Blocked, live.Refreshed)
	}
	// 关键：拿回来的必须是换过之后的新码，不是那张已经过期的旧码
	if live.Img != "data:image/png;base64,TkVX" {
		t.Fatalf("拿回的还是旧码（waitQrcodeRefreshed 没等住）: %q", live.Img)
	}

	after := page.MustInfo().URL
	if before != after {
		t.Fatalf("刷新不该改变页面地址：\n前 %s\n后 %s", before, after)
	}
}

// 没过期就别点，也别把码换掉。
func TestLiveLeavesFreshQrcodeAlone(t *testing.T) {
	liveProbeEnabled(t)

	page, base := newLivePage(t, freshCaptchaHTML)
	page.MustNavigate(base + captchaPath + captchaQuery).MustWaitLoad()

	live, err := NewVerificationAction(page).Live(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	if !live.Blocked || live.Refreshed {
		t.Fatalf("未过期不该刷新: blocked=%v refreshed=%v", live.Blocked, live.Refreshed)
	}
	if live.Img != "data:image/png;base64,T0xE" {
		t.Fatalf("码不该变: %q", live.Img)
	}
}

// 离开 /website-login/ 这一族页面才算通过；被弹回登录墙不算，那时也没有码可读。
func TestLiveVerdicts(t *testing.T) {
	liveProbeEnabled(t)

	page, base := newLivePage(t, freshCaptchaHTML)
	action := NewVerificationAction(page)

	t.Run("回到业务页算通过", func(t *testing.T) {
		page.MustNavigate(base + "/search_result").MustWaitLoad()

		live, err := action.Live(context.Background())
		if err != nil || !live.Verified {
			t.Fatalf("应判为已通过: %+v err=%v", live, err)
		}
		if !action.Verified() {
			t.Fatal("Verified() 也该说通过")
		}
	})

	t.Run("被弹回登录墙不算通过", func(t *testing.T) {
		page.MustNavigate(base + "/website-login/").MustWaitLoad()

		live, err := action.Live(context.Background())
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if live.Verified || live.Blocked {
			t.Fatalf("登录墙既不是通过也不是验证页: %+v", live)
		}
		if action.Verified() {
			t.Fatal("Verified() 不该说通过——那会把一份没通过验证的 cookie 存上去")
		}
	})
}
