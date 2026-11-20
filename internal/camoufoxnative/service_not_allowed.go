package camoufoxnative

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// ServiceNotAllowedPrefix 为 AI Studio 被账号所属组织停用的错误文案前缀：载入 AI Studio 时页面被跳到
// access.workspace.google.com（ServiceNotAllowed 等页，正文为“you do not have access to AI Studio … managed by an organisation
// that has this service turned off”）。完整文案为 "AI Studio 已被账号所属组织停用 url=<host+path>[ | 页面=…]"。
// 这是 Google Workspace 账号所属组织（或 Google）关闭了 AI Studio，重新登录也没用；与缓存无关，不算页面状态类（见 PageStateFailure）
const ServiceNotAllowedPrefix = "AI Studio 已被账号所属组织停用"

// serviceNotAllowedHost 为组织停用服务时跳转到的 Google Workspace 访问控制页的域名
const serviceNotAllowedHost = "access.workspace.google.com"

// ErrServiceNotAllowed 为账号用不了 AI Studio（组织停用，以及服务受限、年龄要求、Workspace 未开放，见 account_restricted.go）的哨兵：
// ServiceNotAllowedError 满足 errors.Is(err, ErrServiceNotAllowed)，aistudio 把上游 RPC 的“AI Studio app is disabled”
// “Account not supported.”“Early Access Apps disabled.”归到同一个哨兵（见 aistudio.ErrOrgDisabled）
var ErrServiceNotAllowed = errors.New(ServiceNotAllowedPrefix)

// ServiceNotAllowedError 为启动、验证或登录时页面停在拒绝账号使用 AI Studio 的页面：access.workspace.google.com（组织停用），
// 以及 servicerestricted、年龄要求、Workspace 说明页（见 account_restricted.go）。URL 只留 host+path（查询参数里有应用编号与回跳地址），
// suffix 为页面快照（可为空，见 PageSnapshotMarker），Kind 为原因种类（空为组织停用）
type ServiceNotAllowedError struct {
	URL    string
	suffix string
	Kind   Restriction
}

func (err *ServiceNotAllowedError) Error() string {
	return err.Restriction().Reason() + " url=" + err.URL + err.suffix
}

// Restriction 返回原因种类，空为组织停用
func (err *ServiceNotAllowedError) Restriction() Restriction {
	return ParseRestriction(string(err.Kind))
}

// Is 让 errors.Is(err, ErrServiceNotAllowed) 成立
func (err *ServiceNotAllowedError) Is(target error) bool {
	return target == ErrServiceNotAllowed
}

// isServiceNotAllowedURL 判断页面地址是否在 access.workspace.google.com（组织停用服务的访问控制页）
func isServiceNotAllowedURL(pageURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(pageURL))
	return err == nil && strings.EqualFold(parsed.Hostname(), serviceNotAllowedHost)
}

// serviceNotAllowedPath 返回页面地址的 host+path（host 小写，不含查询参数与片段）；解析失败时只去掉查询参数
func serviceNotAllowedPath(pageURL string) string {
	pageURL = strings.TrimSpace(pageURL)
	parsed, err := url.Parse(pageURL)
	if err != nil || parsed.Host == "" {
		if cut := strings.IndexAny(pageURL, "?#"); cut >= 0 {
			pageURL = pageURL[:cut]
		}
		return truncateRunes(pageURL, 120)
	}
	return truncateRunes(strings.ToLower(parsed.Host)+parsed.EscapedPath(), 120)
}

// serviceNotAllowed 返回停在拒绝页（组织停用等拒绝账号的页面或地区说明页，见 deniedPageError）的错误，附页面快照
// （一次只读 evaluate，用独立的短上下文，见 pageDiagnosis）
func (client *bidiClient) serviceNotAllowed(ctx context.Context, contextID string, pageURL string) error {
	_, suffix := client.pageDiagnosis(ctx, contextID, false)
	return deniedPageError(pageURL, suffix)
}

// snapshotServiceNotAllowed 从失败处的页面快照原文里读出地址：页面停在拒绝页（组织停用等拒绝账号的页面或地区说明页，
// 见 isDeniedPageURL）时返回它，否则为空。快照本来就要采（见 startupFailureDiagnosis），这里只解析它的 href，不多做 evaluate
func snapshotServiceNotAllowed(raw string) string {
	if raw == "" {
		return ""
	}
	var snapshot struct {
		Href string `json:"href"`
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil || !isDeniedPageURL(snapshot.Href) {
		return ""
	}
	return snapshot.Href
}
