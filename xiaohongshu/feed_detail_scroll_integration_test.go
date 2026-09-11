//go:build integration

package xiaohongshu

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

// These fixtures run in Chromium without contacting Xiaohongshu or loading cookies.
func newCommentScrollPage(t *testing.T) *rod.Page {
	t.Helper()
	bin, err := browser.EnsureBrowser()
	require.NoError(t, err)
	l := launcher.New().Bin(bin).Headless(true)
	b := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	t.Cleanup(func() {
		b.MustClose()
		l.Cleanup()
	})
	page := b.MustPage("about:blank")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return page.Context(ctx)
}

func TestCommentScrollMovingElements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scroll func(*rod.Page)
	}{
		{"comments area", scrollToCommentsArea},
		{"last comment", scrollToLastComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := newCommentScrollPage(t)
			page.MustSetDocumentContent(`<style>
				@keyframes moving { from { transform: translateX(0); } to { transform: translateX(100px); } }
				.comments-container { animation: moving 1s linear infinite alternate; }
			</style><div style="height:3000px"></div>
			<div class="comments-container"><div class="parent-comment">loaded comment</div></div>`)
			require.NoError(t, page.Wait(rod.Eval(`() => document.getAnimations().some(a => a.currentTime > 50)`)))

			require.NotPanics(t, func() { tc.scroll(page) })
			require.Greater(t, page.MustEval(`() => window.scrollY`).Int(), 0,
				"moving comments must still be scrolled into view")
		})
	}
}

func TestCommentScrollHiddenElements(t *testing.T) {
	page := newCommentScrollPage(t)
	page.MustSetDocumentContent(`<div class="note-scroller" style="height:500px;overflow:auto">
		<div style="height:3000px"></div>
		<div class="comments-container" style="display:none"><div class="parent-comment">hidden</div></div>
	</div>`)
	require.NotPanics(t, func() { scrollToCommentsArea(page) })
	require.NoError(t, page.Timeout(2*time.Second).Wait(rod.Eval(`() => document.querySelector('.note-scroller').scrollTop > 0`)),
		"wheel fallback must still trigger lazy loading when the comments area is hidden")
	require.NotPanics(t, func() { scrollToLastComment(page) })
}

func TestCommentScrollContext(t *testing.T) {
	page := newCommentScrollPage(t)
	page.MustSetDocumentContent(`<div style="height:3000px"></div><div class="parent-comment">comment</div>`)
	el := page.MustElement(".parent-comment")
	expired, cancelLookup := context.WithCancel(page.GetContext())
	cancelLookup()
	require.NoError(t, scrollCommentElementIntoView(page, el.Context(expired)),
		"an expired lookup context must not invalidate a fresh scroll operation")
	require.Greater(t, page.MustEval(`() => window.scrollY`).Int(), 0)

	cancelled, cancelPage := context.WithCancel(page.GetContext())
	cancelPage()
	cancelledPage := page.Context(cancelled)
	require.ErrorIs(t, scrollCommentElementIntoView(cancelledPage, el), context.Canceled,
		"the scroll operation must still respect the request cancellation")
	require.NotPanics(t, func() {
		scrollToCommentsArea(cancelledPage)
		scrollToLastComment(cancelledPage)
		smartScroll(cancelledPage, 100)
		moveToCommentScroller(cancelledPage)
	})
	require.ErrorIs(t, NewFeedDetailAction(page).loadAllCommentsWithConfig(
		cancelled, cancelledPage, DefaultFeedDetailConfig()), context.Canceled)
}

func TestCommentScrollFailurePreservesDetail(t *testing.T) {
	page := newCommentScrollPage(t)
	page.MustSetDocumentContent(`<div class="note-scroller" style="height:500px;overflow:auto">
		<div class="comments-container" style="display:none"><div class="parent-comment">cached</div></div>
		<div class="end-container">THE END</div></div>
		<script>window.__INITIAL_STATE__ = {note: {noteDetailMap: {fixture: {
			note: {noteId: "fixture", title: "available note", desc: "available body"},
			comments: {list: [{id: "c1", content: "available comment"}], hasMore: true}
		}}}};</script>`)
	action := NewFeedDetailAction(page)
	require.NotPanics(t, func() {
		require.NoError(t, action.loadAllCommentsWithConfig(page.GetContext(), page, DefaultFeedDetailConfig()))
	})
	detail, err := action.extractFeedDetail(page, "fixture")
	require.NoError(t, err)
	require.Equal(t, "available body", detail.Note.Desc)
	require.Len(t, detail.Comments.List, 1)
	require.Equal(t, "available comment", detail.Comments.List[0].Content)
	require.True(t, detail.Comments.HasMore, "incomplete comments must not be marked as fully loaded")
}
