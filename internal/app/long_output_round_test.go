package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// long- 自动续写第二轮起本服务的期限到期只结束当前这一轮、选号只取 Playground 的测试

// longTextOnlyFrame 为只带一段正文的流帧（不带结束），用于构造写到一半的响应体
func longTextOnlyFrame(text string) string {
	quoted, _ := json.Marshal(text)
	return `[[[[[[null,` + string(quoted) + `]],"model"]]]]`
}

// tricklingBody 返回先写出 prefix、之后每 interval 写一个空白（上游一直有字节、却没有新的事件，空闲期限截不断）的响应体
func tricklingBody(prefix string, interval time.Duration) func() io.ReadCloser {
	return func() io.ReadCloser {
		reader, writer := io.Pipe()
		go func() {
			if _, err := writer.Write([]byte(prefix)); err != nil {
				return
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				if _, err := writer.Write([]byte(" ")); err != nil {
					return
				}
			}
		}()
		return reader
	}
}

// longRoundOutcome 汇总一次 long- 请求的结果：正文、结束原因、用量事件数与合并后的正文 token
type longRoundOutcome struct {
	text    string
	finish  string
	usages  int
	outputs int64
}

func summarizeLongRound(result deadlineResult) longRoundOutcome {
	var outcome longRoundOutcome
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			outcome.text += event.Text
		case aistudio.EventUsage:
			outcome.usages++
			outcome.outputs = event.Usage.OutputTokens
		case aistudio.EventFinish:
			outcome.finish = event.FinishReason
		}
	}
	return outcome
}

// TestLongOutputRuntimeRoundDeadlines 第二轮起本服务的期限到期只结束这一轮，续写体面结束：请求按 max_tokens 正常完成（不是错误、
// 状态 completed），已输出的内容（含第二轮已收到、扣住待比对接缝的正文）完整返回、用量一次，写一行原因，不写“本服务期限截断”。
// 依次为：第二轮发送后迟迟没有响应头（超过首事件后空闲期限，也验证开始发送时恢复了两轮之间暂停的空闲计时）、第二轮写出一段后停住、
// 第二轮上游一直有字节却没有新的事件直到这一轮的总时长上限
func TestLongOutputRuntimeRoundDeadlines(t *testing.T) {
	first := longFrames("第一轮的正文。", 2, [3]int64{20, 100, 120})
	for _, test := range []struct {
		name        string
		delays      map[int]time.Duration
		bodies      map[int]func() io.ReadCloser
		idle, total time.Duration
		text        string
		reason      string
		maxElapsed  time.Duration
	}{
		{
			name: "发送后没有响应头", delays: map[int]time.Duration{1: 3 * time.Second}, idle: 300 * time.Millisecond, total: 10 * time.Second,
			text: "第一轮的正文。", reason: "续写第 2 轮被本服务结束（本服务首事件后空闲期限 300ms 到期），按 max_tokens 结束（已输出的内容完整返回）",
			maxElapsed: 2 * time.Second,
		},
		{
			name: "写出一段后停住", bodies: map[int]func() io.ReadCloser{1: stallingBody(`[[` + longTextOnlyFrame("第二轮写出的一段") + `,`)},
			idle: 300 * time.Millisecond, total: 10 * time.Second,
			text: "第一轮的正文。第二轮写出的一段", reason: "续写第 2 轮被本服务结束（本服务首事件后空闲期限 300ms 到期）",
			maxElapsed: 3 * time.Second,
		},
		{
			name: "这一轮的总时长上限", bodies: map[int]func() io.ReadCloser{1: tricklingBody(`[[`+longTextOnlyFrame("第二轮写出的一段")+`,`, 50*time.Millisecond)},
			idle: 300 * time.Millisecond, total: 900 * time.Millisecond,
			text: "第一轮的正文。第二轮写出的一段", reason: "续写第 2 轮被本服务结束（本服务总时长上限 900ms 到期）",
			maxElapsed: 4 * time.Second,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &longRuntimeUpstream{streams: []string{first}, delays: test.delays, bodies: test.bodies}
			service, pool := longRuntimeService(t, upstream, 0)
			states := watchRequestStates(t, service)
			gc := newGenerateContext(service.lifecycle, time.Now(), test.idle, test.total)
			request := deadlineRequest("req-long-deadline")
			request.LongOutput, request.LongOutputDefaultTokens = true, 300
			result := runDeadlineRequest(t, service, states, request, gc)
			if result.err != nil || result.errEvents != 0 || result.state != "completed" {
				t.Fatalf("err=%v 错误事件=%d state=%s，期望第二轮被结束后按 max_tokens 正常完成: %v",
					result.err, result.errEvents, result.state, serviceLogMessages(service.requests))
			}
			outcome := summarizeLongRound(result)
			if outcome.text != test.text || outcome.finish != "max_tokens" || outcome.usages != 1 || outcome.outputs < 100 {
				t.Fatalf("结果 = %+v，期望正文 %q、按 max_tokens 结束、合并用量一次", outcome, test.text)
			}
			if elapsed := result.endedAt.Sub(result.startedAt); elapsed > test.maxElapsed {
				t.Fatalf("用时 %s，期望期限到期就结束这一轮（不超过 %s）", elapsed, test.maxElapsed)
			}
			if count := progressCount(service, test.reason); count != 1 {
				t.Fatalf("原因 %q 写了 %d 次，期望 1 次: %v", test.reason, count, serviceLogMessages(service.requests))
			}
			if progressLogged(service, "本服务期限截断") {
				t.Fatalf("第二轮被结束不应按整个请求的期限截断: %v", serviceLogMessages(service.requests))
			}
			if accounts := upstream.accounts(); len(accounts) != 2 || accounts[0] != accounts[1] {
				t.Fatalf("各轮账号 = %v，期望同一账号、不换号", accounts)
			}
			assertLeasesReleased(t, pool)
		})
	}
}

