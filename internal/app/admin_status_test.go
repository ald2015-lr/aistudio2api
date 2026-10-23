package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestStatusIncludesEgressAndCatalogSync 服务运行时状态带出口判定、目录同步与登录复核，账户计数带最近 10 分钟新增需要登录，
// 以及坏号数；服务没有运行时这几项为空（最近新增需要登录照常统计）
func TestStatusIncludesEgressAndCatalogSync(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com", "b@example.com"}, nil, 2, 2, 0)
	service := env.service
	service.catalogSync = newCatalogSyncer(service)
	service.service = newProbeTestService(service.service)
	service.authRecheck = newAuthRecheck(service)
	memory := openBadAccountMemory("", service.requests)
	memory.observe("b@example.com", "", egressBare)
	memory.observe("b@example.com", "", egressBare)
	env.manager.badAccounts.Store(memory)
	now := time.Now()
	for index := range 10 {
		service.egress.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: now})
	}
	if err := env.pool.MarkAuthRequired("a@example.com", "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	service.noteAuthRequired("a@example.com", "", authFromGeneration, authFailureError())
	admin := &runtimeAdmin{pool: env.pool, service: service, requests: service.requests, workers: env.manager}

	stopped, err := admin.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Egress != nil || stopped.CatalogSync != nil || stopped.AuthRecheck != nil {
		t.Fatalf("服务没有运行时不应显示出口、目录与登录复核: %+v", stopped)
	}
	if stopped.Accounts.AuthRequired10m != 1 || stopped.Accounts.AuthRequired != 1 || stopped.BadAccounts != 1 {
		t.Fatalf("账户计数 = %+v 坏号 = %d", stopped.Accounts, stopped.BadAccounts)
	}

	service.state.Store(serviceRunning)
	status, err := admin.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Egress) != 1 || status.Egress[0].Mode != "limited" || status.Egress[0].Label != "直连" {
		t.Fatalf("出口状态 = %+v", status.Egress)
	}
	if status.CatalogSync == nil || status.AuthRecheck == nil {
		t.Fatalf("运行时应带目录同步与登录复核状态: %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"egress"`, `"catalog_sync"`, `"auth_recheck"`, `"auth_required_10m":1`, `"bad_accounts":1`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("状态 JSON 缺少 %s: %s", field, encoded)
		}
	}
}

