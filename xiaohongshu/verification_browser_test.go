package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// 临时验证用，跑完删除。需要真实 Chromium：XHS_BROWSER_PROBE=1 go test -run TestProbe ./xiaohongshu -v
func probeEnabled(t *testing.T) {
	if os.Getenv("XHS_BROWSER_PROBE") != "1" {
		t.Skip("set XHS_BROWSER_PROBE=1")
	}
}

const captchaHTML = `<html><head><title>安全验证</title></head><body>
<div id="captcha-div"><div class="qrcode-container">
<img class="qrcode-img" src="data:image/png;base64,iVBORw0KGgo=">
<div class="qrcode-desc">为保护账号安全，请使用已登录该账号的「小红书APP」扫码验证身份</div>
</div></div></body></html>`

// loginQrHTML 登录弹窗形态：同名 .qrcode-img，但不在 #captcha-div 里
const loginQrHTML = `<html><head><title>登录</title></head><body>
<div class="login-container"><img class="qrcode-img" src="data:image/png;base64,TERTVU5H"></div>
</body></html>`

type siteOpts struct {
	redirectAfter time.Duration // >0 时客户端 location.href 跳验证页
	hydrate       bool          // 首屏是否注水 feeds
	heartbeat     bool          // 是否每 300ms 发一次 fetch（模拟站点心跳，永不 network-idle）
	captchaBody   string
}

func newSite(t *testing.T, o siteOpts) string {
	body := o.captchaBody
	if body == "" {
		body = captchaHTML
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/website-login/captcha", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/search_result", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var js strings.Builder
		if o.hydrate {
			js.WriteString(`window.__INITIAL_STATE__={search:{feeds:{_value:[{id:"1",model_type:"note",note_card:{display_title:"t"}}]}}};`)
		}
		if o.heartbeat {
			js.WriteString(`setInterval(()=>fetch("/ping"),300);`)
		}
		if o.redirectAfter > 0 {
			capURL := "/website-login/captcha?redirectPath=%2Fsearch_result&verifyUuid=U1&verifyType=124&verifyBiz=461"
			fmt.Fprintf(&js, `setTimeout(()=>{location.href=%q;}, %d);`, capURL, o.redirectAfter.Milliseconds())
		}
		fmt.Fprintf(w, `<html><head><title>搜索</title></head><body><div class="filter">筛选</div><script>%s</script></body></html>`, js.String())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

// newProbeBrowser 用 rod 的 hijack 把 www.xiaohongshu.com 的请求整个改道到本机
// httptest：浏览器眼里文档 URL 仍然是 https://www.xiaohongshu.com/...（isCaptchaURL
// 靠的就是它），但一个字节都不会发到真实站点。非目标域一律拒掉，兜住任何漏网请求。
func newProbeBrowser(t *testing.T, target string) *rod.Browser {
	ctrl := launcher.New().Headless(true).MustLaunch()
	b := rod.New().ControlURL(ctrl).MustConnect()
	t.Cleanup(func() { b.MustClose() })

	router := b.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		req := h.Request.Req()
		if req.URL.Host != "www.xiaohongshu.com" {
			t.Logf("拦掉非目标域请求: %s", req.URL)
			h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			return
		}
		req.URL.Scheme = "http"
		req.URL.Host = target
		h.LoadResponse(http.DefaultClient, true)
	})
	go router.Run()
	t.Cleanup(func() { _ = router.Stop() })

	return b
}

