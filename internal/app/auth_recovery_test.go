package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

const authTestAccount = "alice@example.com"

// authTestState 返回能生成授权头的认证状态；oauth 为真时带 Chrome 续签材料
func authTestState(t *testing.T, value string, oauth bool) aistudio.StorageState {
	t.Helper()
	cookies := make([]aistudio.StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{
			Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
		})
	}
	state := aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}}
	if oauth {
		if err := state.SetAuthExtension(aistudio.AuthExtension{
			Source: aistudio.AuthSource{Browser: "chrome", Email: authTestAccount},
			OAuth:  &aistudio.ChromeOAuthMaterial{GaiaID: "1", RefreshToken: "refresh"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// authTestPool 创建只有一个账户的账户池，模型目录中有一个可作为 WAA 初始化模型的聊天模型
func authTestPool(t *testing.T, oauth bool) (*aistudio.AccountPool, *aistudio.Account) {
	t.Helper()
	store := aistudio.NewAccountStore(t.TempDir())
	pool := aistudio.NewAccountPool(nil, 2)
	account, publish, err := store.Create(aistudio.DefaultAccountConfig(authTestAccount), authTestState(t, "OLD", oauth))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(account); err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(account.ID, aistudio.BenefitTierFree, []aistudio.Model{{
		ID: "gemini-test", Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	return pool, account
}

// authTestCounts 记录测试续签器的续签与重置次数
type authTestCounts struct {
	refreshes atomic.Int32
	resets    atomic.Int32
}

func newAuthTestRefresher(t *testing.T, counts *authTestCounts) *authRuntimeRefresher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &authRuntimeRefresher{
		refresh: func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
			counts.refreshes.Add(1)
			return authTestState(t, "NEW", false).Cookies, nil
		},
		reset: func(string) error {
			counts.resets.Add(1)
			return nil
		},
		prepareHeaders: func(string) (func(bool), error) { return func(bool) {}, nil },
		requests:       newRequestRegistry(ctx),
	}
}

func authTestLease(t *testing.T, pool *aistudio.AccountPool) (*aistudio.AccountLease, context.Context) {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: authTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease, aistudio.ContextWithAccountLease(context.Background(), lease)
}

func authTestStatus(t *testing.T, pool *aistudio.AccountPool) aistudio.AccountStatus {
	t.Helper()
	status, ok := pool.StatusOf(authTestAccount)
	if !ok {
		t.Fatal("账户不存在")
	}
	return status
}

func authTestSAPISID(t *testing.T, account *aistudio.Account) string {
	t.Helper()
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := state.CookieValue("SAPISID", "https://aistudio.google.com/", time.Now())
	return value
}

func unauthorizedResponse(body string) *aistudio.RPCResponse {
	return &aistudio.RPCResponse{
		StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)),
	}
}

const loginExpiredBody = `[16,"Request had invalid authentication credentials."]`

// TestAuthRetryReplaysOnce 登录失效时续签一次并只重放一次；重放仍失效时账户标为需要登录
func TestAuthRetryReplaysOnce(t *testing.T) {
	pool, account := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	lease, ctx := authTestLease(t, pool)
	var sends atomic.Int32
	response, err := refresher.do(ctx, "ListModels", func() (*aistudio.RPCResponse, error) {
		sends.Add(1)
		return unauthorizedResponse(loginExpiredBody), nil
	})
	var rpcError *aistudio.RPCError
	if response != nil || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusUnauthorized {
		t.Fatalf("重放仍失效时应返回原始 401，实际 response=%v err=%v", response, err)
	}
	if sends.Load() != 2 || counts.refreshes.Load() != 1 || counts.resets.Load() != 1 {
		t.Fatalf("应续签一次并只重放一次: 发送=%d 续签=%d 重置=%d", sends.Load(), counts.refreshes.Load(), counts.resets.Load())
	}
	if got := authTestSAPISID(t, account); got != "NEW" {
		t.Fatalf("续签结果没有写回，SAPISID=%q", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("重放仍失效时应标为需要登录，实际 %s", status.State)
	}
}

// TestAuthRetryKeepsDriveAuthorizationResponse Drive 授权缺失的 401 不续签、不重放，原响应交给调用方按原错误处理
func TestAuthRetryKeepsDriveAuthorizationResponse(t *testing.T) {
	pool, _ := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	_, ctx := authTestLease(t, pool)
	body := `[16,"OAuth error: unauthorized_client."]`
	response, err := refresher.do(ctx, "GenerateAccessToken", func() (*aistudio.RPCResponse, error) {
		return unauthorizedResponse(body), nil
	})
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应原样返回 Drive 授权缺失响应，实际 response=%v err=%v", response, err)
	}
	if raw, readErr := io.ReadAll(response.Body); readErr != nil || string(raw) != body {
		t.Fatalf("响应正文应可再次读取，实际 %q err=%v", raw, readErr)
	}
	if counts.refreshes.Load() != 0 || authTestStatus(t, pool).State == aistudio.AccountAuthRequired {
		t.Fatal("Drive 授权缺失不应续签或标为需要登录")
	}
}

// TestRecoverSkipsHandledStartup Worker 启动阶段已经处理过的登录失效不会触发第二次恢复
func TestRecoverSkipsHandledStartup(t *testing.T) {
	pool, _ := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	_, ctx := authTestLease(t, pool)
	handled := &accountWorkerInitError{
		err:         fmt.Errorf("%w url=https://accounts.google.com/", aistudio.ErrAuthenticationRequired),
		authHandled: true,
	}
	if err := refresher.Recover(ctx, handled); !errors.Is(err, handled) {
		t.Fatalf("应原样返回启动错误，实际 %v", err)
	}
	var sends atomic.Int32
	if _, err := refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
		sends.Add(1)
		return nil, fmt.Errorf("获取账户 WAA preparer: %w", handled)
	}); !errors.Is(err, aistudio.ErrAuthenticationRequired) {
		t.Fatalf("应保留登录失效原因，实际 %v", err)
	}
	if sends.Load() != 1 || counts.refreshes.Load() != 0 || counts.resets.Load() != 0 {
		t.Fatalf("已处理的启动失败不应再次恢复: 发送=%d 续签=%d 重置=%d", sends.Load(), counts.refreshes.Load(), counts.resets.Load())
	}
}

// authTestRPC 按调用次序返回预设结果的普通 RPC 传输
type authTestRPC struct {
	calls   atomic.Int32
	respond func(call int32) (*aistudio.RPCResponse, error)
}

func (transport *authTestRPC) Do(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return transport.respond(transport.calls.Add(1))
}

type authTestProtected struct{}

func (authTestProtected) DoProtected(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return nil, errors.New("测试不发送受保护请求")
}

// TestCountTokensForLeaseSkipsAuthRecovery 降级判定借用生成租约的计数遇到 401 时不续签、不重置运行时，也不改变账户状态
func TestCountTokensForLeaseSkipsAuthRecovery(t *testing.T) {
	pool, _ := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	rpc := &authTestRPC{respond: func(int32) (*aistudio.RPCResponse, error) {
		return unauthorizedResponse(loginExpiredBody), nil
	}}
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: &authRetryTransport{transport: rpc, refresher: refresher},
		Protected: &authRetryProtectedTransport{transport: authTestProtected{}, refresher: refresher},
	})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := authTestLease(t, pool)
	_, err = pooled.CountTokensForLease(context.Background(), lease, aistudio.TokenCountRequest{
		Model:    "gemini-test",
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	})
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusUnauthorized {
		t.Fatalf("计数应原样返回 401，实际 %v", err)
	}
	if rpc.calls.Load() != 1 || counts.refreshes.Load() != 0 || counts.resets.Load() != 0 {
		t.Fatalf("计数不应续签或重置运行时: 发送=%d 续签=%d 重置=%d", rpc.calls.Load(), counts.refreshes.Load(), counts.resets.Load())
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountReady {
		t.Fatalf("计数失败不应改变账户状态，实际 %s", status.State)
	}
}

