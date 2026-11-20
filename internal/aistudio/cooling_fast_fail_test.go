package aistudio

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// 数据接口选号（AcquireFor）候选全部冷却时的快速 429（上游 Mag1cFall/AIStudio2API cd3d4ab 1A、6d8ad7d 7C 期限部分）：
// 最早恢复超过 CooldownQueueLimit 减去已等的冷却、或晚于 ctx 期限时立即返回 AllCoolingError；忙碌、每分钟请求已满等短等待照旧排队

const coolingFastModel = "gemini-cooling-fast"

// coolingFastPool 返回有 accountIDs 账户、单账户并发为 concurrency 的号池，目录里有 coolingFastModel（generateContent、countTokens）
func coolingFastPool(t *testing.T, concurrency int, accountIDs ...string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, concurrency)
	t.Cleanup(pool.WaitBackground)
	model := Model{ID: coolingFastModel, Methods: []string{"generateContent", "countTokens"}}
	for _, accountID := range accountIDs {
		account, publish, err := store.Create(DefaultAccountConfig(accountID), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{model}); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// coolGlobally 给账户写入 d 的全局冷却，返回恢复时间
func coolGlobally(t *testing.T, pool *AccountPool, accountID string, d time.Duration) time.Time {
	t.Helper()
	until := time.Now().Add(d)
	if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额: test"); err != nil {
		t.Fatal(err)
	}
	return until
}

// acquireTimed 取一次租约，返回租约、错误与用时（取得的租约在测试结束时释放）
func acquireTimed(t *testing.T, ctx context.Context, pool *AccountPool, selection AccountSelection) (*AccountLease, error, time.Duration) {
	t.Helper()
	startedAt := time.Now()
	lease, err := pool.AcquireFor(ctx, selection)
	if lease != nil {
		t.Cleanup(func() { _ = lease.Release() })
	}
	return lease, err, time.Since(startedAt)
}

// TestAcquireForAllCoolingFastFail 唯一账户冷却 3 小时（线上实测：countTokens 原先等满 3 秒期限以 504 结束）：按模型、按指定账户选号都立即返回
// AllCoolingError（429，恢复时间为冷却结束）；冷却 20 秒、ctx 500ms 后到期时恢复晚于期限，同样立即返回
func TestAcquireForAllCoolingFastFail(t *testing.T) {
	const accountID = "cool-long@example.com"
	for _, item := range []struct {
		name     string
		cooldown time.Duration
		timeout  time.Duration
	}{
		{"冷却 3 小时", 3 * time.Hour, 3 * time.Second},
		{"恢复晚于期限", 20 * time.Second, 500 * time.Millisecond},
	} {
		t.Run(item.name, func(t *testing.T) {
			pool := coolingFastPool(t, 2, accountID)
			until := coolGlobally(t, pool, accountID, item.cooldown)
			for _, selection := range []AccountSelection{
				{ModelID: coolingFastModel, Method: "countTokens", ModelAccessScope: ModelAccessKey("count-tokens", coolingFastModel)},
				{AccountID: accountID},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), item.timeout)
				_, err, elapsed := acquireTimed(t, ctx, pool, selection)
				cancel()
				var cooling *AllCoolingError
				if !errors.As(err, &cooling) {
					t.Fatalf("%+v：err = %v，期望全部冷却", selection, err)
				}
				if elapsed >= 300*time.Millisecond {
					t.Fatalf("%+v：%s 后才返回，期望立即返回", selection, elapsed)
				}
				if cooling.HTTPStatus() != http.StatusTooManyRequests || cooling.RetryAt().Sub(until).Abs() > time.Second {
					t.Fatalf("%+v：状态 %d、恢复 %s，期望 429、%s", selection, cooling.HTTPStatus(), cooling.RetryAt(), until)
				}
			}
		})
	}
}

// TestAcquireForShortCoolingWaits 冷却在 1 分钟内、早于期限恢复时照旧排队等到恢复取得租约
func TestAcquireForShortCoolingWaits(t *testing.T) {
	const accountID = "cool-short@example.com"
	pool := coolingFastPool(t, 2, accountID)
	coolGlobally(t, pool, accountID, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err, elapsed := acquireTimed(t, ctx, pool, AccountSelection{ModelID: coolingFastModel, Method: "countTokens"})
	if err != nil || lease == nil {
		t.Fatalf("短冷却应等到恢复取得租约，实际 %v", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("%s 就取得了租约，期望等到冷却结束", elapsed)
	}
}

// TestAcquireForBusyNotCooling 还有没冷却、只是忙碌的候选（另一个候选冷却 3 小时）时不按全部冷却返回，照旧排队等它空出
func TestAcquireForBusyNotCooling(t *testing.T) {
	const busyID, coolID = "busy@example.com", "cooling@example.com"
	pool := coolingFastPool(t, 1, busyID, coolID)
	coolGlobally(t, pool, coolID, 3*time.Hour)
	held, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: busyID})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = held.Release() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, selection := range []AccountSelection{
		{ModelID: coolingFastModel, Method: "countTokens"},
		{AccountID: busyID},
	} {
		lease, err, _ := acquireTimed(t, ctx, pool, selection)
		if err != nil || lease == nil || lease.Account().ID != busyID {
			t.Fatalf("%+v：忙碌的候选空出后应取得它，实际 lease=%v err=%v", selection, lease, err)
		}
		_ = lease.Release()
	}
}

