package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xpzouying/headless_browser"
)

// fillBrowserSlots 占满全部名额，返回把它们放回去的清理函数。
func fillBrowserSlots(t *testing.T) func() {
	t.Helper()

	sem := browserSlots()
	n := cap(sem)
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		default:
			t.Fatalf("名额本应全部空闲，第 %d 张却拿不到", i+1)
		}
	}

	return func() {
		for i := 0; i < n; i++ {
			select {
			case <-sem:
			default:
			}
		}
	}
}

// queueForSlot 在后台排队，把 panic 值（成功则为 nil）送回 channel。
func queueForSlot(ctx context.Context) <-chan any {
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		acquireBrowserSlot(ctx)
	}()
	return done
}

// 名额满 + ctx 已取消：不该傻等到 browserAcquireTimeout。
func TestAcquireBrowserSlotCanceledContext(t *testing.T) {
	defer fillBrowserSlots(t)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	select {
	case r := <-queueForSlot(ctx):
		if r == nil {
			t.Fatal("名额已满，acquireBrowserSlot 却取到了名额")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ctx 已取消，排队却仍在等（兜底上限 %s）", browserAcquireTimeout)
	}
}

// 排队途中调用方放弃：等待要跟着结束，而不是继续占着队位等名额。
func TestAcquireBrowserSlotCancelWhileQueued(t *testing.T) {
	defer fillBrowserSlots(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := queueForSlot(ctx)

	// 先确认它确实排上了队
	select {
	case r := <-done:
		t.Fatalf("名额已满，acquireBrowserSlot 不该返回：%v", r)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case r := <-done:
		if r == nil {
			t.Fatal("ctx 取消后 acquireBrowserSlot 却取到了名额")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后排队没有及时结束")
	}
}

// 有空名额时走快路径，直接拿，拿完计数加一。
func TestAcquireBrowserSlotFastPath(t *testing.T) {
	sem := browserSlots()
	if len(sem) != 0 {
		t.Fatalf("测试开始时名额应全空，实际占用 %d", len(sem))
	}

	acquireBrowserSlot(context.Background())
	defer func() { <-sem }()

	if len(sem) != 1 {
		t.Fatalf("取名额后占用数应为 1，实际 %d", len(sem))
	}
}

// 建浏览器 panic 时名额必须还回去，否则漏满 cap 次闸门就永久关死。
func TestNewLeasedBrowserReleasesSlotOnBuildPanic(t *testing.T) {
	sem := browserSlots()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("build 的 panic 应该继续往上抛")
			}
		}()
		newLeasedBrowser(context.Background(), func() *headless_browser.Browser {
			panic("模拟内置浏览器不可用")
		})
	}()

	if len(sem) != 0 {
		t.Fatalf("build panic 后名额没还回去，仍占用 %d", len(sem))
	}
}

// takeSlot 占一张名额，返回只能生效一次的归还函数（同 newLeasedBrowser 里的 release）。
func takeSlot(t *testing.T) func() {
	t.Helper()
	sem := browserSlots()
	select {
	case sem <- struct{}{}:
	default:
		t.Fatal("名额本应空闲")
	}
	var once sync.Once
	return func() { once.Do(func() { <-sem }) }
}

// waitSlotsFree 等到名额全部归还，超时则失败。
func waitSlotsFree(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for len(browserSlots()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s 内名额没有归还，仍占用 %d", within, len(browserSlots()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 调用方卡死、永远走不到 Close：看门狗到点也要把名额收回来。
func TestLeasedBrowserWatchdogReclaimsSlot(t *testing.T) {
	newLease(nil, func() {}, takeSlot(t), 30*time.Millisecond)
	waitSlotsFree(t, time.Second)
}

// 关浏览器卡住（MustClose 走 Background）：Close 只等 browserCloseTimeout 就归还名额。
func TestLeasedBrowserCloseBounded(t *testing.T) {
	old := browserCloseTimeout
	browserCloseTimeout = 30 * time.Millisecond
	defer func() { browserCloseTimeout = old }()

	block := make(chan struct{})
	defer close(block)
	b := newLease(nil, func() { <-block }, takeSlot(t), time.Hour)

	start := time.Now()
	b.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close 卡了 %s", d)
	}
	waitSlotsFree(t, 0)
}

// 关浏览器 panic（Chromium 已崩溃）：不外抛，名额照还。
func TestLeasedBrowserClosePanicReleases(t *testing.T) {
	b := newLease(nil, func() { panic("模拟 MustClose 失败") }, takeSlot(t), time.Hour)
	b.Close()
	waitSlotsFree(t, 0)
}

// limitHold 只收紧不放宽。
func TestLeasedBrowserLimitHold(t *testing.T) {
	t.Run("收紧后按新上限收回", func(t *testing.T) {
		b := newLease(nil, func() {}, takeSlot(t), time.Hour)
		b.limitHold(30 * time.Millisecond)
		waitSlotsFree(t, time.Second)
	})
	t.Run("放宽无效", func(t *testing.T) {
		b := newLease(nil, func() {}, takeSlot(t), 30*time.Millisecond)
		b.limitHold(time.Hour)
		waitSlotsFree(t, time.Second)
	})
	t.Run("正常 Close 后看门狗不再动作", func(t *testing.T) {
		release := takeSlot(t)
		calls := 0
		b := newLease(nil, func() {}, func() { calls++; release() }, 30*time.Millisecond)
		b.Close()
		time.Sleep(80 * time.Millisecond)
		if calls != 1 {
			t.Fatalf("release 应只调用 1 次，实际 %d", calls)
		}
	})
}
