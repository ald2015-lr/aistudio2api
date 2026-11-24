package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// forbiddenDenied 返回号池级 403 判定在 (pool, model) 下记着的账户；exists 为这个键有记录（含成功时间）
func forbiddenDenied(service *trackedService, pool aistudio.PoolScope, model string) (accounts []string, exists bool) {
	service.forbidden.mu.Lock()
	defer service.forbidden.mu.Unlock()
	state, exists := service.forbidden.scopes[forbiddenScopeKey{pool: pool, modelID: model}]
	for _, denial := range state.denied {
		accounts = append(accounts, denial.accountID)
	}
	return accounts, exists
}

// rateLimited 返回号池级限流判定在 (pool, model) 下记着的账户与最近一次成功的时间；exists 为这个键有记录
func rateLimited(service *trackedService, pool aistudio.PoolScope, model string) (accounts []string, lastSuccess time.Time, exists bool) {
	service.rateLimits.mu.Lock()
	defer service.rateLimits.mu.Unlock()
	state, exists := service.rateLimits.scopes[rateLimitScopeKey{pool: pool, modelID: model}]
	for _, failure := range state.failed {
		accounts = append(accounts, failure.accountID)
	}
	return accounts, state.lastSuccess, exists
}

func permissionDenied() error {
	return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
}

// TestExtraBorrowedGuardKey 跨号池借来的账户（默认地址借 Pro 成员）返回单模型 403 与裸限流 429：号池级判定记在 (extra, model) 下，
// (normal, model) 没有记录；成功同理记在补充池下
func TestExtraBorrowedGuardKey(t *testing.T) {
	normal, lent := "guard-normal@example.com", "guard-pro@example.com"
	for _, test := range []struct {
		name  string
		err   error
		check func(t *testing.T, env *borrowEnv)
	}{
		{name: "单模型 403", err: permissionDenied(), check: func(t *testing.T, env *borrowEnv) {
			if got, _ := forbiddenDenied(env.service, aistudio.PoolScopeExtra, testRuntimeModel); !slices.Equal(got, []string{lent}) {
				t.Fatalf("(extra, model) 下的 403 = %v，期望 %s", got, lent)
			}
			if _, exists := forbiddenDenied(env.service, aistudio.PoolScopeNormal, testRuntimeModel); exists {
				t.Fatal("(normal, model) 不应有记录")
			}
		}},
		{name: "裸限流", err: bareRateLimitError(), check: func(t *testing.T, env *borrowEnv) {
			if got, _, _ := rateLimited(env.service, aistudio.PoolScopeExtra, testRuntimeModel); !slices.Equal(got, []string{lent}) {
				t.Fatalf("(extra, model) 下的裸限流 = %v，期望 %s", got, lent)
			}
			if _, _, exists := rateLimited(env.service, aistudio.PoolScopeNormal, testRuntimeModel); exists {
				t.Fatal("(normal, model) 不应有记录")
			}
		}},
		{name: "成功", check: func(t *testing.T, env *borrowEnv) {
			if _, lastSuccess, _ := rateLimited(env.service, aistudio.PoolScopeExtra, testRuntimeModel); lastSuccess.IsZero() {
				t.Fatal("成功应记在 (extra, model) 下")
			}
			if _, exists := forbiddenDenied(env.service, aistudio.PoolScopeExtra, testRuntimeModel); !exists {
				t.Fatal("号池级 403 的成功应记在 (extra, model) 下")
			}
			if _, _, exists := rateLimited(env.service, aistudio.PoolScopeNormal, testRuntimeModel); exists {
				t.Fatal("(normal, model) 不应有记录")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
			env.cool(t, lent)
			env.promote(t, normal)
			env.lend(t, lent)
			release := occupy(t, env.pool, normal)
			defer release()
			if test.err != nil {
				env.upstream.failAccount(lent, test.err)
				// 借来的账户失败后只剩忙碌的主号池：请求排到期限结束
				_, _ = env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 1500*time.Millisecond)
			} else if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second); err != nil || account != lent {
				t.Fatalf("服务账户 %q、错误 %v", account, err)
			}
			if calls := env.upstream.accounts(0); len(calls) == 0 || calls[0] != lent {
				t.Fatalf("上游调用 %v，期望先借 %s", calls, lent)
			}
			test.check(t, env)
		})
	}
}

