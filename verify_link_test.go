package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// fakeProbe 顶掉真浏览器。心跳断开、绝对上限这两条拆浏览器的路径必须有单测盯着——
// 名额票一共 2 张，漏还一次全局并发就少一张——而它们偏偏最难在真浏览器上复现。
type fakeProbe struct {
	mu       sync.Mutex
	live     *xiaohongshu.LiveQrcode
	err      error
	verified bool
	saveErr  error
	saved    atomic.Int32
	calls    atomic.Int32
}

func (p *fakeProbe) Live(context.Context) (*xiaohongshu.LiveQrcode, error) {
	p.calls.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.live, p.err
}

func (p *fakeProbe) Verified() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.verified
}

func (p *fakeProbe) SaveCookies() error {
	p.saved.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.saveErr
}

func (p *fakeProbe) set(fn func(*fakeProbe)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fn(p)
}

func blockedProbe(img string) *fakeProbe {
	return &fakeProbe{live: &xiaohongshu.LiveQrcode{Blocked: true, Img: img}}
}

// shrinkTimings 把会话的几条超时压到毫秒级，跑完还原。
func shrinkTimings(t *testing.T, beat, life time.Duration) {
	t.Helper()

	oldBeat, oldLife, oldTick := verifyBeatTimeout, verifySessionMaxLife, verifyBeatCheckInterval
	verifyBeatTimeout, verifySessionMaxLife, verifyBeatCheckInterval = beat, life, 20*time.Millisecond
	t.Cleanup(func() {
		verifyBeatTimeout, verifySessionMaxLife, verifyBeatCheckInterval = oldBeat, oldLife, oldTick
	})
}

// newTestSession 起一个假会话，返回它和「浏览器关了几次」的计数。
func newTestSession(t *testing.T, probe verifyProbe) (*XiaohongshuService, *verifyLink, *verifySession, *atomic.Int32) {
	t.Helper()

	svc := NewXiaohongshuService()

	link, err := svc.links.issue()
	require.NoError(t, err)

	var closed atomic.Int32
	sess := newVerifySession(svc, link, probe, func() { closed.Add(1) })
	t.Cleanup(sess.stop)

	link.attach(sess, "data:image/png;base64,AAAA")

	return svc, link, sess, &closed
}

func linkState(link *verifyLink) string {
	link.mu.Lock()
	defer link.mu.Unlock()

	return link.state
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("等了 %s 条件仍未满足", timeout)
}

func TestVerifyTokenShape(t *testing.T) {
	token, err := newVerifyToken()
	require.NoError(t, err)

	// 43 = 32 字节 base64url 无填充。短了就是可猜的凭证。
	assert.Len(t, token, 43)
	assert.True(t, validVerifyToken(token))

	// 两次不能一样，且必须是 URL-safe 字符集
	other, err := newVerifyToken()
	require.NoError(t, err)
	assert.NotEqual(t, token, other)
	assert.NotContains(t, token+other, "+")
	assert.NotContains(t, token+other, "/")
	assert.NotContains(t, token+other, "=")

	for _, bad := range []string{
		"", "short",
		strings.Repeat("a", 42), strings.Repeat("a", 44),
		strings.Repeat("a", 42) + "/", // 路径分隔符
		strings.Repeat("a", 42) + "+",
		strings.Repeat("a", 42) + ".",
	} {
		assert.False(t, validVerifyToken(bad), "%q 不该通过校验", bad)
	}
}

// 无效 token、不存在的 token、过期的 token 必须长得一模一样，
// 否则响应本身就成了「这个 token 存在过」的判别器。
func TestVerifyStateHidesTokenExistence(t *testing.T) {
	svc := NewXiaohongshuService()
	router := setupRoutes(NewAppServer(svc, "secret-token"))

	expired, err := svc.links.issue()
	require.NoError(t, err)
	expired.expiresAt = time.Now().Add(-time.Second)

	get := func(token string) (int, string) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, verifyPathPrefix+token+"/state", nil))

		return rec.Code, rec.Body.String()
	}

	unknown, err := newVerifyToken()
	require.NoError(t, err)

	codeBad, bodyBad := get("not-a-token")
	codeUnknown, bodyUnknown := get(unknown)
	codeExpired, bodyExpired := get(expired.token)

	assert.Equal(t, http.StatusOK, codeBad)
	assert.Equal(t, codeBad, codeUnknown)
	assert.Equal(t, codeBad, codeExpired)
	assert.Equal(t, bodyBad, bodyUnknown)
	assert.Equal(t, bodyBad, bodyExpired)
	assert.Contains(t, bodyBad, `"state":"`+verifyStateExpired+`"`)

	// 过期条目要真的从表里清掉，不能只进不出
	svc.links.mu.Lock()
	remaining := len(svc.links.items)
	svc.links.mu.Unlock()
	assert.Zero(t, remaining, "过期条目应已被清理")
}

