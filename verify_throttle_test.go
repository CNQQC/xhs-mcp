package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// 撞上验证页限流之后的退避。applyOpenResult 正是为「不起真浏览器也能跑单测」
// 才从 openSession 里拆出来的，四条分支的落点必须钉死——尤其「限流该退避、别的
// 错误该落终态」这个区分：判反了要么把用户手上那条 15 分钟有效的链接当场判死，
// 要么对着一条注定失败的链接每 2 秒重起一套 Chromium。

// 限流是一分钟就能自愈的临时状态，落非终态的 throttled，退避到点由还开着的网页驱动重试。
func TestApplyOpenResultThrottledBacksOff(t *testing.T) {
	link := newVerifyLink()

	// 生产路径上这个哨兵是被 fmt.Errorf 包过好几层才传上来的（见 readVerifyQrcode），
	// 只认裸哨兵等于这条分支永远走不到，限流会被当成普通错误落成终态。
	err := fmt.Errorf("取验证二维码失败: %w",
		fmt.Errorf("验证页上没有二维码: %w", errors.ErrVerifyThrottled))

	link.applyOpenResult(nil, nil, err)

	link.mu.Lock()
	state, message, tries, retryAt, opening := link.state, link.message, link.throttleTries, link.retryAt, link.opening
	link.mu.Unlock()

	assert.Equal(t, verifyStateThrottled, state, "限流不是终态，不能落成 error")
	assert.Equal(t, verifyMsgThrottled, message)
	assert.Equal(t, 1, tries, "退避阶梯要往前走一档")
	assert.True(t, retryAt.After(time.Now()), "重试时间要落在将来，否则下一拍就又去撞一次限流")
	assert.False(t, opening, "退避期间闸门要放开，到点才好重开")

	// 退避窗口里 poll 不许重开会话：每一次「重试」都是又一次导航到那个正在限流的
	// 页面，每 2 秒来一发就是自己把自己钉死在限流态里，还白起一整套 Chromium。
	res := link.poll(NewXiaohongshuService())
	assert.Equal(t, verifyStateThrottled, res.State)
	assert.Empty(t, res.QR)
	assert.Contains(t, res.Message, "自动重试")
	assert.False(t, link.needsSession(), "退避没到点就不该再起会话")

	// 到点了就该能重开——这正是它跟终态的区别
	link.mu.Lock()
	link.retryAt = time.Time{}
	link.mu.Unlock()
	assert.True(t, link.needsSession(), "退避到点后必须能重新起会话")
}

// 不是限流的错误一律落终态：页面每 2 秒拉一次，下一拍重试就意味着每 2 秒重起一套
// Chromium，比给一句明确的失败糟得多。
func TestApplyOpenResultOtherErrorIsTerminal(t *testing.T) {
	link := newVerifyLink()

	link.applyOpenResult(nil, nil, assertErr("站点改了 DOM"))

	link.mu.Lock()
	defer link.mu.Unlock()

	assert.Equal(t, verifyStateError, link.state)
	assert.Contains(t, link.message, "站点改了 DOM")
	assert.Zero(t, link.throttleTries, "不是限流就别动退避阶梯")
	assert.True(t, link.retryAt.IsZero())
	assert.False(t, link.opening)
}

// 这次访问压根没被拦：没有码可扫，把落点一并说清楚。
func TestApplyOpenResultNotBlocked(t *testing.T) {
	link := newVerifyLink()

	pageURL := "https://www.xiaohongshu.com/search_result?keyword=x"
	link.applyOpenResult(nil, &xiaohongshu.VerificationQrcode{PageURL: pageURL}, nil)

	link.mu.Lock()
	defer link.mu.Unlock()

	assert.Equal(t, verifyStateNotBlocked, link.state)
	assert.Contains(t, link.message, "未被安全验证拦截")
	assert.Contains(t, link.message, pageURL, "落点要带上，不然分不清是真没被拦还是漏判了")
	assert.Empty(t, link.qr)
}

