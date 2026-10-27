package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// MediaOutputInline 表示正文以 data URI（Gemini 原生接口为 inlineData）内联生成的媒体，与原先的行为相同
	MediaOutputInline = "inline"
	// MediaOutputURL 表示生成的媒体按内容保存到媒体目录，正文改为引用本服务 /media/<文件名> 的链接
	MediaOutputURL = "url"

	// DefaultMediaRetention 为媒体文件默认保存期限
	DefaultMediaRetention = 7 * 24 * time.Hour
	// DefaultMediaMaxBytes 为媒体目录默认总量上限（20GB）
	DefaultMediaMaxBytes int64 = 20 << 30

	// minMediaRetention 为不为 0 时保存期限的下限：链接发出后客户端可能过一阵才打开，太短的期限会让刚生成的链接失效
	minMediaRetention = time.Hour
	// minMediaMaxBytes 为不为 0 时总量上限的下限：防止把 20 写成 20 字节，一个视频就有几 MB
	minMediaMaxBytes int64 = 100 << 20
)

// 媒体设置的错误文案
var (
	errMediaOutput    = errors.New("MEDIA_OUTPUT 必须是 inline 或 url")
	errMediaBaseURL   = errors.New("MEDIA_BASE_URL 必须是 http 或 https 地址，例如 https://ai.example.com，可带路径前缀，不能带账号密码、查询参数、片段、空白或括号")
	errMediaRetention = errors.New("MEDIA_RETENTION 必须是 0（不清理）或不少于 1 小时的时长，例如 7d、72h")
	errMediaMaxSize   = errors.New("MEDIA_MAX_SIZE 必须是 0（不限）或不小于 100MB 的大小，例如 20GB、500MB")
)

// MediaSettings 为生成媒体的输出与保存设置（MEDIA_*），保存后立即生效。
// 零值等同 inline 输出、不清理、不限总量；Default 使用 DefaultMediaSettings
type MediaSettings struct {
	// Output 为 inline 或 url（MEDIA_OUTPUT，不区分大小写，空值按 inline）
	Output string `json:"output"`
	// BaseURL 为媒体链接使用的服务地址（MEDIA_BASE_URL），保存时去掉末尾的 /；为空时使用每个请求访问的协议与主机。
	// 经 new-api 等中转或反向代理访问时，请求里的主机多半是内网地址，应填用户能访问到的公网地址
	BaseURL string `json:"base_url"`
	// Retention 为媒体文件的保存期限（MEDIA_RETENTION，默认 7 天），按文件修改时间计算，0 表示不按时间清理
	Retention time.Duration `json:"-"`
	// MaxBytes 为媒体目录的总量上限（MEDIA_MAX_SIZE，默认 20GB），超过时从最旧的文件开始删除，0 表示不限
	MaxBytes int64 `json:"-"`
}

// DefaultMediaSettings 返回默认设置：inline 输出，保存 7 天，总量 20GB
func DefaultMediaSettings() MediaSettings {
	return MediaSettings{Output: MediaOutputInline, Retention: DefaultMediaRetention, MaxBytes: DefaultMediaMaxBytes}
}

// withDefaults 补上零值语义：输出方式去空白转小写、空值按 inline，服务地址去空白与末尾的 /
func (m MediaSettings) withDefaults() MediaSettings {
	m.Output = strings.ToLower(strings.TrimSpace(m.Output))
	if m.Output == "" {
		m.Output = MediaOutputInline
	}
	m.BaseURL = strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	return m
}

// Normalized 返回补齐零值语义后的设置
func (m MediaSettings) Normalized() MediaSettings {
	return m.withDefaults()
}

// LinkMode 判断是否以本服务链接输出生成的媒体
func (m MediaSettings) LinkMode() bool {
	return m.withDefaults().Output == MediaOutputURL
}

// Equal 判断两份设置是否一致（按零值语义补齐后比较）
func (m MediaSettings) Equal(other MediaSettings) bool {
	return m.withDefaults() == other.withDefaults()
}

