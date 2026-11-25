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
// 底层状态（就绪、需要登录等）照常保留，清除标记后原样恢复。标记的来源、持久化与清除规则在 app 层（见 app/org_disabled.go）。
//
// 官网拒绝账号使用 AI Studio 的其他几种原因（按上游 Mag1cFall/AIStudio2API a68acad 移植）走同一套机制，只是原因不同（Restriction）：
// Google 限制了这个账号（页面跳到 accounts.google.com/info/servicerestricted）、账号不满足年龄或使用资格要求（页面跳到
// docs/troubleshoot-ai-studio#age-requirements，RPC 403/7 “Account not supported.”）、账号所属 Workspace 没有开放 AI Studio
// （页面跳到 docs/workspace，RPC 403/7 “Early Access Apps disabled.”）。它们同样满足 errors.Is(err, ErrOrgDisabled)，
// 对外状态同为 AccountOrgDisabled，AccountStatus.Restriction 与状态说明区分原因。
// AI Studio 不支持当前出口地区（页面跳到 docs/available-regions，RPC 403/7 “Region not supported.”）是出口的问题，
// 不标记账户：满足 errors.Is(err, ErrRegionNotSupported)，只在错误文案里写清楚

// AccountOrgDisabled 表示账号所属组织停用了 AI Studio：不参与调度，认证文件更新、管理员清除或长间隔复查通过后恢复
const AccountOrgDisabled AccountState = "org_disabled"

// AccountProxyPaused 表示 IP 池固定分配（PROXY_POOL_FIXED）下账户的固定端口暂停使用（冷却中或被其他进程占用，见 ProxyBinding.Hold）：
// 不参与调度，端口恢复后 IP 池清除 Hold、自动恢复；不换端口、不走默认代理、不直连
const AccountProxyPaused AccountState = "proxy_paused"

// ErrOrgDisabled 为账号所属组织停用了 AI Studio 的哨兵（与 camoufoxnative.ErrServiceNotAllowed 是同一个）：
// 启动、验证、登录时的组织停用页（camoufoxnative.ServiceNotAllowedError）与上游 RPC 的“AI Studio app is disabled”（*RPCError）
// 都满足 errors.Is(err, ErrOrgDisabled)
var ErrOrgDisabled = camoufoxnative.ErrServiceNotAllowed

// OrgDisabledReason 为组织停用的固定文案（账户状态说明、验证原因与日志）
const OrgDisabledReason = camoufoxnative.ServiceNotAllowedPrefix

// Restriction 为官网拒绝账号使用 AI Studio 的原因种类（组织停用、服务受限、年龄要求、Workspace 未开放，见 camoufoxnative.Restriction）
type Restriction = camoufoxnative.Restriction

// 原因种类（AccountStatus.Restriction、app 层登记表的 kind）
const (
	RestrictionOrgDisabled = camoufoxnative.RestrictionOrgDisabled
	RestrictionService     = camoufoxnative.RestrictionService
	RestrictionAge         = camoufoxnative.RestrictionAge
	RestrictionWorkspace   = camoufoxnative.RestrictionWorkspace
)

// ErrRegionNotSupported 为 AI Studio 不支持当前出口地区的哨兵（与 camoufoxnative.ErrRegionNotSupported 是同一个）：
// 页面跳到 docs/available-regions 与上游 RPC 的 403/7 “Region not supported.” 都满足 errors.Is(err, ErrRegionNotSupported)。
// 出口的问题：不标记账户、不计入 403 无权限，只在错误文案里写清楚
var ErrRegionNotSupported = camoufoxnative.ErrRegionNotSupported

// RegionNotSupported 判断错误是否说明 AI Studio 不支持当前出口地区
func RegionNotSupported(err error) bool {
	return err != nil && errors.Is(err, ErrRegionNotSupported)
}

// RestrictionOf 返回组织停用类错误（OrgDisabledFailure 为真）的原因种类：页面停在拒绝页时取页面的种类，上游 RPC 按文案归类；
// 认不出种类时为组织停用
func RestrictionOf(err error) Restriction {
	var blocked *camoufoxnative.ServiceNotAllowedError
	if errors.As(err, &blocked) {
		return blocked.Restriction()
	}
	var rpcError *RPCError
	if errors.As(err, &rpcError) {
		if kind := rpcError.restriction(); kind != "" {
			return kind
		}
	}
	return RestrictionOrgDisabled
}

// RestrictionReason 返回原因种类的固定文案（账户状态说明、验证原因与日志）；空串与不认识的种类为组织停用的文案
func RestrictionReason(kind Restriction) string {
	return ParseRestriction(string(kind)).Reason()
}

// ParseRestriction 把保存的种类名还原为 Restriction：空串与不认识的值按组织停用（见 camoufoxnative.ParseRestriction）
func ParseRestriction(value string) Restriction {
	return camoufoxnative.ParseRestriction(value)
}

// 原因种类对应的不可调度原因（WarmWorkerState.Unschedulable、UnschedulableSet，写进“WAA Worker 释放 | 原因=”）；
// 组织停用为 UnschedulableOrgDisabled
const (
	UnschedulableServiceRestricted = "服务受限"
	UnschedulableAgeRequirement    = "年龄或资格不符"
	UnschedulableWorkspaceDisabled = "Workspace 未开放"
)

