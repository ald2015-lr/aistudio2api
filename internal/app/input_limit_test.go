package app

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	inputLimitA = "limit-a@example.com"
	inputLimitB = "limit-b@example.com"
)

// inputLimitUpstream 是测试用的上游：每次尝试都返回"输入 token 超过上限"，记录账号与通道
type inputLimitUpstream struct {
	channelRecorder
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
	return nil, &aistudio.RPCError{Method: method, StatusCode: upstream.status, Code: upstream.code, Message: upstream.message}
}

// inputLimitService 返回两个账号的生成服务：A 两个通道都能调用 streamTestModel，B 只能经 Playground 调用；
// 按 channels 启用上游通道
func inputLimitService(t *testing.T, upstream *inputLimitUpstream, channels []aistudio.Channel) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := firstEventTestService(t, upstream, 0, inputLimitA, inputLimitB)
	pool.SetUpstreamChannels(channels)
	pool.SetPerAccountConcurrency(1)
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range []string{inputLimitA, inputLimitB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.SetBuildCatalog(inputLimitA, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	service.setStreamPlaygroundModels([]string{streamTestModel})
	preverifyModelAccess(t, pool, inputLimitA, inputLimitB)
	return service, pool
}

// TestInputTokenLimitNoSwitchNoCooldown 上游按 429 返回"输入 token 超过上限"（部分账号的 131072 上限）时：
// Playground 超限只改走同一账号的 Build 再试一次，Build 也超限就按原错误结束，不换到其他账号、不冷却账号、不退避；
// 只启用 Build 时按 400 写法返回的同类错误同样只尝试一次
func TestInputTokenLimitNoSwitchNoCooldown(t *testing.T) {
	for _, test := range []struct {
		name      string
		channels  []aistudio.Channel
		status    int
		code      int64
		message   string
		wantCalls []string
	}{
		{
			name: "两个通道都启用、上游按 429 返回", channels: []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild},
			status: http.StatusTooManyRequests, code: 8, message: "The input token count exceeds the maximum number of tokens allowed 131072",
			wantCalls: []string{inputLimitA + "/playground", inputLimitA + "/build"},
		},
		{
			name: "只启用 Build、上游按 400 返回", channels: []aistudio.Channel{aistudio.ChannelBuild},
			status: http.StatusBadRequest, code: 3, message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).",
			wantCalls: []string{inputLimitA + "/build"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &inputLimitUpstream{status: test.status, code: test.code, message: test.message}
			service, pool := inputLimitService(t, upstream, test.channels)
			_, elapsed, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, service.lifecycle, true, nil)
			if !aistudio.InputTokenLimitError(err) {
				t.Fatalf("err = %v，期望按输入超限的原错误结束", err)
			}
			if calls := upstream.called(); !slices.Equal(calls, test.wantCalls) {
				t.Fatalf("尝试 = %v，期望 %v（不换到其他账号）", calls, test.wantCalls)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("用时 %s，输入超限不应退避等待", elapsed)
			}
			for _, accountID := range []string{inputLimitA, inputLimitB} {
				status, _ := pool.StatusOf(accountID)
				if status.State != aistudio.AccountReady {
					t.Fatalf("账号 %s 状态 = %s，期望输入超限不冷却账号", accountID, status.State)
				}
				for scope, cooldown := range status.Cooldowns {
					if cooldown.Active(time.Now()) {
						t.Fatalf("账号 %s 的 %q 处于冷却（%s），期望输入超限不冷却", accountID, scope, cooldown.Reason)
					}
				}
			}
			if !progressLogged(service, "提示词过长") {
				t.Fatal("请求进度里应写明输入超过模型上限（提示词过长）、按 400 返回")
			}
			if len(test.channels) == 2 && !progressLogged(service, "改用 Build 通道重试") {
				t.Fatal("请求进度里应写明 Playground 超限改走 Build")
			}
		})
	}
}
