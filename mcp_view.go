package main

import (
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// MCP 返回体的投影层。
//
// 服务层的结构体是按小红书原始数据映射的，字段名和形状都随站点走；REST 调用方要的
// 正是那一份。但 MCP 的调用方是大模型，它只读得懂内容，读不懂机器句柄——而句柄按
// 条数成倍出现，一次搜索就能吃掉几千 token。
//
// 所以 MCP 这条路单独投影一遍：句柄换成一个 ref（见 mcp_ref.go），其余只留能读的
// 东西。REST 那条路一个字节都不变。
//
// 字段名一律用短的通俗词（title/author/likes），不跟站点的 displayTitle、likedCount
// 走：键名本身也要占 token，而这一层的读者只有模型。

// noteView 列表与搜索里的一条笔记。
type noteView struct {
	Ref      string `json:"ref"`
	Type     string `json:"type,omitempty"` // normal 图文 / video 视频
	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
	Likes    string `json:"likes,omitempty"`
	Comments string `json:"comments,omitempty"`
	Collects string `json:"collects,omitempty"`
	Duration int    `json:"duration,omitempty"` // 秒，视频笔记才有
}

// noteListView list_feeds / search_feeds 的返回体。
type noteListView struct {
	Notes []noteView `json:"notes"`
	Count int        `json:"count"`
}

// commentView 一条评论。
type commentView struct {
	Ref      string        `json:"ref"`
	Author   string        `json:"author,omitempty"`
	Text     string        `json:"text,omitempty"`
	Likes    string        `json:"likes,omitempty"`
	Time     string        `json:"time,omitempty"`
	Location string        `json:"location,omitempty"`
	Replies  []commentView `json:"replies,omitempty"`
}

// noteDetailView get_feed_detail 的返回体。
type noteDetailView struct {
	Ref      string `json:"ref"`
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	Desc     string `json:"desc,omitempty"`
	Author   string `json:"author,omitempty"`
	Time     string `json:"time,omitempty"`     // 东八区可读时间
	Location string `json:"location,omitempty"` // 发布者 IP 归属地
	Likes    string `json:"likes,omitempty"`
	Comments string `json:"comments,omitempty"`
	Collects string `json:"collects,omitempty"`

	Images int `json:"images,omitempty"` // 图片张数
	// ImageList 只在 include_images=true 时出现。这一项是原样透出的站点数据：
	// 要它的场合是「把图片地址转交给别人」，那就得是真地址。
	ImageList []xiaohongshu.DetailImageInfo `json:"imageList,omitempty"`

	Duration     int    `json:"duration,omitempty"` // 秒
	Subtitle     string `json:"subtitle,omitempty"` // 视频字幕正文
	SubtitleLang string `json:"subtitleLang,omitempty"`

	CommentList  []commentView `json:"commentList,omitempty"`
	MoreComments bool          `json:"moreComments,omitempty"` // 还有没加载完的评论
}

// profileView user_profile / get_my_profile 的返回体。
type profileView struct {
	Nickname string `json:"nickname,omitempty"`
	RedID    string `json:"redId,omitempty"` // 小红书号，是给人看的账号名，不是内部 ID
	Desc     string `json:"desc,omitempty"`
	Location string `json:"location,omitempty"`
	Gender   string `json:"gender,omitempty"`
	// Stats 关注/粉丝/获赞与收藏，站点给什么就是什么，键为「关注」这类中文名
	Stats map[string]string `json:"stats,omitempty"`
	Notes []noteView        `json:"notes,omitempty"`
	Count int               `json:"count,omitempty"`
}

// genderText 站点用 0/1/2 表示性别，数字对模型没有意义。
// 取值含义按站点惯例：1 男 2 女，其余（含 0）当未知，不输出。
func genderText(gender int) string {
	switch gender {
	case 1:
		return "男"
	case 2:
		return "女"
	default:
		return ""
	}
}

// toNoteViews 把一批 Feed 投影成可读列表，顺带把句柄登记进 ref 表。
func toNoteViews(refs *refTable, feeds []xiaohongshu.Feed) []noteView {
	out := make([]noteView, 0, len(feeds))
	for _, f := range feeds {
		card := f.NoteCard
		v := noteView{
			Ref: refs.put(refTarget{
				FeedID:    f.ID,
				XsecToken: f.XsecToken,
				UserID:    card.User.UserID,
			}),
			Type:     card.Type,
			Title:    card.DisplayTitle,
			Author:   card.User.Nickname,
			Likes:    card.InteractInfo.LikedCount,
			Comments: card.InteractInfo.CommentCount,
			Collects: card.InteractInfo.CollectedCount,
		}
		if card.Video != nil {
			v.Duration = card.Video.Capa.Duration
		}
		out = append(out, v)
	}

	return out
}

// toNoteDetailView 把详情投影成可读结构。
//
// feedID/xsecToken 从请求带下来而不是从返回体里取：详情页的 note.xsecToken 与
// 请求时用的那个不一定是同一个，而后续的点赞/评论要复用的是请求时那一个。
func toNoteDetailView(
	refs *refTable, feedID, xsecToken string, detail *xiaohongshu.FeedDetailResponse,
) noteDetailView {
	note := detail.Note

	v := noteDetailView{
		Ref: refs.put(refTarget{
			FeedID:    feedID,
			XsecToken: xsecToken,
			UserID:    note.User.UserID,
		}),
		Type:      note.Type,
		Title:     note.Title,
		Desc:      note.Desc,
		Author:    note.User.Nickname,
		Time:      note.TimeText,
		Location:  note.IPLocation,
		Likes:     note.InteractInfo.LikedCount,
		Comments:  note.InteractInfo.CommentCount,
		Collects:  note.InteractInfo.CollectedCount,
		Images:    note.ImageCount,
		ImageList: note.ImageList,
	}

	if note.Video != nil {
		v.Duration = note.Video.Capa.Duration
		v.Subtitle = note.Video.SubtitleText
		v.SubtitleLang = note.Video.SubtitleLang
	}

	v.CommentList = toCommentViews(refs, feedID, xsecToken, detail.Comments.List)
	v.MoreComments = detail.Comments.HasMore

	return v
}

// toCommentViews 递归投影评论树。
//
// 每条评论都要有自己的 ref：reply_comment_in_feed 要的是评论 ID 和评论者 ID，
// 而这两个同样是模型读不懂、只需要传回来的东西。
func toCommentViews(
	refs *refTable, feedID, xsecToken string, comments []xiaohongshu.Comment,
) []commentView {
	if len(comments) == 0 {
		return nil
	}

	out := make([]commentView, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentView{
			Ref: refs.put(refTarget{
				FeedID:    feedID,
				XsecToken: xsecToken,
				UserID:    c.UserInfo.UserID,
				CommentID: c.ID,
			}),
			Author:   c.UserInfo.Nickname,
			Text:     c.Content,
			Likes:    c.LikeCount,
			Time:     c.CreateTimeText,
			Location: c.IPLocation,
			Replies:  toCommentViews(refs, feedID, xsecToken, c.SubComments),
		})
	}

	return out
}

// toProfileView 把用户主页投影成可读结构。
func toProfileView(
	refs *refTable, info xiaohongshu.UserBasicInfo,
	interactions []xiaohongshu.UserInteractions, feeds []xiaohongshu.Feed,
) profileView {
	v := profileView{
		Nickname: info.Nickname,
		RedID:    info.RedId,
		Desc:     info.Desc,
		Location: info.IpLocation,
		Gender:   genderText(info.Gender),
		Notes:    toNoteViews(refs, feeds),
		Count:    len(feeds),
	}

	if len(interactions) > 0 {
		v.Stats = make(map[string]string, len(interactions))
		for _, it := range interactions {
			// 用中文名当键：type 是 follows/fans 这类站点内部词，name 才是人话
			v.Stats[it.Name] = it.Count
		}
	}

	return v
}
