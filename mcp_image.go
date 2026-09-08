package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// FeedImageArgs 显式视觉通道，每次只取一张，避免整篇笔记灌入视觉上下文。
type FeedImageArgs struct {
	Ref        string `json:"ref,omitempty" jsonschema:"笔记 ref，来自搜索、列表或详情；优先使用"`
	FeedID     string `json:"feed_id,omitempty" jsonschema:"笔记 ID，不传 ref 时必填"`
	XsecToken  string `json:"xsec_token,omitempty" jsonschema:"不传 ref 时必填"`
	ImageIndex int    `json:"image_index,omitempty" jsonschema:"图片序号，从1开始，默认1；每次只返回一张，按需取图"`
}

const feedImageMaxBytes = 10 << 20

func imageToolError(err error) *MCPToolResult {
	return &MCPToolResult{IsError: true, Content: []MCPContent{{Type: "text", Text: "获取笔记图片失败: " + err.Error()}}}
}

func (s *AppServer) handleGetFeedImage(ctx context.Context, args FeedImageArgs) *MCPToolResult {
	target, ok := s.resolveFeed(args.Ref, args.FeedID, args.XsecToken)
	if !ok {
		return refError()
	}
	index := args.ImageIndex
	if index == 0 {
		index = 1
	}
	if index < 1 {
		return imageToolError(fmt.Errorf("image_index 必须大于等于 1"))
	}
	config := xiaohongshu.DefaultFeedDetailConfig()
	config.IncludeImages = true
	result, err := s.xiaohongshuService.GetFeedDetailWithConfig(ctx, target.FeedID, target.XsecToken, false, config)
	if err != nil {
		return imageToolError(err)
	}
	detail, ok := result.Data.(*xiaohongshu.FeedDetailResponse)
	if !ok || detail == nil {
		return imageToolError(fmt.Errorf("详情返回体类型异常"))
	}
	return feedImageResult(ctx, feedImageClient(), detail.Note.ImageList, index)
}

// 只访问笔记图片 CDN；重定向也必须满足相同约束。
func validateFeedImageURL(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	if (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" ||
		!(host == "xhscdn.com" || strings.HasSuffix(host, ".xhscdn.com")) {
		return fmt.Errorf("图片地址不是受支持的小红书 CDN")
	}
	return nil
}

func feedImageClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("图片重定向次数过多")
		}
		return validateFeedImageURL(req.URL)
	}}
}

func feedImageResult(ctx context.Context, client *http.Client, images []xiaohongshu.DetailImageInfo, index int) *MCPToolResult {
	if index < 1 || index > len(images) {
		return imageToolError(fmt.Errorf("图片序号 %d 超出范围，共 %d 张图片", index, len(images)))
	}
	src := images[index-1].URLDefault
	if src == "" {
		src = images[index-1].URLPre
	}
	u, err := url.Parse(src)
	if err != nil {
		return imageToolError(fmt.Errorf("图片地址无效"))
	}
	if err = validateFeedImageURL(u); err != nil {
		return imageToolError(err)
	}
	u.Scheme = "https"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return imageToolError(err)
	}
	req.Header.Set("Referer", "https://www.xiaohongshu.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return imageToolError(fmt.Errorf("图片下载失败，请稍后重试"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return imageToolError(fmt.Errorf("图片下载 HTTP %d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, feedImageMaxBytes+1))
	if err != nil {
		return imageToolError(fmt.Errorf("读取图片失败"))
	}
	if len(data) > feedImageMaxBytes {
		return imageToolError(fmt.Errorf("图片超过 10 MiB 上限"))
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return imageToolError(fmt.Errorf("响应不是受支持的图片格式"))
	}
	return &MCPToolResult{Content: []MCPContent{
		{Type: "text", Text: fmt.Sprintf("第 %d/%d 张图片", index, len(images))},
		{Type: "image", MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)},
	}}
}
