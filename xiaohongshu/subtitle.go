package xiaohongshu

// 视频字幕的下载与解析。
//
// 视频画面本身对调用方（大模型）没有价值，真正可读的是字幕——它等于视频内容的
// 文字转录。字幕的 URL 躺在 __INITIAL_STATE__ 里，但正文要另外下载 .srt，
// 且链接带签名有时效（url 里的 sign/t 参数），客户端拿到 URL 也取不动，
// 所以在服务端取好、去掉时间轴，直接把台词交出去。

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sirupsen/logrus"
)

const (
	// subtitleFetchTimeout 单个字幕文件的下载超时。
	// 实测一条 166 秒视频的 srt 约 4.7KB / 0.2s，给足余量又不拖累详情接口。
	subtitleFetchTimeout = 8 * time.Second

	// subtitleMaxBytes 字幕正文读取上限，防异常大文件撑爆内存（服务器只有 1.6G）。
	subtitleMaxBytes = 2 << 20 // 2MB
)

// subtitleLangPriority 挑字幕的优先级。
// source 是视频的原始语种，最贴近里面实际说的话；其次简中。
var subtitleLangPriority = []string{"source", "zh-CN", "zh"}

// pickSubtitle 从多语言字幕里挑一档，返回语言标识与下载地址。
// 都取不到时返回空串，调用方据此跳过。
func pickSubtitle(subs map[string][]VideoSubtitle) (lang, url string) {
	pick := func(key string) (string, string, bool) {
		for _, item := range subs[key] {
			if item.URL == "" {
				continue
			}
			l := item.Language
			if l == "" {
				l = key
			}
			return l, item.URL, true
		}
		return "", "", false
	}

	for _, key := range subtitleLangPriority {
		if l, u, ok := pick(key); ok {
			return l, u
		}
	}
	// 优先级都没命中时稳定地挑一档，避免同样输入因 map 顺序返回不同语言。
	keys := make([]string, 0, len(subs))
	for key := range subs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if l, u, ok := pick(key); ok {
			return l, u
		}
	}
	return "", ""
}

// parseSRT 去掉 srt 的序号与时间轴，只留台词。
// 每条字幕单独成行——原文本就是按语句切的，保留换行比硬拼成一段更好读。
func parseSRT(r io.Reader) string {
	text, err := parseSRTChecked(r)
	if err != nil {
		logrus.Warnf("解析字幕中断: %v", err)
	}
	return text
}

var subtitleTimingLine = regexp.MustCompile(`^([0-9]{2,6}):([0-9]{2}):([0-9]{2})[,.]([0-9]{3})[ \t]*-->[ \t]*([0-9]{2,6}):([0-9]{2}):([0-9]{2})[,.]([0-9]{3})(?:[ \t]+.*)?$`)

func validSubtitleTimingLine(line string) bool {
	parts := subtitleTimingLine.FindStringSubmatch(line)
	if parts == nil {
		return false
	}
	stamp := func(offset int) (int64, bool) {
		hours, _ := strconv.ParseInt(parts[offset], 10, 64)
		minutes, _ := strconv.ParseInt(parts[offset+1], 10, 64)
		seconds, _ := strconv.ParseInt(parts[offset+2], 10, 64)
		millis, _ := strconv.ParseInt(parts[offset+3], 10, 64)
		return ((hours*60+minutes)*60+seconds)*1000 + millis, minutes < 60 && seconds < 60
	}
	start, startOK := stamp(1)
	end, endOK := stamp(5)
	return startOK && endOK && end >= start
}

// parseSRTChecked 保留可用文本，同时返回扫描或格式错误，避免把部分字幕误报为完整。
func parseSRTChecked(r io.Reader) (string, error) {
	var b strings.Builder
	var block []string
	sawCue, malformed := false, false
	flush := func() {
		lines := block
		block = nil
		if len(lines) == 0 {
			return
		}
		// 字幕序号可省略；时间行后面的纯数字是台词，仍然保留。
		if _, err := strconv.ParseUint(lines[0], 10, 64); err == nil {
			lines = lines[1:]
		}
		if len(lines) == 0 || !validSubtitleTimingLine(lines[0]) {
			malformed = true
			return
		}
		sawCue = true
		for _, line := range lines[1:] {
			if validSubtitleTimingLine(line) {
				// 缺少字幕分隔符时，无法可靠区分序号与数字台词。
				// 保留之前的文本，但不声明完整。
				malformed = true
				break
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			flush()
		} else {
			block = append(block, line)
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return b.String(), err
	}
	if malformed || !sawCue {
		return b.String(), errors.New("invalid or missing subtitle cue")
	}
	return b.String(), nil
}

// fillSubtitleText 下载字幕正文并填进 v。
// 字幕是附加信息，任何一步失败都只告警不报错——不该因为它拖垮整个详情接口。
func fillSubtitleText(ctx context.Context, v *VideoDetail) {
	fillSubtitleTextWithClient(ctx, v, http.DefaultClient)
}

func fillSubtitleTextWithClient(ctx context.Context, v *VideoDetail, client *http.Client) {
	if v == nil {
		return
	}

	// 索引取完就丢，无论下面成不成：里面是带签名有时效的 .srt 直链，客户端取不动，
	// 留在返回体里只是噪音。用 defer 是因为下面每一步失败都直接 return。
	defer func() { v.Subtitles = nil }()
	if len(v.Subtitles) == 0 {
		if v.SubtitleText != "" {
			if v.SubtitleStatus == "" || v.SubtitleStatus == "unknown" {
				v.SubtitleStatus = "available"
			}
		} else if v.Subtitles != nil {
			v.SubtitleStatus = "unavailable"
		} else if v.SubtitleStatus == "" {
			v.SubtitleStatus = "unknown"
		}
		return
	}
	// 研究告警不暴露签名 URL 或原始传输错误。
	fail := func(warning string) {
		v.SubtitleStatus = "failed"
		v.SubtitleWarning = warning
	}

	lang, url := pickSubtitle(v.Subtitles)
	if url == "" {
		fail("字幕索引没有可用地址，未能提取字幕")
		return
	}
	v.SubtitleLang = lang

	ctx, cancel := context.WithTimeout(ctx, subtitleFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logrus.Warnf("构造字幕请求失败(%s): %v", lang, err)
		fail("字幕地址无效，未能提取字幕")
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		logrus.Warnf("下载字幕失败(%s): %v", lang, err)
		fail("字幕下载失败，未能提取字幕")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		logrus.Warnf("下载字幕失败(%s): HTTP %d", lang, resp.StatusCode)
		fail("字幕下载返回异常状态，未能提取字幕")
		return
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, subtitleMaxBytes+1))
	truncated := len(body) > subtitleMaxBytes
	if truncated {
		body = body[:subtitleMaxBytes]
	}
	text, parseErr := parseSRTChecked(strings.NewReader(string(body)))
	if text == "" {
		logrus.Warnf("字幕内容为空(%s)", lang)
		fail("未能从字幕文件提取可用文本")
		return
	}

	v.SubtitleText = text
	v.SubtitleLang = lang
	v.SubtitleStatus = "available"
	v.SubtitleWarning = ""
	if truncated || readErr != nil || parseErr != nil {
		v.SubtitleStatus = "partial"
		v.SubtitleWarning = "字幕读取或解析未完成，返回的字幕文本可能不完整"
	}
	logrus.Infof("字幕已取回: %s，%d 字", lang, utf8.RuneCountInString(text))
}
