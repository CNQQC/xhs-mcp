//go:build integration

package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

func TestHomeReadFixture(t *testing.T) {
	var covers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch request.URL.Path {
		case "/cover":
			covers.Add(1)
		case "/filled":
			fmt.Fprint(writer, `<img src="/cover"><script>
			window.__INITIAL_STATE__={feed:{feeds:{value:[]}}};
			setTimeout(()=>window.__INITIAL_STATE__.feed.feeds.value=[{modelType:'note',id:'expected'}],350);
			</script>`)
		case "/empty":
			fmt.Fprint(writer, `<script>window.__INITIAL_STATE__={feed:{feeds:{_value:[]}}}</script>`)
		}
	}))
	defer server.Close()
	action := NewFeedsListAction(newCommentScrollPage(t))
	ctx := context.Background()
	feeds, err := action.getFeedsListAt(ctx, server.URL+"/filled", 3*time.Second)
	require.NoError(t, err)
	require.Len(t, feeds, 1)
	require.Equal(t, "expected", feeds[0].ID)
	require.Zero(t, covers.Load())
	feeds, err = action.getFeedsListAt(ctx, server.URL+"/empty", 400*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, feeds)
	require.Empty(t, feeds)
	_, err = action.getFeedsListAt(ctx, server.URL+"/missing", 400*time.Millisecond)
	require.ErrorIs(t, err, xhserrors.ErrNoFeeds)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = action.getFeedsListAt(cancelled, server.URL, time.Second)
	require.ErrorIs(t, err, context.Canceled)
}
