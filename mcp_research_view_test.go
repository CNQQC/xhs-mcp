package main

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestResearchSourceURL(t *testing.T) {
	id := "65f1a2b3c4d5e6f7a8b9c0d1"
	for _, tc := range []struct{ name, id, token string }{
		{"missing ID", "", "real-token"},
		{"unsafe ID", "../../other", "real-token"},
		{"wrong length", id[:23], "real-token"},
		{"nonhex ID", "g" + id[1:], "real-token"},
		{"missing token", id, ""},
		{"space token", id, " "},
		{"embedded whitespace", id, "hello world"},
		{"control token", id, "real\ntoken"},
		{"null token", id, "null"},
		{"undefined token", id, "undefined"},
		{"placeholder token", id, "<xsec_token>"},
		{"generic placeholder", id, "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, safeNoteSourceURL(tc.id, tc.token))
		})
	}
	token := "actual+base64/token==&injected=value#fragment?"
	parsed, err := url.Parse(safeNoteSourceURL(id, token))
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "www.xiaohongshu.com", parsed.Host)
	assert.Equal(t, "/explore/"+id, parsed.Path)
	assert.Empty(t, parsed.Fragment)
	assert.Equal(t, token, parsed.Query().Get("xsec_token"))
	assert.Equal(t, "pc_feed", parsed.Query().Get("xsec_source"))
	assert.Len(t, parsed.Query(), 2, "token must not inject query arguments")
}

func TestResearchListMetadataAndCoverage(t *testing.T) {
	refs := newRefTable()
	before := time.Now().UTC()
	view := toNoteListView(refs, []xiaohongshu.Feed{sampleFeed()}, "search")
	after := time.Now().UTC()
	require.Len(t, view.Notes, 1)
	note := view.Notes[0]
	assert.Equal(t, sampleFeed().ID, note.NoteID)
	assert.NotEmpty(t, note.SourceURL)
	retrieved, err := time.Parse(time.RFC3339Nano, note.RetrievedAt)
	require.NoError(t, err)
	expiry, err := time.Parse(time.RFC3339Nano, note.RefExpiresAt)
	require.NoError(t, err)
	assert.False(t, retrieved.Before(before))
	assert.False(t, retrieved.After(after))
	assert.Equal(t, refTTL, expiry.Sub(retrieved))
	assert.Equal(t, refs.expiresAt(note.Ref), note.RefExpiresAt)
	require.NotNil(t, view.Coverage)
	assert.Equal(t, "search", view.Coverage.Source)
	assert.Equal(t, "first_page", view.Coverage.Scope)
	assert.Equal(t, view.Count, view.Coverage.ReturnedCount)
	assert.False(t, view.Coverage.ContinuationSupported)
	assert.False(t, view.Coverage.Exhaustive)

	feed := sampleFeed()
	feed.XsecToken = ""
	withoutToken := toNoteListView(refs, []xiaohongshu.Feed{feed}, "home")
	assert.Equal(t, feed.ID, withoutToken.Notes[0].NoteID)
	assert.Empty(t, withoutToken.Notes[0].SourceURL)

	empty := toNoteListView(refs, nil, "search")
	assert.NotNil(t, empty.Notes)
	assert.Zero(t, empty.Count)
	assert.False(t, empty.Coverage.Exhaustive, "an empty first page does not prove search exhaustion")
}

func TestResearchRefExpiryMetadataNeverRenews(t *testing.T) {
	refs := newRefTable()
	ref, at, expiry := refs.putWithMetadata(refTarget{FeedID: "original"})
	assert.Equal(t, refTTL, expiry.Sub(at))
	stored := refs.items[ref]
	stored.expiresAt = time.Now().UTC().Add(5 * time.Minute)
	refs.items[ref] = stored
	wantExpiry := stored.expiresAt.Format(time.RFC3339Nano)
	for range 3 {
		assert.Equal(t, wantExpiry, refs.expiresAt(ref))
		_, ok := refs.lookup(ref)
		assert.True(t, ok)
	}
	projected := toNoteDetailViewWithRef(refs, "original", "real-token", &xiaohongshu.FeedDetailResponse{}, ref)
	assert.Equal(t, ref, projected.Ref)
	assert.Equal(t, wantExpiry, projected.RefExpiresAt)

	stored.expiresAt = time.Now().Add(-time.Second)
	refs.items[ref] = stored
	assert.Empty(t, refs.expiresAt(ref))
	_, ok := refs.lookup(ref)
	assert.False(t, ok)
	assert.Empty(t, newRefTable().expiresAt(ref), "process-local references have no expiry after a restart")
}

