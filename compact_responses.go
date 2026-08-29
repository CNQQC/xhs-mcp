package main

import "github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"

// compactFeedsListResponse is the stable, small response contract exposed by
// search_feeds.  Keep id and xsecToken because callers need them for
// get_feed_detail and the interaction tools; omit page-internal metadata and
// user identity data.
type compactFeedsListResponse struct {
	Feeds []compactFeed `json:"feeds"`
	Count int           `json:"count"`
}

type compactFeed struct {
	ID        string            `json:"id"`
	XsecToken string            `json:"xsecToken"`
	NoteCard  compactSearchCard `json:"noteCard"`
}

type compactSearchCard struct {
	Type         string               `json:"type"`
	DisplayTitle string               `json:"displayTitle"`
	InteractInfo *compactInteractInfo `json:"interactInfo,omitempty"`
	Cover        *compactSearchCover  `json:"cover,omitempty"`
	Video        *compactSearchVideo  `json:"video,omitempty"`
}

type compactSearchCover struct {
	URLDefault string `json:"urlDefault,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
}

type compactSearchVideo struct {
	Duration int `json:"duration,omitempty"`
}

// compactInteractInfo keeps state and counts that are useful for ranking or
// deciding whether a later interaction is necessary.
type compactInteractInfo struct {
	Liked          bool   `json:"liked"`
	LikedCount     string `json:"likedCount,omitempty"`
	Collected      bool   `json:"collected"`
	CollectedCount string `json:"collectedCount,omitempty"`
	CommentCount   string `json:"commentCount,omitempty"`
	SharedCount    string `json:"sharedCount,omitempty"`
}

func compactFeedsListResponseFrom(result *FeedsListResponse) *compactFeedsListResponse {
	if result == nil {
		return &compactFeedsListResponse{Feeds: []compactFeed{}}
	}

	compact := &compactFeedsListResponse{
		Feeds: make([]compactFeed, 0, len(result.Feeds)),
		Count: result.Count,
	}

	for _, feed := range result.Feeds {
		item := compactFeed{
			ID:        feed.ID,
			XsecToken: feed.XsecToken,
			NoteCard: compactSearchCard{
				Type:         feed.NoteCard.Type,
				DisplayTitle: feed.NoteCard.DisplayTitle,
				InteractInfo: compactInteractInfoFrom(feed.NoteCard.InteractInfo),
				Cover:        compactSearchCoverFrom(feed.NoteCard.Cover),
			},
		}
		if feed.NoteCard.Video != nil {
			item.NoteCard.Video = &compactSearchVideo{Duration: feed.NoteCard.Video.Capa.Duration}
		}
		compact.Feeds = append(compact.Feeds, item)
	}

	return compact
}

// compactFeedDetailResponse is intentionally explicit.  The source structs
// mirror the page's JSON and contain user IDs, avatars, IP locations, every
// video encoding, backup URLs, signed subtitle URLs, and other implementation
// details that are not needed by an AI caller.
type compactFeedDetailResponse struct {
	FeedID string                `json:"feed_id"`
	Data   compactFeedDetailData `json:"data"`
}

type compactFeedDetailData struct {
	Note     compactDetailNote  `json:"note"`
	Comments compactCommentList `json:"comments"`
}

type compactDetailNote struct {
	NoteID       string               `json:"noteId,omitempty"`
	XsecToken    string               `json:"xsecToken,omitempty"`
	Title        string               `json:"title,omitempty"`
	Desc         string               `json:"desc,omitempty"`
	Type         string               `json:"type,omitempty"`
	Time         int64                `json:"time,omitempty"`
	TimeText     string               `json:"timeText,omitempty"`
	InteractInfo *compactInteractInfo `json:"interactInfo,omitempty"`
	ImageList    []compactDetailImage `json:"imageList,omitempty"`
	Video        *compactDetailVideo  `json:"video,omitempty"`
}

type compactDetailImage struct {
	URLDefault string `json:"urlDefault,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	LivePhoto  bool   `json:"livePhoto,omitempty"`
}

