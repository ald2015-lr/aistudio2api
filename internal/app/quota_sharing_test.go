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
		if _, cool := sharing.dailyLimitHit("a", "gemini-test", aistudio.ChannelPlayground, now, now); !cool {
			t.Fatalf("第 %d 次每日限额应同时冷却另一个通道", index)
		}
	}
	other, cool := sharing.dailyLimitHit("a", "gemini-test", aistudio.ChannelPlayground, now, now)
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
