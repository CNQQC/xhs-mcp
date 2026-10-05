package xiaohongshu

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentListTracksSourcePresence(t *testing.T) {
	var comments CommentList
	require.NoError(t, json.Unmarshal([]byte(`{"hasMore":false,"firstRequestFinish":true,"list":[{"id":"c1"}]}`), &comments))
	assert.True(t, comments.HasMoreKnown)
	assert.False(t, comments.HasMore)
	require.NotNil(t, comments.FirstRequestFinish)
	assert.True(t, *comments.FirstRequestFinish)
	require.Len(t, comments.List, 1)
	data, err := json.Marshal(comments)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "firstRequestFinish")
	assert.NotContains(t, string(data), "HasMoreKnown")
	assert.Contains(t, string(data), `"hasMore":false`)
	// 复用目标变量不能带入上一页的已知状态。
	require.NoError(t, json.Unmarshal([]byte(`{}`), &comments))
	assert.False(t, comments.HasMoreKnown)
	assert.Nil(t, comments.FirstRequestFinish)
	assert.Empty(t, comments.List)
	require.NoError(t, json.Unmarshal([]byte(`{"hasMore":true,"firstRequestFinish":false}`), &comments))
	assert.True(t, comments.HasMore)
	assert.False(t, *comments.FirstRequestFinish)
	require.Error(t, json.Unmarshal([]byte(`{"hasMore":"false"}`), &comments))
}

func TestVideoSubtitleMetadataCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, payload, status string
		warning               bool
	}{
		{"missing", `{}`, "unknown", false},
		{"known empty", `{"subtitles":{}}`, "unavailable", false},
		{"null", `{"subtitles":null}`, "unknown", false},
		{"media missing index", `{"mediaV2":"{}"}`, "unknown", false},
		{"media empty index", `{"mediaV2":"{\"video\":{\"subtitles\":{}}}"}`, "unavailable", false},
		{"broken metadata", `{"mediaV2":"not json"}`, "failed", true},
		{"wrong metadata type", `{"mediaV2":42}`, "failed", true},
		{"wrong direct index type", `{"subtitles":42}`, "failed", true},
		{"wrong nested index type", `{"mediaV2":"{\"video\":{\"subtitles\":42}}"}`, "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var video VideoDetail
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &video))
			assert.Equal(t, tc.status, video.SubtitleStatus)
			assert.Equal(t, tc.warning, video.SubtitleWarning != "")
			data, err := json.Marshal(video)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "SubtitleStatus")
			assert.NotContains(t, string(data), "SubtitleWarning")
		})
	}
}