// 网页这两条路由必须在 Bearer 鉴权之外：手机浏览器发不了 Authorization 头。
func TestVerifyRoutesArePublic(t *testing.T) {
	svc := NewXiaohongshuService()
	router := setupRoutes(NewAppServer(svc, "secret-token"))

	link, err := svc.links.issue()
	require.NoError(t, err)

	page := httptest.NewRecorder()
	router.ServeHTTP(page, httptest.NewRequest(http.MethodGet, verifyPathPrefix+link.token, nil))
	require.Equal(t, http.StatusOK, page.Code)

	body := page.Body.String()
	assert.Contains(t, body, "小红书 App 扫码")
	// 页面必须自包含，且不能把 token 写进去
	assert.NotContains(t, body, "http://")
	assert.NotContains(t, body, "https://")
	assert.NotContains(t, body, link.token)
	// 轮询间隔是从 verifyBeatInterval 算出来的，别退化成没替换的占位符
	assert.NotContains(t, body, pollIntervalPlaceholder)
	assert.Contains(t, body, "setTimeout(tick, 2000)")
	// 用户去扫码就必须切走，切走 iOS Safari 就把 JS 挂起。走之前要补一拍心跳，
	// 回来要立刻看一眼——少了这段，心跳恰好断在最要紧的那段时间。
	assert.Contains(t, body, "visibilitychange")
	assert.Contains(t, body, "keepalive:true")

	assert.Equal(t, "no-store", page.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", page.Header().Get("Referrer-Policy"))
	assert.Contains(t, page.Header().Get("X-Robots-Tag"), "noindex")
	assert.Empty(t, page.Header().Get("Access-Control-Allow-Origin"))
}

// token 落进访问日志就等于落盘，这条无鉴权入口的唯一凭证不能这么泄。
func TestVerifyPathsSkippedFromAccessLog(t *testing.T) {
	// gin.Logger 在构造时就抓走 DefaultWriter，所以要先换 writer 再建 router
	var logged strings.Builder
	old := gin.DefaultWriter
	gin.DefaultWriter = &logged
	t.Cleanup(func() { gin.DefaultWriter = old })

	router := setupRoutes(NewAppServer(NewXiaohongshuService(), ""))

	token := strings.Repeat("z", 43)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, verifyPathPrefix+token, nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, verifyPathPrefix+token+"/state", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	out := logged.String()
	assert.NotContains(t, out, token, "token 不该出现在访问日志里")
	assert.Contains(t, out, "/health", "其他路由仍要正常记日志")
}

// state 的几种取值都要能通过 HTTP 拿到，页面上每一种都有对应的说法。
func TestVerifyStateValues(t *testing.T) {
	svc := NewXiaohongshuService()
	router := setupRoutes(NewAppServer(svc, ""))

	state := func(token string) verifyStateResult {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, verifyPathPrefix+token+"/state", nil))
		require.Equal(t, http.StatusOK, rec.Code)

		var res verifyStateResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))

		return res
	}

	t.Run("pending 带码", func(t *testing.T) {
		link, err := svc.links.issue()
		require.NoError(t, err)
		// 挂一个假会话，免得 poll 真去起浏览器
		sess := newVerifySession(svc, link, blockedProbe("data:image/png;base64,QQ=="), func() {})
		t.Cleanup(sess.stop)
		link.attach(sess, "data:image/png;base64,QQ==")

		res := state(link.token)
		assert.Equal(t, verifyStatePending, res.State)
		assert.Equal(t, "data:image/png;base64,QQ==", res.QR)
		assert.Contains(t, res.Message, "自动换一张")
	})

	t.Run("verified 不再带码", func(t *testing.T) {
		link, err := svc.links.issue()
		require.NoError(t, err)
		link.finish(verifyStateVerified, verifyMsgVerified)

		res := state(link.token)
		assert.Equal(t, verifyStateVerified, res.State)
		assert.Empty(t, res.QR)
	})

	t.Run("notblocked", func(t *testing.T) {
		link, err := svc.links.issue()
		require.NoError(t, err)
		link.finish(verifyStateNotBlocked, notBlockedMessage("https://www.xiaohongshu.com/search_result?keyword=x"))

		res := state(link.token)
		assert.Equal(t, verifyStateNotBlocked, res.State)
		assert.Contains(t, res.Message, "未被安全验证拦截")
	})

	t.Run("error 是终态，不会一直重起浏览器", func(t *testing.T) {
		link, err := svc.links.issue()
		require.NoError(t, err)
		link.finish(verifyStateError, "取验证二维码失败：boom")

		for i := 0; i < 3; i++ {
			res := state(link.token)
			assert.Equal(t, verifyStateError, res.State)
		}
		link.mu.Lock()
		defer link.mu.Unlock()
		assert.False(t, link.opening, "终态不该再去起会话")
	})
}

