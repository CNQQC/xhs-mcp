package main

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// MCP 返回体的投影层。
//
// 服务层的结构体是按小红书原始数据映射的，字段名和形状都随站点走；REST 调用方要的
// 正是那一份。但 MCP 的调用方是大模型，它只读得懂内容，读不懂机器句柄——而句柄按
// 条数成倍出现，一次搜索就能吃掉几千 token。
//
// 所以 MCP 这条路单独投影一遍：句柄换成一个 ref（见 mcp_ref.go），其余只留能读的
// 东西，另保留研究引用需要的稳定 note_id、来源链接与抓取时间。REST 数据不受投影影响。
//
// 字段名一律用短的通俗词（title/author/likes），不跟站点的 displayTitle、likedCount
// 走：键名本身也要占 token，而这一层的读者只有模型。

// noteView 列表与搜索里的一条笔记。
type noteView struct {
	NoteID       string `json:"note_id,omitempty"`
	SourceURL    string `json:"source_url,omitempty"`
	RetrievedAt  string `json:"retrieved_at,omitempty"`
	RefExpiresAt string `json:"ref_expires_at,omitempty"`
	Ref          string `json:"ref"`
	Type         string `json:"type,omitempty"` // normal 图文 / video 视频
	Title        string `json:"title,omitempty"`
	Author       string `json:"author,omitempty"`
	Likes        string `json:"likes,omitempty"`
	Comments     string `json:"comments,omitempty"`
	Collects     string `json:"collects,omitempty"`
	Duration     int    `json:"duration,omitempty"` // 秒，视频笔记才有
}

// noteListView list_feeds / search_feeds 的返回体。
type noteListView struct {
	Coverage *listCoverage `json:"coverage,omitempty"`
	Notes    []noteView    `json:"notes"`
	Count    int           `json:"count"`
}

// commentView 一条评论。
//
// 保留作者、内容、发布时间、点赞数、回复所需的 ref 及子回复。
// 发布时间与笔记的 time 字段一致，使用东八区可读时间。
type commentView struct {
	Ref     string        `json:"ref"`
	Author  string        `json:"author,omitempty"`
	Text    string        `json:"text,omitempty"`
	Time    string        `json:"time,omitempty"`
	Likes   string        `json:"likes,omitempty"`
	Replies []commentView `json:"replies,omitempty"`
}

