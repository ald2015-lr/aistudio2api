package aistudio

import (
	"net/url"
	"regexp"
	"strings"
)

// youtubeURLPattern 匹配文本中由 ASCII URL 字符组成的链接，遇到空白、引号、尖括号、反引号与非 ASCII 字符（汉字、全角标点）结束。
// 原先按“非空白、非引号尖括号”匹配，“看下https://youtu.be/xxx讲了什么”会把链接后面的汉字、全角括号、代码里的反引号一起吞进链接：
// 视频 ID 无效，这段汉字又随链接一起从正文里删掉
var youtubeURLPattern = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#\[\]@!$&()*+,;=%]+`)

// youtubeVideoIDPattern 匹配 11 位的 YouTube 视频 ID：只有这样的链接才转为视频，示例里的 VIDEO_ID、${videoId} 这类占位写法原样保留为文本
var youtubeVideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youtubeWatchURL 是发往 AI Studio 的 YouTube 视频地址前缀
const youtubeWatchURL = "https://www.youtube.com/watch?v="

// ExternalMediaForURL 返回 AI Studio 可直接读取的外部媒体
func ExternalMediaForURL(raw string) (*ExternalMedia, bool) {
	raw = strings.TrimRight(strings.TrimSpace(raw), ".,;:!?)]}")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, false
	}
	host := strings.ToLower(parsed.Hostname())
	videoID := ""
	switch host {
	case "youtu.be":
		path := strings.Trim(parsed.Path, "/")
		if path != "" {
			videoID = strings.Split(path, "/")[0]
		}
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		path := strings.Trim(parsed.Path, "/")
		if path == "watch" {
			videoID = parsed.Query().Get("v")
		} else {
			segments := strings.Split(path, "/")
			if len(segments) >= 2 && (segments[0] == "shorts" || segments[0] == "live" || segments[0] == "embed") {
				videoID = segments[1]
			}
		}
	default:
		return nil, false
	}
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, false
	}
	return &ExternalMedia{MIME: "video/*", URL: youtubeWatchURL + url.QueryEscape(videoID)}, true
}

// attachYouTubeMedia 把用户消息文本里的 YouTube 链接（视频 ID 恰好 11 位）转为视频外部媒体并从正文删去链接文本。
// 请求编码（encodeContent）与 WAA binding（bindingPrompt）调用的是同一个函数，两边按同一份结果整体变化，binding 的组成规则不变
func attachYouTubeMedia(content Content) Content {
	if content.Role != RoleUser {
		return content
	}
	existing := make(map[string]struct{})
	for _, part := range content.Parts {
		if part.ExternalMedia != nil {
			existing[part.ExternalMedia.URL] = struct{}{}
		}
	}
	parts := make([]Part, 0, len(content.Parts)+1)
	for _, part := range content.Parts {
		if part.Text != "" {
			matches := youtubeURLPattern.FindAllString(part.Text, -1)
			original := part.Text
			if len(matches) > 0 {
				for _, raw := range matches {
					media, ok := ExternalMediaForURL(raw)
					if !ok || !youtubeVideoIDPattern.MatchString(strings.TrimPrefix(media.URL, youtubeWatchURL)) {
						continue
					}
					if _, duplicate := existing[media.URL]; !duplicate {
						existing[media.URL] = struct{}{}
						parts = append(parts, Part{ExternalMedia: media})
					}
					part.Text = strings.ReplaceAll(part.Text, raw, "")
				}
			}
			if part.Text != original {
				part.Text = strings.TrimSpace(part.Text)
				if part.Text == "" {
					continue
				}
			}
		}
		parts = append(parts, part)
	}
	content.Parts = parts
	return content
}
