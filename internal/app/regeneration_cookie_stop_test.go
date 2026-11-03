package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// rotatingTestCookieSet 返回登录 Cookie 加上值为 sidcc 的三个轮换 Cookie（Google 几乎每次响应都轮换）
func rotatingTestCookieSet(sidcc string) []aistudio.StateCookie {
	expires := float64(time.Now().Add(time.Hour).Unix())
	cookies := make([]aistudio.StateCookie, 0, 6)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"})
	}
	for _, name := range []string{"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC"} {
		cookies = append(cookies, aistudio.StateCookie{Name: name, Value: sidcc, Domain: ".google.com", Path: "/", Expires: expires, Secure: true, SameSite: "None"})
	}
	return cookies
}

func diskSIDCC(t *testing.T, path string) string {
	t.Helper()
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := state.CookieValue("SIDCC", "https://aistudio.google.com/", time.Now())
	return value
}

// TestDeferredCookiesWrittenOnStop 停止生成服务时（Worker 都已重置）把暂存未写盘的轮换 Cookie 立即写出，之后直接写盘：
// 停止再启动时新一代生成服务先建好、旧一代最后才关闭，新一代从磁盘载入认证文件，读到的是最新的 SIDCC
func TestDeferredCookiesWrittenOnStop(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: partitionNormalA})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s1")); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s2")); err != nil {
		t.Fatal(err)
	}
	if got := diskSIDCC(t, path); got != "s1" {
		t.Fatalf("30 秒内只有轮换 Cookie 变化时应暂存，磁盘 SIDCC=%q", got)
	}
	modelRefreshDone := make(chan struct{})
	close(modelRefreshDone)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	if got := diskSIDCC(t, path); got != "s2" {
		t.Fatalf("停止服务后新一代应从磁盘读到最新的 SIDCC，得到 %q", got)
	}
	// 停止之后旧账户池直接写盘，不再留下计时写盘
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s3")); err != nil {
		t.Fatal(err)
	}
	if got := diskSIDCC(t, path); got != "s3" {
		t.Fatalf("停止之后应直接写盘，SIDCC=%q", got)
	}
}

// regenBudgetTransport 为重新生成次数接线测试的上游：第一个发来请求的账号（first）每次都以流尾 503 结束，其余账号每次都以函数调用格式错误结束
type regenBudgetTransport struct {
	mu    sync.Mutex
	first string
	calls map[string]int
}

func (transport *regenBudgetTransport) DoProtected(_ context.Context, request aistudio.GenerateRequest, _ aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	transport.mu.Lock()
	transport.calls[request.AccountID]++
	if transport.first == "" {
		transport.first = request.AccountID
	}
	first := transport.first
	transport.mu.Unlock()
	body := `[[[[[null,10,null,"Malformed function call: f(=)"]]]]]`
	if request.AccountID == first {
		body = `[[],[14,"The service is currently unavailable."]]`
	}
	return &aistudio.RPCResponse{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

// counts 返回第一个账号与其余账号的发送次数
func (transport *regenBudgetTransport) counts() (int, int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	first, others := 0, 0
	for accountID, calls := range transport.calls {
		if accountID == transport.first {
			first += calls
		} else {
			others += calls
		}
	}
	return first, others
}

// TestRegenerationBudgetSharedAcrossSwitch app 层每个请求建一个重新生成次数挂到各次尝试上：第一个账号流尾 503，同账号重新生成 2 次后
// 仍是 503，按 5xx 换号；第二个账号以函数调用格式错误结束，次数已用完，不再各来一轮。总发送次数为 1+2+1
func TestRegenerationBudgetSharedAcrossSwitch(t *testing.T) {
	accounts := switchTestIDs(2)
	transport := &regenBudgetTransport{calls: make(map[string]int)}
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: &catalogRPCTransport{}, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogTestData(t, testRuntimeModel)
	for _, accountID := range accounts {
		client.PrimeAccountCatalog(accountID, aistudio.BenefitTierFree, catalog)
	}
	service, _ := primaryBusyService(t, client, accounts...)
	request := aistudio.GenerateRequest{
		ID: "req-regen-budget", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	// 与 Generate 入口相同加上随机后缀：因结束原因重新生成要换后缀，没加后缀的请求不因结束原因重新生成
	diag := newGenerationDiagnostics(request, "未发送")
	service.repeatNonce.Store(true)
	if !service.maybeAddPromptNonce(&request, diag, false) {
		t.Fatal("应加上随机后缀")
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, diag)
	finish := ""
	for event := range events {
		if event.Kind == aistudio.EventFinish {
			finish = event.FinishReason
		}
	}
	first, second := transport.counts()
	if first != 3 || second != 1 {
		t.Fatalf("第一个账号应发送 3 次（1 次 + 重新生成 2 次）、换号后第二个账号只发送 1 次，实际 %d 与 %d", first, second)
	}
	if finish != "malformed_function_call" {
		t.Fatalf("次数用完后应交出第二个账号的结束原因，得到 %q", finish)
	}
	expectProgress(t, service, "同账号重新生成 | 第 1/2 次 | 流尾错误=HTTP 503", "同账号重新生成 | 第 2/2 次 | 流尾错误=HTTP 503",
		"重新生成次数已用完（2 次），按上游结果结束 | 结束原因=malformed_function_call")
}
