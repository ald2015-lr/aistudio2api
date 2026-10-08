package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const testRuntimeModel = "test-model"

// testAccountPool 在临时目录创建带目录的账户，返回账户池与账户列表
func testAccountPool(t *testing.T, accountIDs ...string) (*aistudio.AccountPool, []*aistudio.Account) {
	t.Helper()
	store := aistudio.NewAccountStore(t.TempDir())
	pool := aistudio.NewAccountPool(nil, 2)
	accounts := make([]*aistudio.Account, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		cookies := make([]aistudio.StateCookie, 0, 3)
		for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
			cookies = append(cookies, aistudio.StateCookie{
				Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
			})
		}
		account, publish, err := store.Create(aistudio.DefaultAccountConfig(accountID), aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(account.ID, aistudio.BenefitTierFree, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
		}}); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	return pool, accounts
}

// TestCandidateErrorsByReason Live 与转录在没有候选时按原因区分：账户都需要重新登录按号池暂时不可调度（503），
// 全部冷却按全部冷却（429，带最早恢复时间），不再一律按没有账户支持该模型返回
func TestCandidateErrorsByReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, "alice@example.com")
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, 1, 1, 1, false)
	defer workers.Close()
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: workers}

	accountID := accounts[0].ID
	until := time.Now().Add(2 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	var cooling *aistudio.AllCoolingError
	if _, err := service.bidiCandidates(ctx, testRuntimeModel, testRuntimeModel); !errors.As(err, &cooling) || cooling.Until.Sub(until).Abs() > time.Second {
		t.Fatalf("Live 全部冷却：err = %v，期望带最早恢复时间的全部冷却错误", err)
	}
	if _, err := service.transcriptionCandidates(ctx, testRuntimeModel); !errors.As(err, &cooling) {
		t.Fatalf("转录全部冷却：err = %v，期望全部冷却错误", err)
	}

	if err := pool.MarkAuthRequired(accountID, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	var notReady *aistudio.AccountsNotReadyError
	if _, err := service.bidiCandidates(ctx, testRuntimeModel, testRuntimeModel); !errors.As(err, &notReady) {
		t.Fatalf("Live 账户需要重新登录：err = %v，期望号池暂时不可调度错误", err)
	}
	if _, err := service.transcriptionCandidates(ctx, testRuntimeModel); !errors.As(err, &notReady) {
		t.Fatalf("转录账户需要重新登录：err = %v，期望号池暂时不可调度错误", err)
	}
}

// TestGenerateWithoutAccountsIsTransient 号池为空时生成请求按号池暂时不可调度结束（对外 503），不按模型不存在
func TestGenerateWithoutAccountsIsTransient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	pool := aistudio.NewAccountPool(nil, 1)
	workers := newAccountWorkerManager(pool, nil, requests, "", "", time.Minute, 1, 1, 1, false)
	defer workers.Close()
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	request := aistudio.GenerateRequest{
		ID: "req-empty-pool", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(ctx, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	var got error
	for event := range events {
		if event.Kind == aistudio.EventError {
			got = event.Err
		}
	}
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(got, &notReady) || !errors.Is(got, aistudio.ErrNoEligibleAccount) {
		t.Fatalf("err = %v，期望号池暂时不可调度错误", got)
	}
}
