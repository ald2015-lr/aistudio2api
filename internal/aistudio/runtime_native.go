package aistudio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// NativeWorker 将单个账户的 Camoufox 或纯 Go WAA runtime 适配为受保护请求 preparer
type NativeWorker struct {
	accountID   string
	runtime     workerRuntime
	operationMu sync.Mutex
	stateMu     sync.RWMutex
	state       WorkerState
}

var _ ProtectedPreparer = (*NativeWorker)(nil)
var _ ProtocolHeaderProvider = (*NativeWorker)(nil)

// NewNativeWorker 启动单个账户的 Camoufox WAA runtime
func NewNativeWorker(ctx context.Context, accountID string, options camoufoxnative.Options) (*NativeWorker, error) {
	if accountID == "" {
		return nil, fmt.Errorf("缺少账户 ID")
	}
	runtime, err := camoufoxnative.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	runtimeState := runtime.State()
	return &NativeWorker{
		accountID: accountID,
		runtime:   runtime,
		state: WorkerState{
			AccountID: accountID,
			Phase:     WorkerReady,
			PID:       runtimeState.PID,
			RuntimeID: "native-webdriver-bidi",
			PageURL:   runtimeState.PageURL,
		},
	}, nil
}

// errWorkerClosed 表示 runtime 已开始关闭，不再生成 proof
var errWorkerClosed = errors.New("WAA worker 已关闭")

// Prepare 生成 fresh proof 并写入请求指定的 WAA field
func (worker *NativeWorker) Prepare(ctx context.Context, request ProtectedRequest) (PreparedProtectedRequest, error) {
	worker.operationMu.Lock()
	defer worker.operationMu.Unlock()
	// 调用方在账户锁外执行 Prepare，可能排在一次关闭之后才拿到 operationMu：关闭失败时状态停在 WorkerClosing，
	// 不能再改回 Busy/Ready，否则清理重试认不出这个关不掉的 runtime
	if phase := worker.phase(); phase == WorkerClosing || phase == WorkerClosed {
		return PreparedProtectedRequest{}, errWorkerClosed
	}
	worker.updateState(func(state *WorkerState) {
		state.Phase = WorkerBusy
		state.RequestCount++
		state.LastError = ""
	})
	digest := sha256.Sum256([]byte(request.Prompt))
	proof, err := worker.runtime.Proof(ctx, fmt.Sprintf("%x", digest), request.Prompt)
	if err != nil {
		if ctx.Err() != nil {
			worker.updateState(func(state *WorkerState) { state.Phase = WorkerReady })
		} else {
			worker.fail(err)
		}
		return PreparedProtectedRequest{}, err
	}
	var payload []any
	if err := json.Unmarshal(request.Body, &payload); err != nil {
		worker.fail(err)
		return PreparedProtectedRequest{}, fmt.Errorf("解析受保护请求: %w", err)
	}
	if request.ProofField < 1 || len(payload) < request.ProofField {
		err := fmt.Errorf("受保护请求缺少 WAA field %d", request.ProofField)
		worker.fail(err)
		return PreparedProtectedRequest{}, err
	}
	payload[request.ProofField-1] = proof
	body, err := json.Marshal(payload)
	if err != nil {
		worker.fail(err)
		return PreparedProtectedRequest{}, fmt.Errorf("编码受保护请求: %w", err)
	}
	headers, err := worker.runtime.ProtocolHeaders(ctx)
	if err != nil {
		if ctx.Err() != nil {
			worker.updateState(func(state *WorkerState) { state.Phase = WorkerReady })
		} else {
			worker.fail(err)
		}
		return PreparedProtectedRequest{}, err
	}
	worker.updateState(func(state *WorkerState) {
		state.Phase = WorkerReady
	})
	return PreparedProtectedRequest{
		Body:    body,
		Headers: headers,
	}, nil
}