// findings 2/8：客户端延迟跳转下，取码工具必须和 Search 得出同一个结论
func TestProbeFetchQrcodeLateClientRedirect(t *testing.T) {
	probeEnabled(t)
	host := newSite(t, siteOpts{redirectAfter: 300 * time.Millisecond, hydrate: false})
	b := newProbeBrowser(t, host)

	page := b.MustPage()
	defer page.MustClose()

	start := time.Now()
	qr, err := NewVerificationAction(page).FetchQrcode(context.Background())
	t.Logf("FetchQrcode 耗时=%s err=%v", time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	t.Logf("blocked=%v type=%s biz=%s url=%s img=%.30q", qr.Blocked, qr.VerifyType, qr.VerifyBiz, qr.PageURL, qr.Img)
	if !qr.Blocked {
		t.Fatalf("延迟跳转形态下应判定为被拦")
	}
	if qr.VerifyType != "124" || qr.VerifyBiz != "461" {
		t.Fatalf("verify 参数不对")
	}
	if !strings.HasPrefix(qr.Img, "data:image/png;base64,iVBOR") {
		t.Fatalf("取到的不是验证页那张码: %q", qr.Img)
	}
}

// 正常搜索页：注水就绪就该立刻收工，不白等满 probeTimeout
func TestProbeFetchQrcodeNotBlockedFast(t *testing.T) {
	probeEnabled(t)
	host := newSite(t, siteOpts{hydrate: true, heartbeat: true})
	b := newProbeBrowser(t, host)

	page := b.MustPage()
	defer page.MustClose()

	start := time.Now()
	qr, err := NewVerificationAction(page).FetchQrcode(context.Background())
	elapsed := time.Since(start)
	t.Logf("FetchQrcode 耗时=%s blocked=%v url=%s err=%v", elapsed.Round(time.Millisecond), qr.Blocked, qr.PageURL, err)
	if err != nil || qr.Blocked {
		t.Fatalf("正常页面不该判被拦: blocked=%v err=%v", qr.Blocked, err)
	}
	if qr.PageURL == "" {
		t.Fatalf("未被拦也必须带回落点")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("注水已就绪却等了 %s", elapsed)
	}
}

// finding 5：不是扫码类验证时，错误里要有 URL 和 verifyType/verifyBiz
func TestProbeFetchQrcodeNonScanChallenge(t *testing.T) {
	probeEnabled(t)
	host := newSite(t, siteOpts{redirectAfter: 100 * time.Millisecond,
		captchaBody: `<html><head><title>安全验证</title></head><body><div id="other">滑块验证</div></body></html>`})
	b := newProbeBrowser(t, host)

	page := b.MustPage()
	defer page.MustClose()

	_, err := NewVerificationAction(page).FetchQrcode(context.Background())
	if err == nil {
		t.Fatal("应当报错")
	}
	t.Logf("err=%v", err)
	for _, want := range []string{"verifyType=124", "verifyBiz=461", "/website-login/captcha", "不是扫码类验证"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误里缺 %q", want)
		}
	}
}

// 登录弹窗的同名 .qrcode-img 不能被当成验证码取回
func TestProbeFetchQrcodeIgnoresLoginQrcode(t *testing.T) {
	probeEnabled(t)
	host := newSite(t, siteOpts{redirectAfter: 100 * time.Millisecond, captchaBody: loginQrHTML})
	b := newProbeBrowser(t, host)

	page := b.MustPage()
	defer page.MustClose()

	qr, err := NewVerificationAction(page).FetchQrcode(context.Background())
	if err == nil {
		t.Fatalf("取到了登录码: %+v", qr)
	}
	t.Logf("err=%v", err)
}

// finding 1：晚到的跳转要被快速识别，且带心跳的正常页面不该白等 10s
func TestProbeSearchTiming(t *testing.T) {
	probeEnabled(t)

	t.Run("晚到的跳转", func(t *testing.T) {
		host := newSite(t, siteOpts{redirectAfter: 300 * time.Millisecond, heartbeat: true})
		b := newProbeBrowser(t, host)
		page := b.MustPage()
		defer page.MustClose()

		start := time.Now()
		_, err := NewSearchAction(page).Search(context.Background(), "美食")
		elapsed := time.Since(start)
		t.Logf("Search 耗时=%s err=%v", elapsed.Round(time.Millisecond), err)
		if err == nil || !strings.Contains(err.Error(), "get_verification_qrcode") {
			t.Fatalf("应报风控拦截: %v", err)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("识别晚到的跳转花了 %s", elapsed)
		}
	})

	t.Run("有心跳的正常页面", func(t *testing.T) {
		host := newSite(t, siteOpts{hydrate: true, heartbeat: true})
		b := newProbeBrowser(t, host)
		page := b.MustPage()
		defer page.MustClose()

		start := time.Now()
		feeds, err := NewSearchAction(page).Search(context.Background(), "美食")
		elapsed := time.Since(start)
		t.Logf("Search 耗时=%s feeds=%d err=%v", elapsed.Round(time.Millisecond), len(feeds), err)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("永不空闲的页面上仍然白等了 %s", elapsed)
		}
	})
}
