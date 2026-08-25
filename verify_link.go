package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// 一次性验证链接：被安全验证拦下时，除了把码透进对话里，再给一条手机浏览器能直接
// 打开的网页。页面上是一张大二维码，服务端自动保鲜，实时显示有没有通过。
//
// 这条入口在 Bearer 鉴权之外——手机浏览器发不了 Authorization 头——所以路径里那个
// 32 字节随机 token 就是唯一凭证：不可猜、不进访问日志、TTL 到了就作废。
//
// 只做搬运：码是小红书发的，扫的是账号本人的 App。不解码、不识别、不自动提交。

const (
	// verifyTokenTTL 链接有效期。用户多半是先收到消息、再走到手机跟前点开，
	// 给短了就是白发一条链接。
	verifyTokenTTL = 15 * time.Minute

	// verifyTokenTTLText 上面这个时长写给人看的说法。Duration 的 %s 是 "15m0s"，
	// 而同一段工具文案里别的时长都是「约 1 分钟」「最多等 90s」这种人话，混着看很跳。
	verifyTokenTTLText = "15 分钟"

	// verifyBeatInterval 网页拉 /state 的间隔，同时就是心跳间隔。写进页面 JS 里。
	verifyBeatInterval = 2 * time.Second
)

// 下面三个是 var 而不是 const，只为单测能把它们调小——心跳断开、绝对上限这两条
// 路径正是最容易漏掉浏览器的地方，必须能在秒级跑完，否则单测里没人会去覆盖它。
var (
	// verifyBeatCheckInterval 会话循环的节奏：查一次心跳、读一拍页面。
	// 拆除最晚发生在 verifyBeatTimeout + 这个间隔之后。
	verifyBeatCheckInterval = 2 * time.Second

	// verifyBeatTimeout 心跳断多久就拆浏览器。页面一关就没人再拉 /state，
	// 浏览器留着只是白占一张名额票（一共 2 张，见 browser_lease.go）。
	//
	// 这个预算必须盖住「用户去扫码」这段必经动作，而不是只盖住「页面关掉了」：
	// 单手机流程是截图 → 切到小红书 App → 扫一扫 → 相册选图 → 确认，iOS Safari
	// 切走的一瞬间就把页面 JS 整个挂起，setTimeout 不再触发，心跳恰恰断在最要紧的
	// 时刻。给短了触发的不是「等一下」，是把 verifyUuid 挑战连同 cookie jar 一起销毁，
	// 用户手上那张截图当场作废——扫完毫无反应，切回来又是一张新码，无限循环。
	// 3 分钟远小于 verifySessionMaxLife，名额票仍有绝对上限兜底；而真正要抢票的
	// 那条路径（再调一次 get_verification_qrcode）开头就是 cancelPending，不用等它。
	verifyBeatTimeout = 3 * time.Minute

	// verifySessionMaxLife 单个会话的绝对上限，兜住「页面开着没人管」：
	// 只看心跳的话，一个忘在后台标签页里的页面能把名额票钉到天荒地老。
	verifySessionMaxLife = 10 * time.Minute
)

// 网页看到的状态。取值要盖全，页面上每一种都有对应的说法。
const (
	verifyStatePending    = "pending"    // 有码，等扫
	verifyStateVerified   = "verified"   // 已通过
	verifyStateNotBlocked = "notblocked" // 这次访问没被拦，没有码可扫
	verifyStateExpired    = "expired"    // token 无效或已过期
	verifyStateError      = "error"      // 服务端没能给出码
)

const (
	verifyMsgPreparing  = "正在准备二维码，请稍候（服务端要现开一个浏览器，大约十几秒）…"
	verifyMsgPending    = "二维码有效期约 1 分钟，过期后服务端会自动换一张，页面保持打开即可。"
	verifyMsgVerified   = "验证已通过，登录状态已保存，可以回去继续原来的操作了。"
	verifyMsgExpired    = "链接已失效。请重新调用 get_verification_qrcode 取一条新的。"
	verifyMsgSuperseded = "这条链接已被更新的一条取代，请打开最新那条验证链接。"
	verifyMsgTakenOver  = "服务端同一时刻只保留一个待扫码会话，本链接的会话已被新的取码请求接管；请改用最新取到的那张码或那条链接。"
	verifyMsgLoginWall  = "验证没有完成，页面被弹回小红书登录页——多半是登录态已经失效，请先用 get_login_qrcode 重新登录。"
	verifyMsgNoCookieOK = "验证已通过，但服务端保存登录状态失败；请直接重试原来的操作，若仍被拦请重新取一条链接。"
)

