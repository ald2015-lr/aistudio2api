package app

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	inputLimitA = "limit-a@example.com" // Free，Playground 与 Build 都能调用
	inputLimitB = "limit-b@example.com" // Free，只能经 Playground 调用
	inputLimitU = "limit-u@example.com" // Ultra，Playground 与 Build 都能调用
)

const (
	inputCapMessage   = "The input token count exceeds the maximum number of tokens allowed 131072"
	inputLimitMessage = "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576)."
)

// inputLimitUpstream 是测试用的上游：failing 中的账号每次都返回"输入 token 超过上限"，其余账号正常返回一段正文；记录账号与通道
type inputLimitUpstream struct {
	channelRecorder
	failing map[string]bool
	status  int
	code    int64
	message string
}

func (upstream *inputLimitUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel, method := "", "GenerateContent"
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
		if lease.Channel() == aistudio.ChannelBuild {
			method = "ProxyStreamedCall"
		}
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	upstream.mu.Unlock()
	if !upstream.failing[request.AccountID] {
		events := make(chan aistudio.Event, 2)
		events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
		events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
		close(events)
		return events, nil
	}
	return nil, &aistudio.RPCError{Method: method, StatusCode: upstream.status, Code: upstream.code, Message: upstream.message}
}

// inputLimitAccount 描述测试账号：权益与可调用 streamTestModel 的通道
type inputLimitAccount struct {
	tier              aistudio.BenefitTier
	playground, build bool
	buildCooling      bool // Build 通道预先进入长时间冷却
}

// inputLimitService 按 accounts 创建账号与目录，启用 channels；流式优先 Playground 让第一次尝试稳定落在 Playground 上。
// 不预写模型资格：账号真的被冷却时 StatusOf 必须能看到（预写未来时间会让 MarkCooldownIfGeneration 静默跳过）
func inputLimitService(t *testing.T, upstream *inputLimitUpstream, channels []aistudio.Channel, accounts map[string]inputLimitAccount) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	service, pool := firstEventTestService(t, upstream, 0, ids...)
	pool.SetUpstreamChannels(channels)
	pool.SetPerAccountConcurrency(1)
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, id := range ids {
		spec := accounts[id]
		models := []aistudio.Model{base}
		if spec.playground {
			models = append(models, target)
		}
		if err := pool.SetCatalog(id, spec.tier, models); err != nil {
			t.Fatal(err)
		}
		if spec.build {
			if err := pool.SetBuildCatalog(id, []aistudio.Model{target}); err != nil {
				t.Fatal(err)
			}
		}
		if spec.buildCooling {
			lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: id})
			if err != nil {
				t.Fatal(err)
			}
			generation, checkedAt := lease.ModelAccessGeneration(), lease.CheckedAt()
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			scope := aistudio.ChannelCooldownScope(aistudio.ChannelBuild, streamTestModel)
			if err := pool.MarkCooldownIfGeneration(id, scope, generation, checkedAt, time.Now().Add(2*time.Hour), "每日限额"); err != nil {
				t.Fatal(err)
			}
		}
	}
	service.setStreamPlaygroundModels([]string{streamTestModel})
	return service, pool
}

// assertNoInputLimitCooldown 断言这些账号都就绪且没有新的生效冷却（allowScope 为测试预先设置的冷却范围）
func assertNoInputLimitCooldown(t *testing.T, pool *aistudio.AccountPool, allowScope string, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		status, _ := pool.StatusOf(accountID)
		if status.State != aistudio.AccountReady {
			t.Fatalf("账号 %s 状态 = %s，期望输入超限不冷却账号", accountID, status.State)
		}
		for scope, cooldown := range status.Cooldowns {
			if scope != allowScope && cooldown.Active(time.Now()) {
				t.Fatalf("账号 %s 的 %q 处于冷却（%s），期望输入超限不冷却", accountID, scope, cooldown.Reason)
			}
		}
	}
}

