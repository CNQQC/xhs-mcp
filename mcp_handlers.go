package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// MCP 工具处理函数

// parseVisibility 从 MCP 参数中解析可见范围
func parseVisibility(args map[string]interface{}) string {
	v, ok := args["visibility"]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// handleCheckLoginStatus 处理检查登录状态
func (s *AppServer) handleCheckLoginStatus(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 检查登录状态")

	status, err := s.xiaohongshuService.CheckLoginStatus(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "检查登录状态失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	var resultText string
	if status.IsLoggedIn {
		resultText = fmt.Sprintf("✅ 已登录\n用户名: %s\n\n你可以使用其他功能了。", status.Username)
	} else {
		resultText = "❌ 未登录\n\n请使用 get_login_qrcode 工具获取二维码进行登录。"
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handleGetLoginQrcode 处理获取登录二维码请求。
// 返回二维码图片的 Base64 编码和超时时间，供前端展示扫码登录。
func (s *AppServer) handleGetLoginQrcode(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取登录扫码图片")

	result, err := s.xiaohongshuService.GetLoginQrcode(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取登录扫码图片失败: " + err.Error()}},
			IsError: true,
		}
	}

	if result.IsLoggedIn {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "你当前已处于登录状态"}},
		}
	}

	now := time.Now()
	deadline := func() string {
		d, err := time.ParseDuration(result.Timeout)
		if err != nil {
			return now.Format("2006-01-02 15:04:05")
		}
		return now.Add(d).Format("2006-01-02 15:04:05")
	}()

	// 已登录：文本 + 图片
	contents := []MCPContent{
		{Type: "text", Text: "请用小红书 App 在 " + deadline + " 前扫码登录 👇"},
		{
			Type:     "image",
			MimeType: "image/png",
			Data:     strings.TrimPrefix(result.Img, "data:image/png;base64,"),
		},
	}
	return &MCPToolResult{Content: contents}
}

// handleGetVerificationQrcode 取安全验证二维码，原样透出给账号本人扫。
//
// 只做搬运：码是小红书发的，扫的是用户自己的 App，服务端这边不解码、不识别、不提交。
func (s *AppServer) handleGetVerificationQrcode(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取安全验证二维码")

	result, err := s.xiaohongshuService.GetVerificationQrcode(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取安全验证二维码失败: " + err.Error()}},
			IsError: true,
		}
	}

	if !result.Blocked {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: result.Message}}}
	}

	// 等待上限说成相对时长而不是绝对时间：这是 hosted MCP，服务端多半跑在 UTC，
	// 而读这句话的人在自己的时区，写「等到 15:04:05」只会让人算错还以为码已经废了。
	// 时长直接取响应里的 Timeout，不另外读包常量——两个来源迟早会对不上。
	text := fmt.Sprintf("%s\n\n请用已登录该账号的小红书 App 扫下面这张码完成验证 👇\n"+
		"二维码约 1 分钟失效，服务端最多等 %s；过期就重新调用本工具取一张新的。\n"+
		"验证通过后 cookies 会自动保存，直接重试原来的操作即可。\n\n%s",
		result.Message, result.Timeout, verificationLinkHint(result))

	mimeType, data, ok := splitDataURI(result.Img)
	if !ok {
		// 取到 src 却不是 data-URI，多半是站点改成了远端图片地址。这时把 src 原样交出去，
		// 也别当 base64 硬塞进 image——那只会让客户端渲染出一张碎图，等于什么都没给。
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: text + "\n\n二维码地址：" + result.Img}},
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{
			{Type: "text", Text: text},
			{Type: "image", MimeType: mimeType, Data: data},
		},
	}
}

// verificationLinkHint 把「浏览器直达」这条路一并交出去。
//
// 两条路都要能走：内联图片给能看到对话的场合，链接给「人在手机跟前、登不上 VPS」
// 的场合。链接那条不会跟着码一起废掉——网页会自己保鲜，扫到为止。
//
// 没配 XHS_PUBLIC_BASE_URL 时绝不拼一条链接出来：服务端猜不到自己的公网地址，
// 猜错的链接用户点不开，只会以为功能坏了。这时把相对路径和该配什么说清楚。
func verificationLinkHint(r *VerificationQrcodeResponse) string {
	if r.VerifyPath == "" {
		return ""
	}

	if r.VerifyURL == "" {
		return fmt.Sprintf("服务端未配置 %s，给不出可点击的链接。"+
			"验证网页的相对路径是 %s（%s 内有效），请自行拼上本服务的公网地址后在手机浏览器打开。",
			publicBaseURLEnv, r.VerifyPath, verifyTokenTTLText)
	}

	return fmt.Sprintf("也可以在手机浏览器里打开这条一次性链接（%s 内有效）：\n%s\n"+
		"页面上是一张大二维码，过期会自动换新，验证一通过就直接显示结果——人不在电脑前也能解封。",
		verifyTokenTTLText, r.VerifyURL)
}