// verifyStateResult /state 的响应体。
type verifyStateResult struct {
	State   string `json:"state"`
	QR      string `json:"qr,omitempty"` // 二维码 src，实测是 data-URI
	Message string `json:"message"`
}

// verifyTokenBytes token 的随机字节数。32 字节的猜中概率不用讨论，而这条 URL
// 是无鉴权入口的唯一凭证，短一个字节都没有理由。
const verifyTokenBytes = 32

// verifyTokenLen base64url 无填充下 32 字节固定 43 个字符。
var verifyTokenLen = base64.RawURLEncoding.EncodedLen(verifyTokenBytes)

func newVerifyToken() (string, error) {
	buf := make([]byte, verifyTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成验证链接 token 失败: %w", err)
	}

	// 用 URL-safe 变体：token 在路径里，标准 base64 的 + / 得转义，= 也只会碍事。
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validVerifyToken 先按长度和字符集把明显不是 token 的挡掉，再去查表。
func validVerifyToken(token string) bool {
	if len(token) != verifyTokenLen {
		return false
	}

	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}

	return true
}

// verifyLinks token 登记表。
type verifyLinks struct {
	mu sync.Mutex
	// 用切片不用 map：查表要常量时间逐条比对（见 lookup），而表里同时也就一两条。
	items []*verifyLink
}

// issue 发一条新链接，同时把此前发出的全部作废。
//
// 「只留最新一条」不是省事，是必须的：scans 全局只保留一个待扫码会话，两条链接
// 同时活着就会各自的页面每 2 秒把对方的浏览器挤掉一次——A 挤掉 B、B 的页面下一拍
// 重起又挤掉 A——谁都活不到被扫，每一轮还白起一整套 Chromium（十几秒、几百 MB）。
// 而工具文案本来就要求「码过期了重新调一次」，用户手机上留着旧标签页再点开新链接
// 是完全正常的路径。现实里也不需要并存：同一个账号同一时刻只可能有一个人在扫。
func (l *verifyLinks) issue() (*verifyLink, error) {
	token, err := newVerifyToken()
	if err != nil {
		return nil, err
	}

	link := &verifyLink{
		token:     token,
		expiresAt: time.Now().Add(verifyTokenTTL),
		state:     verifyStatePending,
		message:   verifyMsgPreparing,
	}

	l.mu.Lock()
	stale := l.items
	l.items = []*verifyLink{link}
	l.mu.Unlock()

	// 放到锁外收尾（同 sweep）：拆浏览器要等页面关闭和进程退出，拿着登记表的锁
	// 做这件事会把所有 /state 请求一起堵住。旧页面下一拍就拿到终态、停止轮询。
	for _, item := range stale {
		item.shutdown(verifyMsgSuperseded)
	}

	return link, nil
}

// lookup 按 token 查表。查不到、格式不对、已过期，一律返回 nil——上层对这三种情况
// 给同一个回答，不泄漏「这个 token 存在过」。
func (l *verifyLinks) lookup(token string) *verifyLink {
	if !validVerifyToken(token) {
		return nil
	}

	l.sweep()

	l.mu.Lock()
	defer l.mu.Unlock()

	want := []byte(token)

	var found *verifyLink
	for _, item := range l.items {
		// 逐条常量时间比对而不是 map 取值，也不提前 break：token 是这条无鉴权入口
		// 唯一的凭证，扫一遍一两条的代价可以忽略。
		if subtle.ConstantTimeCompare([]byte(item.token), want) == 1 {
			found = item
		}
	}

	return found
}

