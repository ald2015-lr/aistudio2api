package aistudio

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// TestRestrictionClassification 上游 RPC 的 403/7 文案按种类归类（按上游 a68acad 移植）：“Account not supported.” 为年龄或使用资格，
// “Early Access Apps disabled.”（含 Core data access 的写法）为 Workspace 未开放，与 “AI Studio app is disabled” 一样满足
// errors.Is(err, ErrOrgDisabled)，包装之后照样识别；页面停在拒绝页时取页面的种类。“Region not supported.” 是出口的问题：
// 只满足 ErrRegionNotSupported，不算拒绝账号。错误文案末尾补上说明；状态或协议错误码不对、文案只是包含这几个词的都不算
func TestRestrictionClassification(t *testing.T) {
	age := DecodeRPCError("GetAiStudioBenefitTier", http.StatusForbidden, []byte(`[7,"Account not supported.",[]]`))
	if age.Code != 7 || age.Message != "Account not supported." {
		t.Fatalf("解码结果不对: %+v", age)
	}
	cases := []struct {
		name   string
		err    error
		kind   Restriction
		region bool
	}{
		{name: "组织停用", err: orgRPC("AI Studio app is disabled."), kind: RestrictionOrgDisabled},
		{name: "年龄或资格", err: age, kind: RestrictionAge},
		{name: "年龄或资格（包装、没有句点）", err: fmt.Errorf("刷新账户 a@example.com 的模型目录: %w", orgRPC("account not supported")), kind: RestrictionAge},
		{name: "Workspace", err: orgRPC("Early Access Apps disabled."), kind: RestrictionWorkspace},
		{name: "Workspace（Core data access）", err: orgRPC("Core data access for Early Access Apps disabled."), kind: RestrictionWorkspace},
		{name: "没有协议错误码", err: &RPCError{Method: "ListModels", StatusCode: http.StatusForbidden, Message: "Account not supported."}, kind: RestrictionAge},
		{name: "服务受限页", err: fmt.Errorf("启动: %w", &camoufoxnative.ServiceNotAllowedError{URL: "accounts.google.com/info/servicerestricted", Kind: RestrictionService}), kind: RestrictionService},
		{name: "地区", err: orgRPC("Region not supported."), region: true},
		{name: "地区（包装）", err: fmt.Errorf("生成: %w", orgRPC("Region not supported")), region: true},
		{name: "单模型 403", err: orgRPC("The caller does not have permission")},
		{name: "只是包含", err: orgRPC("Account not supported. Please verify your age")},
		{name: "401", err: &RPCError{Method: "GetAiStudioBenefitTier", StatusCode: http.StatusUnauthorized, Code: 16, Message: "Account not supported."}},
		{name: "其他协议错误码", err: &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 9, Message: "Region not supported."}},
	}
	for _, item := range cases {
		if got := OrgDisabledFailure(item.err); got != (item.kind != "") {
			t.Errorf("%s：OrgDisabledFailure = %t（%v）", item.name, got, item.err)
		}
		if got := RegionNotSupported(item.err); got != item.region {
			t.Errorf("%s：RegionNotSupported = %t（%v）", item.name, got, item.err)
		}
		if item.kind != "" {
			if got := RestrictionOf(item.err); got != item.kind {
				t.Errorf("%s：RestrictionOf = %q，期望 %q", item.name, got, item.kind)
			}
			if DefinitiveAuthenticationFailure(item.err) {
				t.Errorf("%s：拒绝账号不应算登录失效", item.name)
			}
		}
	}
	if message := age.Error(); !strings.HasSuffix(message, "Account not supported.（"+camoufoxnative.AgeRequirementPrefix+"）") {
		t.Fatalf("年龄或资格的错误文案应补上说明: %s", message)
	}
	if message := orgRPC("Region not supported.").Error(); !strings.Contains(message, camoufoxnative.RegionNotSupportedPrefix) {
		t.Fatalf("地区的错误文案应写明出口地区不受支持: %s", message)
	}
	for _, plain := range []string{"AI Studio app is disabled.", "The caller does not have permission"} {
		if message := orgRPC(plain).Error(); strings.Contains(message, "（") {
			t.Fatalf("其他错误的文案不变: %s", message)
		}
	}
	if !errors.Is(orgRPC("Region not supported."), ErrRegionNotSupported) || !errors.Is(&camoufoxnative.RegionNotSupportedError{}, ErrRegionNotSupported) {
		t.Fatal("上游地区错误与地区说明页应是同一个哨兵")
	}
	if RestrictionOf(errors.New("其他")) != RestrictionOrgDisabled || RestrictionReason("") != OrgDisabledReason {
		t.Fatal("认不出种类时按组织停用")
	}
}