func TestResearchCommentCoverageConservative(t *testing.T) {
	for _, tc := range []struct {
		name, payload, warning string
		complete, hasMore      *bool
	}{
		{"missing flags", `{}`, "", nil, nil},
		{"null flags", `{"hasMore":null,"firstRequestFinish":null}`, "", nil, nil},
		{"unfinished", `{"hasMore":false,"firstRequestFinish":false}`, "", boolPointer(false), boolPointer(false)},
		{"more comments", `{"hasMore":true,"firstRequestFinish":true}`, "", boolPointer(false), boolPointer(true)},
		{"explicitly complete", `{"hasMore":false,"firstRequestFinish":true}`, "", boolPointer(true), boolPointer(false)},
		{"hasMore alone", `{"hasMore":false}`, "", nil, boolPointer(false)},
		{"finished alone", `{"firstRequestFinish":true}`, "", nil, nil},
		{"soft failure", `{"hasMore":false,"firstRequestFinish":true}`, "comment load failed", nil, boolPointer(false)},
		{"soft failure and known more", `{"hasMore":true}`, "comment load failed", boolPointer(false), boolPointer(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var comments xiaohongshu.CommentList
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &comments))
			coverage, warnings := measureDetailCoverage(&xiaohongshu.FeedDetailResponse{Comments: comments, CommentsLoadWarning: tc.warning})
			assert.Equal(t, tc.complete, coverage.Comments.TopLevelComplete)
			assert.Equal(t, tc.hasMore, coverage.Comments.HasMoreTopLevel)
			assert.Equal(t, "returned_top_level_comments", coverage.Comments.RepliesScope)
			if tc.warning != "" {
				assert.Contains(t, warnings, tc.warning)
			} else {
				assert.Empty(t, warnings)
			}
			if tc.complete != nil && *tc.complete {
				assert.Equal(t, boolPointer(true), coverage.Comments.RepliesComplete)
			}
		})
	}
}

func TestResearchReplyCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		comments []xiaohongshu.Comment
		returned int
		complete *bool
	}{
		{"none unknown", nil, 0, nil},
		{"explicit zero", []xiaohongshu.Comment{{SubCommentCount: "0"}}, 0, boolPointer(true)},
		{"missing count", []xiaohongshu.Comment{{}}, 0, nil},
		{"compact count", []xiaohongshu.Comment{{SubCommentCount: "1万"}}, 0, nil},
		{"negative count", []xiaohongshu.Comment{{SubCommentCount: "-1"}}, 0, nil},
		{"unexpanded", []xiaohongshu.Comment{{SubCommentCount: "2", SubComments: []xiaohongshu.Comment{{ID: "r1"}}}}, 1, boolPointer(false)},
		{"complete", []xiaohongshu.Comment{{SubCommentCount: "1", SubComments: []xiaohongshu.Comment{{ID: "r1"}}}}, 1, boolPointer(true)},
		{"conflicting count", []xiaohongshu.Comment{{SubCommentCount: "0", SubComments: []xiaohongshu.Comment{{ID: "r1"}}}}, 1, nil},
		{"partial overrides unknown", []xiaohongshu.Comment{{}, {SubCommentCount: "1"}}, 0, boolPointer(false)},
		{"nested partial", []xiaohongshu.Comment{{SubCommentCount: "1", SubComments: []xiaohongshu.Comment{{SubCommentCount: "2", SubComments: []xiaohongshu.Comment{{ID: "r2"}}}}}}, 2, boolPointer(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			returned, complete := measureReplies(tc.comments)
			assert.Equal(t, tc.returned, returned)
			assert.Equal(t, tc.complete, complete)
		})
	}
}