// sweep 清掉过期条目。表要是只进不出，跑久了就是一直涨的内存；更要紧的是过期条目
// 可能还挂着浏览器，不拆就是一张永远回不来的名额票。
func (l *verifyLinks) sweep() {
	l.mu.Lock()

	now := time.Now()
	kept := l.items[:0]

	var dead []*verifyLink
	for _, item := range l.items {
		if now.Before(item.expiresAt) {
			kept = append(kept, item)

			continue
		}
		dead = append(dead, item)
	}
	l.items = kept

	l.mu.Unlock()

	// 放到锁外收尾：拆浏览器要等页面关闭和进程退出，拿着登记表的锁做这件事
	// 会把所有 /state 请求一起堵住。
	for _, item := range dead {
		item.shutdown(verifyMsgExpired)
	}
}

// verifyLink 一条链接的全部状态。
//
// 快照（state/qr/message）和会话分开：HTTP 侧只读快照、打心跳，一步都不碰浏览器；
// 所有 CDP 操作都在会话自己那条 goroutine 里做。
type verifyLink struct {
	token     string
	expiresAt time.Time

	mu      sync.Mutex
	state   string
	message string
	qr      string
	session *verifySession
	opening bool // 正在后台起浏览器，别再起第二个
}

// poll 处理一次 /state：打一次心跳、读一份快照，必要时在后台起会话。
//
// 这个方法一步都不碰浏览器。起一整套 Chromium 实测十几秒，同步做的话页面那边会
// 先超时再重试，而每次重试都会再起一套——名额票只有 2 张，这么几下就全没了。
func (l *verifyLink) poll(svc *XiaohongshuService) verifyStateResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case verifyStateVerified, verifyStateNotBlocked, verifyStateError, verifyStateExpired:
		// 终态：不再起会话，也没有什么可等的了
		return l.resultLocked()
	}

	if !l.needsSessionLocked() {
		if l.session != nil {
			l.session.beat()
		}

		return l.resultLocked()
	}

	// 会话不在了，链接还在有效期内就重开一个。走到这里只剩两种原因：还没起过，
	// 或者心跳断过一次被拆了（用户切去小红书 App 扫码、切回来）。「被别的会话
	// 接管」不在其中——那一种已经在上面的终态里挡掉了，重开只会挤掉接管者。
	l.session = nil
	l.opening = true
	l.qr = ""
	l.message = verifyMsgPreparing

	go l.openSession(svc)

	return l.resultLocked()
}

// needsSessionLocked 这一拍要不要（重新）起会话。
//
// opening 这个闸门是必需的：起一套 Chromium 十几秒，而页面每 2 秒就拉一次，
// 少了它就是每 2 秒多起一套浏览器。
func (l *verifyLink) needsSessionLocked() bool {
	return !l.opening && (l.session == nil || l.session.stopped())
}

func (l *verifyLink) needsSession() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.needsSessionLocked()
}

// openSession 在后台起浏览器取码。
func (l *verifyLink) openSession(svc *XiaohongshuService) {
	defer func() {
		if r := recover(); r != nil {
			// 名额排队超时和内置浏览器不可用都是 panic 出来的（见 browser_lease.go）。
			// 这是一条独立 goroutine，没有 gin.Recovery 兜着，不接住就是整个进程退出。
			logrus.Errorf("准备验证会话失败: %v", r)
			l.finish(verifyStateError, fmt.Sprintf("服务端准备二维码失败：%v；请重新取一条链接。", r))
		}
	}()

	// ctx 用 Background：这条 goroutine 跟触发它的那个 /state 请求早就没关系了，
	// 挂上请求 ctx 只会让页面刚拉完一拍就把正在起的浏览器取消掉。
	sess, qrcode, err := svc.startVerifySession(context.Background(), l)

	switch {
	case err != nil:
		// 做成终态而不是下一拍重试：页面每 2 秒拉一次，重试就意味着每 2 秒重起
		// 一套 Chromium，比给一句明确的失败糟得多。用户重新取一条链接即可。
		l.finish(verifyStateError, "取验证二维码失败："+err.Error())
	case sess == nil:
		l.finish(verifyStateNotBlocked, notBlockedMessage(qrcode.PageURL))
	default:
		l.attach(sess, qrcode.Img)
	}
}

// attach 会话就绪，挂上去。
//
// 起浏览器那十几秒里链接可能正好过期、被 shutdown 摘掉，于是这里会把一个活会话
// 挂到一条已经不在表里的链接上。不特判：没人再拉它的 /state，心跳很快就断，
// 会话自己的看门狗会在 verifyBeatTimeout 之后把浏览器拆掉、把票还回去。
func (l *verifyLink) attach(sess *verifySession, img string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.opening = false
	l.session = sess
	l.state = verifyStatePending
	l.qr = img
	l.message = verifyMsgPending
}