// TestLongOutputObserverDeadlines 续写进度接收方：决定发下一轮时从此刻重新计算总时长上限并登记这一轮的结束函数（首事件后空闲期限到期时
// 只结束这一轮一次），离绝对总时长上限不到一个首事件后空闲期限时立即结束这一轮（不再发下一轮）；这一轮结束后清除登记
func TestLongOutputObserverDeadlines(t *testing.T) {
	service, _ := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	run := func(longLimit time.Duration) (*generateContext, func(aistudio.ContinuationProgress), chan error) {
		t.Helper()
		gc := newGenerateContext(service.lifecycle, time.Now(), time.Second, 2*time.Second)
		t.Cleanup(gc.cancel)
		if !gc.beginStreaming() {
			t.Fatal("切到首事件后失败")
		}
		if longLimit > 0 {
			gc.mu.Lock()
			gc.longLimit = longLimit
			gc.mu.Unlock()
		}
		observer := service.continuationObserver(continuationAttempt{
			requestID: "req-observer", accountLabel: "alice@example.com", phase: gc, activity: &upstreamActivity{},
		})
		return gc, observer, make(chan error, 4)
	}
	gc, observer, interrupted := run(0)
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Round: 1, Next: true, Interrupt: func(err error) { interrupted <- err }})
	select {
	case err := <-interrupted:
		t.Fatalf("离绝对总时长上限还远时不应结束这一轮: %v", err)
	default:
	}
	reason := errors.New("空闲期限到期")
	if !gc.interruptLongRound(reason) || <-interrupted != reason {
		t.Fatal("登记后空闲期限到期应只结束这一轮")
	}
	if gc.interruptLongRound(reason) {
		t.Fatal("这一轮已经结束过一次，再到期应按原规则结束整个请求")
	}
	// 下一轮重新登记：又可以只结束这一轮一次
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Round: 2, Next: true, Interrupt: func(err error) { interrupted <- err }})
	if !gc.interruptLongRound(reason) || <-interrupted != reason {
		t.Fatal("新的一轮登记后空闲期限到期应只结束这一轮")
	}
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Round: 3, Next: true, Interrupt: func(err error) { interrupted <- err }})
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Round: 4, Next: false, Stop: "上游结束"})
	if gc.interruptLongRound(reason) || gc.Err() != nil {
		t.Fatalf("不再续写后应清除登记，err=%v", gc.Err())
	}
	select {
	case err := <-interrupted:
		t.Fatalf("不应结束已经结束的一轮: %v", err)
	default:
	}

	_, observer, interrupted = run(300 * time.Millisecond)
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Round: 1, Next: true, Interrupt: func(err error) { interrupted <- err }})
	select {
	case err := <-interrupted:
		if !strings.Contains(err.Error(), "离续写的绝对总时长上限 300ms 不到 1s，不再续写") {
			t.Fatalf("结束原因 = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("离绝对总时长上限不到一个空闲期限时应立即结束这一轮")
	}
}

// longChannelService 返回两个账号都能经 Playground 与 Build 调用 streamTestModel 的生成服务（上游记录每次的账号与通道）
func longChannelService(t *testing.T, accounts ...string) (*trackedService, *aistudio.AccountPool, *channelRecorder) {
	t.Helper()
	recorder := &channelRecorder{}
	service, pool := firstEventTestService(t, recorder, 0, accounts...)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	pool.SetPerAccountConcurrency(1)
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
	}
	preverifyModelAccess(t, pool, accounts...)
	return service, pool, recorder
}

// runLongChannelRequest 执行一次 long- 请求，返回这次请求的各次尝试（“账号/通道”）
func runLongChannelRequest(t *testing.T, service *trackedService, recorder *channelRecorder, id string) []string {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: id, Model: streamTestModel, LongOutput: true, LongOutputDefaultTokens: 300,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	before := len(recorder.called())
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	for event := range events {
		if event.Kind == aistudio.EventError {
			t.Fatalf("请求失败: %v", event.Err)
		}
	}
	return recorder.called()[before:]
}

