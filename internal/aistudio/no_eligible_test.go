package aistudio

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

const testNoEligibleModel = "test-model"

// testCatalogPool 返回一个带单个账户与目录的账户池
func testCatalogPool(t *testing.T, models ...Model) (*AccountPool, string) {
	t.Helper()
	const accountID = "alice@example.com"
	pool := testPoolWithAccount(t, accountID)
	if len(models) == 0 {
		models = []Model{{ID: testNoEligibleModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"}}}
	}
	if err := pool.SetCatalog(accountID, BenefitTierFree, models); err != nil {
		t.Fatal(err)
	}
	return pool, accountID
}

// assertPoolNotReady 断言错误属于号池一侧的暂时性原因（503），且仍满足无账户哨兵
func assertPoolNotReady(t *testing.T, err error) {
	t.Helper()
	var notReady *AccountsNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("err = %v，期望号池暂时不可调度错误", err)
	}
	if !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("号池暂时不可调度错误应满足 ErrNoEligibleAccount: %v", err)
	}
	if notReady.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", notReady.HTTPStatus())
	}
}

// TestNoEligibleEmptyPoolIsTransient 号池为空或目录尚未加载属于号池一侧的原因，不能按模型不存在或请求错误返回
func TestNoEligibleEmptyPoolIsTransient(t *testing.T) {
	ctx := context.Background()
	selection := AccountSelection{ModelID: testNoEligibleModel, Method: "generateContent"}

	empty := NewAccountPool(nil, 1)
	_, err := empty.AcquireFor(ctx, selection)
	assertPoolNotReady(t, err)
	_, err = empty.ClassifyCandidates(ctx, selection, nil)
	assertPoolNotReady(t, err)
	assertPoolNotReady(t, empty.NoEligibleError(AccountSelection{}))

	var missing *AccountPool
	_, err = missing.AcquireFor(ctx, selection)
	assertPoolNotReady(t, err)

	// 有账户但目录尚未同步完成
	withoutCatalog := testPoolWithAccount(t, "bob@example.com")
	_, err = withoutCatalog.AcquireFor(ctx, selection)
	assertPoolNotReady(t, err)
	_, err = withoutCatalog.ClassifyCandidates(ctx, selection, nil)
	assertPoolNotReady(t, err)
}

// TestNoEligibleAuthRequiredIsTransient 支持该模型的账户都需要重新登录时按 503 返回，原因里列出账户状态
func TestNoEligibleAuthRequiredIsTransient(t *testing.T) {
	pool, accountID := testCatalogPool(t)
	if err := pool.MarkAuthRequired(accountID, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.AcquireFor(context.Background(), AccountSelection{ModelID: testNoEligibleModel, Method: "generateContent"})
	assertPoolNotReady(t, err)
	var notReady *AccountsNotReadyError
	errors.As(err, &notReady)
	if len(notReady.Reasons) != 1 || notReady.Reasons[0] != accountID+" 需要重新登录（Cookie 失效）" {
		t.Fatalf("原因 = %v，期望列出需要重新登录的账户", notReady.Reasons)
	}
}

// TestNoEligibleUnsupportedIsClientError 目录里有该模型但没有任何账户的权益能调用时，仍按请求本身的原因返回
func TestNoEligibleUnsupportedIsClientError(t *testing.T) {
	pool, _ := testCatalogPool(t, Model{ID: testNoEligibleModel, Methods: []string{"generateContent"}, AccessModes: []int64{4}})
	_, err := pool.AcquireFor(context.Background(), AccountSelection{ModelID: testNoEligibleModel, Method: "generateContent"})
	if !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("err = %v，期望没有账户支持该模型", err)
	}
	var notReady *AccountsNotReadyError
	if errors.As(err, &notReady) {
		t.Fatalf("没有账户支持该模型不是号池暂时性原因: %v", err)
	}
}

// TestNoCandidateErrorCooling 候选为空且有账户在冷却时返回全部冷却错误，带最早恢复时间
func TestNoCandidateErrorCooling(t *testing.T) {
	pool, accountID := testCatalogPool(t)
	until := time.Now().Add(2 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	selection := AccountSelection{ModelID: testNoEligibleModel, ModelAccessScope: testNoEligibleModel, Method: "bidiGenerateContent"}
	groups, err := pool.ClassifyCandidates(context.Background(), selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cooling *AllCoolingError
	if err := pool.NoCandidateError(selection, groups); !errors.As(err, &cooling) {
		t.Fatalf("err = %v，期望全部冷却错误", err)
	}
	if cooling.RetryAt().Sub(until).Abs() > time.Second {
		t.Fatalf("最早恢复时间 = %s，期望 %s", cooling.RetryAt(), until)
	}
	if cooling.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429", cooling.HTTPStatus())
	}
}

// TestAccountsNotReadyKeepsCause 号池暂时不可调度错误同时保留无账户哨兵与内部原因
func TestAccountsNotReadyKeepsCause(t *testing.T) {
	cause := errors.New("runtime 被占用")
	err := error(PoolNotReady("候选账户被其他进程占用", cause))
	if !errors.Is(err, ErrNoEligibleAccount) || !errors.Is(err, cause) {
		t.Fatalf("应同时满足无账户哨兵与内部原因: %v", err)
	}
}
