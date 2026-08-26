package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestLoginSessions 固定「同一时刻只保留一个待扫码会话」这条约束。
//
// 这是浏览器不再堆积的依据：每个待扫码会话都占着一个浏览器活到超时为止，
// 只要新会话没能关掉旧的，进程就会累积。
func TestLoginSessions(t *testing.T) {
	t.Run("开新会话会关掉上一个", func(t *testing.T) {
		var l loginSessions
		closed := 0

		l.start(func() { closed++ }, releasedNow())
		assert.Equal(t, 0, closed, "第一个会话不该被关")

		l.start(func() {}, releasedNow())
		assert.Equal(t, 1, closed, "开第二个时应关掉第一个")
	})

	t.Run("第一个会话无需关闭任何东西", func(t *testing.T) {
		var l loginSessions
		assert.NotPanics(t, func() { l.start(func() {}, releasedNow()) })
	})

	t.Run("会话结束后不会再被关第二次", func(t *testing.T) {
		var l loginSessions
		closed := 0

		seq := l.start(func() { closed++ }, releasedNow())
		l.finish(seq)

		l.start(func() {}, releasedNow())
		assert.Equal(t, 0, closed, "已结束的会话不该再被关闭")
	})

	t.Run("旧会话的收尾不会顶掉新会话", func(t *testing.T) {
		var l loginSessions
		newClosed := 0

		oldSeq := l.start(func() {}, releasedNow())
		l.start(func() { newClosed++ }, releasedNow()) // 新会话上位

		// 旧会话此时才走完收尾，它必须认出自己已不是当前会话
		l.finish(oldSeq)

		// 再开一个：如果上一步误清了登记，新会话就永远关不掉了
		l.start(func() {}, releasedNow())
		assert.Equal(t, 1, newClosed, "新会话仍应被后来者关闭")
	})

	t.Run("并发开会话时每个序号唯一", func(t *testing.T) {
		var l loginSessions
		const n = 50

		var mu sync.Mutex
		seen := make(map[uint64]bool, n)

		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seq := l.start(func() {}, releasedNow())
				mu.Lock()
				seen[seq] = true
				mu.Unlock()
			}()
		}
		wg.Wait()

		assert.Len(t, seen, n, "序号必须唯一，否则 finish 会误清别人的登记")
	})
}

// cancelPending 必须等到对方真的把名额票还回来才返回。
//
// 只叫一声就走的话，「先还后取」这句话根本没兑现：调用方紧接着就去排队取票，而对方
// 还要十几秒才关得掉浏览器，这十几秒里两张票同时被持有，全局浏览器并发掉到 0，
// 任何 search_feeds / list_feeds 都只能排队。
func TestCancelPendingWaitsForRelease(t *testing.T) {
	var l loginSessions

	released := make(chan struct{})

	var canceled atomic.Bool
	l.start(func() { canceled.Store(true) }, released)

	done := make(chan struct{})
	go func() {
		l.cancelPending()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("名额票还没还回来，cancelPending 不该返回")
	case <-time.After(100 * time.Millisecond):
	}
	assert.True(t, canceled.Load(), "该先叫对方收尾，而不是干等着")

	close(released)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("票已经还回来了，cancelPending 应当立刻返回")
	}
}

// 但也不能永远等下去：对方卡在 CDP 上不动时，宁可短暂多占一张票，
// 也不能让取码请求就此挂死。
func TestCancelPendingGivesUpAfterTimeout(t *testing.T) {
	old := scanReleaseTimeout
	scanReleaseTimeout = 100 * time.Millisecond
	t.Cleanup(func() { scanReleaseTimeout = old })

	var l loginSessions
	l.start(func() {}, make(chan struct{})) // 这个通道永远不关：对方再也不还票了

	start := time.Now()

	done := make(chan struct{})
	go func() {
		l.cancelPending()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("等超时了就该放弃并继续，不能一直挂着")
	}

	assert.GreaterOrEqual(t, time.Since(start), scanReleaseTimeout, "放弃之前该等满上限")
}

// releasedNow 表示「名额票已经还回来了」。
//
// 测试里的假会话没有真浏览器要关，start 要的那个通道给一个已关闭的即可——
// cancelPending 会等这个通道，不给就会干等到超时。
func releasedNow() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}
