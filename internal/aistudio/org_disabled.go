package aistudio

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// 账号所属组织停用了 AI Studio（Google Workspace 账号的组织或 Google 为它关闭了这项服务）：载入 AI Studio 页面被跳到
// access.workspace.google.com 的 ServiceNotAllowed 页（camoufoxnative.ServiceNotAllowedError），任何 RPC（GetAiStudioBenefitTier、
// ListModels、GenerateContent 等）都返回 HTTP 403、协议错误码 7、“AI Studio app is disabled.”。重新登录也没用，只有组织重新开放
// 或换号才能恢复，与单模型 403（“The caller does not have permission”，账号对某个模型没有权限）是两回事。
// 账户池对这样的账户记一个账户级标记（MarkOrgDisabled）：对外状态为 AccountOrgDisabled，不参与任何调度（选号、预热、替补、备用统计），
// 底层状态（就绪、需要登录等）照常保留，清除标记后原样恢复。标记的来源、持久化与清除规则在 app 层（见 app/org_disabled.go）

// AccountOrgDisabled 表示账号所属组织停用了 AI Studio：不参与调度，认证文件更新、管理员清除或长间隔复查通过后恢复
const AccountOrgDisabled AccountState = "org_disabled"

// ErrOrgDisabled 为账号所属组织停用了 AI Studio 的哨兵（与 camoufoxnative.ErrServiceNotAllowed 是同一个）：
// 启动、验证、登录时的组织停用页（camoufoxnative.ServiceNotAllowedError）与上游 RPC 的“AI Studio app is disabled”（*RPCError）
// 都满足 errors.Is(err, ErrOrgDisabled)
var ErrOrgDisabled = camoufoxnative.ErrServiceNotAllowed

// OrgDisabledReason 为组织停用的固定文案（账户状态说明、验证原因与日志）
const OrgDisabledReason = camoufoxnative.ServiceNotAllowedPrefix

// 进入组织停用状态的来源，写进“账号所属组织已停用 AI Studio，停止调度 | 来源=”
const (
	OrgDisabledFromStartup = "启动"
	OrgDisabledFromCatalog = "模型目录"
	OrgDisabledFromVerify  = "验证"
	OrgDisabledFromLogin   = "登录"
	OrgDisabledFromRequest = "请求"
	// OrgDisabledFromRecheck 为登录复核（疑似登录失效与需要登录账户的定期复核）的 GetAiStudioBenefitTier 返回 app is disabled
	OrgDisabledFromRecheck = "登录复核"
)

// UnschedulableOrgDisabled 为账户所属组织停用了 AI Studio 时不可调度的原因（WarmWorkerState.Unschedulable、UnschedulableSet）
const UnschedulableOrgDisabled = "组织已停用"

// orgDisabledMessage 为上游 RPC 组织停用的文案（小写比较）
const orgDisabledMessage = "ai studio app is disabled"

// orgDisabled 判断上游错误是否为组织停用：HTTP 403、协议错误码 7（或没有协议错误码）、文案含 “AI Studio app is disabled”。
// 单模型 403（“The caller does not have permission”）文案不同，不算
func (e *RPCError) orgDisabled() bool {
	return e != nil && e.StatusCode == http.StatusForbidden && (e.Code == 7 || e.Code == 0) &&
		strings.Contains(strings.ToLower(e.Message), orgDisabledMessage)
}

// Is 让组织停用的上游错误满足 errors.Is(err, ErrOrgDisabled)，官网停用 Build 生成的上游错误满足
// errors.Is(err, ErrBuildGenerationDisabled)（见 build_pause.go）
func (e *RPCError) Is(target error) bool {
	switch target {
	case ErrOrgDisabled:
		return e.orgDisabled()
	case ErrBuildGenerationDisabled:
		return e.buildGenerationDisabled()
	default:
		return false
	}
}

