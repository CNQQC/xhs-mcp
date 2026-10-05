package xiaohongshu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type subtitleRoundTripper func(*http.Request) (*http.Response, error)

func (f subtitleRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPickSubtitleDeterministic(t *testing.T) {
	lang, url := pickSubtitle(map[string][]VideoSubtitle{
		"source": {{}, {URL: "https://example.test/source.srt"}},
		"zh-CN":  {{URL: "https://example.test/zh.srt"}},
	})
	assert.Equal(t, "source", lang)
	assert.Equal(t, "https://example.test/source.srt", url)
	for range 10 {
		lang, _ = pickSubtitle(map[string][]VideoSubtitle{"fr": {{URL: "fr"}}, "en": {{URL: "en"}}})
		assert.Equal(t, "en", lang)
	}
}

func TestParseSRTKeepsNumericDialogueAndBOM(t *testing.T) {
	text, err := parseSRTChecked(strings.NewReader("\ufeff1\n00:00:01,000 --> 00:00:02,000\n2026\n第二行\n\n2\n00:00:03,000 --> 00:00:04,000\n最后一句\n"))
	require.NoError(t, err)
	assert.Equal(t, "2026\n第二行\n最后一句", text)
}

func TestFillSubtitleCoverage(t *testing.T) {
	valid := "1\n00:00:01,000 --> 00:00:02,000\n第一句\n\n"
	for _, tc := range []struct {
		name, body, status string
		code               int
		transportErr       error
		wantText           bool
	}{
		{"success", valid, "available", 200, nil, true},
		{"HTTP failure", "", "failed", 403, nil, false},
		{"transport failure", "", "failed", 0, errors.New("https://example.test/private?sign=secret"), false},
		{"empty", "", "failed", 200, nil, false},
		{"HTML response", "<html>login needed</html>", "failed", 200, nil, false},
		{"HTML comment is not a cue", "<!-- upstream error -->\n<html>login required</html>\n", "failed", 200, nil, false},
		{"malformed second cue", valid + "2\nnot a timestamp\nskipped dialogue\n", "partial", 200, nil, true},
		{"invalid timestamp range", "1\n00:00:61,000 --> 00:00:62,000\ntext\n", "failed", 200, nil, false},
		{"reversed timestamps", "1\n00:00:02,000 --> 00:00:01,000\ntext\n", "failed", 200, nil, false},
		{"scanner interrupted", valid + "2\n00:00:03,000 --> 00:00:04,000\n" + strings.Repeat("x", 1024*1024+1), "partial", 200, nil, true},
		{"byte limit", valid + "2\n00:00:03,000 --> 00:00:04,000\n" + strings.Repeat("text\n", subtitleMaxBytes/5+1), "partial", 200, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: subtitleRoundTripper(func(r *http.Request) (*http.Response, error) {
				assert.Equal(t, http.MethodGet, r.Method)
				_, deadlineSet := r.Context().Deadline()
				assert.True(t, deadlineSet)
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			v := &VideoDetail{Subtitles: map[string][]VideoSubtitle{"source": {{URL: "https://example.test/subtitle.srt?sign=secret", Language: "zh-CN"}}}}
			fillSubtitleTextWithClient(context.Background(), v, client)
			assert.Equal(t, tc.status, v.SubtitleStatus)
			assert.Equal(t, tc.wantText, v.SubtitleText != "")
			assert.Equal(t, "zh-CN", v.SubtitleLang)
			assert.Nil(t, v.Subtitles)
			assert.NotContains(t, v.SubtitleWarning, "secret")
			assert.NotContains(t, v.SubtitleWarning, "example.test")
			if tc.status == "available" {
				assert.Empty(t, v.SubtitleWarning)
			} else {
				assert.NotEmpty(t, v.SubtitleWarning)
			}
		})
	}
}

func TestFillSubtitleUnknownAndUnavailable(t *testing.T) {
	client := &http.Client{Transport: subtitleRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("no index means no HTTP request")
		return nil, nil
	})}
	for _, tc := range []struct {
		name   string
		video  VideoDetail
		status string
	}{
		{"unknown", VideoDetail{}, "unknown"},
		{"known absent", VideoDetail{Subtitles: map[string][]VideoSubtitle{}}, "unavailable"},
		{"invalid index", VideoDetail{Subtitles: map[string][]VideoSubtitle{"source": {{}}}}, "failed"},
		{"metadata error preserved", VideoDetail{SubtitleStatus: "failed", SubtitleWarning: "metadata failed"}, "failed"},
		{"partial stays partial", VideoDetail{SubtitleStatus: "partial", SubtitleText: "partial text", SubtitleWarning: "truncated"}, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fillSubtitleTextWithClient(context.Background(), &tc.video, client)
			assert.Equal(t, tc.status, tc.video.SubtitleStatus)
			assert.Nil(t, tc.video.Subtitles)
		})
	}
	fillSubtitleTextWithClient(context.Background(), nil, client)
}
