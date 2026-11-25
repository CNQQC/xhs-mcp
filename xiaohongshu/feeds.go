package xiaohongshu

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

type FeedsListAction struct {
	page *rod.Page
}

func NewFeedsListAction(page *rod.Page) *FeedsListAction {
	return &FeedsListAction{page: page}
}

// GetFeedsList 获取页面的 Feed 列表数据
func (f *FeedsListAction) GetFeedsList(ctx context.Context) ([]Feed, error) {
	return f.getFeedsListAt(ctx, "https://www.xiaohongshu.com", 16*time.Second)
}

func (f *FeedsListAction) getFeedsListAt(ctx context.Context, url string, budget time.Duration) ([]Feed, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	page := f.page.Context(ctx).Timeout(60 * time.Second)
	defer page.CancelTimeout()
	defer blockHeavyResources(page)()
	if err := page.Navigate(url); err != nil {
		return nil, err
	}
	read := page.Timeout(budget)
	defer read.CancelTimeout()
	poll := time.NewTicker(300 * time.Millisecond)
	defer poll.Stop()
	var empty bool
	for {
		res, err := read.Eval(`() => {
			const feeds = window.__INITIAL_STATE__?.feed?.feeds;
			const data = feeds?.value !== undefined ? feeds.value : feeds?._value;
			return Array.isArray(data) ? JSON.stringify(data) : "";
		}`)
		if err != nil {
			if !stderrors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			break
		}
		result := res.Value.Str()
		empty = result == "[]"
		if result != "" && !empty {
			var feeds []Feed
			if err := json.Unmarshal([]byte(result), &feeds); err != nil {
				return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
			}
			return onlyNotes(feeds), nil
		}
		select {
		case <-poll.C:
			continue
		case <-read.GetContext().Done():
		}
		break
	}
	if err := page.GetContext().Err(); err != nil {
		return nil, err
	}
	if err := checkRiskVerification(page); err != nil {
		return nil, err
	}
	if empty {
		return []Feed{}, nil
	}
	return nil, errors.ErrNoFeeds
}
