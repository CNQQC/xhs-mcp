package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
)

// 浏览器并发闸门。
//
// 每次 newBrowser() 都是一整个 Chromium 进程组，而在此之前**全项目没有任何并发控制**：
// 18 个 MCP 工具和 20 条 REST 端点共用一个 service 单例，net/http 一请求一 goroutine，
// 两个请求撞上就是两套完整浏览器一起把容器推向 OOM——而 OOM 杀掉的是整个容器，
// 不是那一个请求。
//
// 并发度取 2，依据是在目标机器（2 核 / 容器上限 1200MB）上的实测。注意口径：必须读
// cgroup 的 memory.current，不能把 11 个 chrome 进程的 ps RSS 相加——后者会把进程间
// 共享的二进制映射和 zygote 的 CoW 页重复计算 11 次，实测高估近一倍（996MB vs 528MB），
// 照那个数字算会误判成"并发上限是 1"。
//
//	并发   峰值内存   单请求耗时   吞吐        余量
//	 1      528MB      9.3s       6.5 次/分   672MB
//	 2      736MB      13s        9.2 次/分   464MB   ← 取这个
//	 3      975MB      18.5s      9.7 次/分   225MB
//
// 3 的吞吐只比 2 多 5%（2 核 CPU 已经争抢），延迟却翻倍，余量还掉到 225MB——
// 评论多的详情页能轻松吃掉这点余量。
//
// 用 XHS_BROWSER_CONCURRENCY 覆盖；换机器（尤其是改了 mem_limit 或 CPU 数）要重新实测。
const defaultBrowserConcurrency = 2

// browserAcquireTimeout 排队等名额的上限。
//
// 短排队 + 明确失败，绝不长排队：单请求约 13 秒，排在第 2、3 位也就几十秒；
// 等过这个数说明后面已经堆积，与其让调用方干等到自己超时（MCP 客户端通常 60 秒左右），
// 不如立刻给个说得清的错误。
//
// 这只是兜底上限。调用方自己的 ctx 一旦取消，排队会立刻结束，不必等满这个数。
const browserAcquireTimeout = 60 * time.Second

var (
	browserSem     chan struct{}
	browserSemOnce sync.Once
)

func browserConcurrency() int {
	if v := os.Getenv("XHS_BROWSER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		logrus.Warnf("XHS_BROWSER_CONCURRENCY=%q 不是正整数，回退到 %d", v, defaultBrowserConcurrency)
	}
	return defaultBrowserConcurrency
}

func browserSlots() chan struct{} {
	browserSemOnce.Do(func() {
		n := browserConcurrency()
		browserSem = make(chan struct{}, n)
		logrus.Infof("浏览器并发上限: %d", n)
	})
	return browserSem
}

// browserMaxHold 一张名额票最长能占多久，到点强制收回。
//
// 线上出过两张票同时永久泄漏（2026-09-25 抓的协程栈），两处都卡在归还名额之前：
//   - rod 的 Mouse 绑在建页时的原始 page 上，走的是 context.Background()，外层
//     page.Timeout 对它无效。渲染进程不回 Input.dispatchMouseEvent，滚评论那一步挂了 5 天；
//   - 页面卡满 10 分钟后，defer 里裸的 page.Close() 等 TargetDestroyed 等不到，挂了几个小时。
//
// 两张票都漏掉后，此后每个请求排队 60 秒就失败，整个服务等于停摆。这类「某个 CDP 调用
// 永远不回」没法逐个堵全（鼠标、键盘、关页、关浏览器走的都是 Background），所以在名额
// 这一层兜底：占用超过上限，就关掉 Chromium、收回名额。Chromium 一关，WebSocket 断开，
// rod 会让所有挂着的调用立刻返回错误，卡住的 goroutine 随之退出。
//
// 20 分钟高于任何正常流程，最长的是发布视频（上传 5 分钟 + 等发布按钮 10 分钟）。
// 耗时有明确上界的调用方用 limitHold 收紧。
const browserMaxHold = 20 * time.Minute

// browserCloseTimeout 正常关浏览器的上限。headless_browser.Close 走的是 rod 的
// MustClose，同样是 context.Background()；等不到就先归还名额，关闭留在后台继续。
// 是 var 只为测试能调短。
var browserCloseTimeout = 10 * time.Second

// leasedBrowser 给浏览器实例配一张名额票，Close 时归还。
//
// 内嵌 *headless_browser.Browser 是刻意的：调用方在 b 上只用到 Close 和 NewPage
// （已 grep 核对，19 个调用点无一例外），靠 Go 的方法提升，全部调用点无需改动。
// 被拦截的只有 Close（归还名额）和 NewPage（记下 rod 浏览器，供看门狗强制关闭）。
type leasedBrowser struct {
	*headless_browser.Browser
	release       func()
	closeUnderlay func() // 真正关浏览器的动作，测试里可替换

	mu       sync.Mutex
	rod      *rod.Browser
	watchdog *time.Timer
	deadline time.Time
}

