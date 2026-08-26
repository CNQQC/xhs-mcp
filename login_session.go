package main

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// loginSessions 管理「已发出二维码、还在等扫码」的会话——登录扫码和安全验证扫码
// 共用这一个登记表。
//
// 取一次二维码就要留一个浏览器活着等扫码，否则检测不到登录、也存不了 cookie。
// 但没有任何东西拦着重复调用，于是每调一次就多一个浏览器活到超时为止。
// 这里的约束是：同一时刻只保留一个待扫码会话，开新的就把旧的关掉。
//
// 两条路径必须共用同一个登记表，不能各记各的：那样同一时刻会有两个纯等待、
// 什么都不干的浏览器，正好等于默认名额上限 2，全局并发被吃到 0。现实中也不需要
// 分开——能被安全验证拦住说明已经有登录 cookie，而且两者等的是同一部手机。
type loginSessions struct {
	mu  sync.Mutex
	seq uint64
	cur *scanHandle
}

// scanHandle 当前这个待扫码会话在登记表里的样子。
//
// 分成两半是必需的：cancel 只是「叫对方开始收尾」，而真正的拆除——读完最后一拍
// 页面、关页面、等 Chromium 进程退出、归还名额票——全发生在对方自己那条 goroutine
// 里，往往还要十几秒。只拿着 cancel 的调用方无法知道票什么时候回来。
type scanHandle struct {
	cancel func()

	// released 对方彻底收完尾（浏览器关掉、名额票还回去）之后才关闭。
	// 允许为 nil：那时 cancelPending 退化成从前的「叫一声就走」。
	released <-chan struct{}
}

// scanReleaseTimeout 等上一个会话把名额票还回来的上限。
//
// 正常路径远用不到：cancel 之后对方最多再走一拍循环，然后关页面（自带 5s 上限）、
// 关浏览器。给一个上限只为兜住「对方卡在 CDP 上不动了」——那时宁可短暂多占一张票，
// 也不能让取码请求永远挂在这里。
const scanReleaseTimeout = 10 * time.Second

// start 结束上一个待扫码会话（如果有），登记新的，返回本次会话的序号。
// 序号用于 finish 判断自己是不是仍然是当前会话。
//
// 这里只叫一声、不等对方还票，和 cancelPending 是刻意的两种脾气：start 的调用方
// 手上已经攥着自己那张票了，等在这里只是拿着票干等，一点也不会让票变多。真正需要
// 等的是「还没取票、正准备去取」的那一步，见 cancelPending。
func (l *loginSessions) start(cancel func(), released <-chan struct{}) uint64 {
	l.mu.Lock()
	prev := l.cur
	l.seq++
	seq := l.seq
	l.cur = &scanHandle{cancel: cancel, released: released}
	l.mu.Unlock()

	// 放到锁外调用：取消动作会触发对方 goroutine 的收尾，避免相互等待
	if prev != nil {
		prev.cancel()
	}
	return seq
}

// cancelPending 只结束当前待扫码会话，不登记新的，并且等到它真的把名额票还回来。
//
// 给「先放掉旧会话、再去取浏览器名额」这个顺序用。start 排在取票之后，等于新会话
// 必须先抢到另一张票，才轮得到它去关掉那个已经彻底没用的旧会话——而旧会话钉着的
// 正是一张票。名额只有 2 张（XHS_BROWSER_CONCURRENCY=1 时更是必然失败），
// 这就是标准的 acquire-before-release 自饿死。
//
// 必须等：只叫一声就走的话，「先还后取」这句话根本没兑现——调用方紧接着就去
// acquireBrowserSlot，而对方还要十几秒才关得掉浏览器，这十几秒里两张票同时被
// 持有，全局浏览器并发掉到 0，任何 search_feeds / list_feeds 都只能排队。
func (l *loginSessions) cancelPending() {
	l.mu.Lock()
	prev := l.cur
	l.cur = nil
	l.mu.Unlock()

	if prev == nil {
		return
	}

	// 同 start：放到锁外调用，避免和对方 goroutine 的收尾相互等待
	prev.cancel()

	if prev.released == nil {
		return
	}

	timer := time.NewTimer(scanReleaseTimeout)
	defer timer.Stop()

	select {
	case <-prev.released:
	case <-timer.C:
		logrus.Warnf("等上一个待扫码会话还名额票超过 %s，不再等；这一轮可能短暂多占一张票", scanReleaseTimeout)
	}
}

// finish 会话自己结束时清理登记。仅当它仍是当前会话才清，
// 否则会把后来者的登记抹掉，导致后来者永远不会被 start 关闭。
func (l *loginSessions) finish(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.seq == seq {
		l.cur = nil
	}
}
