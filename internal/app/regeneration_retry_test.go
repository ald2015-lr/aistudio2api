package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// delayedBody 第一次读取前等待 delay（上游先返回响应头，过一会儿才出流帧），之后读出 data
type delayedBody struct {
	delay  time.Duration
	data   *strings.Reader
	waited bool
}

func (body *delayedBody) Read(buffer []byte) (int, error) {
	if !body.waited {
		body.waited = true
		time.Sleep(body.delay)
	}
	return body.data.Read(buffer)
}

func (body *delayedBody) Close() error { return nil }

// scriptedTransport 为 app 层重新生成测试的上游：按账号的第几次发送返回 respond 给出的响应，记下每次发送的请求体；
// 与 WorkerProtectedTransport 一样每次发送给租约记一次（每分钟名额按实际发送计）
type scriptedTransport struct {
	mu      sync.Mutex
	respond func(accountID string, call int, body string) *aistudio.RPCResponse
	bodies  map[string][]string
	order   []string
}

func newScriptedTransport(respond func(accountID string, call int, body string) *aistudio.RPCResponse) *scriptedTransport {
	return &scriptedTransport{respond: respond, bodies: make(map[string][]string)}
}

func (transport *scriptedTransport) DoProtected(ctx context.Context, request aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		lease.RecordGenerationSent()
	}
	transport.mu.Lock()
	transport.bodies[request.AccountID] = append(transport.bodies[request.AccountID], string(rpc.Body))
	transport.order = append(transport.order, request.AccountID)
	call := len(transport.bodies[request.AccountID])
	transport.mu.Unlock()
	return transport.respond(request.AccountID, call, string(rpc.Body)), nil
}

// sent 返回各账号收到的请求体（按发送顺序）与账号的发送顺序
func (transport *scriptedTransport) sent() (map[string][]string, []string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	bodies := make(map[string][]string, len(transport.bodies))
	for accountID, list := range transport.bodies {
		bodies[accountID] = append([]string(nil), list...)
	}
	return bodies, append([]string(nil), transport.order...)
}

func playgroundResponse(status int, body io.ReadCloser) *aistudio.RPCResponse {
	return &aistudio.RPCResponse{StatusCode: status, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}}, Body: body}
}

// regenerationTestService 返回 app 层重新生成测试的服务：accounts 个账号，Client 的模型目录为 models，上游为 transport
func regenerationTestService(t *testing.T, transport *scriptedTransport, accounts []string, models ...string) *trackedService {
	t.Helper()
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: &catalogRPCTransport{}, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogTestData(t, models...)
	for _, accountID := range accounts {
		client.PrimeAccountCatalog(accountID, aistudio.BenefitTierFree, catalog)
	}
	service, pool := primaryBusyService(t, client, accounts...)
	for _, accountID := range accounts {
		poolModels := make([]aistudio.Model, 0, len(models))
		for _, model := range models {
			poolModels = append(poolModels, aistudio.Model{
				ID: model, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
			})
		}
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, poolModels); err != nil {
			t.Fatal(err)
		}
	}
	return service
}

// runRegenerationRequest 执行一次生成，返回收到的正文、结束原因与错误
func runRegenerationRequest(t *testing.T, service *trackedService, request aistudio.GenerateRequest, diag *generationDiagnostics) (string, string, error) {
	t.Helper()
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, diag)
	text, finish := "", ""
	var err error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventFinish:
			finish = event.FinishReason
		case aistudio.EventError:
			err = event.Err
		}
	}
	return text, finish, err
}

const (
	regenTestMalformed = `[[[[[null,10,null,"Malformed function call: f(=)"]]]]]`
	regenTestText      = `[[[[[[[[null,"hi"]],"model"]]]],[[[null,1]]]]]`
)

