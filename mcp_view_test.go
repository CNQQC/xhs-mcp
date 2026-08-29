package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func sampleFeed() xiaohongshu.Feed {
	return xiaohongshu.Feed{
		ID:        "65f1a2b3c4d5e6f7a8b9c0d1",
		XsecToken: "ABQpXKvL3nR8mYt2wZ7hJ4dF6sQ9bN1cE5gA0uI3oP",
		NoteCard: xiaohongshu.NoteCard{
			Type:         "video",
			DisplayTitle: "周末露营装备清单",
			User:         xiaohongshu.User{UserID: "5d8f2a1b000000000102e3c4", Nickname: "露营小分队"},
			InteractInfo: xiaohongshu.InteractInfo{
				LikedCount: "1.7万", CommentCount: "890", CollectedCount: "6512",
				SharedCount: "320", Liked: true, Collected: true,
			},
			Video: &xiaohongshu.Video{Capa: xiaohongshu.VideoCapability{Duration: 186}},
		},
	}
}

// TestNoteViewHasNoMachineFields MCP 返回体里不该再出现任何机器句柄。
//
// 这是这一整套投影层存在的理由：id、xsecToken、userId 对模型零阅读价值，
// 却按条数成倍出现。谁把它们加回来，这里先红。
func TestNoteViewHasNoMachineFields(t *testing.T) {
	tbl := newRefTable()

	views := toNoteViews(tbl, []xiaohongshu.Feed{sampleFeed()})
	require.Len(t, views, 1)

	data, err := json.Marshal(views[0])
	require.NoError(t, err)
	got := string(data)

	for _, machine := range []string{
		"65f1a2b3c4d5e6f7a8b9c0d1",                   // feed id
		"ABQpXKvL3nR8mYt2wZ7hJ4dF6sQ9bN1cE5gA0uI3oP", // xsecToken
		"5d8f2a1b000000000102e3c4",                   // 作者 userId
		"xsecToken", "userId", "noteCard", "displayTitle", "interactInfo",
	} {
		assert.NotContains(t, got, machine, "返回体里不该再有 %s", machine)
	}

	// 能读的东西要在
	for _, keep := range []string{"周末露营装备清单", "露营小分队", "1.7万", "890", "6512", "186"} {
		assert.Contains(t, got, keep)
	}
}

// TestNoteViewRefResolvesBack ref 必须能换回三个句柄，否则工具链就断了。
func TestNoteViewRefResolvesBack(t *testing.T) {
	tbl := newRefTable()

	v := toNoteViews(tbl, []xiaohongshu.Feed{sampleFeed()})[0]

	target, ok := tbl.lookup(v.Ref)
	require.True(t, ok)
	assert.Equal(t, "65f1a2b3c4d5e6f7a8b9c0d1", target.FeedID)
	assert.Equal(t, "ABQpXKvL3nR8mYt2wZ7hJ4dF6sQ9bN1cE5gA0uI3oP", target.XsecToken)
	assert.Equal(t, "5d8f2a1b000000000102e3c4", target.UserID)
}

// TestDetailViewCommentRefs 每条评论（含子评论）都要有自己的 ref，
// 否则模型没法回复具体某一条。
func TestDetailViewCommentRefs(t *testing.T) {
	tbl := newRefTable()

	detail := &xiaohongshu.FeedDetailResponse{
		Note: xiaohongshu.FeedDetail{
			Title: "标题", Desc: "正文", Type: "normal", TimeText: "2023-12-10 16:00",
			IPLocation: "浙江", ImageCount: 9,
			User:         xiaohongshu.User{UserID: "author1", Nickname: "作者"},
			InteractInfo: xiaohongshu.InteractInfo{LikedCount: "100"},
		},
		Comments: xiaohongshu.CommentList{
			HasMore: true,
			List: []xiaohongshu.Comment{{
				ID: "c1", Content: "一级评论", CreateTimeText: "2023-12-10 16:01",
				UserInfo: xiaohongshu.User{UserID: "u-c1", Nickname: "甲"},
				SubComments: []xiaohongshu.Comment{{
					ID: "c2", Content: "子评论",
					UserInfo: xiaohongshu.User{UserID: "u-c2", Nickname: "乙"},
				}},
			}},
		},
	}

	v := toNoteDetailView(tbl, "feed-req", "token-req", detail)

	// 笔记 ref 用的是请求时的句柄
	noteTarget, ok := tbl.lookup(v.Ref)
	require.True(t, ok)
	assert.Equal(t, "feed-req", noteTarget.FeedID)
	assert.Equal(t, "token-req", noteTarget.XsecToken)

	require.Len(t, v.CommentList, 1)
	top := v.CommentList[0]
	assert.Equal(t, "一级评论", top.Text)
	assert.Equal(t, "甲", top.Author)

	topTarget, ok := tbl.lookup(top.Ref)
	require.True(t, ok)
	assert.Equal(t, "c1", topTarget.CommentID)
	assert.Equal(t, "u-c1", topTarget.UserID)
	assert.Equal(t, "feed-req", topTarget.FeedID, "回复评论也要笔记句柄")

	require.Len(t, top.Replies, 1)
	subTarget, ok := tbl.lookup(top.Replies[0].Ref)
	require.True(t, ok)
	assert.Equal(t, "c2", subTarget.CommentID)

	assert.True(t, v.MoreComments)
	assert.Equal(t, 9, v.Images)
	assert.Nil(t, v.ImageList, "没要图片时不带明细")
}

// TestProfileViewGender 性别的 0/1/2 对模型没有意义，换成人话；未知就不输出。
func TestProfileViewGender(t *testing.T) {
	assert.Equal(t, "男", genderText(1))
	assert.Equal(t, "女", genderText(2))
	assert.Empty(t, genderText(0))
	assert.Empty(t, genderText(9))

	v := toProfileView(newRefTable(),
		xiaohongshu.UserBasicInfo{Nickname: "露营小分队", RedId: "12345678", Gender: 0},
		[]xiaohongshu.UserInteractions{{Type: "fans", Name: "粉丝", Count: "1.2万"}},
		nil)

	data, err := json.Marshal(v)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "gender", "性别未知时整个字段都不该出现")
	assert.Contains(t, string(data), "粉丝", "统计项用中文名当键，不用 fans")
	assert.NotContains(t, string(data), "imageb", "头像地址不该出现")
}