func TestResearchRequestCoverageDoesNotPromiseCompleteness(t *testing.T) {
	view := toNoteDetailView(newRefTable(), sampleFeed().ID, sampleFeed().XsecToken, &xiaohongshu.FeedDetailResponse{})
	applyDetailRequestCoverage(&view, true, xiaohongshu.FeedDetailConfig{ClickMoreReplies: true})
	assert.Equal(t, "scrolled", view.Coverage.Comments.Mode)
	assert.Equal(t, xiaohongshu.DefaultFeedDetailConfig().MaxCommentItems, view.Coverage.Comments.RequestedLimit)
	assert.Equal(t, xiaohongshu.DefaultFeedDetailConfig().MaxRepliesThreshold, view.Coverage.Comments.RepliesExpansionThreshold)
	assert.True(t, view.Coverage.Comments.RepliesExpansionRequested)
	assert.Nil(t, view.Coverage.Comments.TopLevelComplete)
	assert.Nil(t, view.Coverage.Comments.RepliesComplete)
	first := toNoteDetailView(newRefTable(), "feed", "token", &xiaohongshu.FeedDetailResponse{})
	applyDetailRequestCoverage(&first, false, xiaohongshu.FeedDetailConfig{ClickMoreReplies: true, MaxCommentItems: 100})
	assert.Equal(t, "first_screen", first.Coverage.Comments.Mode)
	assert.Zero(t, first.Coverage.Comments.RequestedLimit)
	assert.False(t, first.Coverage.Comments.RepliesExpansionRequested)
}

func TestResearchSubtitleCoverage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		note            xiaohongshu.FeedDetail
		status, warning string
	}{
		{"normal", xiaohongshu.FeedDetail{Type: "normal"}, "not_applicable", ""},
		{"video missing data", xiaohongshu.FeedDetail{Type: "video"}, "unknown", ""},
		{"empty video data", xiaohongshu.FeedDetail{Video: &xiaohongshu.VideoDetail{}}, "unknown", ""},
		{"existing subtitle", xiaohongshu.FeedDetail{Video: &xiaohongshu.VideoDetail{SubtitleText: "dialogue"}}, "available", ""},
		{"explicit absence", xiaohongshu.FeedDetail{Video: &xiaohongshu.VideoDetail{SubtitleStatus: "unavailable"}}, "unavailable", ""},
		{"failed extraction", xiaohongshu.FeedDetail{Video: &xiaohongshu.VideoDetail{SubtitleStatus: "failed", SubtitleWarning: "subtitle unavailable"}}, "failed", "subtitle unavailable"},
		{"partial transcript", xiaohongshu.FeedDetail{Video: &xiaohongshu.VideoDetail{SubtitleStatus: "partial", SubtitleText: "partial", SubtitleWarning: "subtitle incomplete"}}, "partial", "subtitle incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coverage, warnings := measureDetailCoverage(&xiaohongshu.FeedDetailResponse{Note: tc.note})
			assert.Equal(t, tc.status, coverage.Subtitle.Status)
			if tc.warning == "" {
				assert.Empty(t, warnings)
			} else {
				assert.Contains(t, warnings, tc.warning)
			}
		})
	}
}

func TestResearchDetailReusesBatchRefWithoutEviction(t *testing.T) {
	refs := newRefTable()
	ref := refs.put(refTarget{FeedID: sampleFeed().ID, XsecToken: sampleFeed().XsecToken})
	for i := 1; i < refMaxEntries; i++ {
		refs.put(refTarget{FeedID: "filler"})
	}
	before := refs.seq
	expiry := refs.expiresAt(ref)
	view := toNoteDetailViewWithRef(refs, sampleFeed().ID, sampleFeed().XsecToken, &xiaohongshu.FeedDetailResponse{}, ref)
	assert.Equal(t, before, refs.seq, "a preserved note ref must not register another note")
	assert.Equal(t, ref, view.Ref)
	assert.Equal(t, expiry, view.RefExpiresAt)
	_, ok := refs.lookup(ref)
	assert.True(t, ok, "projecting an existing ref at capacity must not evict it")
}
