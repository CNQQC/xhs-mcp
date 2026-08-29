package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestCompactFeedsListResponseFrom(t *testing.T) {
	result := &FeedsListResponse{
		Count: 1,
		Feeds: []xiaohongshu.Feed{{
			ID:        "feed-1",
			XsecToken: "token-1",
			ModelType: "note",
			Index:     7,
			NoteCard: xiaohongshu.NoteCard{
				Type:         "video",
				DisplayTitle: "标题",
				User:         xiaohongshu.User{UserID: "user-1", Nickname: "昵称", Avatar: "avatar"},
				InteractInfo: xiaohongshu.InteractInfo{
					Liked:          false,
					LikedCount:     "10",
					Collected:      true,
					CollectedCount: "3",
					CommentCount:   "2",
					SharedCount:    "4",
				},
				Cover: xiaohongshu.Cover{
					URLDefault: "https://img/default.jpg",
					URLPre:     "https://img/pre.jpg",
					FileID:     "internal-file-id",
					Width:      640,
					Height:     480,
					InfoList:   []xiaohongshu.ImageInfo{{ImageScene: "ORIGIN", URL: "https://img/origin.jpg"}},
				},
				Video: &xiaohongshu.Video{Capa: xiaohongshu.VideoCapability{Duration: 12}},
			},
		}},
	}

	encoded, err := json.Marshal(compactFeedsListResponseFrom(result))
	require.NoError(t, err)
	payload := string(encoded)

	assert.Contains(t, payload, `"id":"feed-1"`)
	assert.Contains(t, payload, `"xsecToken":"token-1"`)
	assert.Contains(t, payload, `"displayTitle":"标题"`)
	assert.Contains(t, payload, `"urlDefault":"https://img/default.jpg"`)
	assert.Contains(t, payload, `"duration":12`)
	for _, omitted := range []string{
		`"modelType"`, `"index"`, `"userId"`, `"nickname"`, `"avatar"`,
		`"fileId"`, `"urlPre"`, `"infoList"`,
	} {
		assert.NotContains(t, payload, omitted)
	}
}

func TestCompactFeedDetailResponseFrom(t *testing.T) {
	result := &FeedDetailResponse{
		FeedID: "feed-1",
		Data: &xiaohongshu.FeedDetailResponse{
			Note: xiaohongshu.FeedDetail{
				NoteID:     "feed-1",
				XsecToken:  "token-1",
				Title:      "标题",
				Desc:       "正文",
				Type:       "normal",
				Time:       1700000000000,
				TimeText:   "2023-11-15 06:13",
				IPLocation: "深圳",
				User:       xiaohongshu.User{UserID: "user-1", Nickname: "昵称", Avatar: "avatar"},
				InteractInfo: xiaohongshu.InteractInfo{
					LikedCount:     "10",
					CollectedCount: "3",
					CommentCount:   "2",
					SharedCount:    "4",
				},
				ImageList: []xiaohongshu.DetailImageInfo{{
					URLDefault: "https://img/detail.jpg",
					URLPre:     "https://img/pre.jpg",
					Width:      1080,
					Height:     1440,
				}},
				Video: &xiaohongshu.VideoDetail{
					Capa: xiaohongshu.VideoCapability{Duration: 20},
					Media: xiaohongshu.VideoMedia{Stream: map[string][]xiaohongshu.VideoStream{
						"h264": {
							{MasterURL: "https://video/low", Width: 640, Height: 360},
							{MasterURL: "https://video/default", Width: 1280, Height: 720, DefaultStream: 1, BackupURLs: []string{"https://video/backup"}, VideoCodec: "h264", VMAF: 99},
						},
					}},
					SubtitleText: "字幕正文",
					SubtitleLang: "zh-CN",
					Subtitles:    map[string][]xiaohongshu.VideoSubtitle{"zh-CN": {{URL: "https://subtitle/raw.srt"}}},
				},
			},
			Comments: xiaohongshu.CommentList{
				Cursor:  "opaque-cursor",
				HasMore: true,
				List: []xiaohongshu.Comment{{
					ID:              "comment-1",
					NoteID:          "feed-1",
					Content:         "评论正文",
					LikeCount:       "5",
					CreateTime:      1700000000000,
					CreateTimeText:  "2023-11-15 06:14",
					IPLocation:      "广州",
					UserInfo:        xiaohongshu.User{UserID: "comment-user", Nickname: "评论者"},
					SubCommentCount: "1",
					SubComments: []xiaohongshu.Comment{{
						ID:         "reply-1",
						Content:    "回复正文",
						CreateTime: 1700000001000,
						UserInfo:   xiaohongshu.User{UserID: "reply-user", Nickname: "回复者"},
					}},
				}},
			},
		},
	}

	compact, ok := compactFeedDetailResponseFrom(result)
	require.True(t, ok)
	encoded, err := json.Marshal(compact)
	require.NoError(t, err)
	payload := string(encoded)

	assert.Contains(t, payload, `"feed_id":"feed-1"`)
	assert.Contains(t, payload, `"xsecToken":"token-1"`)
	assert.Contains(t, payload, `"title":"标题"`)
	assert.Contains(t, payload, `"desc":"正文"`)
	assert.Contains(t, payload, `"url":"https://video/default"`)
	assert.Contains(t, payload, `"subtitleText":"字幕正文"`)
	assert.Contains(t, payload, `"id":"comment-1"`)
	assert.Contains(t, payload, `"content":"评论正文"`)
	assert.Contains(t, payload, `"id":"reply-1"`)
	for _, omitted := range []string{
		`"userId"`, `"nickname"`, `"avatar"`, `"ipLocation"`, `"cursor"`,
		`"backupUrls"`, `"videoCodec"`, `"vmaf"`, `"subtitles"`, `"urlPre"`,
	} {
		assert.NotContains(t, payload, omitted)
	}
	assert.NotContains(t, payload, `"createTime":1700000000000`)
}

func TestCompactFeedDetailResponseFromRejectsUnknownData(t *testing.T) {
	compact, ok := compactFeedDetailResponseFrom(&FeedDetailResponse{Data: map[string]any{}})
	assert.False(t, ok)
	assert.Nil(t, compact)
}

func TestCompactResponsesDoNotIndentJSON(t *testing.T) {
	encoded, err := json.Marshal(compactFeedsListResponseFrom(&FeedsListResponse{}))
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(encoded), "\n"))
}
