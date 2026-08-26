package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// 验证网页本体。两条路由都在 Bearer 鉴权之外，token 是唯一凭证，所以这里的每个
// 响应头都是有理由的，别随手删。

// verifyPageHandler GET /verify/:token
//
// 不校验 token：页面本身一个字都不含 token 的存在性信息，真实状态由 /state 给。
// 有效和无效的 token 拿到的是同一个页面，探测者从状态码上什么也学不到。
func (s *AppServer) verifyPageHandler(c *gin.Context) {
	setVerifyHeaders(c)
	c.Data(http.StatusOK, "text/html; charset=utf-8", verifyPageHTML)
}

// verifyStateHandler GET /verify/:token/state
//
// 一律回 200：token 无效、不存在、已过期都给同一个 {state:"expired"}，
// 不让状态码变成「这个 token 存在过」的判别器。
func (s *AppServer) verifyStateHandler(c *gin.Context) {
	setVerifyHeaders(c)

	link := s.xiaohongshuService.links.lookup(c.Param("token"))
	if link == nil {
		c.JSON(http.StatusOK, verifyStateResult{State: verifyStateExpired, Message: verifyMsgExpired})

		return
	}

	c.JSON(http.StatusOK, link.poll(s.xiaohongshuService))
}

func setVerifyHeaders(c *gin.Context) {
	// token 在 URL 里：别进缓存，别经 Referer 漏给任何第三方，别被搜索引擎收录。
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Robots-Tag", "noindex, nofollow, noarchive")
	c.Header("X-Content-Type-Options", "nosniff")

	// 页面必须自包含。CSP 在这里不是防 XSS（页面里没有任何外来输入），
	// 而是把「不许引外部资源」这条约束变成浏览器强制执行的事实。
	c.Header("Content-Security-Policy",
		"default-src 'none'; img-src data:; style-src 'unsafe-inline'; "+
			"script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")

	// corsMiddleware 全局挂了 Access-Control-Allow-Origin: *，对这条无鉴权入口
	// 没必要——空值在 gin 里就是删掉这个头。
	c.Header("Access-Control-Allow-Origin", "")
}

// verifyPageHTML 页面成品。轮询间隔从 verifyBeatInterval 现算，免得 JS 里那个数字
// 和服务端拆浏览器的判定各说各话。
//
// 用占位符替换而不是 Sprintf：模板里全是 CSS，%% 转义漏一个就是一处静默的样式错乱。
var verifyPageHTML = []byte(strings.ReplaceAll(verifyPageTemplate,
	pollIntervalPlaceholder, strconv.FormatInt(verifyBeatInterval.Milliseconds(), 10)))

// pollIntervalPlaceholder 模板里代表轮询间隔（毫秒）的占位符。
const pollIntervalPlaceholder = "__POLL_MS__"

