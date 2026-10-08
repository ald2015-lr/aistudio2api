package app

import (
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestQuotaSharingVerdictCanBeRevised 判定为共用后仍定期复核：另一个通道成功时判定改为独立
func TestQuotaSharingVerdictCanBeRevised(t *testing.T) {
	sharing := newQuotaSharing("", nil)
	sharing.models["gemini-test"] = &quotaSharingStats{Shared: quotaSharedMinEvidence}
	now := time.Now()
	for index := 1; index < quotaRevisitEvery; index++ {
		if _, cool := sharing.dailyLimitHit("a", "gemini-test", aistudio.PoolScopeNormal, aistudio.ChannelPlayground, now, now); !cool {
			t.Fatalf("第 %d 次每日限额应同时冷却另一个通道", index)
		}
	}
	other, cool := sharing.dailyLimitHit("a", "gemini-test", aistudio.PoolScopeNormal, aistudio.ChannelPlayground, now, now)
	if cool || other != aistudio.ChannelBuild {
		t.Fatalf("第 %d 次应放行另一个通道复核", quotaRevisitEvery)
	}
	sharing.attemptSucceeded("a", "gemini-test", aistudio.ChannelBuild, now.Add(time.Second))
	sharing.mu.Lock()
	shared := sharing.sharedLocked("gemini-test")
	sharing.mu.Unlock()
	if shared {
		t.Fatal("复核发现另一个通道仍可用后，判定应不再为共用")
	}
}

// TestQuotaSharingVerdictPerPool 判定按号池分开学习：普通号池账户学到的共用结论不会让 Ultra 账户同时冷却另一个通道，
// Ultra 号池用自己账户的证据学到共用后才同时冷却；普通号池的判定键仍为模型 ID（已保存的记录继续有效）
func TestQuotaSharingVerdictPerPool(t *testing.T) {
	const model = "gemini-test"
	sharing := newQuotaSharing("", nil)
	at := time.Now()
	learn := func(accountID string, pool aistudio.PoolScope) {
		for index := 0; index < quotaSharedMinEvidence; index++ {
			at = at.Add(time.Minute)
			sharing.dailyLimitHit(accountID, model, pool, aistudio.ChannelPlayground, at, at)
			sharing.dailyLimitHit(accountID, model, pool, aistudio.ChannelBuild, at.Add(time.Second), at.Add(time.Second))
		}
		at = at.Add(time.Minute)
	}
	learn("normal-a@example.com", aistudio.PoolScopeNormal)
	if _, coolBoth := sharing.dailyLimitHit("normal-b@example.com", model, aistudio.PoolScopeNormal, aistudio.ChannelPlayground, at, at); !coolBoth {
		t.Fatal("普通号池学到共用结论后，普通账户达到每日限额应同时冷却另一个通道")
	}
	if _, coolBoth := sharing.dailyLimitHit("ultra-a@example.com", model, aistudio.PoolScopeUltra, aistudio.ChannelPlayground, at, at); coolBoth {
		t.Fatal("Ultra 账号第一次达到每日限额就按普通号池学到的结论同时冷却另一个通道")
	}
	sharing.mu.Lock()
	_, normalKey := sharing.models[model]
	sharing.mu.Unlock()
	if !normalKey {
		t.Fatal("普通号池的判定键应仍为模型 ID")
	}
	learn("ultra-b@example.com", aistudio.PoolScopeUltra)
	if _, coolBoth := sharing.dailyLimitHit("ultra-c@example.com", model, aistudio.PoolScopeUltra, aistudio.ChannelPlayground, at, at); !coolBoth {
		t.Fatal("Ultra 号池学到共用结论后，Ultra 账户达到每日限额应同时冷却另一个通道")
	}
}
