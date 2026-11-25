package aistudio

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// IP 池固定分配的暂停（ProxyBinding.Hold）：就绪账户带 Hold 时对外与调度状态为 proxy_paused，不参与任何调度；清除后恢复

const proxyHoldTestText = "IP 池固定端口 10023 冷却中（连续出口连不通），恢复=2026-11-24T10:15:00Z；固定分配不换端口"

// proxyHoldBinding 返回固定端口 10023 的绑定（hold 为空时不暂停）
func proxyHoldBinding(hold string) *ProxyBinding {
	return &ProxyBinding{URL: "http://user:pass@proxy.example.com:10023", Slot: "10023", Hold: hold}
}

func verifyProxyHoldIndex(t *testing.T, pool *AccountPool) {
	t.Helper()
	pool.mu.Lock()
	err := pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatalf("候选索引与账户不一致: %v", err)
	}
}

// TestProxyHoldSchedulingState 就绪 + Hold → proxy_paused（说明为 Hold）；需要登录、组织停用、停用优先；清除后恢复就绪
func TestProxyHoldSchedulingState(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: proxyHoldBinding(proxyHoldTestText)}})
	status, _ := pool.StatusOf(scopeNormalA)
	if status.State != AccountProxyPaused || status.Message != proxyHoldTestText {
		t.Fatalf("状态应为 proxy_paused 并带原因: %+v", status)
	}
	if counts := pool.StateCounts(); counts.Total != 2 || counts.Ready != 1 {
		t.Fatalf("暂停的账户不应计入就绪: %+v", counts)
	}
	if err := pool.MarkAuthRequired(scopeNormalA, "AI Studio 登录已失效"); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountAuthRequired {
		t.Fatalf("需要登录应优先于暂停: %s", status.State)
	}
	if err := pool.MarkReady(scopeNormalA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MarkOrgDisabled(scopeNormalA, ""); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountOrgDisabled {
		t.Fatalf("组织停用应优先于暂停: %s", status.State)
	}
	pool.ClearOrgDisabled(scopeNormalA)
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountProxyPaused {
		t.Fatalf("清除组织停用后仍应暂停: %s", status.State)
	}
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: proxyHoldBinding("")}})
	if status, _ := pool.StatusOf(scopeNormalA); status.State != AccountReady || status.Message != "" {
		t.Fatalf("清除暂停后应回到就绪: %+v", status)
	}
	if label := accountStateLabels[AccountProxyPaused]; label != "IP 池端口暂停使用" {
		t.Fatalf("状态标签 = %q", label)
	}
	verifyProxyHoldIndex(t, pool)
}

// TestProxyHoldIndexConsistent 经 SetProxyBindings 与 UpdateProxyBindings 改 Hold 后候选索引一致；暂停的账户不被选中、不在候选分类与上传候选里
func TestProxyHoldIndexConsistent(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	ctx := scopeContext(PoolScopeNormal)
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	// 先建好候选索引再改 Hold：索引必须随之更新
	if lease, ok, err := pool.TryAcquireFor(ctx, selection); err != nil || !ok {
		t.Fatalf("取租约: ok=%t err=%v", ok, err)
	} else {
		_ = lease.Release()
	}
	pool.UpdateProxyBindings(func(accountID, _ string) *ProxyBinding {
		if accountID == scopeNormalA {
			return proxyHoldBinding(proxyHoldTestText)
		}
		return proxyHoldBinding("")
	})
	verifyProxyHoldIndex(t, pool)
	for range 6 {
		lease, ok, err := pool.TryAcquireFor(ctx, selection)
		if err != nil || !ok {
			t.Fatalf("应能取得 B 的租约: ok=%t err=%v", ok, err)
		}
		if lease.Account().ID != scopeNormalB {
			t.Fatalf("暂停的账户不应被选中: %s", lease.Account().ID)
		}
		_ = lease.Release()
	}
	groups, err := pool.ClassifyCandidatesCached(selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(groups.StandbyReady, scopeNormalA) || slices.Contains(groups.StandbyBusy, scopeNormalA) {
		t.Fatalf("候选分类不应包含暂停的账户: %+v", groups)
	}
	if ids := pool.fileUploadAccountIDs(PoolScopeNormal); slices.Contains(ids, scopeNormalA) {
		t.Fatalf("上传候选不应包含暂停的账户: %v", ids)
	}
	if members := pool.ExtraMembers(time.Now(), 0, nil); slices.ContainsFunc(members, func(member ExtraMember) bool { return member.ID == scopeNormalA }) {
		t.Fatalf("补充池成员不应包含暂停的账户: %+v", members)
	}
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: proxyHoldBinding("")}})
	verifyProxyHoldIndex(t, pool)
	pinned := selection
	pinned.AccountID = scopeNormalA
	lease, ok, err := pool.TryAcquireFor(ctx, pinned)
	if err != nil || !ok {
		t.Fatalf("清除暂停后应立即参与调度: ok=%t err=%v", ok, err)
	}
	_ = lease.Release()
}