// publish 会话那条 goroutine 每拍更新一次快照。
func (l *verifyLink) publish(sess *verifySession, qr string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.session != sess {
		// 已经被新会话取代或者链接已作废，别把旧会话的码盖回去
		return
	}

	l.state = verifyStatePending
	l.qr = qr
	l.message = verifyMsgPending
}

// finish 落终态。会话指针一并摘掉——终态之后 poll 不会再看它，留着只会让人以为
// 还有东西活着。
func (l *verifyLink) finish(state, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.opening = false
	l.session = nil
	l.state = state
	l.message = message
	l.qr = ""
}

// finishFrom 落终态，但仅当 sess 仍是这条链接当前的会话。
//
// 给「会话自己发现被接管了」用：链接可能同时正好过期、或者已经挂上了别的会话，
// 那时旧会话再写一句「你被接管了」只会把更准确的说法盖掉。
func (l *verifyLink) finishFrom(sess *verifySession, state, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.session != sess {
		return
	}

	l.opening = false
	l.session = nil
	l.state = state
	l.message = message
	l.qr = ""
}

// shutdown 作废一条链接（过期，或被新发的链接取代），把还挂着的浏览器拆掉。
//
// 一律落成 expired 这个终态：页面上就是「链接已失效」，poll 不会再起会话，
// 轮询也就停了——两条链接互相驱逐的活锁正是靠这一点断掉的。
func (l *verifyLink) shutdown(message string) {
	l.mu.Lock()
	sess := l.session
	l.session = nil
	l.state = verifyStateExpired
	l.message = message
	l.qr = ""
	l.mu.Unlock()

	if sess != nil {
		// 异步拆：关浏览器要等页面关闭和进程退出，不该让一个 /state 请求替过期
		// 链接背这几秒。stop 自己保证只真正执行一次。
		go sess.stop()
	}
}

func (l *verifyLink) resultLocked() verifyStateResult {
	// 二维码不是 data-URI 就别往页面上发。页面的 CSP 只放行 img-src data:（这是
	// 「页面自包含」那条约束的强制手段），发一个远端地址过去只会被浏览器拦掉，
	// 用户看到一个空白相框、状态还是「等待扫码」，完全不知道发生了什么。
	// mcp_handlers.go 的 splitDataURI 早就为「站点改成远端图片地址」留了降级分支，
	// 网页这条路也得有一条，而且要说清楚该改走哪边。
	if l.qr != "" && !strings.HasPrefix(l.qr, "data:") {
		return verifyStateResult{
			State:   verifyStateError,
			Message: "二维码不再是内嵌图片，网页这条路暂时显示不了；请回到对话里用 get_verification_qrcode 取码。",
		}
	}

	return verifyStateResult{State: l.state, QR: l.qr, Message: l.message}
}

// verifyProbe 会话每一拍要做的事。
//
// 抽成接口只为一件事：心跳断开、绝对上限这两条拆浏览器的路径能不起真浏览器就跑单测。
// 名额票一共 2 张，漏还一次全局并发就少一张，漏两次服务彻底卡死——这条路径必须有
// 单测盯着，而它偏偏是最难在真浏览器上复现的。
type verifyProbe interface {
	Live(ctx context.Context) (*xiaohongshu.LiveQrcode, error)
	Verified() bool
	SaveCookies() error
}