// newLease 组装 leasedBrowser 并启动看门狗。
func newLease(hb *headless_browser.Browser, closeUnderlay, release func(), maxHold time.Duration) *leasedBrowser {
	b := &leasedBrowser{Browser: hb, release: release, closeUnderlay: closeUnderlay}
	b.mu.Lock()
	b.deadline = time.Now().Add(maxHold)
	b.watchdog = time.AfterFunc(maxHold, func() { b.reclaim(maxHold) })
	b.mu.Unlock()
	return b
}

// NewPage 建页面，顺带记下 rod 浏览器——headless_browser 不导出它，看门狗要靠它关掉 Chromium。
func (b *leasedBrowser) NewPage() *rod.Page {
	page := b.Browser.NewPage()
	b.mu.Lock()
	if b.rod == nil {
		b.rod = page.Browser()
	}
	b.mu.Unlock()
	return page
}

// limitHold 把本次占用上限收紧到从现在起 d。只收紧不放宽；看门狗已经触发则不做任何事。
func (b *leasedBrowser) limitHold(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deadline := time.Now().Add(d)
	if b.watchdog == nil || !deadline.Before(b.deadline) || !b.watchdog.Stop() {
		return
	}
	b.deadline = deadline
	b.watchdog = time.AfterFunc(d, func() { b.reclaim(d) })
}

// reclaim 看门狗到点：关掉 Chromium 让卡住的调用全部报错返回，并收回名额。
func (b *leasedBrowser) reclaim(held time.Duration) {
	logrus.Errorf("浏览器已占用 %s 仍未归还，强制关闭 Chromium 并收回名额", held)

	b.mu.Lock()
	rb := b.rod
	b.mu.Unlock()
	if rb != nil {
		if err := rb.Timeout(browserCloseTimeout).Close(); err != nil {
			logrus.Warnf("强制关闭 Chromium 返回: %v", err)
		}
	}
	b.release()
}

// Close 关浏览器并归还名额。名额一定要还：关闭出错不能跳过归还，关闭卡住也只等
// browserCloseTimeout。关闭的 panic（MustClose 遇上已崩溃的 Chromium）在这里吞掉。
func (b *leasedBrowser) Close() {
	defer b.release()

	b.mu.Lock()
	if b.watchdog != nil {
		b.watchdog.Stop()
	}
	b.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logrus.Warnf("关闭浏览器出错: %v", r)
			}
		}()
		b.closeUnderlay()
	}()

	timer := time.NewTimer(browserCloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		logrus.Errorf("关闭浏览器超过 %s 仍未完成，先归还名额", browserCloseTimeout)
	}
}

// acquireBrowserSlot 取一张名额票，取不到就是明确失败。ctx 不能为 nil。
//
// 排队必须能被 ctx 打断。名额只有 2 个，而排队上限 60 秒正好压着 MCP 客户端的超时：
// 客户端断开后如果还占着队位，等名额一空就会为一个没人要的请求拉起一整套 Chromium，
// 把这 2 个名额之一浪费掉十几秒——真正还在等的请求反而可能因此排到超时。
func acquireBrowserSlot(ctx context.Context) {
	sem := browserSlots()

	select {
	case sem <- struct{}{}:
		return
	default:
	}

	// 满了，说明正在排队——记一笔，好在日志里看出压力
	logrus.Warnf("浏览器名额已满（上限 %d），排队中", cap(sem))
	start := time.Now()

	// 用 Timer 而不是 time.After：ctx 先取消时，After 留下的定时器要挂到点才回收。
	timer := time.NewTimer(browserAcquireTimeout)
	defer timer.Stop()

	select {
	case sem <- struct{}{}:
		logrus.Infof("排队 %s 后取得浏览器名额", time.Since(start).Round(time.Millisecond))
	case <-ctx.Done():
		// 这里 panic 而不是返回 error，是为了不改动 19 个调用点的签名；
		// MCP 侧有 withPanicRecovery、REST 侧有 gin.Recovery()，都会转成正常的错误响应。
		panic(fmt.Sprintf("排队等待浏览器名额 %s 后，调用方已取消请求：%v",
			time.Since(start).Round(time.Millisecond), ctx.Err()))
	case <-timer.C:
		panic(fmt.Sprintf("浏览器繁忙：等待 %s 仍未取得名额（并发上限 %d），请稍后重试",
			browserAcquireTimeout, cap(sem)))
	}
}

// newLeasedBrowser 取名额、建浏览器，并保证建失败时名额不会漏掉。ctx 不能为 nil。
func newLeasedBrowser(ctx context.Context, build func() *headless_browser.Browser) *leasedBrowser {
	acquireBrowserSlot(ctx)

	var once sync.Once
	release := func() { once.Do(func() { <-browserSlots() }) }

	// build 可能 panic——browser.NewBrowser 在内置浏览器不可用时是主动 panic 的。
	// 名额必须还回去，否则连续几次之后所有请求都会永久卡在 acquireBrowserSlot 上。
	defer func() {
		if r := recover(); r != nil {
			release()
			panic(r)
		}
	}()

	hb := build()
	return newLease(hb, hb.Close, release, browserMaxHold)
}
