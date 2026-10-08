package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestDefinitiveAuthenticationFailure 登录页跳转、签名 Cookie 失效、HTTP 401 与协议 Code 16 都算登录失效；
// Drive 授权缺失只表示没有 Drive 权限，不算登录失效，也不换号
func TestDefinitiveAuthenticationFailure(t *testing.T) {
	_, signErr := SignAuthorization(nil, aiStudioOrigin, time.Now().Unix())
	driveMissing := &RPCError{
		Method: "GenerateAccessToken", StatusCode: http.StatusUnauthorized, Code: 16, Message: "OAuth error: unauthorized_client.",
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"签名 Cookie 缺失", signErr, true},
		{"登录页跳转", fmt.Errorf("启动 Worker: %w", fmt.Errorf("%w url=https://accounts.google.com/", ErrAuthenticationRequired)), true},
		{"HTTP 401", &RPCError{Method: "ListModels", StatusCode: http.StatusUnauthorized}, true},
		{"协议 Code 16", &RPCError{Method: "GenerateContent", StatusCode: http.StatusOK, Code: 16}, true},
		{"Drive 授权缺失", driveMissing, false},
		{"权限不足", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7}, false},
	}
	for _, item := range cases {
		if got := DefinitiveAuthenticationFailure(item.err); got != item.want {
			t.Fatalf("%s: 判定为登录失效=%v，期望 %v（err=%v）", item.name, got, item.want, item.err)
		}
	}
	if retryableAccountError(driveMissing) {
		t.Fatal("Drive 授权缺失不应换号重试")
	}
	if !retryableAccountError(&RPCError{Method: "ListModels", StatusCode: http.StatusUnauthorized}) {
		t.Fatal("普通 401 仍应换号重试")
	}
}

