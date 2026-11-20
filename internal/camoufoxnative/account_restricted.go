package camoufoxnative

import (
	"errors"
	"net/url"
	"strings"
)

// 官网拒绝账号使用 AI Studio 时页面会被跳到的几种拒绝页（按上游 Mag1cFall/AIStudio2API a68acad 的识别移植）：
//   - access.workspace.google.com（ServiceNotAllowed 等页）：账号所属组织停用了 AI Studio（见 service_not_allowed.go）；
//   - accounts.google.com/info/servicerestricted：Google 限制了这个账号使用 AI Studio（在登录域名下，不是登录页，原先被当成登录失效）；
//   - aistudio.google.com/docs/troubleshoot-ai-studio#age-requirements：账号不满足 AI Studio 的年龄要求；
//   - aistudio.google.com/docs/workspace：账号所属 Google Workspace 没有开放 AI Studio。
//
// 这四种都是账号本身用不了 AI Studio，重新登录也没用：都以 ServiceNotAllowedError 返回（满足 errors.Is(err, ErrServiceNotAllowed)），
// Kind 区分原因，app 层按同一套组织停用机制停止调度、持久化、复查恢复，只是原因文案不同。
// aistudio.google.com/docs/available-regions（AI Studio 不支持当前出口地区）是出口的问题，不是账号的：以 RegionNotSupportedError
// 返回（满足 errors.Is(err, ErrRegionNotSupported)，不满足 ErrServiceNotAllowed），只给出清楚的错误，不标记账号

// Restriction 为官网拒绝账号使用 AI Studio 的原因种类（ServiceNotAllowedError.Kind；aistudio 按 RPC 文案归到同一组种类）
type Restriction string

const (
	// RestrictionOrgDisabled 为账号所属组织停用了 AI Studio（access.workspace.google.com；RPC “AI Studio app is disabled.”）
	RestrictionOrgDisabled Restriction = "org_disabled"
	// RestrictionService 为 Google 限制了该账号使用 AI Studio（accounts.google.com/info/servicerestricted）
	RestrictionService Restriction = "service_restricted"
	// RestrictionAge 为账号不满足 AI Studio 的年龄或使用资格要求（docs/troubleshoot-ai-studio#age-requirements；RPC “Account not supported.”）
	RestrictionAge Restriction = "age_requirement"
	// RestrictionWorkspace 为账号所属 Google Workspace 没有开放 AI Studio（docs/workspace；RPC “Early Access Apps disabled.”）
	RestrictionWorkspace Restriction = "workspace_disabled"
)

// 各种类的固定文案（错误开头、账户状态说明、验证原因与日志）；组织停用沿用 ServiceNotAllowedPrefix
const (
	ServiceRestrictedPrefix = "Google 已限制该账号使用 AI Studio"
	AgeRequirementPrefix    = "账号不满足 AI Studio 的年龄或使用资格要求"
	WorkspaceDisabledPrefix = "账号所属 Google Workspace 没有开放 AI Studio"
)

// ParseRestriction 把保存的种类名还原为 Restriction：空串与不认识的值按组织停用（旧版本只有这一种，记录里没有种类）
func ParseRestriction(value string) Restriction {
	switch kind := Restriction(strings.TrimSpace(value)); kind {
	case RestrictionService, RestrictionAge, RestrictionWorkspace:
		return kind
	default:
		return RestrictionOrgDisabled
	}
}

// Reason 返回种类的固定文案
func (kind Restriction) Reason() string {
	switch kind {
	case RestrictionService:
		return ServiceRestrictedPrefix
	case RestrictionAge:
		return AgeRequirementPrefix
	case RestrictionWorkspace:
		return WorkspaceDisabledPrefix
	default:
		return ServiceNotAllowedPrefix
	}
}

// RegionNotSupportedPrefix 为 AI Studio 不支持当前出口地区的错误文案开头，完整文案为
// "AI Studio 不支持当前出口地区，为该账户或全局代理换用支持地区的出口 url=<host+path>[ | 页面=…]"
const RegionNotSupportedPrefix = "AI Studio 不支持当前出口地区，为该账户或全局代理换用支持地区的出口"

// ErrRegionNotSupported 为 AI Studio 不支持当前出口地区的哨兵：页面跳到 docs/available-regions（RegionNotSupportedError），
// aistudio 把上游 RPC 的 “Region not supported.” 归到同一个哨兵。出口的问题，不标记账号
var ErrRegionNotSupported = errors.New(RegionNotSupportedPrefix)

// RegionNotSupportedError 为启动、验证或登录时页面停在 docs/available-regions：URL 只留 host+path，suffix 为页面快照（可为空）
type RegionNotSupportedError struct {
	URL    string
	suffix string
}