// verifySession 一个待扫码的浏览器会话，由网页心跳续命。
//
// 整个会话只有 run 那一条 goroutine 碰浏览器：读页面、过期就点一下换码、判断有没有
// 通过，全在里面。两个驱动源交叉发 CDP 是这套东西最容易出事的地方——用户开两个
// 标签页就会发生，一路正在点刷新、另一路正在读 src。
type verifySession struct {
	svc   *XiaohongshuService
	link  *verifyLink
	probe verifyProbe

	seq          uint64
	cancel       context.CancelFunc
	closeBrowser func()

	// 三条预算在会话出生时就定死，循环里不再回头读包级变量：一个已经跑起来的
	// 会话不该因为别人改了配置就换一套超时，单测也不必担心改这几个变量会踩到
	// 上一个测试还没退干净的 goroutine。
	beatTimeout time.Duration
	tickEvery   time.Duration
	maxLife     time.Duration

	// takenOver 「我是被别人从 scans 里挤掉的」，而不是自己超时结束的。
	//
	// 这个区分是必需的，不能只看 ctx.Done：poll 在会话停掉之后会重开一个，而重开的
	// 第一步就是 cancelPending——它又会把顶替自己的那个会话杀掉。两条都无条件重开
	// 就是没有收敛点的互相驱逐。被接管是终态（链接改说「用最新那条」），只有心跳
	// 超时才是「用户切走扫码去了，回来再续」这种该重开的情形。
	takenOver atomic.Bool

	lastBeat atomic.Int64 // UnixNano
	done     chan struct{}
	stopOnce sync.Once
}

func newVerifySession(svc *XiaohongshuService, link *verifyLink, probe verifyProbe, closeBrowser func()) *verifySession {
	// 绝对上限兜底：心跳只要不断，会话就会一直续下去。
	ctx, cancel := context.WithTimeout(context.Background(), verifySessionMaxLife)

	sess := &verifySession{
		svc:          svc,
		link:         link,
		probe:        probe,
		cancel:       cancel,
		closeBrowser: closeBrowser,
		done:         make(chan struct{}),
		beatTimeout:  verifyBeatTimeout,
		tickEvery:    verifyBeatCheckInterval,
		maxLife:      verifySessionMaxLife,
	}
	sess.beat()

	// 登记进 scans：登录扫码和验证扫码共用同一张表，同一时刻只留一个待扫码会话。
	// 名额票一共 2 张，两个纯等待、什么都不干的浏览器就能把闸门整体关死。
	//
	// 包一层记下 takenOver：scans 只会在「别人来了」时调这个 cancel，而 stop 走的是
	// 裸 cancel。于是「被接管」和「自己超时」在 ctx.Done 那边就分得开了。
	sess.seq = svc.scans.start(func() {
		sess.takenOver.Store(true)
		cancel()
	})
	logrus.Infof("验证网页会话 #%d 已就绪，心跳上限 %s，绝对上限 %s",
		sess.seq, sess.beatTimeout, sess.maxLife)

	go sess.run(ctx)

	return sess
}

func (s *verifySession) run(ctx context.Context) {
	reason := s.loop(ctx)
	if reason == "" {
		// 循环里已经收过尾了（验证通过 / 被弹回登录墙）
		return
	}

	logrus.Infof("验证网页会话 #%d %s，拆掉浏览器", s.seq, reason)

	// 拆之前最后看一眼：用户可能刚扫完就把页面关了，最后那一拍心跳没上来。
	// 验证结果只写在这个会话的 cookie jar 里，浏览器一关就没了，白扫一次。
	switch {
	case s.probe.Verified():
		s.saveVerified()

	case s.takenOver.Load():
		// 被接管就到此为止，不留给 poll 去重开：重开的第一步 cancelPending 会把
		// 接管者杀掉，两边就此无限互相驱逐。落终态，页面停轮询，用最新那条。
		s.link.finishFrom(s, verifyStateExpired, verifyMsgTakenOver)
	}

	s.stop()
}

// loop 返回非空字符串表示「该拆了」，返回空串表示循环里已经收过尾。
func (s *verifySession) loop(ctx context.Context) string {
	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if s.takenOver.Load() {
				return "被新的待扫码会话接管"
			}

			return fmt.Sprintf("已到绝对上限 %s", s.maxLife)
		case <-s.done:
			return ""
		case <-ticker.C:
		}

		if idle := time.Since(s.beatAt()); idle > s.beatTimeout {
			return fmt.Sprintf("心跳中断 %s，网页多半已经关掉", idle.Round(time.Second))
		}

		if s.tick(ctx) {
			return ""
		}
	}
}