// 真取到码：挂上会话，并且把退避阶梯清零——限流已经过去了。
//
// 不清零的话，下一次再撞上限流会直接从更长的一档起步，几轮就把一条本来还有十几
// 分钟寿命的链接耗成终态。
func TestApplyOpenResultAttachesAndClearsBackoff(t *testing.T) {
	shrinkTimings(t, time.Minute, time.Minute)

	svc := NewXiaohongshuService()

	link, err := svc.links.issue()
	require.NoError(t, err)

	// 先假装前两拍已经被限流退避过
	link.mu.Lock()
	link.state = verifyStateThrottled
	link.message = verifyMsgThrottled
	link.throttleTries = 2
	link.retryAt = time.Now().Add(time.Minute)
	link.mu.Unlock()

	const img = "data:image/png;base64,AAAA"

	sess := newVerifySession(svc, link, blockedProbe(img), func() {})
	t.Cleanup(sess.stop)

	link.applyOpenResult(sess, &xiaohongshu.VerificationQrcode{Blocked: true, Img: img}, nil)

	link.mu.Lock()
	defer link.mu.Unlock()

	assert.Equal(t, verifyStatePending, link.state)
	assert.Equal(t, img, link.qr)
	assert.Same(t, sess, link.session)
	assert.Zero(t, link.throttleTries, "取到码说明限流过去了，阶梯要清零")
	assert.True(t, link.retryAt.IsZero(), "退避到期时间要一起清掉")
	assert.False(t, link.opening)
}

// 退避阶梯必须有尽头。每一次重试都是又一次导航到那个正在限流的页面，跟当初把我们
// 打进限流的动作一模一样；站点这类限流通常是滑动窗口，无限重试是在加重它，不是在
// 躲开它——期间每轮还白起一整套 Chromium、白占一张名额票、顺手 cancelPending 掉
// 别人的待扫码会话。
func TestBackoffExhaustsLadderThenGivesUp(t *testing.T) {
	link := newVerifyLink()
	err := fmt.Errorf("取验证二维码失败: %w", errors.ErrVerifyThrottled)

	for i, base := range verifyThrottleBackoffs {
		link.backoff(err)

		link.mu.Lock()
		state, tries, wait := link.state, link.throttleTries, time.Until(link.retryAt)
		link.mu.Unlock()

		assert.Equal(t, verifyStateThrottled, state, "第 %d 档还不该放弃", i+1)
		assert.Equal(t, i+1, tries)

		// 每一档都得按阶梯取值，且抖动只往长了叠
		assert.Greater(t, wait, base-time.Second, "第 %d 档退避不该短于阶梯值 %s", i+1, base)
		assert.Less(t, wait, jitterCeiling(base), "第 %d 档抖动超过了两成", i+1)
	}

	// 阶梯用完：落终态，页面停轮询，话要说成「过几分钟重新取一条」
	link.backoff(err)

	link.mu.Lock()
	defer link.mu.Unlock()

	assert.Equal(t, verifyStateError, link.state, "退避次数用完必须落终态，不能无限退避")
	assert.Equal(t, verifyMsgThrottledOut, link.message)
	assert.Nil(t, link.session)
	assert.False(t, link.opening, "终态之后不该再去起会话")
}

// 抖动只往长了抖：卡着整点重试正是滑动窗口最容易再次命中的时刻，
// 而抖短了是主动提前去撞一次限流。
func TestThrottleWaitJittersUpwardOnly(t *testing.T) {
	const base = 70 * time.Second

	seen := make(map[time.Duration]bool)
	for range 300 {
		got := throttleWait(base)
		require.GreaterOrEqual(t, got, base, "退避不能比阶梯值还短")
		require.Less(t, got, jitterCeiling(base), "抖动最多再多两成")
		seen[got] = true
	}
	assert.Greater(t, len(seen), 1, "每次都返回同一个值就等于没抖")

	// 抖动算下来不足 1ns 时退回原值，而不是返回 0
	assert.Equal(t, time.Duration(1), throttleWait(1))
}

// jitterCeiling 叠满抖动后的上界（开区间）。
func jitterCeiling(base time.Duration) time.Duration {
	return time.Duration(float64(base) * (1 + verifyThrottleJitter))
}
