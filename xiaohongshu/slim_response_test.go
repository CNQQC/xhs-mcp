package xiaohongshu

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFeedJSONHasNoNoise 钉住「返回体里只留干货」这条约束。
//
// 这些字段调用方（大模型）一个都用不上，却按条数成倍出现：封面和头像是看不了的
// CDN 长链接，nickName 和 nickname 重复，modelType 过滤之后恒为 note。
// 哪天有人把结构体字段加回来，这里会先红。
func TestFeedJSONHasNoNoise(t *testing.T) {
	feeds := onlyNotes([]Feed{{
		ID:        "1",
		XsecToken: "tok",
		ModelType: "note",
		NoteCard: NoteCard{
			Type:         "normal",
			DisplayTitle: "山顶露营",
			User:         User{UserID: "u1", Nickname: "阿甲"},
			InteractInfo: InteractInfo{LikedCount: "100"},
		},
	}})
	require.Len(t, feeds, 1)

	data, err := json.Marshal(feeds[0])
	require.NoError(t, err)
	got := string(data)

	for _, noise := range []string{"cover", "avatar", "nickName", "modelType", "index", "infoList", "urlDefault"} {
		assert.NotContains(t, got, noise, "返回体里不该再有 %s", noise)
	}

	// 干货要还在
	for _, keep := range []string{"山顶露营", "阿甲", "tok", "100"} {
		assert.Contains(t, got, keep)
	}
}

// TestUserNicknameFallback 站点两种拼写都认。
//
// 列表页实测给的是 nickname，但站点两个拼写都在用；只留一个字段之后，
// 遇到只给驼峰的接口不能把名字丢掉。
func TestUserNicknameFallback(t *testing.T) {
	t.Run("小写优先", func(t *testing.T) {
		var u User
		require.NoError(t, json.Unmarshal([]byte(`{"userId":"u1","nickname":"阿甲","nickName":"阿甲"}`), &u))
		assert.Equal(t, "阿甲", u.Nickname)
	})

	t.Run("只有驼峰时也认", func(t *testing.T) {
		var u User
		require.NoError(t, json.Unmarshal([]byte(`{"userId":"u1","nickName":"阿乙"}`), &u))
		assert.Equal(t, "阿乙", u.Nickname, "只给驼峰时不能把昵称丢掉")
	})
}

// TestApplyImagePolicy 图片默认只给张数，显式要了才给明细。
func TestApplyImagePolicy(t *testing.T) {
	newNote := func() FeedDetail {
		return FeedDetail{ImageList: []DetailImageInfo{
			{Width: 1080, Height: 1440, URLDefault: "https://example.com/1.jpg"},
			{Width: 1080, Height: 1440, URLDefault: "https://example.com/2.jpg"},
		}}
	}

	t.Run("默认只给张数", func(t *testing.T) {
		note := newNote()
		applyImagePolicy(&note, false)

		assert.Equal(t, 2, note.ImageCount)
		assert.Nil(t, note.ImageList)

		data, err := json.Marshal(note)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "imageList", "默认不该出现 imageList 这个键")
		assert.Contains(t, string(data), `"imageCount":2`)
	})

	t.Run("显式要图时给明细", func(t *testing.T) {
		note := newNote()
		applyImagePolicy(&note, true)

		assert.Equal(t, 2, note.ImageCount)
		assert.Len(t, note.ImageList, 2)
	})

	t.Run("没有图时张数为 0", func(t *testing.T) {
		var note FeedDetail
		applyImagePolicy(&note, false)

		assert.Equal(t, 0, note.ImageCount)
		assert.Nil(t, note.ImageList)
	})
}

// TestOnlyNotesClearsModelType 过滤完就把 modelType 清掉——它已经恒为 note。
func TestOnlyNotesClearsModelType(t *testing.T) {
	in := []Feed{{ID: "1", ModelType: "note"}}

	got := onlyNotes(in)

	require.Len(t, got, 1)
	assert.Empty(t, got[0].ModelType, "过滤后 modelType 恒为 note，不该再进返回体")
	assert.Equal(t, "note", in[0].ModelType, "不能动调用方传进来的原切片")
}