// 心跳断掉必须拆浏览器、还名额票。这是全套设计里最容易漏的一处：
// 漏一次全局并发就少一张票，漏两次服务彻底卡死。
func TestVerifySessionTornDownWhenHeartbeatStops(t *testing.T) {
	shrinkTimings(t, 150*time.Millisecond, time.Minute)

	svc, link, sess, closed := newTestSession(t, blockedProbe("data:image/png;base64,AAAA"))

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
	assert.True(t, sess.stopped())

	// 票要还干净：scans 里不该再留着这个会话的登记
	svc.scans.mu.Lock()
	assert.Nil(t, svc.scans.cancel, "会话结束后 scans 登记应已清掉")
	svc.scans.mu.Unlock()

	// 拆完再拆也只算一次
	sess.stop()
	sess.stop()
	assert.EqualValues(t, 1, closed.Load())

	// 链接还在有效期内，下一次 /state 应该重新起一个会话——心跳断多半是用户切去
	// 小红书 App 扫码了，回到前台就该续上，和「被接管」那种终态不是一回事。
	// 只判决策，不真的调 poll——那会去起一个真浏览器连小红书。
	assert.Equal(t, verifyStatePending, linkState(link), "心跳断不是终态")
	assert.True(t, link.needsSession(), "会话被拆掉后，再拉 /state 应重新准备")
	_ = svc
}

// 心跳持续打就不能拆——用户还举着手机在扫。
func TestVerifySessionSurvivesWhileBeating(t *testing.T) {
	shrinkTimings(t, 200*time.Millisecond, time.Minute)

	_, link, sess, closed := newTestSession(t, blockedProbe("data:image/png;base64,AAAA"))

	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		sess.beat()
		time.Sleep(30 * time.Millisecond)
	}

	assert.False(t, sess.stopped(), "心跳没断不该拆")
	assert.Zero(t, closed.Load())
	assert.False(t, link.needsSession(), "会话还活着就不该再起一个")

	link.mu.Lock()
	assert.Equal(t, verifyStatePending, link.state)
	link.mu.Unlock()
}

// 绝对上限兜住「页面开着没人管」：心跳一直有，也不能永远占着名额票。
func TestVerifySessionHitsMaxLife(t *testing.T) {
	shrinkTimings(t, time.Minute, 200*time.Millisecond)

	_, _, sess, closed := newTestSession(t, blockedProbe("data:image/png;base64,AAAA"))

	go func() {
		for i := 0; i < 100; i++ {
			sess.beat()
			time.Sleep(10 * time.Millisecond)
		}
	}()

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
}

// 验证通过：存 cookie、拆浏览器、链接落成 verified 终态。
func TestVerifySessionVerifiedSavesCookiesAndStops(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	probe := blockedProbe("data:image/png;base64,AAAA")
	_, link, sess, closed := newTestSession(t, probe)

	probe.set(func(p *fakeProbe) {
		p.live = &xiaohongshu.LiveQrcode{Verified: true, PageURL: "https://www.xiaohongshu.com/search_result?keyword=x"}
	})

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
	assert.True(t, sess.stopped())
	assert.EqualValues(t, 1, probe.saved.Load())

	link.mu.Lock()
	defer link.mu.Unlock()
	assert.Equal(t, verifyStateVerified, link.state)
	assert.Empty(t, link.qr, "通过之后不该再挂着码")
}