// verifyPageTemplate 页面模板。
//
// 自包含：不引任何外部 CSS/JS/字体/图片，二维码本来就是 data-URI。
// token 不写进页面任何位置，JS 从 location.pathname 现取——写进 HTML 就等于多一处
// 可能被复制、被截图、被别的脚本读走的地方。
const verifyPageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="robots" content="noindex,nofollow">
<meta name="referrer" content="no-referrer">
<title>小红书安全验证</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--fg:#1a1a1a;--dim:#6b6b70;--line:#e5e5e8;--ok:#12833b;--bad:#c0392b}
@media (prefers-color-scheme:dark){
:root{--bg:#131316;--card:#1d1d21;--fg:#ededf0;--dim:#9a9aa2;--line:#2e2e34;--ok:#4ade80;--bad:#f87171}
}
*{box-sizing:border-box}
body{margin:0;padding:24px 16px;background:var(--bg);color:var(--fg);
font:16px/1.6 -apple-system,BlinkMacSystemFont,"PingFang SC","Microsoft YaHei",sans-serif;
display:flex;justify-content:center}
main{width:100%;max-width:420px;text-align:center}
h1{margin:0 0 4px;font-size:20px}
.sub{margin:0 0 20px;color:var(--dim);font-size:14px}
.card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:20px}
.frame[hidden]{display:none}
.frame{width:min(70vw,260px);aspect-ratio:1;margin:0 auto;display:flex;align-items:center;
justify-content:center;background:#fff;border-radius:12px;overflow:hidden}
.frame img{width:100%;height:100%;object-fit:contain;image-rendering:pixelated}
.frame .ph{color:#8a8a90;font-size:14px;padding:0 16px}
.badge{margin:16px 0 0;font-size:15px;font-weight:600}
.badge.ok{color:var(--ok)}
.badge.bad{color:var(--bad)}
.msg{margin:8px 0 0;color:var(--dim);font-size:14px;word-break:break-word}
.foot{margin:18px 0 0;color:var(--dim);font-size:12px;line-height:1.7}
.dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--dim);
margin-right:6px;vertical-align:1px;animation:blink 1.4s infinite}
@keyframes blink{0%,100%{opacity:.25}50%{opacity:1}}
</style>
</head>
<body>
<main>
<h1>小红书安全验证</h1>
<p class="sub" id="sub">请用<strong>已登录该账号</strong>的小红书 App 扫码验证身份</p>
<div class="card">
<div class="frame" id="frame"><span class="ph" id="ph">正在准备二维码…</span><img id="qr" alt="安全验证二维码" hidden></div>
<p class="badge" id="badge"><span class="dot"></span>正在准备…</p>
<p class="msg" id="msg"></p>
</div>
<p class="foot">本页只把小红书发来的验证码原样显示出来，由你本人扫码完成验证，
不做任何自动识别或提交。可以放心切到小红书 App 去扫，扫完回到本页即可看到结果；
页面关掉后服务端会自动释放这个浏览器。</p>
</main>
<script>
(function(){
// token 只从地址栏取，不落在页面任何位置
var api = location.pathname.replace(/\/+$/, "") + "/state";
var ph = document.getElementById("ph"), qr = document.getElementById("qr");
var badge = document.getElementById("badge"), msg = document.getElementById("msg");
var sub = document.getElementById("sub"), frame = document.getElementById("frame");
var last = "";

// 服务端每一个 state 在这里都必须有一项。漏一项就会落到 LABEL.error 那条红字终态
// 上，而 render 会跟着返回 false、永久停掉轮询——对 throttled 这种「靠页面继续拉
// /state 才会自动重试」的状态，那等于把它自己等的那个人赶走了。
var LABEL = {
  pending:    ["", '<span class="dot"></span>等待扫码…'],
  throttled:  ["", '<span class="dot"></span>小红书限流中，稍后自动重试…'],
  verified:   ["ok",  "✓ 验证已通过"],
  notblocked: ["ok",  "本次未被拦截"],
  expired:    ["bad", "链接已失效"],
  error:      ["bad", "出了点问题"]
};

function render(s){
  // 两件事要分开判：有没有码可看，和要不要接着拉。
  // throttled 时没有码（服务端正在退避），但必须接着拉——重试完全由这个轮询驱动，
  // 没人拉 /state 就永远不会重开会话，链接会一直僵到 TTL 结束。
  var showQR = s.state === "pending";
  var keepPolling = showQR || s.state === "throttled";
  var l = LABEL[s.state] || LABEL.error;
  badge.className = "badge " + l[0];
  badge.innerHTML = l[1];
  msg.textContent = s.message || "";
  // 没码可看就把扫码那一套整个收起来：留一个空相框和一句「请扫码」，
  // 只会让人以为码没加载出来
  sub.hidden = !showQR;
  frame.hidden = !showQR;
  if (showQR && s.qr) {
    if (s.qr !== last) { qr.src = s.qr; last = s.qr; }
    qr.hidden = false; ph.hidden = true;
  } else if (showQR) {
    qr.hidden = true; ph.hidden = false;
  }
  // 终态才停轮询：再拉下去只是白发请求，服务端那边也已经没有会话了
  return keepPolling;
}

var timer = 0, done = false;

function again(){ clearTimeout(timer); timer = setTimeout(tick, __POLL_MS__); }

function tick(){
  if (done) return;
  clearTimeout(timer);
  fetch(api, {cache:"no-store"})
    .then(function(r){ return r.json(); })
    .then(function(s){ if (render(s)) { again(); } else { done = true; } })
    .catch(again);
}

// 扫这张码必须离开本页（截图 → 小红书 App → 相册选图），而 iOS Safari 一进后台
// 就把 JS 整个挂起，setTimeout 不再触发——心跳恰好断在最要紧的那段时间。
// 走之前补一拍，让服务端的心跳预算从「离开这一刻」而不是上一个 2 秒开始算；
// keepalive 保证这一拍能在页面被挂起后仍然发出去。
document.addEventListener("visibilitychange", function(){
  if (done) return;
  if (document.hidden) {
    try { fetch(api, {cache:"no-store", keepalive:true}); } catch (e) {}
    return;
  }
  tick(); // 回到前台立刻看一眼，别让用户对着旧状态等下一个 2 秒
});

tick();
})();
</script>
</body>
</html>
`