// TestInputTokenLimitNoSwitchNoCooldown 输入 token 超过上限时不冷却账号、不按一般失败换号，按原错误结束（对外 400）：
//   - 部分账号的上限（429 写法）：Playground 超限改走同一账号的 Build 再试一次，Build 也超限就结束，不换到其他 Free 账号
//   - 模型本身的上限（带计数的 400 写法）：直接结束，不改走 Build
//   - 只启用 Build 时同样只尝试一次
//   - Build 通道在长时间冷却中：改走 Build 的再试没有立即可用的账号，仍按输入超限结束，不等冷却、不返回全部冷却
//   - 号池里有更高权益的账号：Free 账号两个通道都超限后改用 Ultra 账号再试一次并完成
func TestInputTokenLimitNoSwitchNoCooldown(t *testing.T) {
	both := []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}
	freeA := inputLimitAccount{tier: aistudio.BenefitTierFree, playground: true, build: true}
	freeB := inputLimitAccount{tier: aistudio.BenefitTierFree, playground: true}
	for _, test := range []struct {
		name          string
		channels      []aistudio.Channel
		accounts      map[string]inputLimitAccount
		status        int
		code          int64
		message       string
		wantCalls     []string
		wantText      bool
		wantProgress  []string
		avoidProgress []string
	}{
		{
			name: "账号上限 429：Playground 改走 Build 一次后结束", channels: both,
			accounts: map[string]inputLimitAccount{inputLimitA: freeA, inputLimitB: freeB},
			status:   http.StatusTooManyRequests, code: 8, message: inputCapMessage,
			wantCalls:    []string{inputLimitA + "/playground", inputLimitA + "/build"},
			wantProgress: []string{"改用 Build 通道重试", "提示词过长"},
		},
		{
			name: "模型上限 400：不改走 Build", channels: both,
			accounts: map[string]inputLimitAccount{inputLimitA: freeA, inputLimitB: freeB},
			status:   http.StatusBadRequest, code: 3, message: inputLimitMessage,
			wantCalls:    []string{inputLimitA + "/playground"},
			wantProgress: []string{"提示词过长"}, avoidProgress: []string{"改用 Build 通道重试"},
		},
		{
			name: "只启用 Build：只尝试一次", channels: []aistudio.Channel{aistudio.ChannelBuild},
			accounts: map[string]inputLimitAccount{inputLimitA: freeA, inputLimitB: freeB},
			status:   http.StatusTooManyRequests, code: 8, message: inputCapMessage,
			wantCalls:    []string{inputLimitA + "/build"},
			wantProgress: []string{"提示词过长"},
		},
		{
			name: "Build 冷却中：再试没有立即可用的账号仍按输入超限结束", channels: both,
			accounts: map[string]inputLimitAccount{
				inputLimitA: {tier: aistudio.BenefitTierFree, playground: true, build: true, buildCooling: true}, inputLimitB: freeB,
			},
			status: http.StatusTooManyRequests, code: 8, message: inputCapMessage,
			wantCalls:    []string{inputLimitA + "/playground"},
			wantProgress: []string{"改用 Build 通道重试", "没有立即可用的账号，按 400 返回"},
		},
		{
			name: "有更高权益的账号：改用 Ultra 再试一次并完成", channels: both,
			accounts: map[string]inputLimitAccount{
				inputLimitA: freeA, inputLimitU: {tier: aistudio.BenefitTierUltra, playground: true, build: true},
			},
			status: http.StatusTooManyRequests, code: 8, message: inputCapMessage,
			wantCalls: []string{inputLimitA + "/playground", inputLimitA + "/build", inputLimitU + "/"},
			wantText:  true, wantProgress: []string{"改用更高权益的账号再试一次"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &inputLimitUpstream{failing: map[string]bool{inputLimitA: true}, status: test.status, code: test.code, message: test.message}
			service, pool := inputLimitService(t, upstream, test.channels, test.accounts)
			_, elapsed, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, service.lifecycle, true, nil)
			if test.wantText {
				if err != nil {
					t.Fatalf("err = %v，期望在更高权益账号上完成", err)
				}
			} else if !aistudio.InputTokenLimitError(err) {
				t.Fatalf("err = %v，期望按输入超限的原错误结束", err)
			}
			calls := upstream.called()
			if len(calls) != len(test.wantCalls) {
				t.Fatalf("尝试 = %v，期望 %v", calls, test.wantCalls)
			}
			for index, want := range test.wantCalls {
				if calls[index] != want && !(strings.HasSuffix(want, "/") && strings.HasPrefix(calls[index], want)) {
					t.Fatalf("尝试 = %v，期望 %v", calls, test.wantCalls)
				}
			}
			if elapsed > 3*time.Second {
				t.Fatalf("用时 %s，输入超限不应退避或排队等待", elapsed)
			}
			ids := make([]string, 0, len(test.accounts))
			for id := range test.accounts {
				ids = append(ids, id)
			}
			assertNoInputLimitCooldown(t, pool, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, streamTestModel), ids...)
			for _, text := range test.wantProgress {
				if !progressLogged(service, text) {
					t.Fatalf("请求进度里应包含 %q", text)
				}
			}
			for _, text := range test.avoidProgress {
				if progressLogged(service, text) {
					t.Fatalf("请求进度里不应包含 %q", text)
				}
			}
		})
	}
}