// orgRPC 返回 GenerateContent 的 HTTP 403、协议错误码 7 上游错误
func orgRPC(message string) *RPCError {
	return &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: message}
}

// TestMarkOrgDisabledRestriction 账户池标记带原因种类：对外状态同为 org_disabled，状态里另带种类、说明为种类的固定文案，
// 不可调度原因按种类写；改记成别的种类时更新种类与说明（不算新写入），经回调以外的 ReportOrgDisabled 按错误归类；清除后种类一并清掉
func TestMarkOrgDisabledRestriction(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree})
	added, err := pool.MarkOrgDisabled(scopeNormalA, RestrictionService)
	if err != nil || !added {
		t.Fatalf("第一次标记应写入: added=%t err=%v", added, err)
	}
	status, _ := pool.StatusOf(scopeNormalA)
	if status.State != AccountOrgDisabled || !status.OrgDisabled || status.Restriction != RestrictionService ||
		status.Message != camoufoxnative.ServiceRestrictedPrefix {
		t.Fatalf("服务受限的状态 = %+v", status)
	}
	if reason := pool.UnschedulableSet([]string{scopeNormalA})[scopeNormalA]; reason != UnschedulableServiceRestricted {
		t.Fatalf("不可调度原因 = %q", reason)
	}
	if added, err := pool.MarkOrgDisabled(scopeNormalA, RestrictionAge); err != nil || added {
		t.Fatalf("改记种类不算新写入: added=%t err=%v", added, err)
	}
	if status, _ = pool.StatusOf(scopeNormalA); status.Restriction != RestrictionAge || status.Message != camoufoxnative.AgeRequirementPrefix {
		t.Fatalf("改记后的状态 = %+v", status)
	}
	if counts := pool.StateCounts(); counts.OrgDisabled != 1 || counts.Ready != 1 {
		t.Fatalf("计数 = %+v", counts)
	}
	// 没有挂回调时 ReportOrgDisabled 按错误归类
	pool.ReportOrgDisabled(scopeNormalB, OrgDisabledFromRequest, orgRPC("Early Access Apps disabled."))
	if status, _ = pool.StatusOf(scopeNormalB); status.Restriction != RestrictionWorkspace ||
		pool.UnschedulableSet([]string{scopeNormalB})[scopeNormalB] != UnschedulableWorkspaceDisabled {
		t.Fatalf("按错误归类的状态 = %+v", status)
	}
	if !pool.ClearOrgDisabled(scopeNormalA) {
		t.Fatal("应清除标记")
	}
	if status, _ = pool.StatusOf(scopeNormalA); status.OrgDisabled || status.Restriction != "" || status.State != AccountReady {
		t.Fatalf("清除后的状态 = %+v", status)
	}
	// 组织停用（空种类）照旧
	if _, err := pool.MarkOrgDisabled(scopeNormalA, ""); err != nil {
		t.Fatal(err)
	}
	if status, _ = pool.StatusOf(scopeNormalA); status.Restriction != RestrictionOrgDisabled || status.Message != OrgDisabledReason ||
		pool.UnschedulableSet([]string{scopeNormalA})[scopeNormalA] != UnschedulableOrgDisabled {
		t.Fatalf("组织停用的状态 = %+v", status)
	}
}