// TestRefreshGivesUpWhenRequestCanceledWhileWaiting 等待同账户其他请求期间本次请求被取消时放弃续签，
// 不刷新 Cookie，也不重置其他请求正在使用的运行时
func TestRefreshGivesUpWhenRequestCanceledWhileWaiting(t *testing.T) {
	pool, account := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	_, leaseCtx := authTestLease(t, pool)
	authTestLease(t, pool)
	ctx, cancel := context.WithTimeout(leaseCtx, 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	if err := refresher.Refresh(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("请求取消时应返回取消原因，实际 %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("请求取消后仍在等待，耗时 %s", elapsed)
	}
	if counts.refreshes.Load() != 0 || counts.resets.Load() != 0 || authTestSAPISID(t, account) != "OLD" {
		t.Fatal("放弃续签时不应刷新 Cookie 或重置运行时")
	}
}

// TestAuthRetryRefreshesAfterWaitLimit 同账户另一个请求（长时间流式输出）超过等待上限仍未结束时，续签不再等待而是继续：
// 有续签材料的账户不能因为等不到其他请求就被标为需要登录（退出调度后不会再自动恢复），本次请求续签后重放成功
func TestAuthRetryRefreshesAfterWaitLimit(t *testing.T) {
	pool, account := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.waitLimit = 50 * time.Millisecond
	lease, ctx := authTestLease(t, pool)
	streaming, _ := authTestLease(t, pool)
	var sends atomic.Int32
	startedAt := time.Now()
	response, err := refresher.do(ctx, "ListModels", func() (*aistudio.RPCResponse, error) {
		if sends.Add(1) == 1 {
			return unauthorizedResponse(loginExpiredBody), nil
		}
		return &aistudio.RPCResponse{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("等待上限后应续签并重放成功，实际 response=%v err=%v", response, err)
	}
	if elapsed := time.Since(startedAt); elapsed < refresher.waitLimit {
		t.Fatalf("续签前应先等待同账户其他请求，实际只等了 %s", elapsed)
	}
	if sends.Load() != 2 || counts.refreshes.Load() != 1 || counts.resets.Load() != 1 || authTestSAPISID(t, account) != "NEW" {
		t.Fatalf("应续签一次并重放一次: 发送=%d 续签=%d 重置=%d", sends.Load(), counts.refreshes.Load(), counts.resets.Load())
	}
	if err := errors.Join(lease.Release(), streaming.Release()); err != nil {
		t.Fatal(err)
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountReady {
		t.Fatalf("有续签材料的账户不应因等待超时被标为需要登录，实际 %s（%s）", status.State, status.Message)
	}
}

// authTestManager 创建使用测试 Worker 启动函数的 Worker 管理器；launch 按调用次序返回结果
func authTestManager(
	t *testing.T,
	pool *aistudio.AccountPool,
	account *aistudio.Account,
	launch func(call int32) (*aistudio.NativeWorker, error),
) (*accountWorkerManager, *atomic.Int32) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager := newAccountWorkerManager(
		pool, []*aistudio.Account{account}, newRequestRegistry(ctx), "", "", time.Minute, 1, 1, 1, false,
	)
	t.Cleanup(func() { _ = manager.Close() })
	launches := &atomic.Int32{}
	manager.launch = func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		return launch(launches.Add(1))
	}
	return manager, launches
}

func loginRedirectError() error {
	return fmt.Errorf("%w url=https://accounts.google.com/ServiceLogin", aistudio.ErrAuthenticationRequired)
}

// TestOnDemandStartRecoversLoginWithLease 按需启动 Worker 遇到登录页跳转时在请求租约内续签一次并重新启动
func TestOnDemandStartRecoversLoginWithLease(t *testing.T) {
	pool, account := authTestPool(t, true)
	manager, launches := authTestManager(t, pool, account, func(call int32) (*aistudio.NativeWorker, error) {
		if call == 1 {
			return nil, loginRedirectError()
		}
		worker, _ := aistudio.NewStubWorker(authTestAccount, 0)
		return worker, nil
	})
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.reset = func(accountID string) error {
		counts.resets.Add(1)
		return manager.Reset(accountID)
	}
	manager.refresher = refresher
	service := &trackedService{pool: pool, workers: manager, requests: manager.requests}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: "gemini-test", Method: "generateContent"})
	if err != nil {
		t.Fatalf("续签后应启动成功，实际 %v", err)
	}
	if launches.Load() != 2 || counts.refreshes.Load() != 1 || counts.resets.Load() != 1 {
		t.Fatalf("应续签一次后重新启动: 启动=%d 续签=%d 重置=%d", launches.Load(), counts.refreshes.Load(), counts.resets.Load())
	}
	if got := authTestSAPISID(t, account); got != "NEW" {
		t.Fatalf("续签结果没有写回，SAPISID=%q", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountReady {
		t.Fatalf("恢复后账户应保持就绪，实际 %s", status.State)
	}
}

// TestStartupRecoveryWithRequestWaitingForOpening 同账户另一个请求正等待本次 Worker 启动结束（waitForOpening）时，
// 启动阶段的续签等待有上限、到期后继续续签并重新启动：两个请求都拿到 Worker，账户保持就绪，不互相等待
func TestStartupRecoveryWithRequestWaitingForOpening(t *testing.T) {
	pool, account := authTestPool(t, true)
	firstLaunch := make(chan struct{})
	releaseFirst := make(chan struct{})
	manager, launches := authTestManager(t, pool, account, func(call int32) (*aistudio.NativeWorker, error) {
		if call == 1 {
			close(firstLaunch)
			<-releaseFirst
			return nil, loginRedirectError()
		}
		worker, _ := aistudio.NewStubWorker(authTestAccount, 0)
		return worker, nil
	})
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.waitLimit = 50 * time.Millisecond
	refresher.reset = func(accountID string) error {
		counts.resets.Add(1)
		return manager.Reset(accountID)
	}
	manager.refresher = refresher
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, starterCtx := authTestLease(t, pool)
	_, waiterCtx := authTestLease(t, pool)
	starterCtx, starterCancel := context.WithTimeout(starterCtx, 10*time.Second)
	defer starterCancel()
	waiterCtx, waiterCancel := context.WithTimeout(waiterCtx, 10*time.Second)
	defer waiterCancel()
	starter := make(chan error, 1)
	go func() {
		_, err := manager.Worker(starterCtx, authTestAccount, "gemini-test")
		starter <- err
	}()
	select {
	case <-firstLaunch:
	case <-ctx.Done():
		t.Fatal("Worker 没有开始启动")
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := manager.Worker(waiterCtx, authTestAccount, "gemini-test")
		waiter <- err
	}()
	// 让第二个请求进入 waitForOpening 再结束第一次启动
	time.Sleep(20 * time.Millisecond)
	close(releaseFirst)
	for name, result := range map[string]chan error{"启动请求": starter, "等待请求": waiter} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s应拿到 Worker，实际 %v", name, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s互相等待没有结束", name)
		}
	}
	if launches.Load() != 2 || counts.refreshes.Load() != 1 || authTestSAPISID(t, account) != "NEW" {
		t.Fatalf("应续签一次后重新启动: 启动=%d 续签=%d", launches.Load(), counts.refreshes.Load())
	}
	if status := authTestStatus(t, pool); status.State == aistudio.AccountAuthRequired {
		t.Fatalf("有续签材料的账户不应因等待超时被标为需要登录（%s）", status.Message)
	}
}

