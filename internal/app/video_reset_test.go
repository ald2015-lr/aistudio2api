package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const videoResetTestModel = "veo-reset-test"

// videoResetTransport 是视频恢复测试的传输层：ListModels 返回带 predictLongRunning 的视频模型；GenerateVideo 时 failing 中账户的
// 前几次调用按协议 Code 5（404 Ambiguous request）失败，其余返回一个 operation；calls 记录各次 GenerateVideo 用的账户
type videoResetTransport struct {
	mu      sync.Mutex
	failing map[string]int
	calls   []string
}

func videoResetResponse(status int, body string) *aistudio.RPCResponse {
	return &aistudio.RPCResponse{
		StatusCode: status, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (transport *videoResetTransport) Do(_ context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	if request.Method != "ListModels" {
		return nil, errors.New("测试不支持 " + request.Method)
	}
	return videoResetResponse(http.StatusOK,
		`[[["models/`+videoResetTestModel+`",null,null,"Veo Reset Test",null,null,null,["predictLongRunning"]]]]`), nil
}

func (transport *videoResetTransport) DoProtected(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return nil, errors.New("测试不发送生成请求")
}

func (transport *videoResetTransport) DoProtectedVideo(
	_ context.Context, request aistudio.VideoRequest, _ aistudio.RPCRequest,
) (*aistudio.RPCResponse, error) {
	transport.mu.Lock()
	transport.calls = append(transport.calls, request.AccountID)
	fail := transport.failing[request.AccountID] > 0
	if fail {
		transport.failing[request.AccountID]--
	}
	transport.mu.Unlock()
	if fail {
		return videoResetResponse(http.StatusNotFound, `[5,"Ambiguous request for service ''"]`), nil
	}
	return videoResetResponse(http.StatusOK, `["operations/video-reset-test"]`), nil
}

func (transport *videoResetTransport) called() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return slices.Clone(transport.calls)
}

// TestVideoRecoveryWaitsForWorkerReset 不固定账号的视频请求经真实的 PooledService.GenerateVideo 遇到协议 Code 5、同账号还有请求占着租约时，
// 恢复照旧等关闭完成后在原账号重放成功，不按 dataResetSwitchWait 只等 50ms：aistudio 层的视频循环遇到 Code 5（404）不换号，
// 等不到就返回未恢复的话请求直接以 Code 5 失败（对外 502），bob 也不会被调用
func TestVideoRecoveryWaitsForWorkerReset(t *testing.T) {
	service, closed := workerResetTestService(t, newWorkerFailureService(nil), resetTestAlice, resetTestBob)
	for _, accountID := range []string{resetTestAlice, resetTestBob} {
		if err := service.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
			{ID: videoResetTestModel, Methods: []string{"predictLongRunning"}, Capabilities: map[string]bool{"video_generation": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	transport := &videoResetTransport{failing: map[string]int{resetTestAlice: 1}}
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: transport, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := aistudio.NewPooledService(service.pool, client)
	if err != nil {
		t.Fatal(err)
	}
	service.service = pooled
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	// alice 上另一个请求 300ms 后才释放租约：关闭要等它结束，长于不固定账号的等待上限 50ms
	const holdFor = 300 * time.Millisecond
	release := holdLease(t, service.pool, resetTestAlice)
	defer release()
	time.AfterFunc(holdFor, release)
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	startedAt := time.Now()
	operation, err := service.GenerateVideo(ctx, aistudio.VideoRequest{Model: videoResetTestModel, Prompt: "a cat"})
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("GenerateVideo 失败（%s 后，调用=%v）: %v，期望等关闭完成后在原账号重放成功", elapsed, transport.called(), err)
	}
	if operation.ID != "operations/video-reset-test" {
		t.Fatalf("operation = %+v", operation)
	}
	if calls := transport.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("GenerateVideo 调用 = %v，期望 alice 失败后关闭完成、在 alice 重放", calls)
	}
	if elapsed < holdFor-50*time.Millisecond {
		t.Fatalf("用时 %s，期望等到同账号的租约释放（约 %s）", elapsed, holdFor)
	}
	waitResetsSettled(t, service.workers)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("alice 的 Worker 关闭 %d 次，期望 1 次", got)
	}
}
