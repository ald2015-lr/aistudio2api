package api

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// switchingCatalogService 的模型目录可以在测试中替换，模拟 ULTRA_INCLUDE_PRO 切换后号池内容变化；
// gate 不为空时 Models 等它关闭后才返回，用来让后台刷新停在进行中
type switchingCatalogService struct {
	poolRecordingService
	catalogMu sync.Mutex
	catalog   []string
	gate      chan struct{}
	calls     chan struct{}
}

func (service *switchingCatalogService) setCatalog(ids []string, gate chan struct{}) {
	service.catalogMu.Lock()
	defer service.catalogMu.Unlock()
	service.catalog, service.gate = ids, gate
}

func (service *switchingCatalogService) Models(ctx context.Context) ([]aistudio.Model, error) {
	service.catalogMu.Lock()
	ids, gate := slices.Clone(service.catalog), service.gate
	service.catalogMu.Unlock()
	select {
	case service.calls <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	models := make([]aistudio.Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, aistudio.Model{ID: id, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}})
	}
	return models, nil
}

func aliasCatalogIDs(t *testing.T, s *server, ctx context.Context) []string {
	t.Helper()
	models, err := s.aliasCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestAliasCatalogStaleAfterPoolSwitch 固定 aliasCatalog 本身的快照行为：快照不随号池切换作废，过期后仍先返回旧快照、
// 只在后台刷新，刷新完成后才换成新目录。名称在快照里解析不到时的同步重算见 aliasCatalogFor（TestAliasCatalogMissRefreshes）
func TestAliasCatalogStaleAfterPoolSwitch(t *testing.T) {
	service := &switchingCatalogService{calls: make(chan struct{}, 1)}
	service.setCatalog([]string{"normal-model"}, nil)
	s := &server{service: service}
	ctx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)
	if got := aliasCatalogIDs(t, s, ctx); !slices.Equal(got, []string{"normal-model"}) {
		t.Fatalf("第一次计算的目录 = %v", got)
	}
	<-service.calls

	// 切换：号池多了一个模型，快照早已过期（带后缀的请求稀少时切换时快照往往已经过期）
	gate := make(chan struct{})
	service.setCatalog([]string{"normal-model", "switched-model"}, gate)
	cache, _ := s.aliasCache(ctx)
	old := cache.models.Load()
	cache.models.Store(&aliasModelSnapshot{models: old.models, expires: time.Now().Add(-10 * time.Minute)})

	if got := aliasCatalogIDs(t, s, ctx); slices.Contains(got, "switched-model") {
		t.Fatalf("切换后触发刷新的请求应按旧目录解析，实际 %v", got)
	}
	select {
	case <-service.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("过期快照应触发后台刷新")
	}
	if got := aliasCatalogIDs(t, s, ctx); slices.Contains(got, "switched-model") {
		t.Fatalf("后台刷新完成前的请求应仍按旧目录解析，实际 %v", got)
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(aliasCatalogIDs(t, s, ctx), "switched-model") {
		if time.Now().After(deadline) {
			t.Fatal("后台刷新完成后应换成新目录")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainCatalogCalls 取走已记下的 Models 调用，返回调用次数
func drainCatalogCalls(service *switchingCatalogService) int {
	count := 0
	for {
		select {
		case <-service.calls:
			count++
		default:
			return count
		}
	}
}

// ageAliasSnapshot 把号池快照的建立时刻往前推，模拟快照已经建立了 age
func ageAliasSnapshot(t *testing.T, s *server, ctx context.Context, age time.Duration) {
	t.Helper()
	cache, _ := s.aliasCache(ctx)
	current := cache.models.Load()
	if current == nil {
		t.Fatal("还没有快照")
	}
	aged := *current
	aged.builtAt = time.Now().Add(-age)
	cache.models.Store(&aged)
}

// TestAliasCatalogMissRefreshes 快照里没有请求的模型（刚重启时 Pro、Ultra 账户的 Build 目录还没恢复，只在 Build 通道提供的
// gemini-4-argon 不在快照里；或快照建于号池切换之前）：带后缀的名称原先解析不到、原名转发后 404，不带后缀的名称却照常可用。
// 现在快照建立超过 aliasMissRefreshAfter 时同步重算一次，当场按新目录解析；快照还新、或名称解析得到时不重算
func TestAliasCatalogMissRefreshes(t *testing.T) {
	service := &switchingCatalogService{calls: make(chan struct{}, 16)}
	service.setCatalog([]string{"normal-model"}, nil)
	s := &server{service: service}
	ctx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)
	aliasCatalogIDs(t, s, ctx)
	drainCatalogCalls(service)
	service.setCatalog([]string{"normal-model", "late-model"}, nil)

	// 快照还新：不重算，仍解析不到，原名保持不变
	fresh := aistudio.GenerateRequest{Model: "late-model-medium"}
	if _, err := s.applyModelAlias(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Model != "late-model-medium" || drainCatalogCalls(service) != 0 {
		t.Fatalf("快照建立不到 %s 时不应重算：模型 = %s", aliasMissRefreshAfter, fresh.Model)
	}

	// 快照变旧：解析不到时同步重算一次，带后缀的名称当场换成目录模型名
	ageAliasSnapshot(t, s, ctx, aliasMissRefreshAfter+time.Second)
	request := aistudio.GenerateRequest{Model: "late-model-medium"}
	if _, err := s.applyModelAlias(ctx, &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "late-model" || request.Alias == nil || request.Alias.Level != "medium" {
		t.Fatalf("快照过旧时应重算后按新目录解析：模型 = %s，别名 = %+v", request.Model, request.Alias)
	}
	if calls := drainCatalogCalls(service); calls != 1 {
		t.Fatalf("应只同步重算一次，实际 %d 次", calls)
	}
	if got := aliasCatalogIDs(t, s, ctx); !slices.Contains(got, "late-model") {
		t.Fatalf("重算后的目录应写回快照，实际 %v", got)
	}

	// 刚重算过：目录里没有的名字不再重算（客户端一直发错名字也不会反复占账户池锁）
	unknown := aistudio.GenerateRequest{Model: "missing-model-low"}
	if _, err := s.applyModelAlias(ctx, &unknown); err != nil {
		t.Fatal(err)
	}
	if unknown.Model != "missing-model-low" || drainCatalogCalls(service) != 0 {
		t.Fatalf("快照刚重算过时不应再重算：模型 = %s", unknown.Model)
	}

	// 快照里解析得到的名称从不触发重算，即使快照已经很旧
	ageAliasSnapshot(t, s, ctx, time.Hour)
	if got := s.resolveModelName(ctx, "normal-model-low"); got != "normal-model" {
		t.Fatalf("解析得到的名称 = %s", got)
	}
	if calls := drainCatalogCalls(service); calls != 0 {
		t.Fatalf("解析得到时不应重算，实际 %d 次", calls)
	}
}

// TestAliasCatalogMissRefreshKeepsSnapshotOnError 同步重算失败（例如请求已取消）时照旧返回原快照，不清掉快照
func TestAliasCatalogMissRefreshKeepsSnapshotOnError(t *testing.T) {
	service := &switchingCatalogService{calls: make(chan struct{}, 16)}
	service.setCatalog([]string{"normal-model"}, nil)
	s := &server{service: service}
	ctx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)
	aliasCatalogIDs(t, s, ctx)
	ageAliasSnapshot(t, s, ctx, aliasMissRefreshAfter+time.Second)
	service.setCatalog([]string{"normal-model", "late-model"}, make(chan struct{}))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	models, err := s.aliasCatalogFor(canceled, "late-model-low")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "normal-model" {
		t.Fatalf("重算失败时应返回原快照，实际 %v", models)
	}
	cache, _ := s.aliasCache(ctx)
	if current := cache.models.Load(); current == nil || len(current.models) != 1 {
		t.Fatal("重算失败时应保留原快照")
	}
}

// TestStoreAliasSnapshotKeepsNewer 开始得早的计算后完成时不覆盖开始得晚的快照（同步重算先完成、后台刷新后完成）
func TestStoreAliasSnapshotKeepsNewer(t *testing.T) {
	var cache aliasCatalogCache
	now := time.Now()
	newer := newAliasSnapshot([]aistudio.Model{{ID: "newer"}}, now)
	if got := storeAliasSnapshot(&cache, newer); got != newer {
		t.Fatal("没有快照时应写入")
	}
	older := newAliasSnapshot([]aistudio.Model{{ID: "older"}}, now.Add(-time.Second))
	if got := storeAliasSnapshot(&cache, older); got != newer || cache.models.Load() != newer {
		t.Fatal("开始得早的计算不应覆盖开始得晚的快照")
	}
	latest := newAliasSnapshot([]aistudio.Model{{ID: "latest"}}, now.Add(time.Second))
	if got := storeAliasSnapshot(&cache, latest); got != latest || cache.models.Load() != latest {
		t.Fatal("开始得晚的计算应写入")
	}
}

// interactionCatalogService 的模型目录取自 ListModels 现场格式的解析结果
type interactionCatalogService struct {
	poolRecordingService
	models []aistudio.Model
}

func (service *interactionCatalogService) Models(context.Context) ([]aistudio.Model, error) {
	return service.models, nil
}

// TestSuffixWithInteractionConfig 2026-11 起 Google 逐个账户给 gemini-4-neon、gemini-4-argon 的目录加上 field 79 [null,null,1,2]：
// 原先它们因此被当成 Interaction（Omni）模型、不算文本对话模型，带后缀的名称（gemini-4-neon-medium）不再解析，原名转发后以模型不存在 404，
// 不带后缀的名称却照常可用。现在照常按后缀改写为目录模型名、设置思考等级；Omni（[null,null,false,2]）仍不按文本模型处理
func TestSuffixWithInteractionConfig(t *testing.T) {
	row := func(id string, config any) []any {
		values := make([]any, 85)
		values[0] = "models/" + id
		values[3] = id
		values[6] = 65536
		values[7] = []string{"countTokens", "generateContent"}
		values[64] = []int{1, 25, 52, 91}
		values[78] = config
		return values
	}
	raw, err := json.Marshal([]any{[]any{
		row("gemini-4-neon", []any{nil, nil, 1, 2}),
		row("gemini-omni-test", []any{nil, nil, false, 2}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	models, err := aistudio.ParseModels(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{service: &interactionCatalogService{models: models}}
	ctx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)

	request := aistudio.GenerateRequest{Model: "gemini-4-neon-medium"}
	if _, err := s.applyModelAlias(ctx, &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "gemini-4-neon" || request.Alias == nil || request.Alias.Level != "medium" {
		t.Fatalf("带 [null,null,1,2] 的 Gemini 4 模型应按后缀改写：模型 = %s，别名 = %+v", request.Model, request.Alias)
	}
	omni := aistudio.GenerateRequest{Model: "gemini-omni-test-medium"}
	if _, err := s.applyModelAlias(ctx, &omni); err != nil {
		t.Fatal(err)
	}
	if omni.Model != "gemini-omni-test-medium" || omni.Alias != nil {
		t.Fatalf("Omni 不按文本模型处理后缀，实际模型 = %s", omni.Model)
	}
}