// TestExtraHomeLeaseGuardKey /pro 用自己号池内账户的补充池 Worker（不跨号池）：号池级判定照旧记在 (pro, model) 下
func TestExtraHomeLeaseGuardKey(t *testing.T) {
	home, lent := "homeguard-pro-a@example.com", "homeguard-pro-b@example.com"
	env := newBorrowEnv(t, nil, nil, []string{home, lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, home)
	env.lend(t, lent)
	release := occupy(t, env.pool, home)
	defer release()
	env.upstream.failAccount(lent, permissionDenied())
	_, _ = env.generate(t, aistudio.PoolScopePro, testRuntimeModel, 1500*time.Millisecond)
	if calls := env.upstream.accounts(0); len(calls) == 0 || calls[0] != lent {
		t.Fatalf("上游调用 %v，期望先借 %s", calls, lent)
	}
	if got, _ := forbiddenDenied(env.service, aistudio.PoolScopePro, testRuntimeModel); !slices.Equal(got, []string{lent}) {
		t.Fatalf("(pro, model) 下的 403 = %v，期望 %s", got, lent)
	}
	if _, exists := forbiddenDenied(env.service, aistudio.PoolScopeExtra, testRuntimeModel); exists {
		t.Fatal("本路由的账户不应记在 (extra, model) 下")
	}
}

// TestExtraReplayCrossPool 跨号池借来的 Worker 在发送中失败、可恢复：用 Pool=Extra 在原账号重放成功，重新启动计入补充池；
// 补充池关闭后不在原账号重放，改为换号
func TestExtraReplayCrossPool(t *testing.T) {
	normal, lent := "replay-normal@example.com", "replay-pro@example.com"
	setup := func(t *testing.T) (*borrowEnv, func()) {
		env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
		env.cool(t, lent)
		env.promote(t, normal)
		env.lend(t, lent)
		return env, occupy(t, env.pool, normal)
	}
	t.Run("在原账号重放", func(t *testing.T) {
		env, release := setup(t)
		defer release()
		env.upstream.failAccount(lent, waaRuntimeFailure("GenerateContent"))
		launches := len(env.launches())
		account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
		if err != nil || account != lent || !slices.Equal(env.upstream.accounts(0), []string{lent, lent}) {
			t.Fatalf("服务账户 %q、错误 %v、上游调用 %v，期望 Worker 重建后在 %s 上重放成功", account, err, env.upstream.accounts(0), lent)
		}
		if got := env.launches()[launches:]; !slices.Equal(got, []string{lent}) {
			t.Fatalf("重放应重新启动原账号一次：%v", got)
		}
		if !env.manager.extraOwnedHas(lent) || !slices.Equal(env.in(aistudio.PoolScopeExtra), []string{lent}) {
			t.Fatal("重放时的重新启动应计入补充池")
		}
	})
	t.Run("补充池关闭后换号", func(t *testing.T) {
		env, release := setup(t)
		env.upstream.beforeAccount(lent, func(context.Context, aistudio.GenerateRequest) error {
			env.disable()
			release()
			return waaRuntimeFailure("GenerateContent")
		})
		account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
		if err != nil || account != normal || !slices.Equal(env.upstream.accounts(0), []string{lent, normal}) {
			t.Fatalf("服务账户 %q、错误 %v、上游调用 %v，期望补充池关闭后换到 %s", account, err, env.upstream.accounts(0), normal)
		}
		if countLogsAt(env.requests, "WARN", extraReplaySwitchMessage) != 1 {
			t.Fatal("应写一行不在原账号重放的说明")
		}
	})
}

// TestExtraReopenMemberGone 借来的 Worker 发送时已关闭、账户已不是成员：跨号池借来的返回 errExtraMemberGone 并换号（请求成功）；
// 本路由的账户按家分区重开
func TestExtraReopenMemberGone(t *testing.T) {
	// gone 让第一次被借的账户在发送前恢复为完全可用、关闭它的 Worker，再按发送时的方式取 Worker（要重开），返回取 Worker 的结果
	gone := func(t *testing.T, env *borrowEnv, accountIDs ...string) *sync.Map {
		results := &sync.Map{}
		var once sync.Once
		for _, accountID := range accountIDs {
			env.upstream.beforeAccount(accountID, func(ctx context.Context, request aistudio.GenerateRequest) error {
				var err error
				ran := false
				once.Do(func() {
					ran = true
					env.recover(t, request.AccountID)
					if resetErr := env.manager.Reset(request.AccountID); resetErr != nil {
						t.Error(resetErr)
					}
					_, err = env.manager.Worker(ctx, request.AccountID, request.Model)
					results.Store(request.AccountID, err)
				})
				if !ran {
					return nil
				}
				return err
			})
		}
		return results
	}
	t.Run("跨号池", func(t *testing.T) {
		normal, lentA, lentB := "gone-normal@example.com", "gone-pro-a@example.com", "gone-pro-b@example.com"
		env := newBorrowEnv(t, []string{normal}, nil, []string{lentA, lentB}, 2, 2)
		env.cool(t, lentA, lentB)
		env.promote(t, normal)
		env.lend(t, lentA, lentB)
		release := occupy(t, env.pool, normal)
		defer release()
		results := gone(t, env, lentA, lentB)
		account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
		calls := env.upstream.accounts(0)
		if err != nil || len(calls) != 2 || calls[0] == calls[1] || account != calls[1] {
			t.Fatalf("服务账户 %q、错误 %v、上游调用 %v，期望换到另一个补充池 Worker 成功", account, err, calls)
		}
		value, _ := results.Load(calls[0])
		if reopenErr, _ := value.(error); !errors.Is(reopenErr, errExtraMemberGone) {
			t.Fatalf("重开 = %v，期望 errExtraMemberGone", reopenErr)
		}
		if env.workerOf(calls[0]) != nil {
			t.Fatal("已不是成员的跨号池账户不应重开")
		}
	})
	t.Run("本路由", func(t *testing.T) {
		home, lent := "gonehome-pro-a@example.com", "gonehome-pro-b@example.com"
		env := newBorrowEnv(t, nil, nil, []string{home, lent}, 1, 1)
		env.cool(t, lent)
		env.promote(t, home)
		env.lend(t, lent)
		release := occupy(t, env.pool, home)
		defer release()
		results := gone(t, env, lent)
		account, err := env.generate(t, aistudio.PoolScopePro, testRuntimeModel, 20*time.Second)
		if err != nil || account != lent {
			t.Fatalf("服务账户 %q、错误 %v，期望在 %s 上按家分区重开后成功", account, err, lent)
		}
		if value, _ := results.Load(lent); value != nil {
			t.Fatalf("按家分区重开失败: %v", value)
		}
		if env.manager.extraOwnedHas(lent) || !slices.Contains(env.in(aistudio.PoolScopePro), lent) {
			t.Fatal("已不是成员的本路由账户应按家分区重开")
		}
	})
}

// TestExtraExcludedRequests 不借用的选号不做补充池分类：固定账户、带资源、不等待、带限定集合、Build 试探、请求本身不能借用（引用文件或视频、
// 需要 Drive、降级判定中）与号池不是请求号池的选号；生成请求指定账户时不建借用计划。Live、转写、Veo 与上传不经 generateForModel，
// 不会带借用计划
func TestExtraExcludedRequests(t *testing.T) {
	normal, lent := "exclude-normal@example.com", "exclude-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	scoped := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal)
	base := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	selectWith := func(t *testing.T, eligible bool, selection aistudio.AccountSelection, immediate bool) int64 {
		t.Helper()
		before := env.manager.extraClassified.Load()
		ctx, cancel := context.WithTimeout(withExtraPlan(scoped, env.service.newExtraPlan(scoped, aistudio.PoolScopeNormal, testRuntimeModel, eligible)), 2*time.Second)
		defer cancel()
		if lease, _ := env.service.acquireLease(ctx, selection, immediate, nil); lease != nil {
			_ = lease.Release()
		}
		return env.manager.extraClassified.Load() - before
	}
	for _, test := range []struct {
		name      string
		eligible  bool
		selection func() aistudio.AccountSelection
		immediate bool
	}{
		{name: "固定账户", eligible: true, selection: func() aistudio.AccountSelection {
			selection := base
			selection.AccountID = normal
			return selection
		}},
		{name: "带资源", eligible: true, selection: func() aistudio.AccountSelection {
			selection := base
			selection.ResourceID = "files/extra-test"
			return selection
		}},
		{name: "不等待", eligible: true, selection: func() aistudio.AccountSelection { return base }, immediate: true},
		{name: "限定集合", eligible: true, selection: func() aistudio.AccountSelection {
			selection := base
			selection.AllowedAccountIDs = []string{normal}
			return selection
		}},
		{name: "Build 试探", eligible: true, selection: func() aistudio.AccountSelection {
			selection := base
			selection.BuildOnly, selection.BuildProbe = true, true
			return selection
		}},
		{name: "请求不能借用", eligible: false, selection: func() aistudio.AccountSelection { return base }},
		{name: "号池不是请求号池", eligible: true, selection: func() aistudio.AccountSelection {
			selection := base
			selection.Pool = aistudio.PoolScopePro
			return selection
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectWith(t, test.eligible, test.selection(), test.immediate); got != 0 {
				t.Fatalf("不借用的选号做了 %d 次补充池分类", got)
			}
		})
	}
	t.Run("指定账户的生成请求", func(t *testing.T) {
		before := env.manager.extraClassified.Load()
		request := aistudio.GenerateRequest{ID: "req-extra-fixed", Model: testRuntimeModel, AccountID: normal,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}}
		requestCtx, cancel := context.WithTimeout(scoped, 10*time.Second)
		defer cancel()
		events := make(chan aistudio.Event, 8)
		go env.service.generateWithRetry(scoped, requestCtx, cancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
		for event := range events {
			if event.Kind == aistudio.EventError {
				t.Fatal(event.Err)
			}
		}
		if got := env.manager.extraClassified.Load() - before; got != 0 {
			t.Fatalf("指定账户的请求做了 %d 次补充池分类", got)
		}
	})
	t.Run("对照：普通选号会借用分类", func(t *testing.T) {
		if got := selectWith(t, true, base, false); got == 0 {
			t.Fatal("普通选号应做补充池分类（对照）")
		}
	})
}

// TestExtraCountTokensOnlyModel 默认地址的 count_tokens 请求只能由补充池服务的付费模型：L 非空时只在 L 里选号；L 为空时按 503 返回；
// 主号池自己能服务的模型不借用
func TestExtraCountTokensOnlyModel(t *testing.T) {
	normal, lent := "count-normal@example.com", "count-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	var mu sync.Mutex
	var served []string
	env.upstream.count = func(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
		lease, err := env.pool.AcquireFor(ctx, aistudio.AccountSelection{
			ModelID: request.Model, Method: "countTokens", Pool: aistudio.PoolScopeFromContext(ctx),
		})
		if err != nil {
			return aistudio.TokenCount{}, err
		}
		mu.Lock()
		served = append(served, lease.Account().ID)
		mu.Unlock()
		_ = lease.Release()
		return aistudio.TokenCount{InputTokens: 1}, nil
	}
	count := func(model string) error {
		ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), 5*time.Second)
		defer cancel()
		_, err := env.service.CountTokens(ctx, aistudio.TokenCountRequest{Model: model})
		return err
	}
	env.promote(t, normal)
	err := count(borrowPaidModel)
	var noServing *aistudio.NoServingAccountError
	if !errors.As(err, &noServing) || noServing.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("L 为空：err = %v，期望 503", err)
	}
	env.cool(t, lent)
	env.lend(t, lent)
	if err := count(borrowPaidModel); err != nil {
		t.Fatal(err)
	}
	if err := count(testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(served, []string{lent, normal}) {
		t.Fatalf("count_tokens 的账户 = %v，期望付费模型借 %s、主号池的模型用 %s", served, lent, normal)
	}
}