// 用户刚扫完就关页面：最后那一拍心跳没上来，拆之前必须再看一眼，
// 否则验证结果随 cookie jar 一起没了，白扫一次。
func TestVerifySessionSavesCookiesOnLastLook(t *testing.T) {
	shrinkTimings(t, 100*time.Millisecond, time.Minute)

	// Live 一直报错（相当于页面读不动），只有 Verified 说过了
	probe := &fakeProbe{err: assertErr("读页面失败"), verified: true}
	_, link, _, closed := newTestSession(t, probe)

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
	assert.EqualValues(t, 1, probe.saved.Load(), "拆之前应补存一次 cookie")

	link.mu.Lock()
	defer link.mu.Unlock()
	assert.Equal(t, verifyStateVerified, link.state)
}

// 被弹回登录墙不是「通过」：页面上已经没有码可扫，要当场说清楚而不是干等到超时。
func TestVerifySessionLoginWallIsNotVerified(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	probe := &fakeProbe{live: &xiaohongshu.LiveQrcode{PageURL: "https://www.xiaohongshu.com/website-login/"}}
	_, link, sess, closed := newTestSession(t, probe)

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
	assert.True(t, sess.stopped())
	assert.Zero(t, probe.saved.Load(), "没通过就绝不能存 cookie")

	link.mu.Lock()
	defer link.mu.Unlock()
	assert.Equal(t, verifyStateError, link.state)
	assert.Contains(t, link.message, "登录页")
}

// 一次读页面失败不当结论：码要留着，下一拍再看。
func TestVerifySessionKeepsQrcodeOnTransientError(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	probe := &fakeProbe{err: assertErr("CDP 抖了一下")}
	_, link, sess, closed := newTestSession(t, probe)

	waitFor(t, 3*time.Second, func() bool { return probe.calls.Load() >= 2 })
	assert.False(t, sess.stopped())
	assert.Zero(t, closed.Load())

	link.mu.Lock()
	defer link.mu.Unlock()
	assert.Equal(t, verifyStatePending, link.state)
	assert.NotEmpty(t, link.qr, "读失败不该把用户正在扫的码撤掉")
}

// 链接过期时把还挂着的浏览器一起拆掉，不然就是一张永远回不来的名额票。
func TestVerifyLinkExpiryTearsDownSession(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	svc := NewXiaohongshuService()

	link, err := svc.links.issue()
	require.NoError(t, err)

	var closed atomic.Int32
	sess := newVerifySession(svc, link, blockedProbe("data:image/png;base64,AAAA"), func() { closed.Add(1) })
	t.Cleanup(sess.stop)
	link.attach(sess, "data:image/png;base64,AAAA")

	link.expiresAt = time.Now().Add(-time.Second)
	svc.links.sweep()

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
}

// 新会话进 scans 会把上一个挤掉——名额票只有 2 张，同一时刻只能留一个待扫码浏览器。
//
// 被挤掉必须是终态：poll 要是照旧重开一个，重开的第一步 cancelPending 又会把接管者
// 杀掉，两边每 2 秒互相驱逐一次、每轮白起一整套 Chromium，谁都活不到被扫。
func TestVerifySessionSupersededByNewScan(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	svc, link, first, closed := newTestSession(t, blockedProbe("data:image/png;base64,AAAA"))

	// 模拟又调了一次 get_verification_qrcode
	svc.scans.start(func() {})

	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })
	assert.True(t, first.stopped())

	// 等终态落定再 poll：还没落定时 poll 会去起一个真浏览器
	waitFor(t, 3*time.Second, func() bool { return linkState(link) == verifyStateExpired })

	// 走真实路径：poll 命中终态就直接返回，一步都不碰浏览器
	res := link.poll(svc)
	assert.Equal(t, verifyStateExpired, res.State)
	assert.Contains(t, res.Message, "接管")
	assert.Empty(t, res.QR)

	link.mu.Lock()
	defer link.mu.Unlock()
	assert.False(t, link.opening, "被接管之后不该再去起会话")
}

