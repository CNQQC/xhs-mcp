package main

import "sync"

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
	mu     sync.Mutex
	seq    uint64
	cancel func()
}

// start 结束上一个待扫码会话（如果有），登记新的，返回本次会话的序号。
// 序号用于 finish 判断自己是不是仍然是当前会话。
func (l *loginSessions) start(cancel func()) uint64 {
	l.mu.Lock()
	prev := l.cancel
	l.seq++
	seq := l.seq
	l.cancel = cancel
	l.mu.Unlock()

	// 放到锁外调用：取消动作会触发对方 goroutine 的收尾，避免相互等待
	if prev != nil {
		prev()
	}
	return seq
}

// cancelPending 只结束当前待扫码会话，不登记新的。
//
// 给「先放掉旧会话、再去取浏览器名额」这个顺序用。start 排在取票之后，等于新会话
// 必须先抢到另一张票，才轮得到它去关掉那个已经彻底没用的旧会话——而旧会话钉着的
// 正是一张票。名额只有 2 张（XHS_BROWSER_CONCURRENCY=1 时更是必然失败），
// 这就是标准的 acquire-before-release 自饿死。
func (l *loginSessions) cancelPending() {
	l.mu.Lock()
	prev := l.cancel
	l.cancel = nil
	l.mu.Unlock()

	// 同 start：放到锁外调用，避免和对方 goroutine 的收尾相互等待
	if prev != nil {
		prev()
	}
}

// finish 会话自己结束时清理登记。仅当它仍是当前会话才清，
// 否则会把后来者的登记抹掉，导致后来者永远不会被 start 关闭。
func (l *loginSessions) finish(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.seq == seq {
		l.cancel = nil
	}
}