// fallbackBorrowEnv 为 MODEL_FALLBACKS 与补充池一起的环境：Ultra 账户在 argon 上冷却（有 carbon），Pro 成员的补充池 Worker 能跑 argon；
// argon 为 Ultra 主用模型（候选全部冷却时立即返回），MODEL_FALLBACKS 为默认（argon 改用 carbon，只在 /ultra 生效）
func fallbackBorrowEnv(t *testing.T) (*borrowEnv, string, string) {
	t.Helper()
	ultra, lent := "fb-ultra@example.com", "fb-pro@example.com"
	env := newBorrowEnv(t, []string{"fb-normal@example.com"}, []string{ultra}, []string{lent}, 1, 1)
	setUltraPrimaryModels(env.service, []string{borrowPaidModel}, 10*time.Second)
	env.service.setModelFallbacks(config.DefaultModelFallbacks, config.ModelFallbackScopeUltra)
	env.promote(t, ultra)
	coolModelFor(t, env.pool, ultra, borrowPaidModel, 3*time.Hour)
	env.cool(t, lent)
	env.lend(t, lent)
	return env, ultra, lent
}

// TestExtraBeforeModelFallback /ultra 请求 argon、Ultra 全部冷却、补充池里有能跑 argon 的 Pro 成员：由补充池服务，不改用 carbon；
// 补充池也不行时照原规则改用 carbon
func TestExtraBeforeModelFallback(t *testing.T) {
	env, ultra, lent := fallbackBorrowEnv(t)
	if account, err := env.generate(t, aistudio.PoolScopeUltra, borrowPaidModel, 10*time.Second); err != nil || account != lent {
		t.Fatalf("服务账户 %q、错误 %v，期望补充池的 %s 跑 argon", account, err, lent)
	}
	for _, call := range env.upstream.called() {
		if call.model != borrowPaidModel {
			t.Fatalf("补充池能服务时不应改用其他模型：%+v", call)
		}
	}
	release := occupy(t, env.pool, lent)
	defer release()
	before := len(env.upstream.called())
	if account, err := env.generate(t, aistudio.PoolScopeUltra, borrowPaidModel, 10*time.Second); err != nil || account != ultra {
		t.Fatalf("服务账户 %q、错误 %v，期望补充池也不行时改用 carbon", account, err)
	}
	if calls := env.upstream.called()[before:]; len(calls) != 1 || calls[0].model != borrowUltraModel {
		t.Fatalf("上游调用 %+v，期望只有改用的 carbon", calls)
	}
}

