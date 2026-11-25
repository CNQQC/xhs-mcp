//go:build integration

package xiaohongshu

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

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
