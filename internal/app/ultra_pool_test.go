package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	ultraTestNormalA = "normal-a@example.com"
	ultraTestNormalB = "normal-b@example.com"
	ultraTestUltraA  = "ultra-a@example.com"
	ultraTestUltraB  = "ultra-b@example.com"
)

// quotaUpstream 是测试用的上游：每个账号都返回每日限额，记录被调用的账号
type quotaUpstream struct {
	mu    sync.Mutex
	calls []string
}

func (upstream *quotaUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *quotaUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *quotaUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	return nil, &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel-FreeTier"},
	}
}

func (upstream *quotaUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// ultraTestService 返回带就绪 Worker 的生成服务：ultraAccounts 中的账户权益为 Ultra，其余为 Free
func ultraTestService(t *testing.T, upstream aistudio.Service, ultraAccounts []string, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := firstEventTestService(t, upstream, 0, accountIDs...)
	for _, accountID := range ultraAccounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return service, pool
}

// runScopedGenerate 在指定号池内执行一次生成，返回最终错误
func runScopedGenerate(t *testing.T, service *trackedService, scope aistudio.PoolScope, scoped bool) error {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: "req-ultra-" + scope.String(), Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	parent := service.lifecycle
	if scoped {
		parent = aistudio.ContextWithPoolScope(parent, scope)
	}
	requestCtx, requestCancel := context.WithTimeout(parent, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(parent, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	return err
}

func sortedUnique(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

// TestGenerateStaysInPool 生成请求的换号只在请求号池内进行：/ultra 只试 Ultra 账号，独占模式的普通路径只试普通账号，
// 非独占模式（不限号池）两边都试；号池内账号都返回限额后按限额（429）结束
func TestGenerateStaysInPool(t *testing.T) {
	for _, test := range []struct {
		name   string
		scope  aistudio.PoolScope
		scoped bool
		want   []string
	}{
		{name: "Ultra 路径", scope: aistudio.PoolScopeUltra, scoped: true, want: []string{ultraTestUltraA, ultraTestUltraB}},
		{name: "独占模式的普通路径", scope: aistudio.PoolScopeNormal, scoped: true, want: []string{ultraTestNormalA, ultraTestNormalB}},
		{name: "非独占模式的普通路径", scope: aistudio.PoolScopeAll, scoped: true, want: []string{ultraTestNormalA, ultraTestNormalB, ultraTestUltraA, ultraTestUltraB}},
		{name: "后台任务不限号池", scope: aistudio.PoolScopeAll, scoped: false, want: []string{ultraTestNormalA, ultraTestNormalB, ultraTestUltraA, ultraTestUltraB}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &quotaUpstream{}
			service, _ := ultraTestService(t, upstream, []string{ultraTestUltraA, ultraTestUltraB},
				ultraTestNormalA, ultraTestNormalB, ultraTestUltraA, ultraTestUltraB)
			err := runScopedGenerate(t, service, test.scope, test.scoped)
			var rpcError *aistudio.RPCError
			if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("err = %v，期望号池内账号都返回每日限额", err)
			}
			if got := sortedUnique(upstream.called()); !slices.Equal(got, test.want) {
				t.Fatalf("尝试的账号 = %v，期望 %v", got, test.want)
			}
		})
	}
}

// TestGenerateWithoutUltraAccounts 没有 Ultra 账户时 /ultra 生成请求按号池暂时不可调度结束（503），错误写明 Ultra 号池
func TestGenerateWithoutUltraAccounts(t *testing.T) {
	upstream := &quotaUpstream{}
	service, _ := ultraTestService(t, upstream, nil, ultraTestNormalA)
	err := runScopedGenerate(t, service, aistudio.PoolScopeUltra, true)
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) || notReady.Pool != aistudio.PoolScopeUltra || !strings.Contains(err.Error(), "Ultra 号池") {
		t.Fatalf("err = %v，期望写明 Ultra 号池的号池暂时不可调度错误", err)
	}
	if calls := upstream.called(); len(calls) != 0 {
		t.Fatalf("没有 Ultra 账户时不应调用上游，实际调用 %v", calls)
	}
}

// TestGenerateRejectsOutOfPoolFile 生成请求引用号池之外账户上的文件时按 400 拒绝，不复制文件、不调用上游
func TestGenerateRejectsOutOfPoolFile(t *testing.T) {
	upstream := &quotaUpstream{}
	service, pool := ultraTestService(t, upstream, []string{ultraTestUltraA}, ultraTestNormalA, ultraTestUltraA)
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: ultraTestNormalA})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.BindFileResource(context.Background(), aistudio.FileRef{ID: "files/normal", Name: "a.txt", MIME: "text/plain"}, 3, "user_data"); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	ctx := aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra)
	_, _, err = service.startGenerate(ctx, aistudio.GenerateRequest{
		ID: "req-ultra-file", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}, {File: &aistudio.FileRef{ID: "files/normal"}}}}},
	})
	var mismatch *aistudio.ResourcePoolMismatchError
	if !errors.As(err, &mismatch) || mismatch.Owner != aistudio.PoolScopeNormal {
		t.Fatalf("err = %v，期望文件属于普通号池的错误", err)
	}
	if calls := upstream.called(); len(calls) != 0 {
		t.Fatalf("跨号池文件不应调用上游，实际调用 %v", calls)
	}
}