// TestAcquireForRateLimitedNotCooling 候选只因每分钟请求已满（ACCOUNT_RPM_LIMIT）取不到、另一个候选冷却 3 小时：不按全部冷却返回，
// 照旧等窗口释放（ctx 到期时以 ctx 的错误结束）
func TestAcquireForRateLimitedNotCooling(t *testing.T) {
	const fullID, coolID = "rpm-full@example.com", "rpm-cool@example.com"
	pool := coolingFastPool(t, 2, fullID, coolID)
	coolGlobally(t, pool, coolID, 3*time.Hour)
	pool.SetAccountRateLimit(1)
	selection := AccountSelection{ModelID: coolingFastModel, Method: "generateContent"}
	first, err := pool.AcquireFor(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	first.RecordGenerationSent()
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err, _ = acquireTimed(t, ctx, pool, selection)
	var cooling *AllCoolingError
	if errors.As(err, &cooling) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("每分钟请求已满时 err = %v，期望照旧排队到期限", err)
	}
}

// TestAcquireForCoolingCumulative 一次选号里冷却排队按累计计算（经 context 把上限注入为 1 秒）：先等约 600ms 的冷却，冷却被延长到
// 再过约 700ms 才恢复时，已等的加上还要等的超过上限，立即返回 AllCoolingError，不再接着等（原先每次复查各自按上限判断）
func TestAcquireForCoolingCumulative(t *testing.T) {
	const accountID = "cool-cumulative@example.com"
	pool := coolingFastPool(t, 2, accountID)
	coolGlobally(t, pool, accountID, 600*time.Millisecond)
	extend := time.AfterFunc(400*time.Millisecond, func() {
		now := time.Now()
		if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), now, now.Add(900*time.Millisecond), "每日限额: test"); err != nil {
			t.Error(err)
		}
	})
	defer extend.Stop()
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), cooldownQueueLimitKey{}, time.Second), 5*time.Second)
	defer cancel()
	lease, err, elapsed := acquireTimed(t, ctx, pool, AccountSelection{ModelID: coolingFastModel, Method: "countTokens"})
	var cooling *AllCoolingError
	if lease != nil || !errors.As(err, &cooling) {
		t.Fatalf("累计超过上限时应按全部冷却返回，实际 lease=%v err=%v", lease, err)
	}
	if elapsed >= 1100*time.Millisecond {
		t.Fatalf("%s 后才返回，期望在冷却被延长后立即返回", elapsed)
	}
}

// TestUploadFileSkipsCoolingAccounts 文件上传的候选里全局冷却的账户排在最后；候选都在冷却（等不到）时立即返回 AllCoolingError，不发上传请求
func TestUploadFileSkipsCoolingAccounts(t *testing.T) {
	const coolA, freeB = "a-cooling@example.com", "b-free@example.com"
	service, transport := driveTestService(t, coolA, freeB)
	coolGlobally(t, service.pool, coolA, 3*time.Hour)
	if ids := service.pool.fileUploadAccountIDs(PoolScopeAll); !slices.Equal(ids, []string{freeB, coolA}) {
		t.Fatalf("候选顺序 = %v，期望冷却中的账户排在最后", ids)
	}
	upload := func() (FileRef, error, time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		startedAt := time.Now()
		file, err := service.UploadFile(ctx, UploadRequest{
			Name: "notes.txt", MIME: "text/plain", Purpose: "assistants", Size: 5, Reader: strings.NewReader("hello"),
		})
		return file, err, time.Since(startedAt)
	}
	if file, err, _ := upload(); err != nil || file.ID == "" || transport.tokenCalls(freeB) != 1 || transport.tokenCalls(coolA) != 0 {
		t.Fatalf("应由没冷却的账户上传：file=%+v err=%v", file, err)
	}
	coolGlobally(t, service.pool, freeB, 2*time.Hour)
	_, err, elapsed := upload()
	var cooling *AllCoolingError
	if !errors.As(err, &cooling) || elapsed >= 300*time.Millisecond {
		t.Fatalf("候选都在冷却时 err = %v（%s），期望立即按全部冷却返回", err, elapsed)
	}
	if transport.tokenCalls(freeB) != 1 || transport.tokenCalls(coolA) != 0 {
		t.Fatal("候选都在冷却时不应发出请求")
	}
	if remaining := time.Until(cooling.Until); remaining > 2*time.Hour || remaining < 110*time.Minute {
		t.Fatalf("恢复时间应取最早恢复的候选，还剩 %s", remaining)
	}
}

// TestCoolingQueueExceeded 按固定时刻判断：距恢复超过 CooldownQueueLimit 减去已等的冷却、或恢复晚于 ctx 期限时不再排队
func TestCoolingQueueExceeded(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	background := context.Background()
	if coolingQueueExceeded(background, now.Add(59*time.Second), now, 0) || !coolingQueueExceeded(background, now.Add(61*time.Second), now, 0) {
		t.Fatal("没有已等的冷却时以 1 分钟为界")
	}
	if !coolingQueueExceeded(background, now.Add(15*time.Second), now, 50*time.Second) ||
		coolingQueueExceeded(background, now.Add(9*time.Second), now, 50*time.Second) {
		t.Fatal("已等 50s 时只剩 10s 可等")
	}
	deadline, cancel := context.WithDeadline(background, now.Add(5*time.Second))
	defer cancel()
	if !coolingQueueExceeded(deadline, now.Add(10*time.Second), now, 0) || coolingQueueExceeded(deadline, now.Add(3*time.Second), now, 0) {
		t.Fatal("恢复晚于期限时不应排队，早于期限时照常排队")
	}
}