// TestStatusUnschedulableCounts 驻留 Worker 中已不可调度的账号按分区计数（普通、Ultra 各自），可调度的不计
func TestStatusUnschedulableCounts(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, 2, 3, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// 持有租约，Worker 不被立即释放，状态里能看到它们
	defer holdLease(t, pool, partitionNormalA)()
	defer holdLease(t, pool, partitionUltraA)()
	for _, accountID := range []string{partitionNormalA, partitionUltraA} {
		if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Unschedulable != 1 || status.UltraWorkers.Unschedulable != 1 {
		t.Fatalf("不可调度计数：普通 %d、Ultra %d，期望各 1", status.Workers.Unschedulable, status.UltraWorkers.Unschedulable)
	}
}

// TestVerifyAccountReportsAuthenticated 验证接口返回验证结论与原因（前端按它计“有效 / 登录失效 / 出错”）：
//   - 浏览器已登录、接口复核通过：authenticated=true，账号就绪；
//   - 浏览器已登录、接口复核 401：authenticated=false，原因说明 Cookie 可能已被作废，保留目录，标需要登录；
//   - 浏览器判定未登录：authenticated=false，原因带跳转到的登录页 host+path（不含查询参数）
func TestVerifyAccountReportsAuthenticated(t *testing.T) {
	isolateDisableNotes(t)
	t.Run("有效", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-ok@example.com")
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		prober := newProbeTestService(nil)
		admin.service.service = prober
		// 验证前账号带着疑似登录失效（复核器里的记录与 5 分钟冷却）：验证通过即清掉，不等随后的目录同步
		recheck := newAuthRecheck(admin.service)
		admin.service.authRecheck = recheck
		recheck.mu.Lock()
		recheck.suspects[accounts[0].ID] = &authSuspect{accountID: accounts[0].ID, since: time.Now(), due: time.Now().Add(time.Hour)}
		recheck.mu.Unlock()
		coolModel(t, admin.pool, accounts[0].ID, "", authSuspectCooldown, authSuspectReasonPrefix+"HTTP 401")
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || !*account.Authenticated || account.VerifyReason != "" || account.State != string(aistudio.AccountReady) {
			t.Fatalf("返回 = %+v，期望有效", account)
		}
		if prober.probed(accounts[0].ID) != 1 {
			t.Fatal("浏览器已登录时应用接口复核一次")
		}
		if recheck.suspected(accounts[0].ID) || suspectCooldown(t, admin.pool, accounts[0].ID) != "" {
			t.Fatal("验证通过应清掉疑似记录与疑似登录失效冷却")
		}
		if !hasLogContaining(admin.requests, "账户验证完成 | 已认证=true | 耗时=", "| 接口复核=通过 | 页面=-") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("接口认证失败", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-api@example.com")
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		prober := newProbeTestService(nil)
		prober.set(accounts[0].ID, authFailureError())
		admin.service.service = prober
		admin.service.authRecheck = newAuthRecheck(admin.service)
		before := len(testStatus(t, admin.pool, accounts[0].ID).Models)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.VerifyReason != verifyAPIRejectedReason ||
			account.State != string(aistudio.AccountAuthRequired) {
			t.Fatalf("返回 = %+v，期望登录失效（接口认证失败）", account)
		}
		if after := len(testStatus(t, admin.pool, accounts[0].ID).Models); after != before || after == 0 {
			t.Fatalf("接口认证失败时应保留目录：之前 %d 个模型，之后 %d 个", before, after)
		}
		if !hasLogContaining(admin.requests, "账户验证完成 | 已认证=false", "| 接口复核=401") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
		// 浏览器已登录、接口 401 是接口证据：定期登录复核照常复核
		if record := admin.service.authRecheck.records[accounts[0].ID]; record != nil && record.browserEvidence {
			t.Fatal("接口 401 不应记为浏览器证据")
		}
	})
	t.Run("浏览器未登录", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-login@example.com")
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{
			Reason: "AI Studio 登录已失效", PageURL: "accounts.google.com/v3/signin/identifier",
		}}
		admin.service.service = newProbeTestService(nil)
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		// 浏览器判定未登录是浏览器证据：定期登录复核不复核它（接口用同一份 Cookie 通过不说明浏览器能登录）
		if record := admin.service.authRecheck.records[accounts[0].ID]; record == nil || !record.browserEvidence {
			t.Fatalf("浏览器判定未登录应记为浏览器证据，记录 = %+v", record)
		}
		want := "AI Studio 登录已失效（跳转到 accounts.google.com/v3/signin/identifier）"
		if account.Authenticated == nil || *account.Authenticated || account.VerifyReason != want || account.Message != want {
			t.Fatalf("返回 = %+v，期望原因 %q", account, want)
		}
		if !hasLogContaining(admin.requests, "账号需要登录 | 来源=管理验证", "跳转到 accounts.google.com/v3/signin/identifier") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
}

// TestEgressActionRoutes 管理页的撤销与恢复判定经 HTTP 路由（POST /api/egress/{id}/withdraw|resume）到达运行时，返回最新状态
func TestEgressActionRoutes(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, nil, 1, 1, 0)
	env.service.state.Store(serviceRunning)
	now := time.Now()
	for index := range 10 {
		env.service.egress.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: now})
	}
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.service.requests, workers: env.manager}
	handler := api.NewHandler(stoppedService{}, api.Config{APIKey: "sk-test", AdminToken: "token", Admin: admin})
	post := func(path string) (*httptest.ResponseRecorder, api.AdminStatus) {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set("X-Admin-Token", "token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var status api.AdminStatus
		_ = json.Unmarshal(recorder.Body.Bytes(), &status)
		return recorder, status
	}
	id := aistudio.EgressID("")
	recorder, status := post("/api/egress/" + id + "/withdraw")
	if recorder.Code != http.StatusOK || len(status.Egress) != 1 || status.Egress[0].Mode != "rejected" || status.Egress[0].RejectedBy != "管理操作" {
		t.Fatalf("撤销：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, status = post("/api/egress/" + id + "/resume")
	if recorder.Code != http.StatusOK || len(status.Egress) != 1 || status.Egress[0].Mode != "normal" {
		t.Fatalf("恢复：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder, _ := post("/api/egress/000000000000/withdraw"); recorder.Code != http.StatusNotFound {
		t.Fatalf("未知出口 status=%d，期望 404", recorder.Code)
	}
}