// splitDataURI 把 data:image/png;base64,XXXX 拆成 mime 和 base64 载荷。
//
// 不照登录那样写死 image/png：验证码的 data-URI 由站点生成，哪天换成 jpeg/webp，
// 硬贴 png 会让客户端渲染出碎图，而这张图是本工具唯一的交付物。
func splitDataURI(src string) (mimeType, data string, ok bool) {
	meta, payload, found := strings.Cut(src, ",")
	if !found || !strings.HasPrefix(meta, "data:") || !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}

	mimeType = strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64")
	if mimeType == "" || payload == "" {
		return "", "", false
	}

	return mimeType, payload, true
}

// handleDeleteCookies 处理删除 cookies 请求，用于登录重置
func (s *AppServer) handleDeleteCookies(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 删除 cookies，重置登录状态")

	err := s.xiaohongshuService.DeleteCookies(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "删除 cookies 失败: " + err.Error()}},
			IsError: true,
		}
	}

	cookiePath := cookies.GetCookiesFilePath()
	resultText := fmt.Sprintf("Cookies 已成功删除，登录状态已重置。\n\n删除的文件路径: %s\n\n下次操作时，需要重新登录。", cookiePath)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handlePublishContent 处理发布内容
func (s *AppServer) handlePublishContent(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 发布内容")

	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	imagePathsInterface, _ := args["images"].([]interface{})
	tagsInterface, _ := args["tags"].([]interface{})
	productsInterface, _ := args["products"].([]interface{})

	var imagePaths []string
	for _, path := range imagePathsInterface {
		if pathStr, ok := path.(string); ok {
			imagePaths = append(imagePaths, pathStr)
		}
	}

	var tags []string
	for _, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags = append(tags, tagStr)
		}
	}

	var products []string
	for _, p := range productsInterface {
		if pStr, ok := p.(string); ok {
			products = append(products, pStr)
		}
	}

	scheduleAt, _ := args["schedule_at"].(string)
	visibility := parseVisibility(args)

	isOriginal, _ := args["is_original"].(bool)

	logrus.Infof("MCP: 发布内容 - 标题: %s, 图片数量: %d, 标签数量: %d, 定时: %s, 原创: %v, visibility: %s, 商品: %v", title, len(imagePaths), len(tags), scheduleAt, isOriginal, visibility, products)

	req := &PublishRequest{
		Title:      title,
		Content:    content,
		Images:     imagePaths,
		Tags:       tags,
		ScheduleAt: scheduleAt,
		IsOriginal: isOriginal,
		Visibility: visibility,
		Products:   products,
	}

	result, err := s.xiaohongshuService.PublishContent(ctx, req)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	resultText := fmt.Sprintf("内容发布成功: %+v", result)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handlePublishVideo 处理发布视频内容（仅本地单个视频文件）
func (s *AppServer) handlePublishVideo(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 发布视频内容（本地）")

	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	videoPath, _ := args["video"].(string)
	tagsInterface, _ := args["tags"].([]interface{})
	productsInterface, _ := args["products"].([]interface{})

	var tags []string
	for _, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags = append(tags, tagStr)
		}
	}

	var products []string
	for _, p := range productsInterface {
		if pStr, ok := p.(string); ok {
			products = append(products, pStr)
		}
	}

	if videoPath == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布失败: 缺少本地视频文件路径",
			}},
			IsError: true,
		}
	}

	scheduleAt, _ := args["schedule_at"].(string)
	visibility := parseVisibility(args)

	logrus.Infof("MCP: 发布视频 - 标题: %s, 标签数量: %d, 定时: %s, visibility: %s, 商品: %v", title, len(tags), scheduleAt, visibility, products)

	req := &PublishVideoRequest{
		Title:      title,
		Content:    content,
		Video:      videoPath,
		Tags:       tags,
		ScheduleAt: scheduleAt,
		Visibility: visibility,
		Products:   products,
	}

	result, err := s.xiaohongshuService.PublishVideo(ctx, req)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	resultText := fmt.Sprintf("视频发布成功: %+v", result)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handleListFeeds 处理获取Feeds列表