// TestCandidatesAndModelsPerPool Live、转录候选与模型目录按请求号池
func TestCandidatesAndModelsPerPool(t *testing.T) {
	service, _ := ultraTestService(t, &quotaUpstream{}, []string{ultraTestUltraA}, ultraTestNormalA, ultraTestUltraA)
	ultra := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeUltra)
	normal := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)
	for _, test := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{name: "Ultra", ctx: ultra, want: []string{ultraTestUltraA}},
		{name: "普通", ctx: normal, want: []string{ultraTestNormalA}},
		{name: "不限号池", ctx: context.Background(), want: []string{ultraTestNormalA, ultraTestUltraA}},
	} {
		live, err := service.bidiCandidates(test.ctx, testRuntimeModel, testRuntimeModel)
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedUnique(live); !slices.Equal(got, test.want) {
			t.Fatalf("%s Live 候选 = %v，期望 %v", test.name, got, test.want)
		}
		transcription, err := service.transcriptionCandidates(test.ctx, testRuntimeModel)
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedUnique(transcription); !slices.Equal(got, test.want) {
			t.Fatalf("%s 转录候选 = %v，期望 %v", test.name, got, test.want)
		}
	}

	// Ultra 账户都需要重新登录：Ultra Live 候选按号池暂时不可调度，普通号池不受影响
	if err := service.pool.MarkAuthRequired(ultraTestUltraA, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	var notReady *aistudio.AccountsNotReadyError
	if _, err := service.bidiCandidates(ultra, testRuntimeModel, testRuntimeModel); !errors.As(err, &notReady) || notReady.Pool != aistudio.PoolScopeUltra {
		t.Fatalf("Ultra 账户需要重新登录时 Live 候选：err = %v，期望写明 Ultra 号池的 503", err)
	}
	if _, err := service.bidiCandidates(normal, testRuntimeModel, testRuntimeModel); err != nil {
		t.Fatalf("普通号池 Live 候选不应受影响: %v", err)
	}

	// 模型目录按号池：只有停用的账户支持模型时该号池的模型列表为空，另一号池照常
	service.models = []aistudio.Model{{ID: testRuntimeModel, Methods: []string{"generateContent"}}}
	lease, err := service.pool.AcquireAccount(context.Background(), ultraTestUltraA)
	if err != nil {
		t.Fatal(err)
	}
	disabled := lease.Account().Config
	disabled.Enabled = false
	if err := lease.SaveConfig(disabled); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	if models, _ := service.Models(ultra); len(models) != 0 {
		t.Fatalf("Ultra 账户都停用后 Ultra 号池模型目录 = %v，期望为空", models)
	}
	if models, _ := service.Models(normal); len(models) != 1 {
		t.Fatalf("普通号池模型目录 = %v，期望 1 个模型", models)
	}
}
