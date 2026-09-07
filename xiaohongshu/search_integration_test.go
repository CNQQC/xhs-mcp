//go:build integration

// 集成测试：起有头浏览器 + 触网 + 需登录态，默认 go test 不编译不运行。
// 手动跑：go test -tags integration ./xiaohongshu/ -run TestSearch
package xiaohongshu

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

func TestSearchWithoutFilters(t *testing.T) {
	b := browser.NewBrowser(false)
	defer b.Close()

	for _, keyword := range []string{"美食", "贵州自驾"} {
		t.Run(keyword, func(t *testing.T) {
			page := b.NewPage()
			defer func() {
				_ = page.Close()
			}()

			action := NewSearchAction(page)
			feeds, err := action.Search(context.Background(), keyword)
			require.NoError(t, err)
			require.NotEmpty(t, feeds, "unfiltered search should not be empty")

			fmt.Printf("关键词 %q 成功获取到 %d 个 Feed\n", keyword, len(feeds))
		})
	}
}

func TestSearchWithFilters(t *testing.T) {
	b := browser.NewBrowser(false)
	defer b.Close()

	page := b.NewPage()
	defer func() {
		_ = page.Close()
	}()

	action := NewSearchAction(page)

	filter := FilterOption{
		NoteType:    "图文",
		PublishTime: "一天内",
	}

	feeds, err := action.Search(context.Background(), "dn432", filter)
	require.NoError(t, err)
	require.NotEmpty(t, feeds, "feeds should not be empty")

	fmt.Printf("成功获取到 %d 个筛选后的 Feed\n", len(feeds))

	for _, feed := range feeds {
		fmt.Printf("Feed ID: %s\n", feed.ID)
		fmt.Printf("Feed Title: %s\n", feed.NoteCard.DisplayTitle)
	}
}
