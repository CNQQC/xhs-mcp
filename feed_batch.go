package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// 批量获取笔记详情。
//
// 逐条调 get_feed_detail 时，每条都要单独起一整个 Chromium、占一张浏览器名额
// （见 browser_lease.go），模型还得一轮轮地等。批量版只起一个浏览器、占一张名额，
// 在里面同时开几个标签页抓，一次调用返回全部结果。

const (
	// feedBatchMax 单次最多几条。线上实测（2 核 / 1200MB，含等首屏评论）10 条要 57 秒，
	// 贴着 MCP 客户端常见的 60 秒超时；6 条约 35 秒，留足余量。
	feedBatchMax = 6

	// feedBatchConcurrency 同一浏览器里同时开的标签页数。线上实测 10 条：
	// 3 路 56 秒、峰值 891MB；2 路 57 秒、峰值 751MB——2 核 CPU 是瓶颈，多开不快，只多吃内存。
	feedBatchConcurrency = 2
)

// FeedDetailsArgs get_feed_details 的参数
type FeedDetailsArgs struct {
	Refs          []string `json:"refs" jsonschema:"笔记 ref 列表，取自 search_feeds / list_feeds / user_profile 返回的 ref，一次最多6条"`
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
	// 名额占用上限：最坏每轮都卡满单页超时，另留 1 分钟收尾
	rounds := (len(targets) + feedBatchConcurrency - 1) / feedBatchConcurrency
	b.limitHold(time.Duration(rounds)*xiaohongshu.FeedDetailTimeout(false) + time.Minute)

	return fetchConcurrently(ctx, len(targets), feedBatchConcurrency,
		func(ctx context.Context, i int) (*xiaohongshu.FeedDetailResponse, error) {
			page := b.NewPage()
			// 关页必须限时（closePage）。批量里更要紧：rod 的 Page.Close 持有整个浏览器的
			// targetsLock，一个标签页关不掉，其余标签页的开页、关页都会被这把锁堵住。
			defer closePage(page)

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
	return s.handleGetFeedDetailsWithFetch(ctx, args, func(ctx context.Context, targets []refTarget, config xiaohongshu.FeedDetailConfig) []feedDetailResult {
		return s.xiaohongshuService.GetFeedDetails(ctx, targets, config)
	})
}

// 注入抓取函数，离线验证部分失败、去重与输入顺序，不启动真实浏览器。
func (s *AppServer) handleGetFeedDetailsWithFetch(ctx context.Context, args FeedDetailsArgs,
	fetch func(context.Context, []refTarget, xiaohongshu.FeedDetailConfig) []feedDetailResult,
) *MCPToolResult {
	if len(args.Refs) == 0 {
		return newMCPErrorResult(xhserrors.ErrInvalidArgument, "批量获取Feed详情失败: refs 不能为空")
	}
	if len(args.Refs) > feedBatchMax {
		return newMCPErrorResult(xhserrors.ErrInvalidArgument, fmt.Sprintf(
			"批量获取Feed详情失败: 一次最多 %d 条，本次 %d 条，请分批调用", feedBatchMax, len(args.Refs)))
	}

	// 相同笔记与访问令牌只抓一次；返回项仍严格保持请求的数量、顺序和 ref。
	notes := make([]noteBatchItem, len(args.Refs))
	var targets []refTarget
	var positions [][]int
	type fetchKey struct{ feedID, token string }
	seen := make(map[fetchKey]int)
	setFailure := func(i int, target refTarget, err error, message string) {
		info := classifyMCPError(err, message)
		info.RequestID = requestIDFromContext(ctx)
		notes[i] = noteBatchItem{
			noteDetailView: noteDetailView{Ref: args.Refs[i], NoteID: target.FeedID},
			Error:          message, ErrorInfo: info,
		}
	}
	for i, ref := range args.Refs {
		target, ok := s.refs.lookup(ref)
		if !ok {
			setFailure(i, refTarget{}, xhserrors.ErrRefExpired, errBatchRefExpired)
			continue
		}
		key := fetchKey{target.FeedID, target.XsecToken}
		if j, exists := seen[key]; exists {
			positions[j] = append(positions[j], i)
			continue
		}
		seen[key] = len(targets)
		targets = append(targets, target)
		positions = append(positions, []int{i})
	}

	logrus.Infof("MCP: 批量获取Feed详情 - 共 %d 条，去重后有效 %d 条", len(args.Refs), len(targets))
	if len(targets) > 0 {
		config := xiaohongshu.DefaultFeedDetailConfig()
		config.IncludeImages = args.IncludeImages
		results := fetch(ctx, targets, config)
		for j, target := range targets {
			r := feedDetailResult{err: fmt.Errorf("内部错误: 批量详情未返回结果")}
			if j < len(results) {
				r = results[j]
			}
			if r.err == nil && r.detail == nil {
				r.err = fmt.Errorf("内部错误: 批量详情返回空结果")
			}
			for _, i := range positions[j] {
				if r.err != nil {
					setFailure(i, target, r.err, r.err.Error())
					continue
				}
				view := toNoteDetailViewWithRef(s.refs, target.FeedID, target.XsecToken, r.detail, args.Refs[i])
				applyDetailRequestCoverage(&view, false, config)
				notes[i] = noteBatchItem{noteDetailView: view}
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