// OrgDisabledFailure 判断错误是否说明账号所属组织停用了 AI Studio（页面跳到组织停用页，或上游 RPC 返回 “AI Studio app is disabled”）
func OrgDisabledFailure(err error) bool {
	return err != nil && errors.Is(err, ErrOrgDisabled)
}

// OrgDisabledReporter 为识别出组织停用时的回调（经 ReportOrgDisabled：模型目录同步、CountTokens、上传、视频等 aistudio 自己处理
// 失败的路径，以及 app 层的 Worker 启动），由 app 层挂上：登记、持久化、写日志并登记释放 Worker（见 app/org_disabled.go）。
// 没有挂上时只写账户池的标记
type OrgDisabledReporter func(accountID string, source string, cause error)

// SetOrgDisabledReporter 挂上组织停用的回调（见 OrgDisabledReporter）；nil 时取消
func (p *AccountPool) SetOrgDisabledReporter(report OrgDisabledReporter) {
	if p == nil {
		return
	}
	if report == nil {
		p.orgDisabledReporter.Store(nil)
		return
	}
	p.orgDisabledReporter.Store(&report)
}

// ReportOrgDisabled 报告账户所属组织停用了 AI Studio（aistudio 内部的目录同步与请求失败，以及 app 层 Worker 启动时页面停在
// 组织停用页）：有回调时交给回调（回调负责登记与写标记），没有时直接写标记。调用方不能持有账户池锁
func (p *AccountPool) ReportOrgDisabled(accountID string, source string, cause error) {
	if p == nil {
		return
	}
	if report := p.orgDisabledReporter.Load(); report != nil && *report != nil {
		(*report)(accountID, source, cause)
		return
	}
	_, _ = p.MarkOrgDisabled(accountID, OrgDisabledReason)
}

// MarkOrgDisabled 给账户记上组织停用的标记，返回标记是否新写入（账户已有标记时只更新说明）。对外状态随即为 AccountOrgDisabled
// （停用账户仍显示为停用），不再给它任何不固定账户或指定账户的调度租约；管理操作（AcquireAccount、TryAcquireIdleAccount）不受影响。
// 标记变化时唤醒等待调度的请求
func (p *AccountPool) MarkOrgDisabled(accountID string, reason string) (bool, error) {
	if p == nil {
		return false, ErrAccountNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = OrgDisabledReason
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false, ErrAccountNotFound
	}
	if account.orgDisabled && account.orgDisabledReason == reason {
		return false, nil
	}
	added := !account.orgDisabled
	account.orgDisabled = true
	account.orgDisabledReason = reason
	p.accountChangedLocked(account)
	p.notifyLocked()
	return added, nil
}

// ClearOrgDisabled 清除账户的组织停用标记，返回是否清除了（账户不存在或没有标记时为假）：对外状态回到底层状态（就绪、需要登录等），
// 立即重新参与调度
func (p *AccountPool) ClearOrgDisabled(accountID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || !account.orgDisabled {
		return false
	}
	account.orgDisabled = false
	account.orgDisabledReason = ""
	p.accountChangedLocked(account)
	p.notifyLocked()
	return true
}

// OrgDisabled 返回账户是否带着组织停用的标记
func (p *AccountPool) OrgDisabled(accountID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	return account != nil && account.orgDisabled
}

// schedulingState 返回调度与对外状态用的账户状态：带组织停用标记、没有停用的账户为 AccountOrgDisabled，其余为底层状态 State。
// 候选索引、选号与状态推导都经它读取，底层状态的写入（认证结果、目录同步、管理操作）照常进行、不覆盖标记；调用方持有 p.mu
func (account *Account) schedulingState() AccountState {
	if account.orgDisabled && account.State != AccountDisabled {
		return AccountOrgDisabled
	}
	return account.State
}

// stateMessageLocked 返回账户状态的说明：组织停用时为标记的说明，其余为底层状态的说明；调用方持有 p.mu
func (account *Account) stateMessageLocked() string {
	if account.schedulingState() == AccountOrgDisabled && account.Config.Enabled {
		return account.orgDisabledReason
	}
	return account.stateMessage
}
