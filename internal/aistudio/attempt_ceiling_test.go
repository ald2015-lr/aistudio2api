package aistudio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 不固定账户的数据请求（CountTokens、Live、转写、Veo）的换号上限测试：上游对每个账户都返回 429，按选号观察者记下的次数核对尝试了几个账户

// ceilingModel 为四条路径都能选中的测试模型
const ceilingModel = "ceiling-model"

// ceilingRateLimited 为测试上游对每个请求返回的 429（只有 “Quota exceeded”）
func ceilingRateLimited() *RPCResponse {
	return &RPCResponse{
		StatusCode: http.StatusTooManyRequests, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`[8,"Quota exceeded"]`)),
	}
}

// ceilingTransport 对每个普通 RPC 返回 429
type ceilingTransport struct{}

func (ceilingTransport) Do(context.Context, RPCRequest) (*RPCResponse, error) {
	return ceilingRateLimited(), nil
}

// ceilingProtected 对受保护的生成、Veo 与 Live 会话建立都返回 429
type ceilingProtected struct{}

func (ceilingProtected) DoProtected(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
	return ceilingRateLimited(), nil
}

func (ceilingProtected) DoProtectedVideo(context.Context, VideoRequest, RPCRequest) (*RPCResponse, error) {
	return ceilingRateLimited(), nil
}

func (ceilingProtected) OpenBidiProtected(context.Context, BidiRequest, RequestContext, *AccountLease, func() error) (*BidiSession, error) {
	return nil, &RPCError{Method: "BidiGenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded"}
}

// ceilingService 返回 count 个账户的号池服务：账户目录与 Client 的目录都有 ceilingModel（四条路径的方法与能力），上游一律返回 429
func ceilingService(t *testing.T, count int) *PooledService {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: ceilingProtected{}})
	if err != nil {
		t.Fatal(err)
	}
	model := Model{
		ID:           ceilingModel,
		Methods:      []string{"generateContent", "countTokens", "bidiGenerateContent", "predictLongRunning"},
		Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
	}
	for index := range count {
		account, publish, err := store.Create(DefaultAccountConfig(fmt.Sprintf("ceiling-%d@example.com", index)), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(account.ID, BenefitTierFree, []Model{model}); err != nil {
			t.Fatal(err)
		}
		// Live 会话建立要先在 Client 的目录里找到模型（找不到时不换号）
		client.catalogs[account.ID] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{model.ID: {model: model}}}
	}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// countSelections 返回带选号观察者的 context 与选中账户的计数
func countSelections(ctx context.Context) (context.Context, *atomic.Int32) {
	var selected atomic.Int32
	return ContextWithAccountSelectionObserver(ctx, func(*Account) { selected.Add(1) }), &selected
}

// runCeilingPath 在 service 上执行一条数据路径，返回尝试的账户数与错误
func runCeilingPath(t *testing.T, service *PooledService, ctx context.Context, path string) (int, error) {
	t.Helper()
	ctx, selected := countSelections(ctx)
	var err error
	switch path {
	case "CountTokens":
		_, err = service.CountTokens(ctx, TokenCountRequest{
			Model: ceilingModel, Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
		})
	case "转写":
		audio := []byte("RIFF0000WAVE")
		_, err = service.Transcribe(ctx, TranscriptionRequest{
			Model: ceilingModel, Name: "a.wav", MIME: "audio/wav", Size: int64(len(audio)), Reader: bytes.NewReader(audio),
		})
	case "Veo":
		_, err = service.GenerateVideo(ctx, VideoRequest{Model: ceilingModel, Prompt: "a cat"})
	case "Live":
		_, err = service.OpenBidi(ctx, BidiRequest{Model: ceilingModel})
	default:
		t.Fatalf("未知路径 %s", path)
	}
	return int(selected.Load()), err
}

