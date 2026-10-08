package aistudio

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// stubRuntime 是不启动浏览器的 Worker 后端，只用于其他包测试 Worker 的关闭与替换流程
type stubRuntime struct {
	mu            sync.Mutex
	closeFailures int
	closed        int
}

func (runtime *stubRuntime) Proof(context.Context, string, string) (string, error) {
	return "", errors.New("stub worker 不生成 proof")
}

func (runtime *stubRuntime) ProtocolHeaders(context.Context) (http.Header, error) {
	return http.Header{}, nil
}

func (runtime *stubRuntime) SendProtected(context.Context, string, http.Header, []byte) (*camoufoxnative.ProtectedResponse, error) {
	return nil, errors.New("stub worker 不发送请求")
}

func (runtime *stubRuntime) StorageCookies(context.Context) ([]byte, error) { return nil, nil }

func (runtime *stubRuntime) State() camoufoxnative.State { return camoufoxnative.State{} }

func (runtime *stubRuntime) Close() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closeFailures > 0 {
		runtime.closeFailures--
		return errors.New("stub worker 关闭失败")
	}
	runtime.closed++
	return nil
}

// NewStubWorker 返回不启动浏览器的就绪 Worker，供测试使用：前 closeFailures 次 Close 返回错误，
// 返回的函数报告成功关闭的次数
func NewStubWorker(accountID string, closeFailures int) (*NativeWorker, func() int) {
	runtime := &stubRuntime{closeFailures: closeFailures}
	worker := &NativeWorker{
		accountID: accountID,
		runtime:   runtime,
		state:     WorkerState{AccountID: accountID, Phase: WorkerReady, RuntimeID: "stub"},
	}
	return worker, func() int {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.closed
	}
}