// SendProtected 经账户 WAA runtime 流式发送已准备的请求
func (worker *NativeWorker) SendProtected(ctx context.Context, request ProtectedRequest) (*RPCResponse, error) {
	response, err := worker.runtime.SendProtected(ctx, request.URL, request.Headers, request.Body)
	if err != nil {
		// 浏览器只是一时繁忙导致命令超时（控制连接正常）时不判定 Worker 失败，避免无谓地重建浏览器；
		// 请求本身仍会作为超时错误换号重试
		if ctx.Err() == nil && !worker.transientRuntimeError(err) {
			worker.fail(err)
		}
		return nil, err
	}
	return &RPCResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header,
		Body:       response.Body,
	}, nil
}

// BrowserStorageState 返回账户 WAA runtime 当前 Cookie 状态
func (worker *NativeWorker) BrowserStorageState(ctx context.Context) (StorageState, error) {
	encoded, err := worker.runtime.StorageCookies(ctx)
	if err != nil {
		return StorageState{}, err
	}
	var cookies []StateCookie
	if err := json.Unmarshal(encoded, &cookies); err != nil {
		return StorageState{}, fmt.Errorf("解析浏览器 Cookie: %w", err)
	}
	state := StorageState{Cookies: cookies}
	if err := state.Validate(); err != nil {
		return StorageState{}, err
	}
	return state, nil
}

// ProtocolHeaders 返回当前账户官网请求的动态公共头
func (worker *NativeWorker) ProtocolHeaders(ctx context.Context, accountID string) (http.Header, error) {
	if accountID != "" && accountID != worker.accountID {
		return nil, fmt.Errorf("runtime 账户不匹配")
	}
	return worker.runtime.ProtocolHeaders(ctx)
}

// State 返回账户 WAA runtime 状态；浏览器控制连接已断开（浏览器进程退出等）时如实报告失败，
// 让进行中的请求换号重试、让 Worker 被重建，而不是继续把请求分给已经不可用的浏览器
func (worker *NativeWorker) State() WorkerState {
	worker.stateMu.RLock()
	state := worker.state
	worker.stateMu.RUnlock()
	if state.Phase == WorkerReady || state.Phase == WorkerBusy {
		if err := worker.runtimeBroken(); err != nil {
			state.Phase = WorkerFailed
			state.LastError = err.Error()
		}
	}
	return state
}

// runtimeBroken 返回浏览器控制连接断开的原因；纯 Go runtime 或连接正常时返回 nil
func (worker *NativeWorker) runtimeBroken() error {
	if runtime, ok := worker.runtime.(interface{ Broken() error }); ok {
		return runtime.Broken()
	}
	return nil
}

// transientRuntimeError 判断错误是否只是浏览器暂时繁忙：命令超时且控制连接仍然正常
func (worker *NativeWorker) transientRuntimeError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && worker.runtimeBroken() == nil
}

// Close 关闭账户 WAA runtime
func (worker *NativeWorker) Close() error {
	worker.operationMu.Lock()
	defer worker.operationMu.Unlock()
	worker.updateState(func(state *WorkerState) {
		state.Phase = WorkerClosing
		state.LastError = ""
	})
	err := worker.runtime.Close()
	worker.updateState(func(state *WorkerState) {
		if err != nil {
			state.LastError = err.Error()
			return
		}
		state.Phase = WorkerClosed
	})
	return err
}

// phase 返回记录的阶段，不检查浏览器控制连接
func (worker *NativeWorker) phase() WorkerPhase {
	worker.stateMu.RLock()
	defer worker.stateMu.RUnlock()
	return worker.state.Phase
}

func (worker *NativeWorker) updateState(update func(*WorkerState)) {
	worker.stateMu.Lock()
	defer worker.stateMu.Unlock()
	update(&worker.state)
}

func (worker *NativeWorker) fail(err error) {
	worker.updateState(func(state *WorkerState) {
		state.Phase = WorkerFailed
		state.LastError = err.Error()
	})
}
