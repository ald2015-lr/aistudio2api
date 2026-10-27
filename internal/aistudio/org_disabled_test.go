package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// TestOrgDisabledClassification 上游 RPC 的 HTTP 403、协议错误码 7、“AI Studio app is disabled” 与启动时的组织停用页
// 都归为组织停用（errors.Is(err, ErrOrgDisabled)），包装之后照样识别；普通单模型 403（The caller does not have permission）、
// 401、429 与文案相同但状态不同的错误不算，组织停用也不算登录失效
func TestOrgDisabledClassification(t *testing.T) {
	disabled := DecodeRPCError("GetAiStudioBenefitTier", http.StatusForbidden, []byte(`[7,"AI Studio app is disabled.",[]]`))
	if disabled.Code != 7 || disabled.Message != "AI Studio app is disabled." {
		t.Fatalf("解码结果不对: %+v", disabled)
	}
	wrapped := fmt.Errorf("刷新账户 a@example.com 的模型目录: %w", errors.Join(disabled, nil))
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"GetAiStudioBenefitTier", disabled, true},
		{"包装后", wrapped, true},
		{"模型列表", &RPCError{Method: "ListModels", StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled."}, true},
		{"生成", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "ai studio APP IS DISABLED"}, true},
		{"没有协议错误码", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Message: "AI Studio app is disabled."}, true},
		{"组织停用页", fmt.Errorf("启动: %w", &camoufoxnative.ServiceNotAllowedError{URL: "access.workspace.google.com/ServiceNotAllowed"}), true},
		{"单模型 403", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}, false},
		{"401", &RPCError{Method: "GetAiStudioBenefitTier", StatusCode: http.StatusUnauthorized, Code: 16, Message: "AI Studio app is disabled."}, false},
		{"429", &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "AI Studio app is disabled."}, false},
		{"其他协议错误码", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 9, Message: "AI Studio app is disabled."}, false},
		{"空", nil, false},
	}
	for _, item := range cases {
		if got := OrgDisabledFailure(item.err); got != item.want {
			t.Fatalf("%s：OrgDisabledFailure = %t，期望 %t（%v）", item.name, got, item.want, item.err)
		}
	}
	if DefinitiveAuthenticationFailure(disabled) {
		t.Fatal("组织停用不应算登录失效")
	}
	if !errors.Is(disabled, camoufoxnative.ErrServiceNotAllowed) {
		t.Fatal("上游组织停用与组织停用页应是同一个哨兵")
	}
}

