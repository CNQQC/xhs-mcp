package main

import (
	"context"
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