// TestExtraRouteScopeNeverRewritten 借用前后请求的号池不变（上游调用时 context 上的号池为路由号池）；MODEL_FALLBACKS 改用后号池同样不变
func TestExtraRouteScopeNeverRewritten(t *testing.T) {
	normal, lent := "scope-normal@example.com", "scope-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	release := occupy(t, env.pool, normal)
	if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second); err != nil || account != lent {
		t.Fatalf("服务账户 %q、错误 %v", account, err)
	}
	release()
	for _, call := range env.upstream.called() {
		if call.scope != aistudio.PoolScopeNormal {
			t.Fatalf("借用后请求的号池 = %v，期望仍是普通号池", call.scope)
		}
	}
	fallback, _, lentPro := fallbackBorrowEnv(t)
	releasePro := occupy(t, fallback.pool, lentPro)
	defer releasePro()
	if _, err := fallback.generate(t, aistudio.PoolScopeUltra, borrowPaidModel, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, call := range fallback.upstream.called() {
		if call.scope != aistudio.PoolScopeUltra {
			t.Fatalf("改用其他模型后请求的号池 = %v，期望仍是 Ultra 号池", call.scope)
		}
	}
}

// TestExtraMaxAttempts 默认地址只有 1 个可调度账户、补充池有 4 个可借出的 Worker：换号上限计入补充池（5 次）；可借出的更多时不超过每请求硬上限 8
func TestExtraMaxAttempts(t *testing.T) {
	cooling := func() error { return fmt.Errorf("发出前已冷却: %w", aistudio.ErrAccountCoolingDown) }
	run := func(t *testing.T, members int, failures int) (*borrowEnv, error) {
		normal := "attempts-normal@example.com"
		proIDs := extraIDsOf("attempts-pro", members)
		env := newBorrowEnv(t, []string{normal}, nil, proIDs, members, members)
		env.cool(t, proIDs...)
		env.promote(t, normal)
		env.lend(t, proIDs...)
		for range failures {
			env.upstream.failFirst = append(env.upstream.failFirst, cooling())
		}
		_, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
		return env, err
	}
	t.Run("计入补充池", func(t *testing.T) {
		env, err := run(t, 4, 4)
		accounts := env.upstream.accounts(0)
		if err != nil || len(accounts) != 5 || len(sortedUnique(accounts)) != 5 {
			t.Fatalf("错误 %v、上游调用 %v，期望前 4 个账号失败后第 5 个成功", err, accounts)
		}
	})
	t.Run("不超过硬上限", func(t *testing.T) {
		env, err := run(t, 9, 20)
		if err == nil || len(env.upstream.accounts(0)) != generateSwitchCeiling {
			t.Fatalf("错误 %v、上游调用 %d 次，期望 %d 次后结束", err, len(env.upstream.accounts(0)), generateSwitchCeiling)
		}
	})
}