// TestOrgDisabledStopsScheduling 账户记上组织停用后：对外状态为 org_disabled、说明为固定文案，计入 OrgDisabled；
// 不再参与任何调度（不固定账户与指定账户的选号、候选分类、可接替统计、可调度计数），不可调度原因为“组织已停用”；
// 管理操作仍能取得租约。底层状态照常写入（期间标需要登录，登录复核不取它），清除标记后原样恢复；就绪账户清除后立即重新参与调度
func TestOrgDisabledStopsScheduling(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	ctx := scopeContext(PoolScopeNormal)
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	added, err := pool.MarkOrgDisabled(scopeNormalA, "")
	if err != nil || !added {
		t.Fatalf("第一次标记应写入: added=%t err=%v", added, err)
	}
	if added, err := pool.MarkOrgDisabled(scopeNormalA, OrgDisabledReason); err != nil || added {
		t.Fatalf("重复标记不应算新写入: added=%t err=%v", added, err)
	}
	if _, err := pool.MarkOrgDisabled("missing@example.com", ""); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的账户应返回 ErrAccountNotFound: %v", err)
	}
	status, _ := pool.StatusOf(scopeNormalA)
	if status.State != AccountOrgDisabled || status.Message != OrgDisabledReason || !pool.OrgDisabled(scopeNormalA) {
		t.Fatalf("状态应为组织停用: %+v", status)
	}
	if counts := pool.StateCounts(); counts.Total != 2 || counts.Ready != 1 || counts.OrgDisabled != 1 {
		t.Fatalf("状态计数 = %+v，期望就绪 1、组织停用 1", counts)
	}
	for range 6 {
		lease, ok, err := pool.TryAcquireFor(ctx, selection)
		if err != nil || !ok {
			t.Fatalf("应能取得 B 的租约: ok=%t err=%v", ok, err)
		}
		if lease.Account().ID != scopeNormalB {
			t.Fatalf("组织停用的账户不应被选中: %s", lease.Account().ID)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	pinned := selection
	pinned.AccountID = scopeNormalA
	if lease, ok, err := pool.TryAcquireFor(ctx, pinned); ok {
		_ = lease.Release()
		t.Fatalf("指定组织停用的账户也不应取得调度租约: err=%v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = pool.AcquireFor(waitCtx, pinned)
	cancel()
	if err == nil || !errors.Is(err, ErrNoEligibleAccount) || !strings.Contains(err.Error(), "所属组织已停用 AI Studio") {
		t.Fatalf("指定组织停用的账户应按号池暂时不可调度返回并写明原因: %v", err)
	}
	groups, err := pool.ClassifyCandidatesCached(selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range append(append([]string{}, groups.StandbyReady...), groups.StandbyBusy...) {
		if accountID == scopeNormalA {
			t.Fatalf("候选分类不应包含组织停用的账户: %+v", groups)
		}
	}
	if spare := pool.SpareAccounts(PoolScopeNormal, nil, []string{scopeSharedModel}, 0, 0); spare != 1 {
		t.Fatalf("可接替统计应只算 B，得到 %d", spare)
	}
	if _, schedulable := pool.EnabledAccountsIn(PoolScopeNormal); schedulable != 1 {
		t.Fatalf("可调度计数应只算 B，得到 %d", schedulable)
	}
	if reason := pool.UnschedulableSet([]string{scopeNormalA, scopeNormalB})[scopeNormalA]; reason != UnschedulableOrgDisabled {
		t.Fatalf("不可调度原因 = %q，期望 %q", reason, UnschedulableOrgDisabled)
	}
	pool.mu.Lock()
	verifyErr := pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if verifyErr != nil {
		t.Fatalf("候选索引与账户不一致: %v", verifyErr)
	}
	// 管理操作（验证、登录、复查目录）不受调度状态限制
	admin, err := pool.AcquireAccount(context.Background(), scopeNormalA)
	if err != nil {
		t.Fatalf("管理操作应能取得组织停用账户的租约: %v", err)
	}
	if err := admin.Release(); err != nil {
		t.Fatal(err)
	}
	// 期间写入的底层状态不覆盖标记，清除后原样恢复
	if err := pool.MarkAuthRequired(scopeNormalA, "AI Studio 登录已失效"); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountOrgDisabled {
		t.Fatalf("标需要登录不应覆盖组织停用: %s", status.State)
	}
	// 登录复核不取带组织停用标记的账户（底层仍是需要登录，复核通过也不能调度），清除标记后照常取
	for _, accountID := range pool.AuthRequiredAccountIDs() {
		if accountID == scopeNormalA {
			t.Fatal("登录复核不应取组织停用的账户")
		}
	}
	summaries := pool.LoginSummaries()
	for _, summary := range summaries {
		if summary.ID == scopeNormalA && (!summary.OrgDisabled || summary.State != AccountAuthRequired) {
			t.Fatalf("目录扫描用的轻量状态应带标记与底层状态: %+v", summary)
		}
	}
	if !pool.ClearOrgDisabled(scopeNormalA) || pool.ClearOrgDisabled(scopeNormalA) {
		t.Fatal("清除应只在有标记时成功一次")
	}
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountAuthRequired || status.Message != "AI Studio 登录已失效" {
		t.Fatalf("清除后应回到底层状态: %+v", status)
	}
	if required := pool.AuthRequiredAccountIDs(); len(required) != 1 || required[0] != scopeNormalA {
		t.Fatalf("清除后登录复核应照常取需要登录的账户: %v", required)
	}
	if err := pool.MarkReady(scopeNormalA); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := pool.TryAcquireFor(ctx, pinned)
	if err != nil || !ok {
		t.Fatalf("清除后应立即重新参与调度: ok=%t err=%v", ok, err)
	}
	_ = lease.Release()
	if counts := pool.StateCounts(); counts.OrgDisabled != 0 || counts.Ready != 2 {
		t.Fatalf("清除后计数 = %+v", counts)
	}
}

// TestOrgDisabledDisabledAccount 停用账户记上组织停用后仍显示为停用（不计入 OrgDisabled），标记照样保留、状态里另带 OrgDisabled，
// 不可调度原因为已停用
func TestOrgDisabledDisabledAccount(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree})
	if _, err := pool.MarkOrgDisabled(scopeNormalA, ""); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireAccount(context.Background(), scopeNormalA)
	if err != nil {
		t.Fatal(err)
	}
	config := lease.Account().Config
	config.Enabled = false
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	status, _ := pool.StatusOf(scopeNormalA)
	if status.State != AccountDisabled || !pool.OrgDisabled(scopeNormalA) || !status.OrgDisabled {
		t.Fatalf("停用账户应显示停用且保留标记（状态里另带 OrgDisabled，管理页据此看出原因）: %+v", status)
	}
	if counts := pool.StateCounts(); counts.OrgDisabled != 0 {
		t.Fatalf("停用账户不计入组织停用: %+v", counts)
	}
	if reason := pool.UnschedulableSet([]string{scopeNormalA})[scopeNormalA]; reason != UnschedulableDisabled {
		t.Fatalf("不可调度原因 = %q", reason)
	}
}

// TestOrgDisabledReported aistudio 自己处理失败的路径（CountTokens、上传、视频等的 markRetryableFailure，目录同步的 catalogFailure）
// 遇到组织停用时交给回调（来源分别为请求、模型目录），不写冷却；没有回调时直接写账户池的标记。普通单模型 403 照旧写短冷却
func TestOrgDisabledReported(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	service := &PooledService{pool: pool}
	disabled := &RPCError{Method: "CountTokens", StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled."}
	type report struct {
		accountID, source string
		cause             error
	}
	var reports []report
	pool.SetOrgDisabledReporter(func(accountID string, source string, cause error) {
		reports = append(reports, report{accountID, source, cause})
	})
	lease, err := pool.AcquireAccount(context.Background(), scopeNormalA)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.markRetryableFailure(lease, scopeSharedModel, disabled); err != nil {
		t.Fatal(err)
	}
	if err := service.catalogFailure(lease, disabled); !errors.Is(err, ErrOrgDisabled) {
		t.Fatalf("目录同步的失败应原样返回: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0].source != OrgDisabledFromRequest || reports[1].source != OrgDisabledFromCatalog ||
		reports[0].accountID != scopeNormalA || !errors.Is(reports[0].cause, ErrOrgDisabled) {
		t.Fatalf("应交给回调: %+v", reports)
	}
	if status, _ := pool.StatusOf(scopeNormalA); len(status.Cooldowns) != 0 || pool.OrgDisabled(scopeNormalA) {
		t.Fatalf("有回调时由回调写标记，不写冷却: %+v", status)
	}
	// 普通单模型 403 不算组织停用，照旧写短冷却
	lease, err = pool.AcquireAccount(context.Background(), scopeNormalB)
	if err != nil {
		t.Fatal(err)
	}
	denied := &RPCError{Method: "CountTokens", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
	if err := service.markRetryableFailure(lease, scopeSharedModel, denied); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	if status, _ := pool.StatusOf(scopeNormalB); len(status.Cooldowns) != 1 || len(reports) != 2 {
		t.Fatalf("单模型 403 应写短冷却、不回调: cooldowns=%v reports=%d", status.Cooldowns, len(reports))
	}
	// 导出的 ReportOrgDisabled（app 层 Worker 启动确认时调用）同样交给回调
	pool.ReportOrgDisabled(scopeNormalB, OrgDisabledFromStartup, disabled)
	if len(reports) != 3 || reports[2].accountID != scopeNormalB || reports[2].source != OrgDisabledFromStartup || pool.OrgDisabled(scopeNormalB) {
		t.Fatalf("ReportOrgDisabled 应交给回调: %+v", reports)
	}
	// 没有回调时直接写账户池的标记
	pool.SetOrgDisabledReporter(nil)
	lease, err = pool.AcquireAccount(context.Background(), scopeNormalA)
	if err != nil {
		t.Fatal(err)
	}
	_ = service.catalogFailure(lease, disabled)
	_ = lease.Release()
	if !pool.OrgDisabled(scopeNormalA) {
		t.Fatal("没有回调时应直接写标记")
	}
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountOrgDisabled || !status.OrgDisabled {
		t.Fatalf("状态应为组织停用: %+v", status)
	}
	pool.ReportOrgDisabled(scopeNormalB, OrgDisabledFromStartup, disabled)
	if !pool.OrgDisabled(scopeNormalB) {
		t.Fatal("没有回调时 ReportOrgDisabled 应直接写标记")
	}
}