// TestRegenerationVariantAndFirstEventTimeout 因函数调用格式错误重新生成：换了随机后缀（第二次的请求体与第一次后缀不同，原来的后缀不再出现），
// 并且首事件计时按每次发送重新起算。FIRST_EVENT_TIMEOUT=1.5s，上游每次 800ms 后才出流帧：第一次以函数调用格式错误结束，重新生成那次返回正文。
// 原先首事件计时从第一次发送算起、重新生成时不重置，第 1.5 秒取消尝试、记成首事件超时并换号，换号后又是同样两次，最后对外 504、
// 共 4 次上游发送；现在同一账号 2 次发送即交出正文
func TestRegenerationVariantAndFirstEventTimeout(t *testing.T) {
	accounts := switchTestIDs(2)
	transport := newScriptedTransport(func(_ string, call int, _ string) *aistudio.RPCResponse {
		body := regenTestText
		if call == 1 {
			body = regenTestMalformed
		}
		return playgroundResponse(http.StatusOK, &delayedBody{delay: 800 * time.Millisecond, data: strings.NewReader(body)})
	})
	service := regenerationTestService(t, transport, accounts, testRuntimeModel)
	service.firstEventTimeout.Store(int64(1500 * time.Millisecond))
	request := aistudio.GenerateRequest{
		ID: "req-regen-variant", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "查一下天气"}}}},
	}
	diag := newGenerationDiagnostics(request, "未发送")
	service.repeatNonce.Store(true)
	if !service.maybeAddPromptNonce(&request, diag, false) || diag.promptNonce == "" {
		t.Fatal("应加上随机后缀")
	}
	text, finish, err := runRegenerationRequest(t, service, request, diag)
	if err != nil || text != "hi" || finish != "stop" {
		t.Fatalf("重新生成后应交出正文：text=%q finish=%q err=%v", text, finish, err)
	}
	bodies, order := transport.sent()
	if len(order) != 2 || order[0] != order[1] {
		t.Fatalf("应在同一账号发送 2 次（第一次 + 重新生成 1 次），实际发送顺序 %v", order)
	}
	first, second := bodies[order[0]][0], bodies[order[0]][1]
	if !strings.Contains(first, diag.promptNonce) || strings.Contains(second, diag.promptNonce) || first == second {
		t.Fatal("重新生成的请求体应换一个随机后缀，原来的后缀不再出现")
	}
	if !strings.Contains(second, "查一下天气") {
		t.Fatal("重新生成只换后缀，提示词照旧")
	}
	expectProgress(t, service, "同账号重新生成 | 第 1/2 次 | 结束原因=malformed_function_call", "输入=已换随机后缀")
	if progressLogged(service, "上游首事件超时") {
		t.Fatal("上游每次都按时返回，重新生成不应被记成首事件超时")
	}
}

// TestRegenerationSkippedWithoutNonce 没加随机后缀的请求（这里是 REPEAT_PROMPT_NONCE 关闭）以函数调用格式错误结束时不在同账号原样重发，
// 交出这次的结果并写一行说明
func TestRegenerationSkippedWithoutNonce(t *testing.T) {
	accounts := switchTestIDs(1)
	transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
		return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(regenTestMalformed)))
	})
	service := regenerationTestService(t, transport, accounts, testRuntimeModel)
	request := aistudio.GenerateRequest{
		ID: "req-regen-no-nonce", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	diag := newGenerationDiagnostics(request, "未发送")
	if service.maybeAddPromptNonce(&request, diag, false) {
		t.Fatal("REPEAT_PROMPT_NONCE 关闭时不应加后缀")
	}
	_, finish, err := runRegenerationRequest(t, service, request, diag)
	if err != nil || finish != "malformed_function_call" {
		t.Fatalf("应交出这次的结果：finish=%q err=%v", finish, err)
	}
	if _, order := transport.sent(); len(order) != 1 {
		t.Fatalf("没加随机后缀时不应原样重发，发送了 %d 次", len(order))
	}
	expectProgress(t, service, "原样重发多半得到同样的结果，不因结束原因重新生成 | 结束原因=malformed_function_call")
}