// noteDetailView get_feed_detail 的返回体。
type noteDetailView struct {
	NoteID       string          `json:"note_id,omitempty"`
	SourceURL    string          `json:"source_url,omitempty"`
	RetrievedAt  string          `json:"retrieved_at,omitempty"`
	RefExpiresAt string          `json:"ref_expires_at,omitempty"`
	Coverage     *detailCoverage `json:"coverage,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
	Ref          string          `json:"ref"`
	Type         string          `json:"type,omitempty"`
	Title        string          `json:"title,omitempty"`
	Desc         string          `json:"desc,omitempty"`
	Author       string          `json:"author,omitempty"`
	Time         string          `json:"time,omitempty"`     // 东八区可读时间
	Location     string          `json:"location,omitempty"` // 发布者 IP 归属地
	Likes        string          `json:"likes,omitempty"`
	Comments     string          `json:"comments,omitempty"`
	Collects     string          `json:"collects,omitempty"`

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

// noteBatchItem get_feed_details 里的一条。失败项保留 ref、已知 note_id 和错误信息。
type noteBatchItem struct {
	noteDetailView
	Error     string    `json:"error,omitempty"`
	ErrorInfo *MCPError `json:"error_info,omitempty"`
}

// noteBatchView get_feed_details 的返回体，notes 顺序与请求的 refs 一致。
type noteBatchView struct {
	Notes  []noteBatchItem `json:"notes"`
	Count  int             `json:"count"`
	Failed int             `json:"failed,omitempty"`
}

// profileView user_profile / get_my_profile 的返回体。
type profileView struct {
	Coverage *listCoverage `json:"coverage,omitempty"`
	Nickname string        `json:"nickname,omitempty"`
	RedID    string        `json:"redId,omitempty"` // 小红书号，是给人看的账号名，不是内部 ID
	Desc     string        `json:"desc,omitempty"`
	Location string        `json:"location,omitempty"`
	Gender   string        `json:"gender,omitempty"`
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
		ref, retrievedAt, expiresAt := refs.putWithMetadata(refTarget{
			FeedID: f.ID, XsecToken: f.XsecToken, UserID: card.User.UserID,
		})
		v := noteView{
			Ref:          ref,
			NoteID:       f.ID,
			SourceURL:    safeNoteSourceURL(f.ID, f.XsecToken),
			RetrievedAt:  retrievedAt.Format(time.RFC3339Nano),
			RefExpiresAt: expiresAt.Format(time.RFC3339Nano),
			Type:         card.Type,
			Title:        card.DisplayTitle,
			Author:       card.User.Nickname,
			Likes:        card.InteractInfo.LikedCount,
			Comments:     card.InteractInfo.CommentCount,
			Collects:     card.InteractInfo.CollectedCount,
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
	return toNoteDetailViewWithRef(refs, feedID, xsecToken, detail, "")
}

// toNoteDetailViewWithRef 复用批量请求中的 ref，不重复登记笔记，
// 不延长 TTL，也不因多余登记而淘汰已有 ref。
func toNoteDetailViewWithRef(
	refs *refTable, feedID, xsecToken string, detail *xiaohongshu.FeedDetailResponse, existingRef string,
) noteDetailView {
	note := detail.Note

	ref := existingRef
	retrievedAt := time.Now().UTC()
	expiresAt := ""
	if ref == "" {
		var expiry time.Time
		ref, retrievedAt, expiry = refs.putWithMetadata(refTarget{
			FeedID: feedID, XsecToken: xsecToken, UserID: note.User.UserID,
		})
		expiresAt = expiry.Format(time.RFC3339Nano)
	}
	v := noteDetailView{
		Ref:          ref,
		NoteID:       feedID,
		SourceURL:    safeNoteSourceURL(feedID, xsecToken),
		RetrievedAt:  retrievedAt.Format(time.RFC3339Nano),
		RefExpiresAt: expiresAt,
		Type:         note.Type,
		Title:        note.Title,
		Desc:         note.Desc,
		Author:       note.User.Nickname,
		Time:         note.TimeText,
		Location:     note.IPLocation,
		Likes:        note.InteractInfo.LikedCount,
		Comments:     note.InteractInfo.CommentCount,
		Collects:     note.InteractInfo.CollectedCount,
		Images:       note.ImageCount,
		ImageList:    note.ImageList,
	}

	if note.Video != nil {
		v.Duration = note.Video.Capa.Duration
		v.Subtitle = note.Video.SubtitleText
		v.SubtitleLang = note.Video.SubtitleLang
	}

	v.CommentList = toCommentViews(refs, feedID, xsecToken, detail.Comments.List)
	v.MoreComments = detail.Comments.HasMore
	v.Coverage, v.Warnings = measureDetailCoverage(detail)
	if existingRef != "" {
		v.RefExpiresAt = refs.expiresAt(existingRef)
	}

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
			Author:  c.UserInfo.Nickname,
			Text:    c.Content,
			Time:    c.CreateTimeText,
			Likes:   c.LikeCount,
			Replies: toCommentViews(refs, feedID, xsecToken, c.SubComments),
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
		Coverage: newListCoverage("profile", len(feeds)),
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

// source_url 是访问链接，不保证永久可用。缺少可用 token 时仍保留 note_id，
// 但不使用无效 ID、占位 token 或不安全的字符串拼接构造来源链接。
func safeNoteSourceURL(noteID, token string) string {
	if len(noteID) != 24 || strings.TrimSpace(token) == "" || strings.TrimSpace(token) != token {
		return ""
	}
	for _, c := range noteID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	for _, c := range token {
		if unicode.IsControl(c) || unicode.IsSpace(c) {
			return ""
		}
	}
	switch strings.ToLower(token) {
	case "null", "undefined", "token", "xsec_token", "your_token", "your_xsec_token", "...", "<token>", "<xsec_token>":
		return ""
	}
	query := url.Values{"xsec_token": {token}, "xsec_source": {"pc_feed"}}
	return "https://www.xiaohongshu.com/explore/" + noteID + "?" + query.Encode()
}

type listCoverage struct {
	Source                string `json:"source"`
	Scope                 string `json:"scope"`
	ReturnedCount         int    `json:"returned_count"`
	ContinuationSupported bool   `json:"continuation_supported"`
	Exhaustive            bool   `json:"exhaustive"`
}

func newListCoverage(source string, count int) *listCoverage {
	return &listCoverage{Source: source, Scope: "first_page", ReturnedCount: count}
}

func toNoteListView(refs *refTable, feeds []xiaohongshu.Feed, source string) noteListView {
	return noteListView{Notes: toNoteViews(refs, feeds), Count: len(feeds), Coverage: newListCoverage(source, len(feeds))}
}

type detailCoverage struct {
	Comments commentCoverage  `json:"comments"`
	Subtitle subtitleCoverage `json:"subtitle"`
}

type commentCoverage struct {
	Mode                      string `json:"mode"`
	ReturnedTopLevel          int    `json:"returned_top_level"`
	ReturnedReplies           int    `json:"returned_replies"`
	HasMoreTopLevel           *bool  `json:"has_more_top_level,omitempty"`
	FirstRequestFinished      *bool  `json:"first_request_finished,omitempty"`
	TopLevelComplete          *bool  `json:"top_level_complete,omitempty"`
	RepliesComplete           *bool  `json:"replies_complete,omitempty"`
	RepliesScope              string `json:"replies_scope"`
	RequestedLimit            int    `json:"requested_limit,omitempty"`
	RepliesExpansionRequested bool   `json:"replies_expansion_requested"`
	RepliesExpansionThreshold int    `json:"replies_expansion_threshold,omitempty"`
}

type subtitleCoverage struct {
	Status   string `json:"status"`
	Language string `json:"language,omitempty"`
}

// applyDetailRequestCoverage 记录实际请求，不虚构续页游标。
// 即使请求了更大的评论上限，也不能据此认定结果完整。
func applyDetailRequestCoverage(v *noteDetailView, loadAll bool, config xiaohongshu.FeedDetailConfig) {
	if v.Coverage == nil {
		return
	}
	v.Coverage.Comments.Mode = "first_screen"
	v.Coverage.Comments.RequestedLimit = 0
	v.Coverage.Comments.RepliesExpansionRequested = false
	v.Coverage.Comments.RepliesExpansionThreshold = 0
	if loadAll {
		v.Coverage.Comments.Mode = "scrolled"
		limit := config.MaxCommentItems
		if limit <= 0 {
			limit = xiaohongshu.DefaultFeedDetailConfig().MaxCommentItems
		}
		v.Coverage.Comments.RequestedLimit = limit
		v.Coverage.Comments.RepliesExpansionRequested = config.ClickMoreReplies
		if config.ClickMoreReplies {
			threshold := config.MaxRepliesThreshold
			if threshold <= 0 {
				threshold = xiaohongshu.DefaultFeedDetailConfig().MaxRepliesThreshold
			}
			v.Coverage.Comments.RepliesExpansionThreshold = threshold
		}
	}
}

func boolPointer(value bool) *bool { return &value }

func measureDetailCoverage(detail *xiaohongshu.FeedDetailResponse) (*detailCoverage, []string) {
	comments := detail.Comments
	coverage := &detailCoverage{
		Comments: commentCoverage{Mode: "loaded_page", ReturnedTopLevel: len(comments.List), FirstRequestFinished: comments.FirstRequestFinish, RepliesScope: "returned_top_level_comments"},
		Subtitle: subtitleCoverage{Status: "not_applicable"},
	}
	var warnings []string
	if comments.HasMoreKnown || comments.HasMore {
		coverage.Comments.HasMoreTopLevel = boolPointer(comments.HasMore)
	}
	if comments.HasMore {
		coverage.Comments.TopLevelComplete = boolPointer(false)
	} else if comments.HasMoreKnown && comments.FirstRequestFinish != nil && *comments.FirstRequestFinish && detail.CommentsLoadWarning == "" {
		coverage.Comments.TopLevelComplete = boolPointer(true)
	}
	if comments.FirstRequestFinish != nil && !*comments.FirstRequestFinish {
		coverage.Comments.TopLevelComplete = boolPointer(false)
	}
	coverage.Comments.ReturnedReplies, coverage.Comments.RepliesComplete = measureReplies(comments.List)
	if len(comments.List) == 0 && coverage.Comments.TopLevelComplete != nil && *coverage.Comments.TopLevelComplete {
		coverage.Comments.RepliesComplete = boolPointer(true)
	}
	if detail.CommentsLoadWarning != "" {
		// 加载失败本身不能证明有遗漏评论，保留未知状态，
		// 与已观察到 hasMore 或首批请求未完成的情况区分。
		warnings = append(warnings, detail.CommentsLoadWarning)
	}
	if note := detail.Note; note.Video != nil {
		coverage.Subtitle.Status = note.Video.SubtitleStatus
		coverage.Subtitle.Language = note.Video.SubtitleLang
		if coverage.Subtitle.Status == "" {
			coverage.Subtitle.Status = "unknown"
			if note.Video.SubtitleText != "" {
				coverage.Subtitle.Status = "available"
			}
		}
		if note.Video.SubtitleWarning != "" {
			warnings = append(warnings, note.Video.SubtitleWarning)
		}
	} else if detail.Note.Type == "video" {
		coverage.Subtitle.Status = "unknown"
	}
	return coverage, warnings
}

// 只有每条已返回父评论的精确回复数都与已返回子评论匹配，才认定回复完整。
// 「1万」等缩写、缺失计数和未展开回复保留为未知或不完整。
func measureReplies(comments []xiaohongshu.Comment) (returned int, complete *bool) {
	if len(comments) == 0 {
		return 0, nil
	}
	known, all := true, true
	for _, comment := range comments {
		returned += len(comment.SubComments)
		count, err := strconv.Atoi(strings.TrimSpace(comment.SubCommentCount))
		if err != nil || count < 0 {
			known = false
		} else if count > len(comment.SubComments) {
			all = false
		} else if count < len(comment.SubComments) {
			known = false
		}
		// SubComments 通常是扁平列表；源数据有更深嵌套时，同样统计。
		for _, child := range comment.SubComments {
			if len(child.SubComments) > 0 || child.SubCommentCount != "" && child.SubCommentCount != "0" {
				n, childComplete := measureReplies([]xiaohongshu.Comment{child})
				returned += n
				if childComplete == nil {
					known = false
				} else if !*childComplete {
					all = false
				}
			}
		}
	}
	if !all {
		return returned, boolPointer(false)
	}
	if known {
		return returned, boolPointer(true)
	}
	return returned, nil
}