// RestrictionUnschedulable 返回原因种类对应的不可调度原因
func RestrictionUnschedulable(kind Restriction) string {
	switch ParseRestriction(string(kind)) {
	case RestrictionService:
		return UnschedulableServiceRestricted
	case RestrictionAge:
		return UnschedulableAgeRequirement
	case RestrictionWorkspace:
		return UnschedulableWorkspaceDisabled
	default:
		return UnschedulableOrgDisabled
	}
}

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

// workspaceDisabledMessage 为上游 RPC 的 Workspace 没有开放 AI Studio 的文案（小写比较，也认
// “Core data access for Early Access Apps disabled.”）
const workspaceDisabledMessage = "early access apps disabled"

// accountDenied 判断上游错误是否为 HTTP 403、协议错误码 7（或没有协议错误码）：账号或出口被拒绝的几种文案都是这个形状
func (e *RPCError) accountDenied() bool {
	return e != nil && e.StatusCode == http.StatusForbidden && (e.Code == 7 || e.Code == 0)
}

// deniedMessageIs 判断上游文案是否就是 want（去掉首尾空白与句末句点，不区分大小写）
func (e *RPCError) deniedMessageIs(want string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(e.Message), "."), want)
}

// restriction 返回上游错误对应的拒绝账号的种类：HTTP 403、协议错误码 7（或没有协议错误码），文案含 “AI Studio app is disabled”
// 为组织停用，为 “Account not supported.” 为年龄或使用资格，含 “Early Access Apps disabled” 为 Workspace 未开放；其他为空。
// 单模型 403（“The caller does not have permission”）与 “Region not supported.”（出口地区，见 regionNotSupported）文案不同，不算
func (e *RPCError) restriction() Restriction {
	if !e.accountDenied() {
		return ""
	}
	message := strings.ToLower(e.Message)
	switch {
	case strings.Contains(message, orgDisabledMessage):
		return RestrictionOrgDisabled
	case e.deniedMessageIs("Account not supported"):
		return RestrictionAge
	case strings.Contains(message, workspaceDisabledMessage):
		return RestrictionWorkspace
	}
	return ""
}

// orgDisabled 判断上游错误是否为拒绝账号使用 AI Studio（组织停用、年龄或使用资格、Workspace 未开放，见 restriction）
func (e *RPCError) orgDisabled() bool {
	return e.restriction() != ""
}

// regionNotSupported 判断上游错误是否为 AI Studio 不支持当前出口地区：HTTP 403、协议错误码 7（或没有协议错误码）、
// 文案为 “Region not supported.”
func (e *RPCError) regionNotSupported() bool {
	return e.accountDenied() && e.deniedMessageIs("Region not supported")
}

// deniedNote 为拒绝账号或出口的上游错误在 Error() 末尾补的说明（“（原因）”），其他错误为空：
// 上游原文 “Account not supported.”“Region not supported.” 看不出该怎么处理
func (e *RPCError) deniedNote() string {
	if e.regionNotSupported() {
		return "（" + camoufoxnative.RegionNotSupportedPrefix + "）"
	}
	if kind := e.restriction(); kind != "" && kind != RestrictionOrgDisabled {
		return "（" + kind.Reason() + "）"
	}
	return ""
}

// Is 让拒绝账号使用 AI Studio 的上游错误满足 errors.Is(err, ErrOrgDisabled)，不支持出口地区的满足 errors.Is(err, ErrRegionNotSupported)，
// 官网停用 Build 生成的上游错误满足 errors.Is(err, ErrBuildGenerationDisabled)（见 build_pause.go）
func (e *RPCError) Is(target error) bool {
	switch target {
	case ErrOrgDisabled:
		return e.orgDisabled()
	case ErrRegionNotSupported:
		return e.regionNotSupported()
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
	_, _ = p.MarkOrgDisabled(accountID, RestrictionOf(cause))
}

// MarkOrgDisabled 给账户记上组织停用的标记（kind 为原因种类，空为组织停用，说明取种类的固定文案），返回标记是否新写入
// （账户已有标记时只更新种类与说明）。对外状态随即为 AccountOrgDisabled（停用账户仍显示为停用），不再给它任何不固定账户或
// 指定账户的调度租约；管理操作（AcquireAccount、TryAcquireIdleAccount）不受影响。标记变化时唤醒等待调度的请求
func (p *AccountPool) MarkOrgDisabled(accountID string, kind Restriction) (bool, error) {
	if p == nil {
		return false, ErrAccountNotFound
	}
	kind = ParseRestriction(string(kind))
	reason := kind.Reason()
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false, ErrAccountNotFound
	}
	if account.orgDisabled && account.orgDisabledKind == kind && account.orgDisabledReason == reason {
		return false, nil
	}
	added := !account.orgDisabled
	account.orgDisabled = true
	account.orgDisabledKind = kind
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
	account.orgDisabledKind = ""
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
	// IP 池固定分配下固定端口暂停使用（见 ProxyBinding.Hold）：只盖住就绪状态，需要登录、不可用、停用与组织停用照旧优先
	if account.State == AccountReady && account.ProxyHold() != "" {
		return AccountProxyPaused
	}
	return account.State
}

// stateMessageLocked 返回账户状态的说明：组织停用时为标记的说明，固定端口暂停使用时为暂停原因，其余为底层状态的说明；调用方持有 p.mu
func (account *Account) stateMessageLocked() string {
	switch state := account.schedulingState(); {
	case state == AccountOrgDisabled && account.Config.Enabled:
		return account.orgDisabledReason
	case state == AccountProxyPaused && account.Config.Enabled:
		return account.ProxyHold()
	}
	return account.stateMessage
}
