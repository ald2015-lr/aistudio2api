package api

import (
	"context"
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

// TestAliasCatalogStaleAfterPoolSwitch 固定文档里写的别名快照行为：快照不随号池切换作废，过期后仍先返回旧快照、
// 只在后台刷新，所以切换后触发刷新的那个请求与刷新完成前的请求都按旧目录解析，与切换过去了多久无关；刷新完成后才换成新目录
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