// tick 读一拍页面状态，返回 true 表示会话已经收尾。
func (s *verifySession) tick(ctx context.Context) bool {
	live, err := s.probe.Live(ctx)
	if err != nil {
		// 一次读失败不当结论，下一拍再看。页面上原来那张码继续挂着，
		// 总比把用户正举着手机对着的图撤掉强。
		logrus.Warnf("验证网页会话 #%d 读页面失败: %v", s.seq, err)

		return false
	}

	switch {
	case live.Verified:
		s.saveVerified()
		s.stop()

		return true

	case live.Blocked:
		if live.Refreshed {
			logrus.Infof("验证网页会话 #%d 的二维码已过期，就地点击换了一张新码", s.seq)
		}
		s.link.publish(s, live.Img)

		return false

	default:
		// 还在 /website-login/ 里但已经不是验证页：被弹回了登录墙，页面上没有码可扫，
		// 继续等只会等到心跳超时，不如当场说清楚。
		logrus.Warnf("验证网页会话 #%d 被弹回登录墙：%s", s.seq, live.PageURL)
		s.link.finish(verifyStateError, verifyMsgLoginWall)
		s.stop()

		return true
	}
}

func (s *verifySession) saveVerified() {
	if err := s.probe.SaveCookies(); err != nil {
		logrus.Errorf("安全验证已通过但保存 cookies 失败，会话 #%d: %v", s.seq, err)
		s.link.finish(verifyStateVerified, verifyMsgNoCookieOK)

		return
	}

	logrus.Infof("安全验证已通过，cookies 已保存，会话 #%d", s.seq)
	s.link.finish(verifyStateVerified, verifyMsgVerified)
}

func (s *verifySession) beat() { s.lastBeat.Store(time.Now().UnixNano()) }

func (s *verifySession) beatAt() time.Time { return time.Unix(0, s.lastBeat.Load()) }

func (s *verifySession) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// stop 拆会话。所有路径都汇到这里——正常收尾、心跳断、绝对上限、验证通过、链接过期、
// 被新会话取代——且只会真正执行一次。名额票一共 2 张，漏还一次全局并发就少一张，
// 漏两次整个服务卡死。
func (s *verifySession) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.svc.scans.finish(s.seq)
		s.cancel()

		// headless_browser.Close 走的是 rod 的 MustClose：Chromium 自己先崩了的话
		// 这一步是 panic 而不是 error。stop 有一个调用点是裸 goroutine
		// （shutdown 里的 go sess.stop()），没有 gin.Recovery 兜着，漏出去就是整个
		// 进程退出。名额票倒是不会丢——leasedBrowser.Close 用 defer 还票。
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorf("关闭验证网页会话 #%d 的浏览器时出错: %v", s.seq, r)
			}
		}()

		s.closeBrowser()
	})
}

// verifyPathPrefix 验证网页的路径前缀。这一族路由故意放在 Bearer 鉴权组之外——
// 手机浏览器发不了 Authorization 头，路径里那个 token 本身就是凭证。
const verifyPathPrefix = "/verify/"

// publicBaseURLEnv 本服务的公网基址，例如 https://api.zhangzr.xyz/xhs。
const publicBaseURLEnv = "XHS_PUBLIC_BASE_URL"

// verifyLinkAddress 返回可点击的完整链接和相对路径。
//
// 没配 XHS_PUBLIC_BASE_URL 就只给相对路径，绝不拿请求里的 Host 猜一条出来：反代
// 后面 Host 常常是 127.0.0.1:18060，猜出来的链接用户点不开，比没有更糟——用户会以为
// 功能坏了，而不是知道该去配一个环境变量。
func verifyLinkAddress(token string) (fullURL, path string) {
	path = verifyPathPrefix + token

	base := strings.TrimRight(os.Getenv(publicBaseURLEnv), "/")
	if base == "" {
		return "", path
	}

	// 查询串和 fragment 也要挡掉，不能只看 scheme 和 host。base 带 ?x=y 时下面那句
	// 字符串拼接会把 token 接到查询串里而不是路径里：链接既路由不到 /verify/:token，
	// 又绕过了按路径前缀判断的 skipVerifyAccessLog，token 反而被原样写进访问日志——
	// 正是这套设计最要防的那件事。
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		logrus.Warnf("%s=%q 不是合法的绝对地址（只接受 scheme://host[/prefix]，不能带查询串或 fragment），忽略",
			publicBaseURLEnv, base)

		return "", path
	}

	return base + path, path
}
