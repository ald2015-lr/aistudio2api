package aistudio

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// 关闭失败后 runtime 停在 WorkerClosing：之后才排到的 Prepare 不能再生成 proof，也不能把状态改回 Busy/Ready，
// 否则后台清理重试认不出这个关不掉的 runtime
func TestNativeWorkerPrepareAfterFailedCloseKeepsClosing(t *testing.T) {
	var proofs atomic.Int32
	worker, _ := newStubWorker("a@example.com", &stubRuntime{closeFailures: 1, hooks: StubWorkerHooks{
		Proof: func(context.Context) (string, error) {
			proofs.Add(1)
			return "!proof", nil
		},
	}})
	request := ProtectedRequest{Body: []byte(`[null,"x"]`), ProofField: 1}
	prepared, err := worker.Prepare(context.Background(), request)
	if err != nil || string(prepared.Body) != `["!proof","x"]` {
		t.Fatalf("就绪 Worker 应写入 proof，实际 body=%s err=%v", prepared.Body, err)
	}
	if err := worker.Close(); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	if _, err := worker.Prepare(context.Background(), request); !errors.Is(err, errWorkerClosed) {
		t.Fatalf("关闭中的 Worker 应拒绝生成 proof，实际 %v", err)
	}
	if proofs.Load() != 1 {
		t.Fatalf("关闭中的 Worker 不应再生成 proof，实际生成 %d 次", proofs.Load())
	}
	if phase := worker.State().Phase; phase != WorkerClosing {
		t.Fatalf("Prepare 不应改写关闭中的状态，实际 %s", phase)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("第二次关闭应成功: %v", err)
	}
	if _, err := worker.Prepare(context.Background(), request); !errors.Is(err, errWorkerClosed) {
		t.Fatalf("已关闭的 Worker 应拒绝生成 proof，实际 %v", err)
	}
}