// borrowHandler 返回经公开 API 处理链的 handler（ULTRA_EXCLUSIVE=true、PRO_POOL=true）与访问日志捕获
func borrowHandler(env *borrowEnv) (http.Handler, *accessLogCapture) {
	admin := &accessLogCapture{}
	return api.NewHandler(env.service, api.Config{
		APIKey: "sk-test", Admin: admin,
		UltraExclusive: func() bool { return true }, ProPool: func() bool { return true },
	}), admin
}

// postChat 经处理链发出一个 OpenAI 对话请求，返回响应与最后一条访问日志
func postChat(t *testing.T, handler http.Handler, admin *accessLogCapture, model string) (*httptest.ResponseRecorder, api.AccessLog) {
	t.Helper()
	recorder, entries := postFallbackTo(t, handler, admin, "/v1/chat/completions",
		`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	if len(entries) == 0 {
		t.Fatalf("没有访问日志，状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	return recorder, entries[len(entries)-1]
}

// TestAliasRefreshOnExtraToggle 切换 EXTRA_POOL_DEFAULT_PAID 后，默认地址上付费模型的后缀写法（gemini-4-argon-medium）立即能解析或不能解析：
// 别名快照按号池规则版本重建
func TestAliasRefreshOnExtraToggle(t *testing.T) {
	normal, lent := "alias-normal@example.com", "alias-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	handler, admin := borrowHandler(env)
	alias := borrowPaidModel + "-medium"
	if recorder, entry := postChat(t, handler, admin, alias); recorder.Code != http.StatusOK || entry.Model != borrowPaidModel || entry.Account != lent {
		t.Fatalf("开启：状态 %d、模型 %q、账户 %q，响应 %s", recorder.Code, entry.Model, entry.Account, recorder.Body.String())
	}
	version := env.service.PoolRuleVersion()
	env.configure(config.ExtraPoolModeShare, false, false, 1, 1)
	if env.service.PoolRuleVersion() == version {
		t.Fatal("切换 EXTRA_POOL_DEFAULT_PAID 应递增号池规则版本")
	}
	if recorder, _ := postChat(t, handler, admin, alias); recorder.Code != http.StatusNotFound {
		t.Fatalf("关闭默认地址借付费模型后：状态 %d，响应 %s，期望 404", recorder.Code, recorder.Body.String())
	}
	env.configure(config.ExtraPoolModeShare, true, false, 1, 1)
	if recorder, entry := postChat(t, handler, admin, alias); recorder.Code != http.StatusOK || entry.Model != borrowPaidModel {
		t.Fatalf("再打开：状态 %d、模型 %q，响应 %s", recorder.Code, entry.Model, recorder.Body.String())
	}
	// 生成入口与 count_tokens 的别名回退：本路由解析不到时按补充池的目录解析
	if err := env.pool.SetCatalog(lent, aistudio.BenefitTierPro, []aistudio.Model{
		borrowChat(testRuntimeModel), borrowChat(extraCoolModel),
		{ID: borrowPaidModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
			CapabilityOptions: map[string][]string{"aliases": {"argon-latest"}}},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal)
	if got := env.service.canonicalModelFor(ctx, "argon-latest"); got != borrowPaidModel {
		t.Fatalf("默认地址的别名 = %q，期望按补充池的目录解析为 %s", got, borrowPaidModel)
	}
	env.configure(config.ExtraPoolModeShare, false, false, 1, 1)
	if got := env.service.canonicalModelFor(ctx, "argon-latest"); got != "argon-latest" {
		t.Fatalf("关闭默认地址借付费模型后别名 = %q，期望原样", got)
	}
}

// TestExtraServedMarked 由补充池服务的请求：请求日志与访问日志 extra=true，请求时间线与 /trace 有“由补充池服务”行，活动请求 extra=true；
// 借用的尝试失败、最后由主号池服务的请求为 false，失败的那次尝试在 attempts 里 extra=true；没有借用的请求不出现 extra
func TestExtraServedMarked(t *testing.T) {
	normal, lent := "marked-normal@example.com", "marked-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	handler, admin := borrowHandler(env)

	// 没有借用：补充池 Worker 忙、主号池空闲，不出现 extra
	releaseLent := occupy(t, env.pool, lent)
	if recorder, entry := postChat(t, handler, admin, testRuntimeModel); recorder.Code != http.StatusOK || entry.Account != normal || entry.Extra {
		t.Fatalf("主号池服务：状态 %d、账户 %q、extra=%t", recorder.Code, entry.Account, entry.Extra)
	}
	releaseLent()
	if data, _ := json.Marshal(requestLogData(api.AccessLog{RequestID: "x"})); strings.Contains(string(data), `"extra"`) {
		t.Fatalf("没有借用的请求日志不应出现 extra：%s", data)
	}

	// 由补充池服务：活动请求、访问日志与请求日志都标 extra
	release := occupy(t, env.pool, normal)
	held := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	env.upstream.beforeAccount(lent, func(context.Context, aistudio.GenerateRequest) error {
		once.Do(func() { close(entered) })
		<-held
		return nil
	})
	done := make(chan api.AccessLog, 1)
	go func() {
		_, entry := postChat(t, handler, admin, testRuntimeModel)
		done <- entry
	}()
	<-entered
	active := false
	for _, request := range env.requests.list() {
		active = active || request.Extra
	}
	close(held)
	entry := <-done
	if !active {
		t.Fatal("借用中的活动请求应标 extra")
	}
	if entry.Account != lent || !entry.Extra {
		t.Fatalf("由补充池服务：账户 %q、extra=%t", entry.Account, entry.Extra)
	}
	if data, _ := json.Marshal(requestLogData(entry)); !strings.Contains(string(data), `"extra":true`) {
		t.Fatalf("请求日志应带 extra：%s", data)
	}
	if countLogsAt(env.requests, "INFO", "由补充池服务 | 方式=分担 | 账号所在号池=Pro 号池") == 0 {
		t.Fatal("请求时间线应有“由补充池服务”行")
	}

	// 借用的尝试失败、最后由主号池服务：结果为 false，失败的尝试为 true
	env.upstream.beforeAccount(lent, func(context.Context, aistudio.GenerateRequest) error {
		release()
		return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13, Message: "Internal error encountered."}
	})
	_, entry = postChat(t, handler, admin, testRuntimeModel)
	if entry.Account != normal || entry.Extra || len(entry.Attempts) != 1 || !entry.Attempts[0].Extra {
		t.Fatalf("中途借过、最后由主号池服务：账户 %q、extra=%t、尝试 %+v", entry.Account, entry.Extra, entry.Attempts)
	}

	// /trace：排查记录里有“由补充池服务”行
	env.upstream.beforeAccount(lent, nil)
	releaseAgain := occupy(t, env.pool, normal)
	defer releaseAgain()
	trace := aistudio.NewTrace()
	parent := aistudio.ContextWithTrace(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), trace)
	if account, err := env.generateCtx(t, parent, testRuntimeModel, 10*time.Second, ""); err != nil || account != lent {
		t.Fatalf("服务账户 %q、错误 %v", account, err)
	}
	data, err := trace.Finish()
	if err != nil || !strings.Contains(string(data), "由补充池服务 | 方式=分担") {
		t.Fatalf("排查记录应有“由补充池服务”行：%v", err)
	}
}

// TestExtraLogTexts 请求侧的日志文本、级别与去重（§5.3）：由补充池服务（分担、补充）、overflow 第一次借用时写一次原因、排队进度里的补充池原因
// 与计数（只在借用生效时追加）
func TestExtraLogTexts(t *testing.T) {
	normal, lentA, lentB := "logs-normal@example.com", "logs-pro-a@example.com", "logs-pro-b@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lentA, lentB}, 2, 2)
	env.configure(config.ExtraPoolModeOverflow, true, false, 2, 2)
	env.cool(t, lentA, lentB)
	env.promote(t, normal)
	env.lend(t, lentA, lentB)
	release := occupy(t, env.pool, normal)
	defer release()
	// overflow：第一次借用的账户失败、再借另一个：原因只写一次，“由补充池服务”每次借用写一次
	env.upstream.failFirst = []error{&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13, Message: "Internal error encountered."}}
	if _, err := env.generateCtx(t, aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), testRuntimeModel, 20*time.Second, "req-logs-overflow"); err != nil {
		t.Fatal(err)
	}
	if got := countLogsAt(env.requests, "INFO", "主号池没有可用账号，改用补充池 | 原因=账号全部忙碌"); got != 1 {
		t.Fatalf("overflow 的原因写了 %d 行，期望每个请求一行", got)
	}
	if got := countSourceLogs(env.requests, "", "由补充池服务 | 方式=补充 | 账号所在号池=Pro 号池"); got != 2 {
		t.Fatalf("“由补充池服务”写了 %d 行，期望每次借用一行", got)
	}
	if countSourceLogs(env.requests, lentA, "由补充池服务 | 方式=补充")+countSourceLogs(env.requests, lentB, "由补充池服务 | 方式=补充") != 2 {
		t.Fatal("“由补充池服务”的来源应为借到的账户")
	}
	// 排队进度：补充池能借的都在忙，主号池没有可等的（全部冷却）：原因为补充池 Worker 全部忙碌，末尾追加补充池计数
	coolModelFor(t, env.pool, normal, testRuntimeModel, 2*time.Hour)
	releaseA, releaseB := occupy(t, env.pool, lentA), occupy(t, env.pool, lentB)
	defer releaseA()
	defer releaseB()
	progress := newLeaseWaitProgress("req-logs-queue")
	progress.firstDelay = 10 * time.Millisecond
	ctx := withLeaseWaitProgress(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), progress)
	if _, err := env.acquire(t, ctx, testRuntimeModel, 1500*time.Millisecond); err == nil {
		t.Fatal("补充池都忙时应排队")
	}
	if countLogsAt(env.requests, "INFO", "排队等待 | 原因="+waitReasonExtraBusy, "| 补充池空闲=0 | 补充池忙碌=2") == 0 {
		t.Fatal("排队进度应写补充池 Worker 全部忙碌并追加补充池计数")
	}
	// 补充池关闭：排队进度不追加补充池计数
	env.disable()
	progress = newLeaseWaitProgress("req-logs-off")
	progress.firstDelay = 10 * time.Millisecond
	ctx = withLeaseWaitProgress(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), progress)
	_, _ = env.acquire(t, ctx, testRuntimeModel, 1200*time.Millisecond)
	env.requests.mu.Lock()
	defer env.requests.mu.Unlock()
	for _, entry := range env.requests.logs {
		if entry.Request != nil && entry.Request.ID == "req-logs-off" && strings.Contains(entry.Message, "补充池") {
			t.Fatalf("补充池关闭时排队进度不应写补充池：%s", entry.Message)
		}
	}
}
