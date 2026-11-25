//go:build integration

package xiaohongshu

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/require"
)

func TestFirstPageSnapshotReads(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialFlag string
		finishLater bool
		reads       int
		warn        bool
	}{
		{"ready", "true", false, 1, false},
		{"pending", "false", true, 2, false},
		{"unknown", "undefined", false, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := newCommentScrollPage(t)
			html := fmt.Sprintf(`<script>
			window.reads=0;
			const comments={firstRequestFinish:%s,list:[]};
			const note={noteId:'fixture',title:'body',toJSON(){
				window.reads++;
				if (%t && window.reads===1) setTimeout(()=>{comments.firstRequestFinish=true;comments.list=[{id:'loaded'}]},25);
				return {noteId:this.noteId,title:this.title};
			}};
			window.__INITIAL_STATE__={note:{noteDetailMap:{fixture:{note,comments}}}};
			</script>`, tc.initialFlag, tc.finishLater)
			config := DefaultFeedDetailConfig()
			config.FirstCommentsTimeout = time.Second
			url := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(html))
			detail, err := NewFeedDetailAction(page).getFeedDetailFromURL(page.GetContext(), "fixture", url, false, config)
			require.NoError(t, err)
			require.Equal(t, tc.reads, page.MustEval(`() => window.reads`).Int())
			require.Equal(t, "body", detail.Note.Title)
			require.Equal(t, tc.warn, detail.CommentsLoadWarning != "")
			if tc.finishLater {
				require.Len(t, detail.Comments.List, 1)
				require.Equal(t, "loaded", detail.Comments.List[0].ID)
			}
		})
	}
}

func TestSkipCommentsDoesNotReadCommentState(t *testing.T) {
	page := newCommentScrollPage(t)
	html := `<script>
	window.commentReads=0;
	window.__INITIAL_STATE__={note:{noteDetailMap:{fixture:{
	 note:{noteId:"fixture",title:"body",interactInfo:{commentCount:"7"}},
	 get comments(){ window.commentReads++; throw Error("unavailable"); }
	}}}};
	</script>`
	config := DefaultFeedDetailConfig()
	config.SkipComments = true
	url := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(html))
	detail, err := NewFeedDetailAction(page).getFeedDetailFromURL(page.GetContext(), "fixture", url, false, config)
	require.NoError(t, err)
	require.Equal(t, 0, page.MustEval(`() => window.commentReads`).Int())
	require.Equal(t, "7", detail.Note.InteractInfo.CommentCount)
	require.Empty(t, detail.Comments.List)
	require.Empty(t, detail.CommentsLoadWarning)
}

func TestSelectiveResourceBlocking(t *testing.T) {
	var imageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(writer, `<img src="/photo" onerror="window.imageFailed=true"><script src="/script"></script>`)
		case "/photo":
			imageRequests.Add(1)
		case "/script":
			writer.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(writer, `fetch("/comments").then(r=>r.text()).then(value=>window.comments=value)`)
		case "/comments":
			fmt.Fprint(writer, "ready")
		}
	}))
	defer server.Close()
	page := newCommentScrollPage(t)
	stop := blockHeavyResources(page)
	defer stop()
	var state proto.FetchEnable
	require.True(t, page.LoadState(&state))
	var types []proto.NetworkResourceType
	for _, pattern := range state.Patterns {
		types = append(types, pattern.ResourceType)
	}
	require.ElementsMatch(t, []proto.NetworkResourceType{proto.NetworkResourceTypeImage, proto.NetworkResourceTypeMedia, proto.NetworkResourceTypeFont}, types)
	require.NoError(t, page.Navigate(server.URL))
	require.NoError(t, page.Wait(rod.Eval(`() => window.imageFailed && window.comments === 'ready'`)))
	require.Zero(t, imageRequests.Load())
}