// TestOnDemandStartLoginFailureReturnsNotReady 按需启动确认登录失效且无法续签时，账户标为需要登录，
// 请求按账户需要重新登录返回（503 列出原因），而不是 502
func TestOnDemandStartLoginFailureReturnsNotReady(t *testing.T) {
	pool, account := authTestPool(t, false)
	manager, launches := authTestManager(t, pool, account, func(int32) (*aistudio.NativeWorker, error) {
		return nil, loginRedirectError()
	})
	counts := &authTestCounts{}
	manager.refresher = newAuthTestRefresher(t, counts)
	service := &trackedService{pool: pool, workers: manager, requests: manager.requests}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: "gemini-test", Method: "generateContent"})
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) || !errors.Is(err, aistudio.ErrAuthenticationRequired) {
		t.Fatalf("应返回账户需要重新登录并保留原因，实际 %v", err)
	}
	if launches.Load() != 1 || counts.refreshes.Load() != 0 {
		t.Fatalf("没有续签材料时不应续签或重复启动: 启动=%d 续签=%d", launches.Load(), counts.refreshes.Load())
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("应标为需要登录，实际 %s", status.State)
	}
}

// TestPrewarmStartMarksLoginFailureWithoutLease 预热没有请求租约：不另取租约（LastUsed 不变），
// 没有续签材料的账户直接标为需要登录；有续签材料的账户保持就绪，留给按需启动恢复
func TestPrewarmStartMarksLoginFailureWithoutLease(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(fmt.Sprintf("oauth=%v", oauth), func(t *testing.T) {
			pool, account := authTestPool(t, oauth)
			manager, _ := authTestManager(t, pool, account, func(int32) (*aistudio.NativeWorker, error) {
				return nil, loginRedirectError()
			})
			counts := &authTestCounts{}
			manager.refresher = newAuthTestRefresher(t, counts)
			_, err := manager.ensureWorker(context.Background(), authTestAccount, "", false)
			if !errors.Is(err, aistudio.ErrAuthenticationRequired) {
				t.Fatalf("应返回登录失效，实际 %v", err)
			}
			status := authTestStatus(t, pool)
			if status.LastUsed != nil || counts.refreshes.Load() != 0 {
				t.Fatalf("预热不应取租约或续签: LastUsed=%v 续签=%d", status.LastUsed, counts.refreshes.Load())
			}
			want := aistudio.AccountAuthRequired
			if oauth {
				want = aistudio.AccountReady
			}
			if status.State != want {
				t.Fatalf("账户状态 = %s，期望 %s", status.State, want)
			}
		})
	}
}

// TestReloadCredentialsRequeuesWhileLeased 需要登录的账户仍有未结束的请求时，载入新认证文件等不到账户空闲就留到下一轮
func TestReloadCredentialsRequeuesWhileLeased(t *testing.T) {
	previous := credentialReloadAcquireWait
	credentialReloadAcquireWait = 50 * time.Millisecond
	t.Cleanup(func() { credentialReloadAcquireWait = previous })
	pool, account := authTestPool(t, false)
	lease, _ := authTestLease(t, pool)
	if err := lease.MarkAuthenticationRequired("HTTP 401"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := &runtimeAdmin{pool: pool, requests: newRequestRegistry(ctx)}
	summary := pool.LoginSummaries()[0]
	if summary.State != aistudio.AccountAuthRequired {
		t.Fatalf("轻量状态应为需要登录，实际 %s", summary.State)
	}
	if admin.applyLoginUpdate(ctx, summary) {
		t.Fatal("账户仍有请求未结束时应返回 false，留到下一轮再试")
	}
	if authTestStatus(t, pool).State != aistudio.AccountAuthRequired || authTestSAPISID(t, account) != "OLD" {
		t.Fatal("未取得账户时不应改变账户状态或认证文件")
	}
}