// TestSameCheckedAtRequiredOverridesValid 同一请求先确认登录有效、随后返回认证失败时以失败为准，账户不能继续显示为就绪
func TestSameCheckedAtRequiredOverridesValid(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.MarkAuthenticationValid(); err != nil {
		t.Fatal(err)
	}
	if err := lease.MarkAuthenticationRequired("HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	status, _ := pool.StatusOf("alice@example.com")
	if status.State != AccountAuthRequired || status.Message != "HTTP 401" {
		t.Fatalf("同一顺序时间先有效后失效应写回需要登录，实际 state=%s message=%q", status.State, status.Message)
	}
}

// TestAuthRequiredWithLeaseShownAsAuthRequired 需要登录的账户仍有未结束的租约时显示需要登录，不被“忙碌”盖住；
// 就绪账户有租约时仍显示忙碌
func TestAuthRequiredWithLeaseShownAsAuthRequired(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	if err := pool.SetCatalog("alice@example.com", BenefitTierFree, []Model{{
		ID: "gemini-test", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountBusy {
		t.Fatalf("就绪账户有租约时应显示忙碌，实际 %s", status.State)
	}
	if summary := pool.BootstrapSummary(); summary.Available != 1 {
		t.Fatalf("忙碌的就绪账户应计入可预热，实际 %d", summary.Available)
	}
	if err := lease.MarkAuthenticationRequired("HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountAuthRequired {
		t.Fatalf("需要登录的账户仍有租约时应显示需要登录，实际 %s", status.State)
	}
	if counts := pool.StateCounts(); counts.AuthRequired != 1 || counts.Busy != 0 {
		t.Fatalf("状态统计应计入需要登录，实际 %+v", counts)
	}
	if summaries := pool.LoginSummaries(); summaries[0].State != AccountAuthRequired {
		t.Fatalf("目录监视的轻量状态应为需要登录，实际 %s", summaries[0].State)
	}
	if summary := pool.BootstrapSummary(); summary.Available != 0 {
		t.Fatalf("需要登录的账户不应计入可预热，实际 %d", summary.Available)
	}
}

// TestAdminStateOrdersAgainstEarlierLease 管理操作写入就绪后，此前开始的请求较晚返回的认证失败不能覆盖它
func TestAdminStateOrdersAgainstEarlierLease(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	time.Sleep(2 * time.Millisecond)
	if err := pool.MarkReady("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := lease.MarkAuthenticationRequired("旧请求的 401"); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountReady {
		t.Fatalf("管理操作之前开始的请求不应覆盖就绪状态，实际 %s（%s）", status.State, status.Message)
	}
}

// TestMarkAuthenticationRequiredIfGeneration 没有租约的启动路径按启动前的认证代际写回认证失败：
// 期间保存过新登录或已有更晚的结果时不写入
func TestMarkAuthenticationRequiredIfGeneration(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	startedAt := time.Now()
	generation := pool.AuthGeneration("alice@example.com")
	saver, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saver.SaveStorageState(testStorageState("RELOGIN")); err != nil {
		t.Fatal(err)
	}
	if err := saver.Release(); err != nil {
		t.Fatal(err)
	}
	if marked, err := pool.MarkAuthenticationRequiredIfGeneration("alice@example.com", generation, startedAt, "登录态失效"); err != nil || marked {
		t.Fatalf("启动期间保存过新登录时不应写入: marked=%v err=%v", marked, err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountReady {
		t.Fatalf("新登录不应被旧启动结果覆盖，实际 %s", status.State)
	}

	generation = pool.AuthGeneration("alice@example.com")
	if marked, err := pool.MarkAuthenticationRequiredIfGeneration("alice@example.com", generation, startedAt, "登录态失效"); err != nil || !marked {
		t.Fatalf("当前代际的启动失败应写入: marked=%v err=%v", marked, err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountAuthRequired || status.Message != "登录态失效" {
		t.Fatalf("应标为需要登录，实际 state=%s message=%q", status.State, status.Message)
	}

	time.Sleep(2 * time.Millisecond)
	if err := pool.MarkReady("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if marked, _ := pool.MarkAuthenticationRequiredIfGeneration("alice@example.com", generation, startedAt, "登录态失效"); marked {
		t.Fatal("管理端恢复就绪后，更早开始的启动结果不应再写入")
	}
}

// TestWaitForAuthRefreshWaitsForOtherLeases 续签等待同账户其他正常请求结束；其他租约已提交新认证时立即返回
func TestWaitForAuthRefreshWaitsForOtherLeases(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	ctx := context.Background()
	refresher, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer refresher.Release()
	other, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	end, ok := refresher.BeginAuthRefresh()
	if !ok {
		t.Fatal("BeginAuthRefresh 被拒绝")
	}
	defer end()

	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := refresher.WaitForAuthRefresh(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("其他请求未结束时应等到期限，实际 %v", err)
	}

	done := make(chan error, 1)
	go func() {
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		done <- refresher.WaitForAuthRefresh(waitCtx)
	}()
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("其他请求结束后应立即返回，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("其他请求结束后仍在等待")
	}

	// 续签窗口期间账户不接新租约：先结束窗口，再由另一个租约保存新登录
	end()
	saver, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Release()
	if err := saver.SaveStorageState(testStorageState("RELOGIN")); err != nil {
		t.Fatal(err)
	}
	immediateCtx, immediateCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer immediateCancel()
	if err := refresher.WaitForAuthRefresh(immediateCtx); err != nil {
		t.Fatalf("其他租约已提交新认证时应立即返回，实际 %v", err)
	}
}

// TestForwardEventsConfirmsAuthOnFinish 流式请求只在正常结束时确认登录有效：正文之后才返回的 401 写回需要登录，
// 结束之后的事件不再转发
func TestForwardEventsConfirmsAuthOnFinish(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	run := func(events ...Event) []Event {
		t.Helper()
		lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		source := make(chan Event, len(events))
		for _, event := range events {
			source <- event
		}
		close(source)
		destination := make(chan Event, len(events)+1)
		forwardEventsWithLease(context.Background(), source, destination, lease, pool, "")
		forwarded := make([]Event, 0, len(events))
		for event := range destination {
			forwarded = append(forwarded, event)
		}
		return forwarded
	}

	unauthorized := &RPCError{Method: "GenerateContent", StatusCode: http.StatusUnauthorized, Message: "Unauthorized"}
	run(Event{Kind: EventText, Text: "部分正文"}, Event{Kind: EventError, Err: unauthorized})
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountAuthRequired {
		t.Fatalf("正文之后返回的 401 应写回需要登录，实际 %s", status.State)
	}

	if err := pool.MarkReady("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	forwarded := run(
		Event{Kind: EventText, Text: "正文"},
		Event{Kind: EventFinish, FinishReason: "stop"},
		Event{Kind: EventText, Text: "结束之后"},
	)
	if len(forwarded) != 2 || forwarded[1].Kind != EventFinish {
		t.Fatalf("结束之后的事件不应转发，实际 %+v", forwarded)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.State != AccountReady {
		t.Fatalf("正常结束应保持就绪，实际 %s", status.State)
	}
}