// TestPrefillRetryOnAnotherAccount 原生续写被上游拒绝、本账号每分钟名额已满（ACCOUNT_RPM_LIMIT=1）不能马上重发时，不把 400 交给客户端：
// 换到第二个账号，下一次尝试补用户消息成功。第一个账号不冷却，换号不写“账号切换”，进度写明不准入的原因
func TestPrefillRetryOnAnotherAccount(t *testing.T) {
	model := "gemma-prefill-retry-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	accounts := switchTestIDs(2)
	transport := newScriptedTransport(func(_ string, _ int, body string) *aistudio.RPCResponse {
		if !strings.Contains(body, "Continue exactly") {
			return playgroundResponse(http.StatusBadRequest, io.NopCloser(strings.NewReader(`[3,"Requests ending with a model turn are not supported."]`)))
		}
		return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(regenTestText)))
	})
	service := regenerationTestService(t, transport, accounts, testRuntimeModel, model)
	service.pool.SetAccountRateLimit(1)
	request := aistudio.GenerateRequest{
		ID: "req-prefill-retry", Model: model, PrefillTail: true,
		Contents: []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "写一首诗"}}},
			{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "春眠不觉晓，"}}},
		},
	}
	text, finish, err := runRegenerationRequest(t, service, request, newGenerationDiagnostics(request, "未发送"))
	if err != nil || text != "hi" || finish != "stop" {
		t.Fatalf("换号后补用户消息应成功：text=%q finish=%q err=%v", text, finish, err)
	}
	bodies, order := transport.sent()
	if len(order) != 2 || order[0] == order[1] {
		t.Fatalf("应在第一个账号原样发送 1 次、换号后第二个账号补用户消息 1 次，实际发送顺序 %v", order)
	}
	if strings.Contains(bodies[order[0]][0], "Continue exactly") || !strings.Contains(bodies[order[1]][0], "Continue exactly") {
		t.Fatal("第一次应原样以模型轮结尾发送，换号后的那次补用户消息")
	}
	if status, _ := service.pool.StatusOf(order[0]); len(status.Cooldowns) != 0 {
		t.Fatalf("续写被拒与账号额度无关，不应冷却：%+v", status.Cooldowns)
	}
	expectProgress(t, service, "上游不接受以模型轮结尾的请求，但每分钟名额 1/1（已发出 1、预占 0，ACCOUNT_RPM_LIMIT），不在本次尝试重发",
		"上游不接受以模型轮结尾的请求，本账号不能马上重发（每分钟名额 1/1", "换号再试，下一次补用户消息 | 第 1/2 次")
	if hasLogContaining(service.requests, "账号切换") {
		t.Fatal("续写被拒后的再试与账号无关，不应写账号切换")
	}
	// 上游的 400 本身不换号；包成 PrefillRetryError 后可换号（请求已取消时不换）
	rejected := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Requests ending with a model turn are not supported."}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableGenerateAccountError(context.Background(), rejected) ||
		!retryableGenerateAccountError(context.Background(), &aistudio.PrefillRetryError{Err: rejected, Denied: "每分钟名额 1/1"}) ||
		retryableGenerateAccountError(canceled, &aistudio.PrefillRetryError{Err: rejected}) {
		t.Fatal("PrefillRetryError 应可换号，上游的 400 本身不换号")
	}
}

// TestAuthRequiredFlushesDeferredCookies 账号被标为需要登录时先写出暂存的轮换 Cookie 再记认证文件的修改时间：原先暂存稍后写盘（这里是
// 停止服务时写出）会改掉修改时间，下次启动按“认证文件改过”清掉记录，失效账户又排到预热前面
func TestAuthRequiredFlushesDeferredCookies(t *testing.T) {
	ctx := context.Background()
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	const accountID = "pro@example.com"
	catalogTestAccounts(t, root, accountID)
	requests := newRequestRegistry(ctx)
	first := newCatalogGeneration(t, root, &catalogRPCTransport{listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7)), tier: "[1]"})
	if _, err := first.pooled.RefreshAccountModels(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := first.pooled.CatalogSnapshot(accountID)
	if !ok {
		t.Fatal("缺少目录快照")
	}
	cache := openCatalogCache(dir, requests)
	cache.put(accountID, snapshot, time.Now())
	lease, err := first.pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Account().StoragePath
	// 第一次带上轮换 Cookie（其他 Cookie 有变化，立即写盘），第二次只有轮换 Cookie 变化、刚写过盘：暂存
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s1")); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s2")); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	if got := diskSIDCC(t, path); got != "s1" {
		t.Fatalf("第二次应暂存，磁盘 SIDCC=%q", got)
	}
	syncer := newCatalogSyncer(&trackedService{lifecycle: ctx, pool: first.pool, requests: requests})
	syncer.cache = cache
	syncer.noteAuthRequired(accountID, time.Now())
	if got := diskSIDCC(t, path); got != "s2" {
		t.Fatalf("记需要登录之前应写出暂存的 Cookie，磁盘 SIDCC=%q", got)
	}
	// 停止服务：写出全部暂存（这里已经没有了），修改时间不再改变
	if err := first.pool.SetStorageDeferral(false); err != nil {
		t.Fatal(err)
	}
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	second := newCatalogGeneration(t, root, nil)
	summary := openCatalogCache(dir, requests).restore(second.pool, second.client, true)
	if _, suspect := summary.Suspects[accountID]; !suspect {
		t.Fatal("认证文件在记需要登录之后没有变过，重启后应列为疑似失效")
	}
}

