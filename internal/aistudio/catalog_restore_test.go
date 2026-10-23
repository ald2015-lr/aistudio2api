package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogTestRow 返回 ListModels 的一行：ID、显示名、generateContent 与聊天能力（可作为 WAA 初始化模型），可选温度默认值
func catalogTestRow(id string, temperature float64) []any {
	row := make([]any, 65)
	row[0] = "models/" + id
	row[3] = strings.ToUpper(id)
	row[6] = 8192
	row[7] = []string{"generateContent"}
	if temperature > 0 {
		row[8] = temperature
	}
	row[64] = []int{1}
	return row
}

// catalogTestRaw 返回 ListModels 的响应正文
func catalogTestRaw(t *testing.T, rows ...[]any) string {
	t.Helper()
	if rows == nil {
		rows = [][]any{}
	}
	data, err := json.Marshal([]any{rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// catalogTestTransport 为目录测试的假上游：ListModels 依次返回 listModels（最后一个重复），GetAiStudioBenefitTier 返回 tier，
// 其他方法返回 [5]；记录各方法的调用次数与最后一次请求头
type catalogTestTransport struct {
	mu         sync.Mutex
	listModels []string
	tier       string
	calls      map[string]int
	headers    map[string]http.Header
}

func newCatalogTestTransport(listModels ...string) *catalogTestTransport {
	return &catalogTestTransport{
		listModels: listModels, tier: "[]", calls: make(map[string]int), headers: make(map[string]http.Header),
	}
}

func (transport *catalogTestTransport) Do(_ context.Context, rpc RPCRequest) (*RPCResponse, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.calls[rpc.Method]++
	transport.headers[rpc.Method] = rpc.Header.Clone()
	body := "[5]"
	switch rpc.Method {
	case "ListModels":
		body = transport.listModels[min(transport.calls[rpc.Method]-1, len(transport.listModels)-1)]
	case "GetAiStudioBenefitTier":
		body = transport.tier
	}
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (transport *catalogTestTransport) count(method string) int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.calls[method]
}

func (transport *catalogTestTransport) setListModels(bodies ...string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.listModels = bodies
	transport.calls["ListModels"] = 0
}

func catalogTestClient(t *testing.T, transport RPCTransport) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{
		Transport: transport,
		Protected: ProtectedTransportFunc(func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
			return nil, errors.New("unused")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// catalogTestPool 创建有若干账户的账户池（账户都没有目录）
func catalogTestPool(t *testing.T, ids ...string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	for _, id := range ids {
		account, publish, err := store.Create(DefaultAccountConfig(id), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// TestParseModelCatalogRawRoundTrip 原文保留为稀疏数组展开后的 JSON，重新解析得到相同的模型与默认值；
// 内容相同、字节不同（稀疏写法、空白）的目录内容键相同，内容不同时不同
func TestParseModelCatalogRawRoundTrip(t *testing.T) {
	raw := catalogTestRaw(t, catalogTestRow("alpha", 0.7), catalogTestRow("beta", 0))
	parsed, err := parseModelCatalog(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	data, err := ParseModelCatalogRaw(parsed.raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data.Raw(), parsed.raw) || data.Len() != 2 {
		t.Fatalf("原文或模型数不符：len=%d", data.Len())
	}
	again, err := ParseModelCatalogRaw(data.Raw())
	if err != nil {
		t.Fatal(err)
	}
	if again.ContentKey() != data.ContentKey() || len(data.ContentKey()) != 32 {
		t.Fatalf("重新解析的内容键 %q 与原来的 %q 不同", again.ContentKey(), data.ContentKey())
	}
	if entry := again.catalog.entries["alpha"]; entry.defaults.Temperature == nil || *entry.defaults.Temperature != 0.7 {
		t.Fatalf("重新解析后默认值丢失: %+v", entry.defaults)
	}
	// 稀疏写法与多余空白：字节不同、内容相同
	sparse := strings.ReplaceAll(strings.ReplaceAll(raw, ",null", ","), "[null", "[ ")
	if sparse == raw {
		t.Fatal("稀疏写法没有生效")
	}
	sparseData, err := ParseModelCatalogRaw([]byte(sparse))
	if err != nil {
		t.Fatal(err)
	}
	if sparseData.ContentKey() != data.ContentKey() {
		t.Fatal("内容相同的目录内容键应相同")
	}
	other, err := ParseModelCatalogRaw([]byte(catalogTestRaw(t, catalogTestRow("alpha", 0.5), catalogTestRow("beta", 0))))
	if err != nil {
		t.Fatal(err)
	}
	if other.ContentKey() == data.ContentKey() {
		t.Fatal("默认值不同的目录内容键应不同")
	}
	if BuildCatalogKey(data.Models()) == data.ContentKey() {
		t.Fatal("Build 与 Playground 的内容键不应相同")
	}
}

// TestRestoreCatalogsOneLock 一次恢复多个账户：模型、权益、initializedAt、候选可用；eligibilityVersion 只加一次，
// 已经同步过目录的账户不覆盖，号池里没有的账户列入 Missing
func TestRestoreCatalogsOneLock(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	pool := catalogTestPool(t, append(ids, "synced@example.com")...)
	synced := []Model{{ID: "synced-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	if err := pool.SetCatalog("synced@example.com", BenefitTierFree, synced); err != nil {
		t.Fatal(err)
	}
	data, err := ParseModelCatalogRaw([]byte(catalogTestRaw(t, catalogTestRow("alpha", 0))))
	if err != nil {
		t.Fatal(err)
	}
	models := data.Models()
	build := []Model{{ID: "build-only", Methods: []string{"generateContent"}}}
	items := make([]CatalogRestore, 0, len(ids)+2)
	for _, id := range append(ids, "synced@example.com", "gone@example.com") {
		items = append(items, CatalogRestore{AccountID: id, Tier: BenefitTierFree, Playground: models, Build: build, BuildSynced: true, EmptyCount: 1, EmptySince: time.Unix(100, 0)})
	}
	pool.mu.Lock()
	before := pool.eligibilityVersion
	pool.mu.Unlock()
	result := pool.RestoreCatalogs(items)
	pool.mu.Lock()
	after := pool.eligibilityVersion
	account := pool.byID["a@example.com"]
	initialized := !account.initializedAt.IsZero()
	emptyCount := account.emptyCatalogs
	syncedModels := pool.byID["synced@example.com"].Models
	pool.mu.Unlock()
	if after != before+1 {
		t.Fatalf("eligibilityVersion %d → %d，期望只加一次", before, after)
	}
	if len(result.Restored) != len(ids) || len(result.Missing) != 1 || result.Missing[0] != "gone@example.com" {
		t.Fatalf("恢复结果 %+v", result)
	}
	if !initialized || emptyCount != 1 {
		t.Fatal("恢复后 initializedAt 应为恢复时刻，连续空结果应带回")
	}
	if len(syncedModels) != 1 || syncedModels[0].ID != "synced-model" {
		t.Fatal("已经同步过目录的账户不应被缓存覆盖")
	}
	if build, ok := pool.BuildCatalogOf("b@example.com"); !ok || len(build) != 1 {
		t.Fatal("Build 目录应恢复")
	}
	status, _ := pool.StatusOf("c@example.com")
	if len(status.Models) != 1 || status.Models[0] != "alpha" || status.State != AccountReady {
		t.Fatalf("恢复后的状态 %+v", status)
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{ModelID: "alpha", Method: "generateContent"})
	if err != nil {
		t.Fatalf("恢复的账户应是候选: %v", err)
	}
	_ = lease.Release()
}

// TestRestoreCatalogsTierMismatchSkipped 缓存权益与运行态不一致时不恢复（权益决定号池与权益头），按缺失同步
func TestRestoreCatalogsTierMismatchSkipped(t *testing.T) {
	pool := catalogTestPool(t, "a@example.com")
	result := pool.RestoreCatalogs([]CatalogRestore{{
		AccountID: "a@example.com", Tier: BenefitTierPro, Playground: []Model{{ID: "alpha"}},
	}})
	if len(result.Restored) != 0 || len(result.TierMismatch) != 1 {
		t.Fatalf("恢复结果 %+v", result)
	}
	if status, _ := pool.StatusOf("a@example.com"); len(status.Models) != 0 {
		t.Fatal("权益不符时不应恢复目录")
	}
}

// TestRestoreCatalogsFingerprintMismatchStale 指纹与运行态记录不一致时仍恢复、标为 Stale；一致时不标
func TestRestoreCatalogsFingerprintMismatchStale(t *testing.T) {
	pool := catalogTestPool(t, "same@example.com", "stale@example.com")
	models := []Model{{ID: "alpha", Methods: []string{"generateContent"}}}
	fingerprint, err := accountCatalogFingerprint(BenefitTierFree, models)
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.byID["same@example.com"].runtime.CatalogFingerprint = fingerprint
	pool.byID["stale@example.com"].runtime.CatalogFingerprint = "older"
	pool.mu.Unlock()
	result := pool.RestoreCatalogs([]CatalogRestore{
		{AccountID: "same@example.com", Playground: models},
		{AccountID: "stale@example.com", Playground: models},
	})
	if len(result.Restored) != 2 || len(result.Stale) != 1 || result.Stale[0] != "stale@example.com" {
		t.Fatalf("恢复结果 %+v", result)
	}
}

// TestPrimeAccountCatalogTierHeader Pro 账户预置后，带权益的 RPC 带 X-AIStudio-G1-Tier；查模型不再请求 ListModels
func TestPrimeAccountCatalogTierHeader(t *testing.T) {
	const accountID = "pro@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	client := catalogTestClient(t, transport)
	data, err := ParseModelCatalogRaw([]byte(catalogTestRaw(t, catalogTestRow("alpha", 0.3))))
	if err != nil {
		t.Fatal(err)
	}
	client.PrimeAccountCatalog(accountID, BenefitTierPro, data)
	response, err := client.do(context.Background(), "CountTokens", accountID, "", []byte("[]"), false)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	transport.mu.Lock()
	header := transport.headers["CountTokens"].Get("X-AIStudio-G1-Tier")
	transport.mu.Unlock()
	if header != BenefitTierPro.HeaderValue() {
		t.Fatalf("权益头 = %q，期望 %q", header, BenefitTierPro.HeaderValue())
	}
	entry, err := client.modelEntry(context.Background(), accountID, "models/alpha")
	if err != nil || entry.defaults.Temperature == nil || *entry.defaults.Temperature != 0.3 {
		t.Fatalf("预置目录的模型 entry=%+v err=%v", entry, err)
	}
	if calls := transport.count("ListModels"); calls != 0 {
		t.Fatalf("预置后不应再请求 ListModels，实际 %d 次", calls)
	}
	snapshot, tier, ok := client.AccountCatalog(accountID)
	if !ok || tier != BenefitTierPro || snapshot.ContentKey() != data.ContentKey() {
		t.Fatal("AccountCatalog 应返回预置的目录与权益")
	}
	// 同一份目录预置给多个账户时共用一份
	client.PrimeAccountCatalog("other@example.com", BenefitTierFree, data)
	client.catalogMu.RLock()
	shared := len(client.catalogShared)
	client.catalogMu.RUnlock()
	if shared != 1 {
		t.Fatalf("共用目录 %d 份，期望 1 份", shared)
	}
}

// catalogTestService 返回带假上游的账户池服务
func catalogTestService(t *testing.T, transport *catalogTestTransport, ids ...string) (*PooledService, *AccountPool, *Client) {
	t.Helper()
	pool := catalogTestPool(t, ids...)
	client := catalogTestClient(t, transport)
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool, client
}

// TestEmptyCatalogDoesNotOverwrite 账户已有非空目录时 [[]] 不覆盖账户池目录、也不覆盖 Client 目录，返回 ErrEmptyModelCatalog；
// 随后查模型照常命中，不报 ErrModelNotFound、不在请求路径上重新拉取
func TestEmptyCatalogDoesNotOverwrite(t *testing.T) {
	const accountID = "a@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	service, pool, client := catalogTestService(t, transport, accountID)
	if models, err := service.RefreshAccountModels(context.Background(), accountID); err != nil || len(models) != 1 {
		t.Fatalf("首次同步 models=%v err=%v", models, err)
	}
	transport.setListModels(catalogTestRaw(t))
	_, err := service.RefreshAccountModels(context.Background(), accountID)
	var empty *EmptyModelCatalogError
	if !errors.Is(err, ErrEmptyModelCatalog) || !errors.As(err, &empty) || empty.Count != 1 || empty.Models != 1 {
		t.Fatalf("空目录应返回 ErrEmptyModelCatalog，实际 %v", err)
	}
	if status, _ := pool.StatusOf(accountID); len(status.Models) != 1 {
		t.Fatal("账户池目录不应被空结果覆盖")
	}
	if _, err := client.modelEntry(context.Background(), accountID, "alpha"); err != nil {
		t.Fatalf("Client 目录不应被空结果覆盖: %v", err)
	}
	if calls := transport.count("ListModels"); calls != 1 {
		t.Fatalf("查模型不应重新拉取，ListModels %d 次", calls)
	}
	snapshot, ok := service.CatalogSnapshot(accountID)
	if !ok || snapshot.Playground.Len() != 1 || snapshot.EmptyCount != 1 {
		t.Fatalf("快照 %+v ok=%v", snapshot, ok)
	}
	// 非空结果清零连续空结果
	transport.setListModels(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	if _, err := service.RefreshAccountModels(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := service.CatalogSnapshot(accountID); snapshot.EmptyCount != 0 || !snapshot.EmptySince.IsZero() {
		t.Fatal("非空结果应清零连续空结果")
	}
}

// TestEmptyCatalogAcceptedAfterStreak 连续 3 次空结果但不到 1 小时仍保留；连续 3 次且跨 1 小时后两侧都接受空目录。
// 账户原来没有目录时照常写空目录
func TestEmptyCatalogAcceptedAfterStreak(t *testing.T) {
	const accountID = "a@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	service, pool, client := catalogTestService(t, transport, accountID, "fresh@example.com")
	if _, err := service.RefreshAccountModels(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	transport.setListModels(catalogTestRaw(t))
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := service.RefreshAccountModels(context.Background(), accountID); !errors.Is(err, ErrEmptyModelCatalog) {
			t.Fatalf("第 %d 次空结果不到 1 小时应保留原目录，实际 %v", attempt, err)
		}
	}
	pool.mu.Lock()
	pool.byID[accountID].emptyCatalogSince = time.Now().Add(-emptyCatalogAcceptSpan - time.Minute)
	pool.mu.Unlock()
	models, err := service.RefreshAccountModels(context.Background(), accountID)
	if err != nil || len(models) != 0 {
		t.Fatalf("连续 3 次且跨 1 小时后应接受空目录，models=%v err=%v", models, err)
	}
	if status, _ := pool.StatusOf(accountID); len(status.Models) != 0 {
		t.Fatal("账户池应接受空目录")
	}
	if _, err := client.modelEntry(context.Background(), accountID, "alpha"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("Client 也应接受空目录，实际 %v", err)
	}
	if snapshot, ok := service.CatalogSnapshot(accountID); !ok || snapshot.EmptyCount != 4 || snapshot.Playground.Len() != 0 {
		t.Fatalf("接受后快照 %+v", snapshot)
	}
	// 没有目录的账户第一次就写空目录（不改首次语义）
	if models, err := service.RefreshAccountModels(context.Background(), "fresh@example.com"); err != nil || len(models) != 0 {
		t.Fatalf("没有目录的账户 models=%v err=%v", models, err)
	}
}

// TestTryAcquireIdleAccount 有活动租约时立即返回空、不登记独占等待、不发请求；空闲时取得独占租约
func TestTryAcquireIdleAccount(t *testing.T) {
	const accountID = "a@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	service, pool, _ := catalogTestService(t, transport, accountID)
	active, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := pool.TryAcquireIdleAccount(context.Background(), accountID)
	if err != nil || acquired || lease != nil {
		t.Fatalf("有活动租约时应立即返回空，lease=%v acquired=%v err=%v", lease, acquired, err)
	}
	pool.mu.Lock()
	waiters := pool.byID[accountID].exclusiveWaiters
	pool.mu.Unlock()
	if waiters != 0 {
		t.Fatal("不应登记独占等待")
	}
	if _, busy, err := service.TryRefreshAccountModels(context.Background(), accountID); !busy || err != nil {
		t.Fatalf("账户忙时 busy 应为真，busy=%v err=%v", busy, err)
	}
	if calls := transport.count("ListModels"); calls != 0 {
		t.Fatalf("账户忙时不应发请求，ListModels %d 次", calls)
	}
	// 活动租约照常可以再取（没有被独占等待挡住）
	second, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatalf("没有独占等待时应照常发出租约: %v", err)
	}
	_ = second.Release()
	_ = active.Release()
	lease, acquired, err = pool.TryAcquireIdleAccount(context.Background(), accountID)
	if err != nil || !acquired || lease == nil {
		t.Fatalf("空闲时应取得独占租约，acquired=%v err=%v", acquired, err)
	}
	if again, acquiredAgain, _ := pool.TryAcquireIdleAccount(context.Background(), accountID); acquiredAgain || again != nil {
		t.Fatal("独占期间不应再取得")
	}
	_ = lease.Release()
	if models, busy, err := service.TryRefreshAccountModels(context.Background(), accountID); busy || err != nil || len(models) != 1 {
		t.Fatalf("空闲时应同步，models=%v busy=%v err=%v", models, busy, err)
	}
}

// TestTryAcquireIdleAccountCountsEgressOther 后台刷新取得的独占租约带着账户的出口通道（与 AcquireAccount 一致）：
// 刷新经 Client.do 发出的 RPC 计入通道的非生成发送（OtherSent1m），不计入生成发送、不预占
func TestTryAcquireIdleAccountCountsEgressOther(t *testing.T) {
	const accountID = "a@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	service, pool, _ := catalogTestService(t, transport, accountID)
	sent := func() int {
		transport.mu.Lock()
		defer transport.mu.Unlock()
		total := 0
		for _, calls := range transport.calls {
			total += calls
		}
		return total
	}
	before, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
	sentBefore := sent()
	if _, busy, err := service.TryRefreshAccountModels(context.Background(), accountID); busy || err != nil {
		t.Fatalf("空闲时应同步，busy=%v err=%v", busy, err)
	}
	delta := sent() - sentBefore
	if delta == 0 {
		t.Fatal("后台刷新应发出请求")
	}
	after, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
	if got := after.OtherSent1m - before.OtherSent1m; got != delta || after.Sent1m != before.Sent1m || after.Pending != 0 {
		t.Fatalf("快照 %+v → %+v，期望非生成发送增加 %d、生成发送不变、预占 0", before, after, delta)
	}
}

// TestBackgroundRefreshKeepsGlobalCooldown 后台目录刷新（TryRefreshAccountModels）成功不清账号的全局冷却（如全局每日限额到次日零点）：
// 目录能列出不代表生成额度已恢复，原先目录到期就把冷却中的账号放回候选。管理操作（RefreshAccountModels）照旧清
func TestBackgroundRefreshKeepsGlobalCooldown(t *testing.T) {
	const accountID = "a@example.com"
	transport := newCatalogTestTransport(catalogTestRaw(t, catalogTestRow("alpha", 0)))
	service, pool, _ := catalogTestService(t, transport, accountID)
	if _, err := service.RefreshAccountModels(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	cool := func() {
		t.Helper()
		if err := pool.MarkCooldownIfGeneration(
			accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(10*time.Hour), "每日限额: 全局",
		); err != nil {
			t.Fatal(err)
		}
		if !pool.AccountLongCooling(accountID, "", ShortCooldownMax) {
			t.Fatal("没有写上长全局冷却")
		}
	}
	cool()
	if models, busy, err := service.TryRefreshAccountModels(context.Background(), accountID); busy || err != nil || len(models) == 0 {
		t.Fatalf("models=%d busy=%v err=%v，期望后台刷新成功", len(models), busy, err)
	}
	if !pool.AccountLongCooling(accountID, "", ShortCooldownMax) {
		t.Fatal("后台目录刷新清掉了 10 小时的全局冷却")
	}
	if status, _ := pool.StatusOf(accountID); status.State != AccountCooldown {
		t.Fatalf("状态 %s，期望仍为冷却", status.State)
	}
	if _, err := service.RefreshAccountModels(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	if pool.AccountCoolingDown(accountID, "") {
		t.Fatal("管理操作的目录刷新应照旧清全局冷却")
	}
}