func (err *RegionNotSupportedError) Error() string {
	return RegionNotSupportedPrefix + " url=" + err.URL + err.suffix
}

// Is 让 errors.Is(err, ErrRegionNotSupported) 成立
func (err *RegionNotSupportedError) Is(target error) bool {
	return target == ErrRegionNotSupported
}

const (
	// accountsHost 为 Google 账号页的域名（登录页与 servicerestricted 页都在这里）
	accountsHost = "accounts.google.com"
	// aiStudioHost 为 AI Studio 的域名（年龄要求、Workspace、地区说明页都在 /docs 下）
	aiStudioHost = "aistudio.google.com"
	// serviceRestrictedPath 为 Google 限制账号使用某项服务时跳到的页面路径
	serviceRestrictedPath = "/info/servicerestricted"
)

// deniedPageReadyExpression 为就绪条件里的拒绝页判断（JS）：组织停用页、AI Studio 的年龄要求、Workspace 与地区说明页；
// servicerestricted 页在 accounts.google.com 下，已由登录页的条件结束等待，读到地址后先于登录页识别
const deniedPageReadyExpression = `location.hostname === '` + serviceNotAllowedHost + `' || ` +
	`(location.hostname === '` + aiStudioHost + `' && /\/(available-regions|docs\/workspace|troubleshoot-ai-studio)\/?$/.test(location.pathname))`

// parsePageURL 解析页面地址，host 转为小写；解析失败或没有 host 时返回 nil
func parsePageURL(pageURL string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || parsed.Host == "" {
		return nil
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed
}

// pageRestriction 返回页面地址对应的拒绝种类，不是拒绝账号的页面时为空（地区说明页也为空，见 isRegionNotSupportedURL）
func pageRestriction(pageURL string) Restriction {
	parsed := parsePageURL(pageURL)
	if parsed == nil {
		return ""
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	switch host := parsed.Hostname(); {
	case host == serviceNotAllowedHost:
		return RestrictionOrgDisabled
	case host == accountsHost && strings.EqualFold(path, serviceRestrictedPath):
		return RestrictionService
	case host != aiStudioHost:
		return ""
	case strings.HasSuffix(path, "/troubleshoot-ai-studio"):
		return RestrictionAge
	case strings.HasSuffix(path, "/docs/workspace"):
		return RestrictionWorkspace
	}
	return ""
}

// isRegionNotSupportedURL 判断页面地址是否为 AI Studio 的地区说明页（不支持当前出口地区时跳到这里）
func isRegionNotSupportedURL(pageURL string) bool {
	parsed := parsePageURL(pageURL)
	return parsed != nil && parsed.Hostname() == aiStudioHost && strings.HasSuffix(strings.TrimSuffix(parsed.Path, "/"), "/available-regions")
}

// isDeniedPageURL 判断页面地址是否为拒绝页：拒绝账号的四种（见 pageRestriction）或地区说明页
func isDeniedPageURL(pageURL string) bool {
	return pageRestriction(pageURL) != "" || isRegionNotSupportedURL(pageURL)
}

// deniedPagePath 返回拒绝页地址写进错误的部分：host+path（同 serviceNotAllowedPath，不带查询参数），AI Studio 说明页另带片段
// （#age-requirements 说明是哪一节；片段里没有账号信息）
func deniedPagePath(pageURL string) string {
	path := serviceNotAllowedPath(pageURL)
	if parsed := parsePageURL(pageURL); parsed != nil && parsed.Hostname() == aiStudioHost && parsed.Fragment != "" {
		path = truncateRunes(path+"#"+parsed.EscapedFragment(), 160)
	}
	return path
}

// deniedPageError 返回停在拒绝页的错误（suffix 为页面快照，可为空）：地区说明页为 RegionNotSupportedError，
// 其余为带种类的 ServiceNotAllowedError；不是拒绝页时为 nil
func deniedPageError(pageURL string, suffix string) error {
	if isRegionNotSupportedURL(pageURL) {
		return &RegionNotSupportedError{URL: deniedPagePath(pageURL), suffix: suffix}
	}
	if kind := pageRestriction(pageURL); kind != "" {
		return &ServiceNotAllowedError{URL: deniedPagePath(pageURL), suffix: suffix, Kind: kind}
	}
	return nil
}

// deniedPageFailure 判断错误是否为停在拒绝页（拒绝账号或不支持出口地区）
func deniedPageFailure(err error) bool {
	return err != nil && (errors.Is(err, ErrServiceNotAllowed) || errors.Is(err, ErrRegionNotSupported))
}