// TestReservedWorkerStartFlushesDeferredCookies Worker 冷启动前先写出暂存的轮换 Cookie：浏览器与纯 Go runtime 从认证文件读 Cookie，
// 启动时读到的是最新的 SIDCC，与每次都写盘时相同
func TestReservedWorkerStartFlushesDeferredCookies(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: partitionNormalA})
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s1")); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookieSet("s2")); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	if got := diskSIDCC(t, path); got != "s1" {
		t.Fatalf("第二次应暂存，磁盘 SIDCC=%q", got)
	}
	seen := ""
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		seen = diskSIDCC(t, path)
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if seen != "s2" {
		t.Fatalf("Worker 启动时从认证文件读到的 SIDCC=%q，期望最新的 s2", seen)
	}
}

// TestFirstEventDeadlineRestart 首事件计时的重新起算：关闭时总是真；起算后到期前重新起算，从这一刻再给一个完整的超时；已停止或已到期时返回假
func TestFirstEventDeadlineRestart(t *testing.T) {
	if !newFirstEventDeadline(0, func(error) {}).RestartFirstEvent() {
		t.Fatal("关闭时重新起算应返回真")
	}
	fired := make(chan error, 1)
	deadline := newFirstEventDeadline(800*time.Millisecond, func(cause error) { fired <- cause })
	if deadline.FirstEventWindow() != 800*time.Millisecond {
		t.Fatalf("FirstEventWindow = %s", deadline.FirstEventWindow())
	}
	deadline.start()
	time.Sleep(400 * time.Millisecond)
	if !deadline.RestartFirstEvent() {
		t.Fatal("到期前重新起算应返回真")
	}
	select {
	case <-fired:
		t.Fatal("重新起算后不应按第一次起算的时刻到期")
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case cause := <-fired:
		var timeout *firstEventTimeoutError
		if !errors.As(cause, &timeout) {
			t.Fatalf("到期原因 = %v", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重新起算后应在一个完整的超时后到期")
	}
	if deadline.RestartFirstEvent() || deadline.stop() == nil {
		t.Fatal("到期后重新起算应返回假，stop 应交出超时错误")
	}
	stopped := newFirstEventDeadline(time.Minute, func(error) {})
	stopped.start()
	_ = stopped.stop()
	if stopped.RestartFirstEvent() {
		t.Fatal("已停止时重新起算应返回假")
	}
}

// TestPromptNonceVariant 换随机后缀：每次换成新的后缀（连续两次也不同），预填充补上的用户消息在后时换的是原来带后缀的那条，
// 末尾是链接时单独成 part 的后缀同样能换；不修改调用方的数据；没加后缀的请求不挂换输入函数
func TestPromptNonceVariant(t *testing.T) {
	if promptNonceVariant("") != nil {
		t.Fatal("没加随机后缀时不应有换输入函数")
	}
	for _, text := range []string{"写一首诗", "看看 https://example.com/page"} {
		nonce := newPromptNonce()
		request, ok := applyPromptNonce(aistudio.GenerateRequest{Contents: []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}},
			{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "春眠不觉晓，"}}},
		}}, nonce)
		if !ok {
			t.Fatal("应加上随机后缀")
		}
		request.Contents = aistudio.AppendPrefillContinuation(request.Contents)
		original := request.Contents[0].Parts[len(request.Contents[0].Parts)-1].Text
		variant := promptNonceVariant(nonce)
		first, ok := variant(request)
		if !ok {
			t.Fatalf("%q：应换上新的后缀", text)
		}
		second, ok := variant(first)
		if !ok {
			t.Fatalf("%q：第二次也应能换", text)
		}
		firstText := first.Contents[0].Parts[len(first.Contents[0].Parts)-1].Text
		secondText := second.Contents[0].Parts[len(second.Contents[0].Parts)-1].Text
		if strings.HasSuffix(firstText, nonce) || firstText == secondText || !strings.Contains(firstText, strings.TrimSuffix(original, nonce)) {
			t.Fatalf("%q：换后缀的结果不对", text)
		}
		if request.Contents[0].Parts[len(request.Contents[0].Parts)-1].Text != original ||
			first.Contents[2].Parts[0].Text != aistudio.PrefillContinuationPrompt {
			t.Fatalf("%q：不应修改调用方的数据，补上的续写消息照旧", text)
		}
	}
	// 请求里找不到原来的后缀（例如被别处改写过）时换不了
	if _, ok := promptNonceVariant(newPromptNonce())(aistudio.GenerateRequest{Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}},
	}}); ok {
		t.Fatal("找不到原来的后缀时应返回假")
	}
}