// Validate 校验设置；错误文案为固定文本
func (m MediaSettings) Validate() error {
	m = m.withDefaults()
	if m.Output != MediaOutputInline && m.Output != MediaOutputURL {
		return errMediaOutput
	}
	if err := validateMediaBaseURL(m.BaseURL); err != nil {
		return err
	}
	if m.Retention < 0 || m.Retention > 0 && m.Retention < minMediaRetention {
		return errMediaRetention
	}
	if m.MaxBytes < 0 || m.MaxBytes > 0 && m.MaxBytes < minMediaMaxBytes {
		return errMediaMaxSize
	}
	return nil
}

// validateMediaBaseURL 要求空值或 http/https 的绝对地址：链接写在 Markdown 的括号里，空白、括号、引号与尖括号会截断链接；
// 账号密码会随回复发给每个用户，查询参数与片段之后再拼 /media/ 得不到正确地址
func validateMediaBaseURL(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, " \t\r\n()<>\"'`\\") {
		return errMediaBaseURL
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(value, "?") || strings.Contains(value, "#") {
		return errMediaBaseURL
	}
	return nil
}

// Summary 返回热更新日志里的一段：输出=url | 链接地址=https://ai.example.com | 保存期限=7d | 总量上限=20GB
func (m MediaSettings) Summary() string {
	m = m.withDefaults()
	base := m.BaseURL
	if base == "" {
		base = "按请求地址"
	}
	retention := FormatMediaRetention(m.Retention)
	if m.Retention == 0 {
		retention = "不清理"
	}
	size := FormatMediaSize(m.MaxBytes)
	if m.MaxBytes == 0 {
		size = "不限"
	}
	return fmt.Sprintf("输出=%s | 链接地址=%s | 保存期限=%s | 总量上限=%s", m.Output, base, retention, size)
}

func (m MediaSettings) envValues() map[string]string {
	m = m.withDefaults()
	return map[string]string{
		"MEDIA_OUTPUT":    m.Output,
		"MEDIA_BASE_URL":  m.BaseURL,
		"MEDIA_RETENTION": FormatMediaRetention(m.Retention),
		"MEDIA_MAX_SIZE":  FormatMediaSize(m.MaxBytes),
	}
}

// loadMediaSettings 从 env 读取媒体设置：键存在且非空时解析，没有写的项沿用默认值；关闭清理必须写 0
func loadMediaSettings(values map[string]string, media *MediaSettings) error {
	if value, ok := values["MEDIA_OUTPUT"]; ok && strings.TrimSpace(value) != "" {
		media.Output = strings.ToLower(strings.TrimSpace(value))
	}
	if value, ok := values["MEDIA_BASE_URL"]; ok {
		media.BaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
	}
	if value, ok := values["MEDIA_RETENTION"]; ok && strings.TrimSpace(value) != "" {
		retention, err := ParseMediaRetention(value)
		if err != nil {
			return err
		}
		media.Retention = retention
	}
	if value, ok := values["MEDIA_MAX_SIZE"]; ok && strings.TrimSpace(value) != "" {
		size, err := ParseMediaSize(value)
		if err != nil {
			return err
		}
		media.MaxBytes = size
	}
	return nil
}

// ParseMediaRetention 解析保存期限：Go 时长写法（72h、90m）或整数天数加 d（7d），单独的 0 表示不清理。
// 只解析写法，范围由 Validate 校验
func ParseMediaRetention(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.ParseInt(days, 10, 64)
		if err != nil || count < 0 || count > 36500 {
			return 0, errMediaRetention
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, errMediaRetention
	}
	return duration, nil
}