func (s *AppServer) handleListFeeds(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取Feeds列表")

	result, err := s.xiaohongshuService.ListFeeds(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feeds列表失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取Feeds列表成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleSearchFeeds 处理搜索Feeds
func (s *AppServer) handleSearchFeeds(ctx context.Context, args SearchFeedsArgs) *MCPToolResult {
	logrus.Info("MCP: 搜索Feeds")

	if args.Keyword == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "搜索Feeds失败: 缺少关键词参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 搜索Feeds - 关键词: %s", args.Keyword)

	filter := xiaohongshu.FilterOption{
		SortBy:      args.Filters.SortBy,
		NoteType:    args.Filters.NoteType,
		PublishTime: args.Filters.PublishTime,
		SearchScope: args.Filters.SearchScope,
		Location:    args.Filters.Location,
	}

	result, err := s.xiaohongshuService.SearchFeeds(ctx, args.Keyword, filter)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "搜索Feeds失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("搜索Feeds成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleGetFeedDetail 处理获取Feed详情
func (s *AppServer) handleGetFeedDetail(ctx context.Context, args map[string]any) *MCPToolResult {
	logrus.Info("MCP: 获取Feed详情")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	loadAll := false
	if raw, ok := args["load_all_comments"]; ok {
		switch v := raw.(type) {
		case bool:
			loadAll = v
		case string:
			if parsed, err := strconv.ParseBool(v); err == nil {
				loadAll = parsed
			}
		case float64:
			loadAll = v != 0
		}
	}

	// 解析评论配置参数，如果未提供则使用默认值
	config := xiaohongshu.DefaultCommentLoadConfig()

	if raw, ok := args["click_more_replies"]; ok {
		switch v := raw.(type) {
		case bool:
			config.ClickMoreReplies = v
		case string:
			if parsed, err := strconv.ParseBool(v); err == nil {
				config.ClickMoreReplies = parsed
			}
		}
	}

	if raw, ok := args["max_replies_threshold"]; ok {
		switch v := raw.(type) {
		case float64:
			config.MaxRepliesThreshold = int(v)
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				config.MaxRepliesThreshold = parsed
			}
		case int:
			config.MaxRepliesThreshold = v
		}
	}

	if raw, ok := args["max_comment_items"]; ok {
		switch v := raw.(type) {
		case float64:
			config.MaxCommentItems = int(v)
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				config.MaxCommentItems = parsed
			}
		case int:
			config.MaxCommentItems = v
		}
	}

	if raw, ok := args["scroll_speed"].(string); ok && raw != "" {
		config.ScrollSpeed = raw
	}

	logrus.Infof("MCP: 获取Feed详情 - Feed ID: %s, loadAllComments=%v, config=%+v", feedID, loadAll, config)

	result, err := s.xiaohongshuService.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAll, config)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取Feed详情成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleUserProfile 获取用户主页
func (s *AppServer) handleUserProfile(ctx context.Context, args map[string]any) *MCPToolResult {
	logrus.Info("MCP: 获取用户主页")

	userID, ok := args["user_id"].(string)
	if !ok || userID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: 缺少user_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 获取用户主页 - User ID: %s", userID)

	tab, _ := args["tab"].(string)

	result, err := s.xiaohongshuService.UserProfile(ctx, userID, xsecToken, tab)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取用户主页，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleLikeFeed 处理点赞/取消点赞
func (s *AppServer) handleLikeFeed(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少feed_id参数"}}, IsError: true}
	}
	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少xsec_token参数"}}, IsError: true}
	}
	unlike, _ := args["unlike"].(bool)

	var res *ActionResult
	var err error

	if unlike {
		res, err = s.xiaohongshuService.UnlikeFeed(ctx, feedID, xsecToken)
	} else {
		res, err = s.xiaohongshuService.LikeFeed(ctx, feedID, xsecToken)
	}

	if err != nil {
		action := "点赞"
		if unlike {
			action = "取消点赞"
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: action + "失败: " + err.Error()}}, IsError: true}
	}

	action := "点赞"
	if unlike {
		action = "取消点赞"
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("%s成功 - Feed ID: %s", action, res.FeedID)}}}
}

