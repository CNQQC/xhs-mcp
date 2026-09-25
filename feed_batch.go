package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// 批量获取笔记详情。
//
// 逐条调 get_feed_detail 时，每条都要单独起一整个 Chromium、占一张浏览器名额
// （见 browser_lease.go），模型还得一轮轮地等。批量版只起一个浏览器、占一张名额，
// 在里面同时开几个标签页抓，一次调用返回全部结果。

const (
	// feedBatchMax 单次最多几条。10 条按 3 路并发约 4 轮，控制在 MCP 客户端
	// 常见的 60 秒超时以内。
	feedBatchMax = 10

	// feedBatchConcurrency 同一浏览器里同时开的标签页数。
	// 多开一个标签页只多一个渲染进程（图片视频已被拦），比多起一个浏览器省得多；
	// 但目标机只有 2 核 / 1200MB，另一张浏览器名额也可能同时在用，所以取保守的 3。
	feedBatchConcurrency = 3
)

// FeedDetailsArgs get_feed_details 的参数
type FeedDetailsArgs struct {
	Refs          []string `json:"refs" jsonschema:"笔记 ref 列表，取自 search_feeds / list_feeds / user_profile 返回的 ref，一次最多10条"`
	IncludeImages bool     `json:"include_images,omitempty" jsonschema:"是否连每张图的尺寸与地址一起返回，默认false只给张数"`
}

// feedDetailResult 批量里的一条，err 非空即这条失败。
type feedDetailResult struct {
	detail *xiaohongshu.FeedDetailResponse
	err    error
}

// GetFeedDetails 批量获取详情，结果与 targets 一一对应。只取首屏评论，不滚动加载。
func (s *XiaohongshuService) GetFeedDetails(ctx context.Context, targets []refTarget, config xiaohongshu.FeedDetailConfig) []feedDetailResult {
	b := newBrowser(ctx)
	defer b.Close()

	return fetchConcurrently(ctx, len(targets), feedBatchConcurrency,
		func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error) {
			page := b.NewPage()
			// 关页要有上限（见 service.go 的 pageCloseTimeout）。批量里更要紧：rod 的
			// Page.Close 持有整个浏览器的 targetsLock，一个标签页关不掉，其余标签页的
			// 开页、关页都会被这把锁堵住，整批挂死、名额不还。
			defer func() {
				if err := page.Timeout(pageCloseTimeout).Close(); err != nil {
					logrus.Warnf("关闭第 %d 个标签页失败: %v", i+1, err)
				}
			}()

			return xiaohongshu.NewFeedDetailAction(page).
				GetFeedDetailWithConfig(ctx, targets[i].FeedID, targets[i].XsecToken, false, config)
		})
}

// fetchConcurrently 最多 limit 路并发执行 fetch(0..n-1)，结果按下标返回。
// ctx 取消后不再开新的，没开始的记为 ctx 错误。
func fetchConcurrently(
	ctx context.Context, n, limit int,
	fetch func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error),
) []feedDetailResult {
	results := make([]feedDetailResult, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i := range n {
		if i > 0 {
			// 错开开页时间，别让几个标签页在同一瞬间一起导航
			humanize.Delay(ctx, humanize.BeforeClick)
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		// 两个分支可能同时就绪，以 ctx 为准
		if err := ctx.Err(); err != nil {
			for j := i; j < n; j++ {
				results[j].err = err
			}
			break
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = safeFetch(ctx, i, fetch)
		}()
	}

	wg.Wait()
	return results
}

// safeFetch 把 panic 转成这一条的错误。
// 子 goroutine 里的 panic 没有 withPanicRecovery 兜底，不接住会带崩整个进程。
func safeFetch(
	ctx context.Context, i int,
	fetch func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error),
) (r feedDetailResult) {
	defer func() {
		if p := recover(); p != nil {
			logrus.Errorf("批量详情第 %d 条 panic: %v\n%s", i+1, p, debug.Stack())
			r = feedDetailResult{err: fmt.Errorf("内部错误: %v", p)}
		}
	}()

	detail, err := fetch(ctx, i)
	return feedDetailResult{detail: detail, err: err}
}

// errBatchRefExpired 批量工具里单条 ref 失效时的提示。
const errBatchRefExpired = "ref 无效或已过期，请重新调用 search_feeds / list_feeds 取新的 ref"

// handleGetFeedDetails 批量获取详情。单条失败只记在那一条上，不影响其余。
func (s *AppServer) handleGetFeedDetails(ctx context.Context, args FeedDetailsArgs) *MCPToolResult {
	if len(args.Refs) == 0 {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "批量获取Feed详情失败: refs 不能为空"}},
			IsError: true,
		}
	}
	if len(args.Refs) > feedBatchMax {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: fmt.Sprintf(
				"批量获取Feed详情失败: 一次最多 %d 条，本次 %d 条，请分批调用", feedBatchMax, len(args.Refs))}},
			IsError: true,
		}
	}

	// 先解 ref，解不出的直接记失败，不占浏览器
	notes := make([]noteBatchItem, len(args.Refs))
	var targets []refTarget
	var pos []int // targets[j] 对应 notes[pos[j]]
	for i, ref := range args.Refs {
		target, ok := s.refs.lookup(ref)
		if !ok {
			notes[i] = noteBatchItem{noteDetailView: noteDetailView{Ref: ref}, Error: errBatchRefExpired}
			continue
		}
		targets = append(targets, target)
		pos = append(pos, i)
	}

	logrus.Infof("MCP: 批量获取Feed详情 - 共 %d 条，有效 %d 条", len(args.Refs), len(targets))

	if len(targets) > 0 {
		config := xiaohongshu.DefaultFeedDetailConfig()
		config.IncludeImages = args.IncludeImages

		for j, r := range s.xiaohongshuService.GetFeedDetails(ctx, targets, config) {
			i := pos[j]
			if r.err != nil {
				notes[i] = noteBatchItem{noteDetailView: noteDetailView{Ref: args.Refs[i]}, Error: r.err.Error()}
				continue
			}
			notes[i] = noteBatchItem{
				noteDetailView: toNoteDetailView(s.refs, targets[j].FeedID, targets[j].XsecToken, r.detail),
			}
		}
	}

	view := noteBatchView{Notes: notes, Count: len(notes)}
	for _, n := range notes {
		if n.Error != "" {
			view.Failed++
		}
	}

	result := marshalResult(view, "批量获取Feed详情")
	// 全军覆没才算工具报错；部分失败看各条的 error
	if view.Failed == view.Count {
		result.IsError = true
	}
	return result
}
