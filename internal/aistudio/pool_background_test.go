package aistudio

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestBackgroundWritesWaitBlocksUntilFinished 有进行中的后台写入时 wait 阻塞，写入结束后返回；结束函数重复调用无副作用，
// 计数归零后还能继续登记新写入
func TestBackgroundWritesWaitBlocksUntilFinished(t *testing.T) {
	var writes backgroundWrites
	writes.wait()
	finish := writes.begin()
	released := make(chan struct{})
	go func() {
		writes.wait()
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("有进行中的写入时 wait 不应返回")
	case <-time.After(50 * time.Millisecond):
	}
	finish()
	finish()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("写入结束后 wait 应返回")
	}
	again := writes.begin()
	again()
	writes.wait()
}

// TestMarkModelAccessVerifiedAsyncWaitBackground 后台保存模型资格后 WaitBackground 返回时写入与回调都已完成：
// 回调收到 changed=true，磁盘上的运行态已有该模型的成功记录
func TestMarkModelAccessVerifiedAsyncWaitBackground(t *testing.T) {
	const accountID = "alice@example.com"
	pool := testPoolWithAccount(t, accountID)
	if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{{
		ID: "gemini-test", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var changedSeen atomic.Bool
	var callbackErr atomic.Pointer[error]
	pool.MarkModelAccessVerifiedAsync(accountID, "gemini-test", pool.ModelAccessGeneration(accountID), time.Now(), func(changed bool, err error) {
		calls.Add(1)
		changedSeen.Store(changed)
		if err != nil {
			callbackErr.Store(&err)
		}
	})
	pool.WaitBackground()
	if calls.Load() != 1 || !changedSeen.Load() {
		t.Fatalf("回调次数=%d changed=%v，期望恰好一次且有变化", calls.Load(), changedSeen.Load())
	}
	if err := callbackErr.Load(); err != nil {
		t.Fatalf("回调收到错误: %v", *err)
	}
	pool.mu.Lock()
	runtimePath := pool.byID[accountID].RuntimePath
	pool.mu.Unlock()
	state, err := readRuntime(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if access := state.ModelAccess["gemini-test"]; access.State != ModelAccessVerified {
		t.Fatalf("磁盘运行态 ModelAccess[gemini-test] = %+v，期望 WaitBackground 返回时已写入 verified", access)
	}
}