// TestAttemptCeilingLimitsCountTokens 6 个账户都返回 429：带换号上限 4 时 CountTokens 只试 4 个账户，没有上限时照旧都试一遍
func TestAttemptCeilingLimitsCountTokens(t *testing.T) {
	service := ceilingService(t, 6)
	attempts, err := runCeilingPath(t, service, WithAttemptCeiling(context.Background(), 4), "CountTokens")
	if attempts != 4 || err == nil {
		t.Fatalf("尝试 %d 个账户 err=%v，期望 4 个后返回上游 429", attempts, err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("应按上游 429 结束: %v", err)
	}
	unlimited := ceilingService(t, 6)
	if attempts, _ := runCeilingPath(t, unlimited, context.Background(), "CountTokens"); attempts != 6 {
		t.Fatalf("没有上限时尝试 %d 个账户，期望 6 个", attempts)
	}
}

// TestAttemptCeilingSmallPoolUnchanged 账户数不超过上限的小号池（1 个、3 个）换号上限不变；指定账户的请求只用这一个；上限为 0 时不设上限
func TestAttemptCeilingSmallPoolUnchanged(t *testing.T) {
	for _, count := range []int{1, 3} {
		service := ceilingService(t, count)
		ctx := WithAttemptCeiling(context.Background(), 4)
		if got := accountAttemptLimit(ctx, service.pool, false); got != count {
			t.Fatalf("%d 个账户的换号上限 = %d，期望 %d", count, got, count)
		}
		if attempts, _ := runCeilingPath(t, service, ctx, "CountTokens"); attempts != count {
			t.Fatalf("%d 个账户时 CountTokens 尝试 %d 个", count, attempts)
		}
		if got := accountAttemptLimit(ctx, service.pool, true); got != 1 {
			t.Fatalf("指定账户的换号上限 = %d，期望 1", got)
		}
	}
	service := ceilingService(t, 6)
	if got := accountAttemptLimit(WithAttemptCeiling(context.Background(), 0), service.pool, false); got != 6 {
		t.Fatalf("上限为 0 时换号上限 = %d，期望不限（6）", got)
	}
	if attemptCeilingFrom(context.Background()) != 0 || attemptCeilingFrom(WithAttemptCeiling(context.Background(), 4)) != 4 {
		t.Fatal("换号上限没有写进 context")
	}
}

// TestCountTokensAttemptLimitComputedOnce CountTokens 的换号上限在循环前算一次：可调度账户的统计（账户池锁内扫描全部账户）
// 每个请求只做一次，不再每轮重算
func TestCountTokensAttemptLimitComputedOnce(t *testing.T) {
	service := ceilingService(t, 6)
	before := attemptLimitScans.Load()
	attempts, _ := runCeilingPath(t, service, WithAttemptCeiling(context.Background(), 4), "CountTokens")
	if attempts != 4 {
		t.Fatalf("尝试 %d 个账户，期望 4 个", attempts)
	}
	if scans := attemptLimitScans.Load() - before; scans != 1 {
		t.Fatalf("可调度账户统计了 %d 次，期望 1 次", scans)
	}
}

// TestAttemptCeilingTranscribeVeoLive 6 个账户都返回 429：带换号上限 4 时转写、Veo、Live 各试 4 个账户（没有上限时多于 4 个）
func TestAttemptCeilingTranscribeVeoLive(t *testing.T) {
	for _, path := range []string{"转写", "Veo", "Live"} {
		t.Run(path, func(t *testing.T) {
			service := ceilingService(t, 6)
			attempts, err := runCeilingPath(t, service, WithAttemptCeiling(context.Background(), 4), path)
			if err == nil || attempts != 4 {
				t.Fatalf("尝试 %d 个账户 err=%v，期望试满 4 个后返回上游 429", attempts, err)
			}
			unlimited := ceilingService(t, 6)
			if attempts, _ := runCeilingPath(t, unlimited, context.Background(), path); attempts <= 4 {
				t.Fatalf("没有上限时尝试 %d 个账户，期望多于 4 个（证明上限在起作用）", attempts)
			}
		})
	}
}

// TestAttemptCeilingLimitsUpload 自动选号的上传在 GenerateAccessToken 401（出口被拒、Cookie 批量作废）时换号：带换号上限 4 时只试 4 个账户，
// 只有这 4 个写全局短冷却；没有上限时照旧把就绪账户试一遍（第 2 轮复查：原先一次上传能把整个号池试个遍、全部写冷却）
func TestAttemptCeilingLimitsUpload(t *testing.T) {
	upload := func(ctx context.Context) (*PooledService, *accessTokenTransport, int, error) {
		transport := &accessTokenTransport{}
		store := NewAccountStore(t.TempDir())
		pool := NewAccountPool(nil, 2)
		t.Cleanup(pool.WaitBackground)
		for index := range 6 {
			account, publish, err := store.Create(DefaultAccountConfig(fmt.Sprintf("upload-%d@example.com", index)), testStorageState("OLD"))
			if err != nil {
				t.Fatal(err)
			}
			if err := pool.Add(account); err != nil {
				t.Fatal(err)
			}
			if err := publish.Release(); err != nil {
				t.Fatal(err)
			}
		}
		client := &Client{transport: transport, catalogs: make(map[string]modelCatalog), tiers: make(map[string]BenefitTier)}
		service, err := NewPooledService(pool, client)
		if err != nil {
			t.Fatal(err)
		}
		ctx, selected := countSelections(ctx)
		data := []byte("hello")
		_, err = service.UploadFile(ctx, UploadRequest{
			Name: "a.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: bytes.NewReader(data),
		})
		return service, transport, int(selected.Load()), err
	}
	service, transport, attempts, err := upload(WithAttemptCeiling(context.Background(), 4))
	if attempts != 4 || !AccessTokenAuthFailure(err) {
		t.Fatalf("尝试 %d 个账户 err=%v，期望试满 4 个后按 GenerateAccessToken 授权失败结束", attempts, err)
	}
	transport.mu.Lock()
	calls := len(transport.calls)
	transport.mu.Unlock()
	if calls != 4 {
		t.Fatalf("GenerateAccessToken 调用 %d 次，期望 4 次", calls)
	}
	cooled := 0
	for index := range 6 {
		if service.pool.AccountCoolingDown(fmt.Sprintf("upload-%d@example.com", index), "") {
			cooled++
		}
	}
	if cooled != 4 {
		t.Fatalf("写了全局冷却的账户 %d 个，期望只有试过的 4 个", cooled)
	}
	if _, _, attempts, _ := upload(context.Background()); attempts != 6 {
		t.Fatalf("没有上限时尝试 %d 个账户，期望 6 个", attempts)
	}
}

// TestVideoAllowedAccountIDs 不固定账户的 Veo 带候选限定（app 层在出口限流期间去掉受限出口上的备用账户）时只在候选里选号，
// 每个候选最多试一次：候选都失败（已写冷却）后立即返回，不等它们的冷却到期
func TestVideoAllowedAccountIDs(t *testing.T) {
	service := ceilingService(t, 6)
	allowed := map[string]bool{"ceiling-4@example.com": true, "ceiling-5@example.com": true}
	var selected []string
	ctx := ContextWithAccountSelectionObserver(context.Background(), func(account *Account) { selected = append(selected, account.ID) })
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	startedAt := time.Now()
	if _, err := service.GenerateVideo(ctx, VideoRequest{
		Model: ceilingModel, Prompt: "a cat", AllowedAccountIDs: []string{"ceiling-4@example.com", "ceiling-5@example.com"},
	}); err == nil || ctx.Err() != nil {
		t.Fatalf("err=%v，期望候选都试过后按上游 429 结束", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("用时 %s，候选都失败后应立即返回", elapsed)
	}
	if len(selected) != 2 {
		t.Fatalf("选中 %v，期望两个候选各试一次", selected)
	}
	for _, accountID := range selected {
		if !allowed[accountID] {
			t.Fatalf("选中了候选以外的账户 %s（全部：%v）", accountID, selected)
		}
	}
}