// TestLongOutputSelectsPlaygroundAcrossAccounts long- 请求先在所有账号的 Playground 里选号：某个账号的 Playground 冷却、它的 Build 可用时
// 取另一个账号的 Playground（轮询多次都不落到冷却账号的 Build）；所有账号的 Playground 都不可用时才退回 Build（按普通请求处理，写一行说明）
func TestLongOutputSelectsPlaygroundAcrossAccounts(t *testing.T) {
	const accountA, accountB = "long-a@example.com", "long-b@example.com"
	service, pool, recorder := longChannelService(t, accountA, accountB)
	cool := func(accountID string) {
		t.Helper()
		// 检查时间晚于预先写入的模型资格（一小时后），冷却才会写入；只冷却 Playground（作用域为模型本身），Build 仍可用
		checkedAt := time.Now().Add(90 * time.Minute)
		write, err := pool.TryMarkCooldownIfGeneration(accountID, streamTestModel, pool.ModelAccessGeneration(accountID), checkedAt, checkedAt.Add(2*time.Hour), "测试冷却")
		if err != nil || write != aistudio.CooldownWritten {
			t.Fatalf("写入冷却: %v %v", write, err)
		}
		if !service.pool.AccountChannelAvailable(accountID, aistudio.AccountSelection{ModelID: streamTestModel, Method: "generateContent", BuildOnly: true}) {
			t.Fatal("测试要求该账号的 Build 仍可用")
		}
	}
	cool(accountA)
	for index := range 4 {
		if calls := runLongChannelRequest(t, service, recorder, "req-long-select"); !slices.Equal(calls, []string{accountB + "/playground"}) {
			t.Fatalf("第 %d 次尝试 = %v，期望取另一个账号的 Playground", index+1, calls)
		}
	}
	if progressLogged(service, longOutputBuildNote) {
		t.Fatal("取到 Playground 时不应写经 Build 发送的说明")
	}
	cool(accountB)
	calls := runLongChannelRequest(t, service, recorder, "req-long-fallback")
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/build") || !progressLogged(service, longOutputBuildNote) {
		t.Fatalf("尝试 = %v，期望所有 Playground 都冷却时退回 Build 并写一行说明", calls)
	}
}

// TestLongOutputNotUsedAsBuildProbe Build 生成暂停期间，到了试探时间且超过 buildProbeRelax 仍没有别的请求可试时，long- 请求也不拿来
// 做 Build 试探（Build 上不续写），照常走 Playground；同样时刻的普通请求才拿来试探
func TestLongOutputNotUsedAsBuildProbe(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Minute + buildProbeRelax)
	upstream.recovered.Store(true)
	request := aistudio.GenerateRequest{
		ID: "req-long-probe", Model: buildOffShared, LongOutput: true, LongOutputDefaultTokens: 300,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	for range events {
	}
	if calls := upstream.called(); len(calls) != 1 || !strings.HasSuffix(calls[0], "/playground") || !pool.BuildGenerationPaused() {
		t.Fatalf("long- 请求的尝试 = %v，期望不拿来做 Build 试探", calls)
	}
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-plain-probe"); err != nil ||
		len(calls) != 1 || !strings.HasSuffix(calls[0], "/build") {
		t.Fatalf("普通请求的尝试 = %v err = %v，期望拿来试探（确认测试条件成立）", calls, err)
	}
}

// TestContinuationObserverMergedRules 续写进度接收方与批次 B 的共用规则：等待号池准入的进度行写出与重新生成不准入时相同写法的原因
// （每分钟名额、出口令牌或冷却截止，见 aistudio/resend_admission.go）；决定发下一轮时置上“已进入续写后续轮次”的标记，之后的请求阶段
// 不再写阶段日志、不启动首事件计时（续写各轮不重新起算首事件计时）
func TestContinuationObserverMergedRules(t *testing.T) {
	service, _ := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	gc := newGenerateContext(service.lifecycle, time.Now(), time.Second, 2*time.Second)
	t.Cleanup(gc.cancel)
	if !gc.beginStreaming() {
		t.Fatal("切到首事件后失败")
	}
	rounds := &atomic.Bool{}
	observer := service.continuationObserver(continuationAttempt{
		requestID: "req-merged", accountLabel: "alice@example.com", phase: gc, activity: &upstreamActivity{}, rounds: rounds,
	})
	observer(aistudio.ContinuationProgress{Stage: aistudio.ContinuationRoundEnd, Mode: aistudio.ContinuationSimulated, Round: 1, Next: true, Interrupt: func(error) {}})
	if !rounds.Load() {
		t.Fatal("决定发下一轮时应置上已进入续写后续轮次的标记")
	}
	detail := "每分钟名额 3/3（已发出 3、预占 0，ACCOUNT_RPM_LIMIT）"
	observer(aistudio.ContinuationProgress{
		Stage: aistudio.ContinuationWaiting, Mode: aistudio.ContinuationSimulated, Round: 2, Wait: 12 * time.Second,
		WaitReason: aistudio.ContinuationWaitRate, WaitDetail: detail,
	})
	if !progressLogged(service, "自动续写第 2 轮等待账号每分钟窗口 | 方式=模拟续写 | 原因="+detail+" | 等待=12s") {
		t.Fatal("等待进度行应写出与重新生成不准入时相同写法的原因")
	}
}
