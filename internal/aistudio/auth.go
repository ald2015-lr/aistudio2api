package aistudio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

const (
	// LoginMethodIsolatedBrowser 表示独立浏览器登录
	LoginMethodIsolatedBrowser = "isolated_browser"
)

// StateCookie 表示 Playwright storage state 中的 Cookie
type StateCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	HTTPOnly     bool    `json:"httpOnly"`
	Secure       bool    `json:"secure"`
	SameSite     string  `json:"sameSite"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

// StorageItem 表示浏览器本地存储项
type StorageItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StorageOrigin 表示 Playwright storage state 中的站点数据
type StorageOrigin struct {
	Origin       string        `json:"origin"`
	LocalStorage []StorageItem `json:"localStorage"`
}

// StorageState 表示可原样写回的 Playwright storage state
type StorageState struct {
	Cookies []StateCookie   `json:"cookies"`
	Origins []StorageOrigin `json:"origins"`
	extra   map[string]json.RawMessage
}

// AuthSource 记录认证状态的来源
type AuthSource struct {
	Browser string `json:"browser"`
	Profile string `json:"profile,omitempty"`
	Email   string `json:"email,omitempty"`
}

// ChromeOAuthMaterial 保存 Chrome DBSC 续签材料
type ChromeOAuthMaterial struct {
	GaiaID            string `json:"gaia_id"`
	RefreshToken      string `json:"refresh_token"`
	WrappedBindingKey []byte `json:"wrapped_binding_key"`
}

// AuthExtension 保存 aistudio2api 认证扩展
type AuthExtension struct {
	Source AuthSource           `json:"source"`
	OAuth  *ChromeOAuthMaterial `json:"oauth,omitempty"`
}

const authExtensionKey = "aistudio2api"

// SetAuthExtension 写入 aistudio2api 认证扩展
func (s *StorageState) SetAuthExtension(extension AuthExtension) error {
	if s == nil {
		return fmt.Errorf("storage state 为空")
	}
	raw, err := json.Marshal(extension)
	if err != nil {
		return fmt.Errorf("编码认证扩展: %w", err)
	}
	if s.extra == nil {
		s.extra = make(map[string]json.RawMessage)
	}
	s.extra[authExtensionKey] = raw
	return nil
}

// DeviceBoundCookie 为设备绑定会话（DBSC）的 Cookie 名：在开了设备绑定的浏览器里登录的 Google 会话带它，会话要定期用浏览器设备里的
// 密钥证明自己才能续期，复制出来的认证文件约 2 小时后失效（见 app/device_bound.go）
const DeviceBoundCookie = "__Secure-1PSIDRTS"

// DeviceBoundSession 判断认证状态是否带设备绑定会话的 Cookie（只看名字，不读值）
func (s StorageState) DeviceBoundSession() bool {
	for _, cookie := range s.Cookies {
		if cookie.Name == DeviceBoundCookie {
			return true
		}
	}
	return false
}

// HasChromeRenewal 判断认证扩展里是否保存了 Chrome OAuth 续签材料（只看有没有，不读值）；认证扩展解析不了时按没有
func (s StorageState) HasChromeRenewal() bool {
	extension, _, err := s.AuthExtension()
	return err == nil && extension.OAuth != nil
}

// AuthExtension 返回 aistudio2api 认证扩展
func (s StorageState) AuthExtension() (AuthExtension, bool, error) {
	raw, exists := s.extra[authExtensionKey]
	if !exists {
		return AuthExtension{}, false, nil
	}
	var extension AuthExtension
	if err := json.Unmarshal(raw, &extension); err != nil {
		return AuthExtension{}, true, fmt.Errorf("解析认证扩展: %w", err)
	}
	return extension, true, nil
}

// LoginMethod 描述可发布的账户登录入口
type LoginMethod struct {
	ID          string `json:"id"`
	Interactive bool   `json:"interactive"`
}

// IsolatedLoginRequest 描述独立浏览器登录所需的稳定环境。OnboardingCompleted 不为空时，登录或验证中自动完成 AI Studio 首次使用页
// （勾选必选项、点继续，不涉及同意的步骤点跳过）之后调用它（见 camoufoxnative.LoginOptions.OnboardingCompleted），供调用方写日志
type IsolatedLoginRequest struct {
	AccountID           string
	Directory           string
	Proxy               string
	Locale              string
	Timezone            string
	OnboardingCompleted func(camoufoxnative.OnboardingResult)
}

// IsolatedLoginResult 返回独立浏览器导出的认证状态
type IsolatedLoginResult struct {
	StorageState StorageState
	Email        string
	VerifiedAt   time.Time
	DriveError   string
}

// LoginVerification 表示隔离运行时对登录态的验证结果
type LoginVerification struct {
	Authenticated bool      `json:"authenticated"`
	VerifiedAt    time.Time `json:"verified_at"`
	Reason        string    `json:"reason,omitempty"`
	// PageURL 为验证结束时隔离浏览器所在页面的 host+path（不含查询参数与片段，查询参数里有账号与回跳地址）：
	// 判定登录已失效时用来区分普通登录页（会话失效）与安全验证、风控页（challenge、speedbump、rejected 等）
	PageURL string `json:"page_url,omitempty"`
	// OrgDisabled 为页面被跳到组织停用页（access.workspace.google.com）：账号所属组织停用了 AI Studio，Authenticated 为假、
	// Reason 为 OrgDisabledReason，不是登录失效（见 org_disabled.go）。页面被跳到服务受限、年龄要求、Workspace 说明页时同样为真，
	// Restriction 为原因种类、Reason 为它的固定文案
	OrgDisabled bool        `json:"org_disabled,omitempty"`
	Restriction Restriction `json:"restriction,omitempty"`
}

// IsolatedLoginDriver 定义独立浏览器登录与验证合同
type IsolatedLoginDriver interface {
	Login(context.Context, IsolatedLoginRequest) (IsolatedLoginResult, error)
	Verify(context.Context, IsolatedLoginRequest, StorageState) (LoginVerification, error)
}

// SupportedLoginMethods 返回当前可发布的登录入口
func SupportedLoginMethods() []LoginMethod {
	return []LoginMethod{{ID: LoginMethodIsolatedBrowser, Interactive: true}}
}

// LoadStorageState 读取并校验 Playwright storage state
func LoadStorageState(filePath string) (StorageState, error) {
	state, _, err := LoadStorageStateDigest(filePath)
	return state, err
}

// StorageDigest 为认证文件原始内容（字节）的 SHA-256 指纹，用来判断磁盘上的文件是不是本服务最近一次读入或写入的那一份
// （见 AccountPool.StorageFileOwned）
type StorageDigest [sha256.Size]byte

// LoadStorageStateDigest 与 LoadStorageState 相同，另返回文件原始内容的指纹
func LoadStorageStateDigest(filePath string) (StorageState, StorageDigest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return StorageState{}, StorageDigest{}, fmt.Errorf("读取 storage state: %w", err)
	}
	var state StorageState
	if err := json.Unmarshal(data, &state); err != nil {
		return StorageState{}, StorageDigest{}, fmt.Errorf("解析 storage state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return StorageState{}, StorageDigest{}, err
	}
	return state, sha256.Sum256(data), nil
}

// WriteStorageState 原子写回 Playwright storage state
func WriteStorageState(filePath string, state StorageState) error {
	_, err := writeStorageStateDigest(filePath, state)
	return err
}

// writeStorageStateDigest 与 WriteStorageState 相同，另返回写入内容的指纹
func writeStorageStateDigest(filePath string, state StorageState) (StorageDigest, error) {
	data, err := encodeStorageState(state)
	if err != nil {
		return StorageDigest{}, err
	}
	if err := atomicWriteFile(filePath, data, 0o600); err != nil {
		return StorageDigest{}, err
	}
	return sha256.Sum256(data), nil
}

// encodeStorageState 返回认证状态写进文件的内容（校验后缩进编码、末尾换行），与 WriteStorageState 写出的逐字节相同
func encodeStorageState(state StorageState) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("编码 storage state: %w", err)
	}
	return append(data, '\n'), nil
}

// Validate 校验 storage state 的浏览器字段
func (s StorageState) Validate() error {
	for index, cookie := range s.Cookies {
		if strings.TrimSpace(cookie.Name) == "" || strings.TrimSpace(cookie.Domain) == "" {
			return fmt.Errorf("storage state Cookie %d 缺少名称或域", index)
		}
		if cookie.Path == "" || cookie.Path[0] != '/' {
			return fmt.Errorf("storage state Cookie %s 的路径无效", cookie.Name)
		}
		switch cookie.SameSite {
		case "", "Lax", "Strict", "None":
		default:
			return fmt.Errorf("storage state Cookie %s 的 SameSite 无效", cookie.Name)
		}
	}
	for index, origin := range s.Origins {
		parsed, err := url.Parse(origin.Origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("storage state origin %d 无效", index)
		}
	}
	return nil
}

// CookieHeader 为目标 URL 构造当前有效的 Cookie 请求头
func (s StorageState) CookieHeader(targetURL string, now time.Time) (string, error) {
	target, err := url.Parse(targetURL)
	if err != nil || target.Scheme == "" || target.Hostname() == "" {
		return "", fmt.Errorf("目标 URL 无效")
	}
	type candidate struct {
		cookie StateCookie
		index  int
	}
	candidates := make([]candidate, 0, len(s.Cookies))
	for index, cookie := range s.Cookies {
		if cookie.Value == "" || cookie.Expires >= 0 && cookie.Expires <= float64(now.UnixNano())/1e9 {
			continue
		}
		if cookie.Secure && !strings.EqualFold(target.Scheme, "https") {
			continue
		}
		if !cookieDomainMatches(cookie.Domain, target.Hostname()) || !cookiePathMatches(cookie.Path, target.EscapedPath()) {
			continue
		}
		candidates = append(candidates, candidate{cookie: cookie, index: index})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if len(candidates[left].cookie.Path) == len(candidates[right].cookie.Path) {
			return candidates[left].index < candidates[right].index
		}
		return len(candidates[left].cookie.Path) > len(candidates[right].cookie.Path)
	})
	parts := make([]string, 0, len(candidates))
	for _, item := range candidates {
		parts = append(parts, item.cookie.Name+"="+item.cookie.Value)
	}
	return strings.Join(parts, "; "), nil
}

// CookieValue 返回目标 URL 下最具体的同名 Cookie
func (s StorageState) CookieValue(name string, targetURL string, now time.Time) (string, bool) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return "", false
	}
	selectedPath := -1
	selected := ""
	for _, cookie := range s.Cookies {
		if cookie.Name != name || cookie.Value == "" {
			continue
		}
		if cookie.Expires >= 0 && cookie.Expires <= float64(now.UnixNano())/1e9 {
			continue
		}
		if cookie.Secure && !strings.EqualFold(target.Scheme, "https") {
			continue
		}
		if !cookieDomainMatches(cookie.Domain, target.Hostname()) || !cookiePathMatches(cookie.Path, target.EscapedPath()) {
			continue
		}
		if len(cookie.Path) > selectedPath {
			selected = cookie.Value
			selectedPath = len(cookie.Path)
		}
	}
	return selected, selectedPath >= 0
}

// MergeSetCookieHeaders 将响应轮换的 Cookie 合并到 storage state。替换已有 Cookie 时写回原下标：RFC 6265 5.3
// 要求替换时保留旧 Cookie 的创建时间，浏览器的 Cookie 请求头顺序因此不变（CookieHeader 同路径长度按下标排序）；
// 新 Cookie 追加到末尾，过期或 Max-Age<0 时删除
func (s *StorageState) MergeSetCookieHeaders(headers []string, sourceURL string, now time.Time) error {
	source, err := url.Parse(sourceURL)
	if err != nil || source.Scheme == "" || source.Hostname() == "" {
		return fmt.Errorf("来源 URL 无效")
	}
	for _, header := range headers {
		response := http.Response{Header: http.Header{"Set-Cookie": []string{header}}}
		cookies := response.Cookies()
		if len(cookies) != 1 {
			return fmt.Errorf("Set-Cookie 格式无效")
		}
		incoming := cookies[0]
		domain := strings.ToLower(incoming.Domain)
		if domain == "" {
			domain = strings.ToLower(source.Hostname())
		}
		cookiePath := incoming.Path
		if cookiePath == "" {
			cookiePath = defaultCookiePath(source.EscapedPath())
		}
		index := -1
		for existingIndex, existing := range s.Cookies {
			if existing.Name == incoming.Name && strings.EqualFold(existing.Domain, domain) && existing.Path == cookiePath && existing.PartitionKey == "" {
				index = existingIndex
				break
			}
		}
		if incoming.MaxAge < 0 || !incoming.Expires.IsZero() && !incoming.Expires.After(now) {
			if index >= 0 {
				s.Cookies = append(s.Cookies[:index], s.Cookies[index+1:]...)
			}
			continue
		}
		expires := float64(-1)
		if incoming.MaxAge > 0 {
			expires = float64(now.Add(time.Duration(incoming.MaxAge)*time.Second).UnixNano()) / 1e9
		} else if !incoming.Expires.IsZero() {
			expires = float64(incoming.Expires.UnixNano()) / 1e9
		}
		cookie := StateCookie{
			Name:     incoming.Name,
			Value:    incoming.Value,
			Domain:   domain,
			Path:     cookiePath,
			Expires:  expires,
			HTTPOnly: incoming.HttpOnly,
			Secure:   incoming.Secure,
			SameSite: sameSiteText(incoming.SameSite),
		}
		if index >= 0 {
			s.Cookies[index] = cookie
		} else {
			s.Cookies = append(s.Cookies, cookie)
		}
	}
	return nil
}

// MarshalJSON 保留 storage state 的扩展根字段
func (s StorageState) MarshalJSON() ([]byte, error) {
	value := make(map[string]json.RawMessage, len(s.extra)+2)
	for key, raw := range s.extra {
		value[key] = append(json.RawMessage(nil), raw...)
	}
	cookieValues := s.Cookies
	if cookieValues == nil {
		cookieValues = []StateCookie{}
	}
	cookies, err := json.Marshal(cookieValues)
	if err != nil {
		return nil, err
	}
	originValues := s.Origins
	if originValues == nil {
		originValues = []StorageOrigin{}
	}
	origins, err := json.Marshal(originValues)
	if err != nil {
		return nil, err
	}
	value["cookies"] = cookies
	value["origins"] = origins
	return json.Marshal(value)
}

// UnmarshalJSON 解析 storage state 并保存扩展根字段
func (s *StorageState) UnmarshalJSON(data []byte) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var cookies []StateCookie
	if raw, ok := value["cookies"]; ok {
		if err := json.Unmarshal(raw, &cookies); err != nil {
			return err
		}
	}
	var origins []StorageOrigin
	if raw, ok := value["origins"]; ok {
		if err := json.Unmarshal(raw, &origins); err != nil {
			return err
		}
	}
	delete(value, "cookies")
	delete(value, "origins")
	s.Cookies = cookies
	s.Origins = origins
	s.extra = value
	return nil
}

func cookieDomainMatches(cookieDomain string, hostname string) bool {
	domain := strings.TrimPrefix(strings.ToLower(cookieDomain), ".")
	host := strings.ToLower(hostname)
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func cookiePathMatches(cookiePath string, requestPath string) bool {
	if requestPath == "" {
		requestPath = "/"
	}
	if cookiePath == "/" {
		return true
	}
	if requestPath == cookiePath {
		return true
	}
	return strings.HasPrefix(requestPath, cookiePath) && (strings.HasSuffix(cookiePath, "/") || len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' || requestPath == "/" {
		return "/"
	}
	directory := path.Dir(requestPath)
	if directory == "." {
		return "/"
	}
	return directory
}

func sameSiteText(value http.SameSite) string {
	switch value {
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return "Lax"
	}
}

func atomicWriteFile(filePath string, data []byte, mode os.FileMode) error {
	target, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("解析文件路径: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建文件目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("设置文件权限: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("替换文件: %w", err)
	}
	return nil
}