type compactDetailVideo struct {
	Duration     int    `json:"duration,omitempty"`
	URL          string `json:"url,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	SubtitleText string `json:"subtitleText,omitempty"`
	SubtitleLang string `json:"subtitleLang,omitempty"`
}

type compactCommentList struct {
	List    []compactComment `json:"list"`
	HasMore bool             `json:"hasMore"`
}

type compactComment struct {
	ID              string           `json:"id,omitempty"`
	Content         string           `json:"content,omitempty"`
	LikeCount       string           `json:"likeCount,omitempty"`
	CreateTime      int64            `json:"createTime,omitempty"`
	CreateTimeText  string           `json:"createTimeText,omitempty"`
	SubCommentCount string           `json:"subCommentCount,omitempty"`
	SubComments     []compactComment `json:"subComments,omitempty"`
}

func compactInteractInfoFrom(info xiaohongshu.InteractInfo) *compactInteractInfo {
	return &compactInteractInfo{
		Liked:          info.Liked,
		LikedCount:     info.LikedCount,
		Collected:      info.Collected,
		CollectedCount: info.CollectedCount,
		CommentCount:   info.CommentCount,
		SharedCount:    info.SharedCount,
	}
}

func compactSearchCoverFrom(cover xiaohongshu.Cover) *compactSearchCover {
	url := cover.URLDefault
	if url == "" {
		url = cover.URL
	}
	if url == "" {
		url = cover.URLPre
	}
	if url == "" && cover.Width == 0 && cover.Height == 0 {
		return nil
	}
	return &compactSearchCover{URLDefault: url, Width: cover.Width, Height: cover.Height}
}

func compactFeedDetailResponseFrom(result *FeedDetailResponse) (*compactFeedDetailResponse, bool) {
	if result == nil {
		return nil, false
	}

	detail, ok := feedDetailValue(result.Data)
	if !ok {
		return nil, false
	}

	compact := &compactFeedDetailResponse{
		FeedID: result.FeedID,
		Data: compactFeedDetailData{
			Note: compactDetailNoteFrom(detail.Note),
			Comments: compactCommentList{
				List:    compactCommentsFrom(detail.Comments.List),
				HasMore: detail.Comments.HasMore,
			},
		},
	}
	return compact, true
}

func feedDetailValue(value any) (xiaohongshu.FeedDetailResponse, bool) {
	switch detail := value.(type) {
	case *xiaohongshu.FeedDetailResponse:
		if detail == nil {
			return xiaohongshu.FeedDetailResponse{}, false
		}
		return *detail, true
	case xiaohongshu.FeedDetailResponse:
		return detail, true
	default:
		return xiaohongshu.FeedDetailResponse{}, false
	}
}

func compactDetailNoteFrom(note xiaohongshu.FeedDetail) compactDetailNote {
	compact := compactDetailNote{
		NoteID:       note.NoteID,
		XsecToken:    note.XsecToken,
		Title:        note.Title,
		Desc:         note.Desc,
		Type:         note.Type,
		Time:         note.Time,
		TimeText:     note.TimeText,
		InteractInfo: compactInteractInfoFrom(note.InteractInfo),
	}

	if len(note.ImageList) > 0 {
		compact.ImageList = make([]compactDetailImage, 0, len(note.ImageList))
		for _, image := range note.ImageList {
			compact.ImageList = append(compact.ImageList, compactDetailImage{
				URLDefault: image.URLDefault,
				Width:      image.Width,
				Height:     image.Height,
				LivePhoto:  image.LivePhoto,
			})
		}
	}

	if note.Video != nil {
		compact.Video = compactVideoFrom(*note.Video)
	}
	return compact
}

func compactVideoFrom(video xiaohongshu.VideoDetail) *compactDetailVideo {
	compact := &compactDetailVideo{
		Duration:     video.Capa.Duration,
		SubtitleText: video.SubtitleText,
		SubtitleLang: video.SubtitleLang,
	}

	if stream := bestVideoStream(video.Media.Stream); stream != nil {
		compact.URL = videoStreamURL(stream)
		compact.Width = stream.Width
		compact.Height = stream.Height
	}
	return compact
}

func bestVideoStream(streams map[string][]xiaohongshu.VideoStream) *xiaohongshu.VideoStream {
	var best *xiaohongshu.VideoStream
	for _, bucket := range streams {
		for i := range bucket {
			candidate := &bucket[i]
			if videoStreamURL(candidate) == "" {
				continue
			}
			if best == nil || videoStreamScoreGreater(candidate, best) {
				best = candidate
			}
		}
	}
	return best
}

func videoStreamScore(stream *xiaohongshu.VideoStream) [3]int {
	defaultStream := 0
	if stream.DefaultStream != 0 {
		defaultStream = 1
	}
	return [3]int{defaultStream, stream.Width * stream.Height, stream.Width}
}

func videoStreamScoreGreater(left, right *xiaohongshu.VideoStream) bool {
	leftScore := videoStreamScore(left)
	rightScore := videoStreamScore(right)
	for i := range leftScore {
		if leftScore[i] == rightScore[i] {
			continue
		}
		return leftScore[i] > rightScore[i]
	}
	return videoStreamURL(left) < videoStreamURL(right)
}

func videoStreamURL(stream *xiaohongshu.VideoStream) string {
	if stream.MasterURL != "" {
		return stream.MasterURL
	}
	if len(stream.BackupURLs) > 0 {
		return stream.BackupURLs[0]
	}
	return ""
}

func compactCommentsFrom(comments []xiaohongshu.Comment) []compactComment {
	compact := make([]compactComment, 0, len(comments))
	for _, comment := range comments {
		item := compactComment{
			ID:              comment.ID,
			Content:         comment.Content,
			LikeCount:       comment.LikeCount,
			CreateTime:      comment.CreateTime,
			CreateTimeText:  comment.CreateTimeText,
			SubCommentCount: comment.SubCommentCount,
		}
		if item.CreateTimeText != "" {
			item.CreateTime = 0
		}
		if len(comment.SubComments) > 0 {
			item.SubComments = compactCommentsFrom(comment.SubComments)
		}
		compact = append(compact, item)
	}
	return compact
}