// 发新链接就把旧的全部作废。两条链接同时活着，各自的页面会每 2 秒互相把对方的
// 浏览器挤掉一次，直到 15 分钟 TTL 到期都没人能扫成。
func TestVerifyLinkIssueSupersedesPrevious(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	svc := NewXiaohongshuService()

	old, err := svc.links.issue()
	require.NoError(t, err)

	var closed atomic.Int32
	sess := newVerifySession(svc, old, blockedProbe("data:image/png;base64,AAAA"), func() { closed.Add(1) })
	t.Cleanup(sess.stop)
	old.attach(sess, "data:image/png;base64,AAAA")

	fresh, err := svc.links.issue()
	require.NoError(t, err)

	// 旧链接的浏览器要还回去，旧页面下一拍拿到终态、停轮询
	waitFor(t, 3*time.Second, func() bool { return closed.Load() == 1 })

	res := old.poll(svc)
	assert.Equal(t, verifyStateExpired, res.State)
	assert.Empty(t, res.QR)

	// 表里只剩最新那条：旧 token 连查都查不到了
	assert.Nil(t, svc.links.lookup(old.token))
	assert.Same(t, fresh, svc.links.lookup(fresh.token))
}

// 没配 XHS_PUBLIC_BASE_URL 就不许拼链接：反代后面 Host 常常是 127.0.0.1:18060，
// 猜出来的链接用户点不开，比没有更糟。
func TestVerifyLinkAddress(t *testing.T) {
	token := strings.Repeat("a", 43)

	t.Run("未配置只给相对路径", func(t *testing.T) {
		t.Setenv(publicBaseURLEnv, "")
		full, path := verifyLinkAddress(token)
		assert.Empty(t, full)
		assert.Equal(t, verifyPathPrefix+token, path)
	})

	t.Run("配置了就给完整链接，末尾斜杠不重复", func(t *testing.T) {
		t.Setenv(publicBaseURLEnv, "https://api.zhangzr.xyz/xhs/")
		full, path := verifyLinkAddress(token)
		assert.Equal(t, "https://api.zhangzr.xyz/xhs"+verifyPathPrefix+token, full)
		assert.Equal(t, verifyPathPrefix+token, path)
	})

	t.Run("配歪了当没配", func(t *testing.T) {
		t.Setenv(publicBaseURLEnv, "api.zhangzr.xyz")
		full, _ := verifyLinkAddress(token)
		assert.Empty(t, full, "缺 scheme 的地址拼出来点不开，宁可不给")
	})
}

// 工具文案里必须真的把链接交出去；没配环境变量时要说清楚该配什么，
// 而不是默默给一条错的。
func TestVerificationLinkHint(t *testing.T) {
	withURL := &VerificationQrcodeResponse{
		VerifyURL:  "https://api.zhangzr.xyz/xhs/verify/abc",
		VerifyPath: "/verify/abc",
	}
	assert.Contains(t, verificationLinkHint(withURL), "https://api.zhangzr.xyz/xhs/verify/abc")

	noURL := &VerificationQrcodeResponse{VerifyPath: "/verify/abc"}
	hint := verificationLinkHint(noURL)
	assert.Contains(t, hint, publicBaseURLEnv)
	assert.Contains(t, hint, "/verify/abc")
	assert.NotContains(t, hint, "http")

	assert.Empty(t, verificationLinkHint(&VerificationQrcodeResponse{}))
}

// assertErr 只为造一个非 nil error
type assertErr string

func (e assertErr) Error() string { return string(e) }

// Chromium 自己先崩了的话 headless_browser.Close 是 panic（rod 的 MustClose）。
// stop 有一个裸 goroutine 调用点，漏出去就是整个进程退出。
func TestVerifySessionSurvivesPanickyClose(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	svc := NewXiaohongshuService()

	link, err := svc.links.issue()
	require.NoError(t, err)

	var closed atomic.Int32
	sess := newVerifySession(svc, link, blockedProbe("data:image/png;base64,AAAA"), func() {
		closed.Add(1)
		panic("browser already gone")
	})

	assert.NotPanics(t, sess.stop)
	assert.EqualValues(t, 1, closed.Load())
	assert.True(t, sess.stopped())
}
