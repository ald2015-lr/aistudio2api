package aistudio

import (
	"context"
	"testing"
	"time"
)

// TestAcquireAccountConcurrentCatalogSync 管理操作取独占租约时与目录同步并发：租约里记录的认证代际与目录代际必须在账户池锁内读取，
// 目录同步在锁内递增目录代际；原先在解锁之后读取，-race 下报告数据竞争，租约可能记下撕裂的代际
func TestAcquireAccountConcurrentCatalogSync(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shared := Model{ID: scopeSharedModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}}
	extra := Model{ID: "extra-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	catalogs := [][]Model{{shared}, {shared, extra}}
	done := make(chan error, 1)
	go func() {
		for round := 0; round < 200 && ctx.Err() == nil; round++ {
			// 目录内容交替变化，每次都递增目录代际
			if err := pool.SetCatalog(scopeNormalA, BenefitTierFree, catalogs[round%2]); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for round := 0; round < 200; round++ {
		lease, err := pool.AcquireAccount(ctx, scopeNormalA)
		if err != nil {
			t.Fatalf("第 %d 次取独占租约失败: %v", round, err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("并发目录同步失败: %v", err)
	}
}