// FormatMediaRetention 把保存期限写成 ParseMediaRetention 能读回的文本：整天写成 7d，整小时写成 36h，整分钟写成 90m，
// 其余按 Go 时长写法，0 写成 0
func FormatMediaRetention(duration time.Duration) string {
	if duration == 0 {
		return "0"
	}
	for _, unit := range []struct {
		size   time.Duration
		suffix string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if duration > 0 && duration%unit.size == 0 {
			return strconv.FormatInt(int64(duration/unit.size), 10) + unit.suffix
		}
	}
	return duration.String()
}

// mediaSizeUnits 为大小单位（按 1024 进位，GB 与 GiB 相同），从大到小排列，FormatMediaSize 按这个顺序取能整除的最大单位
var mediaSizeUnits = []struct {
	names []string
	bytes int64
}{
	{names: []string{"tb", "tib", "t"}, bytes: 1 << 40},
	{names: []string{"gb", "gib", "g"}, bytes: 1 << 30},
	{names: []string{"mb", "mib", "m"}, bytes: 1 << 20},
	{names: []string{"kb", "kib", "k"}, bytes: 1 << 10},
	{names: []string{"b"}, bytes: 1},
}

// ParseMediaSize 解析总量上限：整数加单位（B、KB、MB、GB、TB，不区分大小写，按 1024 进位），不写单位按字节，0 表示不限。
// 只解析写法，范围由 Validate 校验
func ParseMediaSize(value string) (int64, error) {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	digits := strings.TrimRightFunc(value, func(character rune) bool { return character < '0' || character > '9' })
	unit := value[len(digits):]
	count, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || count < 0 {
		return 0, errMediaMaxSize
	}
	multiplier := int64(1)
	if unit != "" {
		multiplier = 0
		for _, candidate := range mediaSizeUnits {
			for _, name := range candidate.names {
				if unit == name {
					multiplier = candidate.bytes
				}
			}
		}
		if multiplier == 0 {
			return 0, errMediaMaxSize
		}
	}
	if count > 0 && count > (1<<62)/multiplier {
		return 0, errMediaMaxSize
	}
	return count * multiplier, nil
}

// FormatMediaSize 把总量上限写成 ParseMediaSize 能读回的文本：取能整除的最大单位（20GB、500MB），0 写成 0
func FormatMediaSize(size int64) string {
	if size == 0 {
		return "0"
	}
	for _, unit := range mediaSizeUnits {
		if size%unit.bytes == 0 {
			return strconv.FormatInt(size/unit.bytes, 10) + strings.ToUpper(unit.names[0])
		}
	}
	return strconv.FormatInt(size, 10) + "B"
}

// mediaSettingsJSON 为管理接口与 JSON 配置里媒体设置的写法：期限与大小用文本
type mediaSettingsJSON struct {
	Output    string `json:"output"`
	BaseURL   string `json:"base_url"`
	Retention string `json:"retention"`
	MaxSize   string `json:"max_size"`
}

func (m MediaSettings) toJSON() mediaSettingsJSON {
	m = m.withDefaults()
	return mediaSettingsJSON{
		Output: m.Output, BaseURL: m.BaseURL,
		Retention: FormatMediaRetention(m.Retention), MaxSize: FormatMediaSize(m.MaxBytes),
	}
}

// mediaSettingsOrDefault 用于 JSON 解析：旧数据没有该字段（或为 null）时取默认设置；期限或大小为空时取默认值
func mediaSettingsOrDefault(value *mediaSettingsJSON) (MediaSettings, error) {
	settings := DefaultMediaSettings()
	if value == nil {
		return settings, nil
	}
	settings.Output = value.Output
	settings.BaseURL = value.BaseURL
	if strings.TrimSpace(value.Retention) != "" {
		retention, err := ParseMediaRetention(value.Retention)
		if err != nil {
			return MediaSettings{}, err
		}
		settings.Retention = retention
	}
	if strings.TrimSpace(value.MaxSize) != "" {
		size, err := ParseMediaSize(value.MaxSize)
		if err != nil {
			return MediaSettings{}, err
		}
		settings.MaxBytes = size
	}
	return settings.withDefaults(), nil
}