// TestProxyHoldUnschedulableReason 不可调度原因为“IP 池端口暂停”，需要登录与停用优先
func TestProxyHoldUnschedulableReason(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	pool.SetProxyBindings([]ProxyBindingUpdate{
		{AccountID: scopeNormalA, Binding: proxyHoldBinding(proxyHoldTestText)},
		{AccountID: scopeNormalB, Binding: proxyHoldBinding(proxyHoldTestText)},
	})
	if err := pool.MarkAuthRequired(scopeNormalB, "AI Studio 登录已失效"); err != nil {
		t.Fatal(err)
	}
	reasons := pool.UnschedulableSet([]string{scopeNormalA, scopeNormalB})
	if reasons[scopeNormalA] != UnschedulableProxyPaused || reasons[scopeNormalB] != UnschedulableAuthRequired {
		t.Fatalf("不可调度原因 = %v", reasons)
	}
}

// TestProxyHoldNoEligibleMessage 指定暂停的账户：按号池暂时不可调度返回，内部原因带暂停文案
func TestProxyHoldNoEligibleMessage(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: proxyHoldBinding(proxyHoldTestText)}})
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent", AccountID: scopeNormalA}
	waitCtx, cancel := context.WithTimeout(scopeContext(PoolScopeNormal), 300*time.Millisecond)
	defer cancel()
	_, err := pool.AcquireFor(waitCtx, selection)
	if err == nil || !errors.Is(err, ErrNoEligibleAccount) || !strings.Contains(err.Error(), "IP 池端口暂停使用") ||
		!strings.Contains(err.Error(), "固定分配不换端口") {
		t.Fatalf("指定暂停的账户应按不可调度返回并写明原因: %v", err)
	}
	if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "proxy.example.com") {
		t.Fatalf("原因不应含地址: %v", err)
	}
}

// TestProxyHoldOffUnchanged 只改 URL、Hold 不变时不经 accountChangedLocked（索引快照不变），与没有 Hold 字段时相同
func TestProxyHoldOffUnchanged(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: proxyHoldBinding("")}})
	pool.mu.Lock()
	index := pool.schedulingLocked()
	state := index.state[pool.byID[scopeNormalA].position]
	pool.mu.Unlock()
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: scopeNormalA, Binding: &ProxyBinding{URL: "http://user:pass@proxy.example.com:10024", Slot: "10024"}}})
	pool.mu.Lock()
	unchanged := pool.sched == index && index.state[pool.byID[scopeNormalA].position] == state && state == AccountReady
	pool.mu.Unlock()
	if !unchanged {
		t.Fatal("Hold 不变时候选索引应保持原样")
	}
	verifyProxyHoldIndex(t, pool)
	if previous := (&Account{}).ProxyHold(); previous != "" || (*Account)(nil).ProxyHold() != "" {
		t.Fatal("没有绑定时 Hold 应为空")
	}
}