// handleFavoriteFeed 处理收藏/取消收藏
func (s *AppServer) handleFavoriteFeed(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少feed_id参数"}}, IsError: true}
	}
	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少xsec_token参数"}}, IsError: true}
	}
	unfavorite, _ := args["unfavorite"].(bool)

	var res *ActionResult
	var err error

	if unfavorite {
		res, err = s.xiaohongshuService.UnfavoriteFeed(ctx, feedID, xsecToken)
	} else {
		res, err = s.xiaohongshuService.FavoriteFeed(ctx, feedID, xsecToken)
	}

	if err != nil {
		action := "收藏"
		if unfavorite {
			action = "取消收藏"
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: action + "失败: " + err.Error()}}, IsError: true}
	}

	action := "收藏"
	if unfavorite {
		action = "取消收藏"
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("%s成功 - Feed ID: %s", action, res.FeedID)}}}
}

// handlePostComment 处理发表评论到Feed
func (s *AppServer) handlePostComment(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 发表评论到Feed")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	content, ok := args["content"].(string)
	if !ok || content == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少content参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 发表评论 - Feed ID: %s, 内容长度: %d", feedID, len(content))

	result, err := s.xiaohongshuService.PostCommentToFeed(ctx, feedID, xsecToken, content)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	resultText := fmt.Sprintf("评论发表成功 - Feed ID: %s", result.FeedID)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handleReplyComment 处理回复评论
func (s *AppServer) handleReplyComment(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 回复评论")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	commentID, _ := args["comment_id"].(string)
	userID, _ := args["user_id"].(string)
	if commentID == "" && userID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少comment_id或user_id参数",
			}},
			IsError: true,
		}
	}

	content, ok := args["content"].(string)
	if !ok || content == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少content参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 回复评论 - Feed ID: %s, Comment ID: %s, User ID: %s, 内容长度: %d", feedID, commentID, userID, len(content))

	result, err := s.xiaohongshuService.ReplyCommentToFeed(ctx, feedID, xsecToken, commentID, userID, content)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	responseText := fmt.Sprintf("评论回复成功 - Feed ID: %s, Comment ID: %s, User ID: %s", result.FeedID, result.TargetCommentID, result.TargetUserID)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: responseText,
		}},
	}
}

// handleGetMyProfile 获取当前登录用户主页
func (s *AppServer) handleGetMyProfile(ctx context.Context, tab string) *MCPToolResult {
	logrus.Infof("MCP: 获取我的主页 tab=%s", tab)

	result, err := s.xiaohongshuService.GetMyProfile(ctx, tab)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取我的主页失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取我的主页成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleGetUnreadCount 获取通知未读数
func (s *AppServer) handleGetUnreadCount(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取通知未读数")

	result, err := s.xiaohongshuService.GetUnreadCount(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取未读数失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "获取未读数")
}

// handleListNotifications 获取通知列表
func (s *AppServer) handleListNotifications(ctx context.Context, tab string, limit int) *MCPToolResult {
	logrus.Infof("MCP: 获取通知列表 tab=%s limit=%d", tab, limit)

	result, err := s.xiaohongshuService.ListNotifications(ctx, tab, limit)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取通知列表失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "获取通知列表")
}

// handleReplyNotification 回复通知里的评论
func (s *AppServer) handleReplyNotification(ctx context.Context, commentID, content string) *MCPToolResult {
	logrus.Infof("MCP: 回复通知评论 comment=%s", commentID)

	result, err := s.xiaohongshuService.ReplyNotification(ctx, commentID, content)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "回复失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "回复")
}

// marshalMCPResult 把结果序列化成 MCP 文本内容。
func marshalMCPResult(result any, action string) *MCPToolResult {
	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("%s成功，但序列化失败: %v", action, err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{Type: "text", Text: string(jsonData)}},
	}
}

// handleLikeNotification 给通知里的评论点赞/取消点赞
func (s *AppServer) handleLikeNotification(ctx context.Context, commentID string, unlike bool) *MCPToolResult {
	logrus.Infof("MCP: 通知点赞 comment=%s unlike=%v", commentID, unlike)

	result, err := s.xiaohongshuService.LikeNotification(ctx, commentID, unlike)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "点赞失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "点赞")
}
