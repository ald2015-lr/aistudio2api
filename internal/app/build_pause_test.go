package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

const (
	// buildOffShared 能经 Playground 与 Build 调用（Playground 目录由各测试决定哪些账号有），buildOffOnly 只在 Build 目录里
	buildOffShared = "buildoff-shared"
	buildOffOnly   = "buildoff-only"
	buildOffA      = "buildoff-a@example.com"
	buildOffB      = "buildoff-b@example.com"
	buildOffC      = "buildoff-c@example.com"
	buildOffD      = "buildoff-d@example.com"

	buildOffStreamMessage = "PROXY_GENERATION_DISABLED: streamGenerateContent is not supported"
	buildOffUnaryMessage  = "PROXY_GENERATION_DISABLED: generateContent is not supported"

	// buildOffNoBackoff 为“不退避”的用时上限：换号前最短的退避为 200ms（generateRetryBackoff），整个请求用时低于它说明一次也没有退避
	buildOffNoBackoff = 150 * time.Millisecond
)

// buildOffUpstream 是测试用的上游：Build 通道返回 PROXY_GENERATION_DISABLED（trailer 为真时作为首个错误事件，即流尾状态；
// 否则作为错误响应；unary 为真时按单次代理调用），recovered 后 Build 正常；playgroundCap 为真时 Playground 返回账号输入上限，
// playgroundQuota 为真时 Playground 返回每日额度 429；buildFailing 中的账号的 Build 返回 503（先于 PROXY_GENERATION_DISABLED）。
// 成功时正文为“hello from <通道>”
type buildOffUpstream struct {
	channelRecorder
	trailer         bool
	unary           bool
	playgroundCap   bool
	playgroundQuota bool
	buildFailing    []string
	recovered       atomic.Bool
}

// buildOffQuotaError 为 Playground 的每日额度 429
func buildOffQuotaError() error {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
	}
}

func (upstream *buildOffUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	upstream.mu.Unlock()
	switch {
	case channel == string(aistudio.ChannelBuild) && slices.Contains(upstream.buildFailing, request.AccountID):
		return nil, &aistudio.RPCError{
			Method: "ProxyStreamedCall", StatusCode: http.StatusServiceUnavailable, Code: 14, Message: "The service is currently unavailable.",
		}
	case channel == string(aistudio.ChannelBuild) && !upstream.recovered.Load():
		method, message := "ProxyStreamedCall", buildOffStreamMessage
		if upstream.unary {
			method, message = "ProxyUnaryCall", buildOffUnaryMessage
		}
		err := &aistudio.RPCError{Method: method, StatusCode: http.StatusBadRequest, Code: 3, Message: message}
		if !upstream.trailer {
			return nil, err
		}
		events := make(chan aistudio.Event, 1)
		events <- aistudio.Event{Kind: aistudio.EventError, Err: err}
		close(events)
		return events, nil
	case channel == string(aistudio.ChannelPlayground) && upstream.playgroundCap:
		return nil, &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: inputCapMessage}
	case channel == string(aistudio.ChannelPlayground) && upstream.playgroundQuota:
		return nil, buildOffQuotaError()
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello from " + channel}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// buildOffService 建立账号：playground 中的账号能经 Playground 调用 buildOffShared，全部账号的 Build 目录有 buildOffShared 与
// buildOffOnly；按 fill-first 选号、Build 在前，账号按 ID 顺序。Build 暂停的时钟可调、试探间隔没有抖动
func buildOffService(t *testing.T, upstream *buildOffUpstream, channels []aistudio.Channel, accounts []string, playground []string) (*trackedService, *aistudio.AccountPool, *time.Time) {
	t.Helper()
	service, pool := firstEventTestService(t, upstream, 0, accounts...)
	pool.SetUpstreamChannels(channels)
	pool.SetRoutingStrategy("fill-first")
	shared := aistudio.Model{ID: buildOffShared, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	only := aistudio.Model{ID: buildOffOnly, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range accounts {
		models := []aistudio.Model{base}
		if slices.Contains(playground, accountID) {
			models = append(models, shared)
		}
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, models); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{shared, only}); err != nil {
			t.Fatal(err)
		}
	}
	// 预写未来的模型资格：请求结束后的后台写入不再改动账户状态文件，不与删除临时目录竞争
	future := time.Now().Add(time.Hour)
	for _, accountID := range accounts {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		generation := lease.ModelAccessGeneration()
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		for _, model := range []string{buildOffShared, buildOffOnly} {
			for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
				if _, err := pool.MarkModelAccessVerifiedIfGeneration(accountID, aistudio.ChannelCooldownScope(channel, model), generation, future); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	now := time.Now()
	tracker := newBuildPauseTracker(pool, service.requests)
	tracker.now = func() time.Time { return now }
	tracker.jitter = func(time.Duration) time.Duration { return 0 }
	service.buildPause = tracker
	return service, pool, &now
}

// runBuildOffRequest 执行一次生成，返回正文、错误、这次请求的尝试（“账号/通道”）与用时
func runBuildOffRequest(t *testing.T, service *trackedService, upstream *buildOffUpstream, model string, stream bool, id string) (string, error, []string, time.Duration) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: id, Model: model, Stream: stream,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	before := len(upstream.called())
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	startedAt := time.Now()
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, startedAt, request, "", events, newGenerationDiagnostics(request, "未发送"))
	var text string
	var err error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	return text, err, upstream.called()[before:], time.Since(startedAt)
}

// runtimeLogCount 返回运行日志（不含请求进度）里包含 text 的行数
func runtimeLogCount(service *trackedService, text string) int {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	count := 0
	for _, entry := range service.requests.logs {
		if entry.Event == "runtime.message" && strings.Contains(entry.Message, text) {
			count++
		}
	}
	return count
}

// assertNoBuildOffCooldown 断言账号都就绪、没有生效的冷却：PROXY_GENERATION_DISABLED 不冷却任何通道的额度
func assertNoBuildOffCooldown(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		status, _ := pool.StatusOf(accountID)
		if status.State != aistudio.AccountReady {
			t.Fatalf("账号 %s 状态 = %s，期望就绪", accountID, status.State)
		}
		for scope, cooldown := range status.Cooldowns {
			if cooldown.Active(time.Now()) {
				t.Fatalf("账号 %s 的 %q 处于冷却（%s），PROXY_GENERATION_DISABLED 不应冷却", accountID, scope, cooldown.Reason)
			}
		}
	}
}

// buildOffWarning 为进入暂停的 WARN 开头；按账号与全局分别以“范围=该账号”“范围=全部账号”区分
const buildOffWarning = "官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂停 Build，生成改走 Playground | 下次试探="

// warningCount 返回进入暂停的 WARN 中范围为 scope 的行数
func warningCount(service *trackedService, scope string) int {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	count := 0
	for _, entry := range service.requests.logs {
		if entry.Event == "runtime.message" && entry.Level == "WARN" && strings.HasPrefix(entry.Message, buildOffWarning) &&
			strings.Contains(entry.Message, "| 范围="+scope) {
			count++
		}
	}
	return count
}

// TestBuildDisabledSwitchesToPlayground 生成请求在 Build 上遇到 PROXY_GENERATION_DISABLED：不返回客户端，立即在同账号的 Playground 重试，
// 客户端拿到 Playground 的正常结果；流式与非流式、错误响应与流尾状态、流式代理与单次代理调用都一样。不冷却、不退避；
// 该账号随即暂停 Build（下一个请求直接走它的 Playground），WARN 只写一行
func TestBuildDisabledSwitchesToPlayground(t *testing.T) {
	channels := []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}
	for _, test := range []struct {
		name           string
		stream         bool
		trailer, unary bool
	}{
		{name: "流式、错误响应", stream: true},
		{name: "流式、流尾状态", stream: true, trailer: true},
		{name: "非流式、单次代理调用", unary: true},
		{name: "非流式、流尾状态", trailer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &buildOffUpstream{trailer: test.trailer, unary: test.unary}
			service, pool, _ := buildOffService(t, upstream, channels, []string{buildOffA, buildOffB}, []string{buildOffA, buildOffB})
			text, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, test.stream, "req-buildoff-1")
			if err != nil || text != "hello from playground" {
				t.Fatalf("正文 = %q err = %v，期望 Playground 的正常结果", text, err)
			}
			if want := []string{buildOffA + "/build", buildOffA + "/playground"}; !slices.Equal(calls, want) {
				t.Fatalf("尝试 = %v，期望 %v（同账号改走 Playground）", calls, want)
			}
			if elapsed >= buildOffNoBackoff {
				t.Fatalf("用时 %s，改走 Playground 不应退避", elapsed)
			}
			if !progressLogged(service, "改走 Playground 重试 | 同账号=是") {
				t.Fatal("请求进度应写明改走 Playground")
			}
			assertNoBuildOffCooldown(t, pool, buildOffA, buildOffB)

			_, err, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, test.stream, "req-buildoff-2")
			if err != nil || !slices.Equal(calls, []string{buildOffA + "/playground"}) {
				t.Fatalf("第二个请求尝试 = %v err = %v，期望该账号的 Build 已暂停、直接走 Playground", calls, err)
			}
			if got := warningCount(service, "该账号"); got != 1 {
				t.Fatalf("按账号暂停的 WARN 写了 %d 行，期望 1 行", got)
			}
			if runtimeLogCount(service, "账号切换") != 0 {
				t.Fatal("改走 Playground 不应逐个请求写“账号切换”")
			}
			if status := service.buildPauses().status(); status.Paused || status.PausedAccounts != 1 {
				t.Fatalf("状态 = %+v，期望 1 个账号暂停、没有全局暂停", status)
			}
		})
	}
}

// TestBuildDisabledGlobalPauseAndProbe 10 分钟内 3 个不同账号遇到即全局暂停（一个请求里依次换号，客户端无感知），之后不再走 Build；
// 30 分钟后放行一个真实请求试探：试探失败时这次请求照常换到 Playground、不重复写 WARN，下次试探顺延；试探成功即解除全局与各账号暂停，
// 写一行 INFO，选号与通道顺序恢复原样
func TestBuildDisabledGlobalPauseAndProbe(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC, buildOffD}
	// 只有 D 能经 Playground 调用：A、B、C 遇到后只能换号
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, []string{buildOffD})
	text, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-global-1")
	want := []string{buildOffA + "/build", buildOffB + "/build", buildOffC + "/build", buildOffD + "/playground"}
	if err != nil || text != "hello from playground" || !slices.Equal(calls, want) {
		t.Fatalf("正文 = %q err = %v 尝试 = %v，期望依次换号后在 D 的 Playground 完成 %v", text, err, calls, want)
	}
	if elapsed >= buildOffNoBackoff {
		t.Fatalf("用时 %s，换号不应退避", elapsed)
	}
	if !pool.BuildGenerationPaused() || warningCount(service, "全部账号") != 1 || warningCount(service, "该账号") != 2 {
		t.Fatalf("全局暂停 = %t，全局 WARN %d 行、按账号 WARN %d 行，期望全局暂停且分别为 1、2 行",
			pool.BuildGenerationPaused(), warningCount(service, "全部账号"), warningCount(service, "该账号"))
	}
	status := service.buildPauses().status()
	if !status.Paused || status.PausedAccounts != 3 || !status.NextProbe.Equal(now.Add(buildPauseInterval)) || !status.Since.Equal(*now) {
		t.Fatalf("状态 = %+v，期望全局暂停、3 个账号、下次试探在 %s 后", status, buildPauseInterval)
	}
	assertNoBuildOffCooldown(t, pool, accounts...)

	_, err, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-global-2")
	if err != nil || !slices.Equal(calls, []string{buildOffD + "/playground"}) {
		t.Fatalf("全局暂停后尝试 = %v err = %v，期望直接走 Playground", calls, err)
	}

	// 到了试探时间：放行一个真实请求走 Build，失败时这次请求照常换到 Playground
	*now = now.Add(buildPauseInterval + time.Minute)
	text, err, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-fail")
	if err != nil || text != "hello from playground" || !slices.Equal(calls, []string{buildOffA + "/build", buildOffD + "/playground"}) {
		t.Fatalf("试探失败：正文 = %q err = %v 尝试 = %v，期望 A 的 Build 试探后在 D 的 Playground 完成", text, err, calls)
	}
	if !progressLogged(service, "这次请求作为试探改走 Build") || !pool.BuildGenerationPaused() {
		t.Fatal("试探应写进请求进度，失败后仍全局暂停")
	}
	if warningCount(service, "全部账号") != 1 || warningCount(service, "该账号") != 2 {
		t.Fatal("试探失败不应重复写 WARN")
	}
	if status := service.buildPauses().status(); !status.NextProbe.Equal(now.Add(buildPauseInterval)) {
		t.Fatalf("试探失败后下次试探 = %s，期望顺延 %s", status.NextProbe, buildPauseInterval)
	}
	_, _, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-not-due")
	if !slices.Equal(calls, []string{buildOffD + "/playground"}) {
		t.Fatalf("没到试探时间时尝试 = %v，期望不走 Build", calls)
	}

	// 官网恢复后的下一次试探成功：解除全局与各账号暂停，写一行 INFO
	*now = now.Add(buildPauseInterval + time.Minute)
	upstream.recovered.Store(true)
	text, err, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-probe-ok")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("试探成功：正文 = %q err = %v 尝试 = %v，期望 A 的 Build 完成", text, err, calls)
	}
	if pool.BuildGenerationPaused() || runtimeLogCount(service, "Build 通道的生成已恢复，解除暂停 | 范围=全部账号") != 1 {
		t.Fatal("试探成功应解除全局暂停并写一行 INFO")
	}
	if status := service.buildPauses().status(); status.Paused || status.PausedAccounts != 0 || !status.NextProbe.IsZero() {
		t.Fatalf("恢复后状态 = %+v，期望没有任何暂停", status)
	}
	_, _, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-after")
	if !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("恢复后尝试 = %v，期望通道顺序与原来一致（Build 在前）", calls)
	}
}

// TestBuildAccountPauseExpiry 按账号的暂停到期后，该账号在 Build 上的下一次真实请求就是试探：再遇到就再暂停一段（不重复写 WARN，
// 请求照常改走 Playground），成功即解除并写一行 INFO
func TestBuildAccountPauseExpiry(t *testing.T) {
	upstream := &buildOffUpstream{}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground},
		[]string{buildOffA, buildOffB}, []string{buildOffA, buildOffB})
	if _, err, _, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-expiry-1"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(buildPauseInterval + time.Second)
	_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-expiry-2")
	if err != nil || !slices.Equal(calls, []string{buildOffA + "/build", buildOffA + "/playground"}) {
		t.Fatalf("到期后尝试 = %v err = %v，期望 A 的 Build 再试一次后改走 Playground", calls, err)
	}
	if warningCount(service, "该账号") != 1 {
		t.Fatal("同一账号再次遇到不应重复写 WARN")
	}
	*now = now.Add(buildPauseInterval + time.Second)
	upstream.recovered.Store(true)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-expiry-3")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("恢复后尝试 = %v 正文 = %q err = %v", calls, text, err)
	}
	if runtimeLogCount(service, "Build 通道的生成已恢复，解除暂停 | 范围=该账号") != 1 || service.buildPauses().status().PausedAccounts != 0 {
		t.Fatal("按账号暂停的试探成功应解除并写一行 INFO")
	}
	assertNoBuildOffCooldown(t, pool, buildOffA, buildOffB)
}

// pauseBuildGlobally 让三个账号都记一次 PROXY_GENERATION_DISABLED，进入全局暂停
func pauseBuildGlobally(t *testing.T, service *trackedService, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		service.buildPauses().disabled(accountID, accountID, false)
	}
	if !service.pool.BuildGenerationPaused() {
		t.Fatal("三个账号遇到后应全局暂停")
	}
}

// TestBuildDisabledInputLimit 输入超过 Playground 上限、要改走 Build 的请求：Build 全局暂停时不再改走 Build，直接按 400 返回并写明
// 上限与原因；还没有全局暂停时改走 Build 遇到 PROXY_GENERATION_DISABLED，同样按这个 400 结束，不把原文交给客户端；都不冷却账号
func TestBuildDisabledInputLimit(t *testing.T) {
	channels := []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}
	assertInputUnavailable := func(t *testing.T, err error) {
		t.Helper()
		var unavailable *aistudio.BuildGenerationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableInputLimit || unavailable.InputLimit != 131072 ||
			unavailable.HTTPStatus() != http.StatusBadRequest || !strings.Contains(unavailable.PublicMessage(), "131072") {
			t.Fatalf("err = %v，期望输入超过 Playground 上限（131072）且 Build 已停用的 400", err)
		}
		if aistudio.BuildGenerationDisabledFailure(err) {
			t.Fatal("给客户端的错误不应再是 PROXY_GENERATION_DISABLED 原文")
		}
	}
	t.Run("全局暂停中：不再改走 Build", func(t *testing.T) {
		upstream := &buildOffUpstream{playgroundCap: true}
		service, pool, _ := buildOffService(t, upstream, channels, []string{buildOffA, buildOffB, buildOffC}, []string{buildOffA, buildOffB, buildOffC})
		pauseBuildGlobally(t, service, buildOffA, buildOffB, buildOffC)
		_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-cap-paused")
		assertInputUnavailable(t, err)
		if !slices.Equal(calls, []string{buildOffA + "/playground"}) {
			t.Fatalf("尝试 = %v，期望只试 Playground", calls)
		}
		if !progressLogged(service, "无法改走 Build，按 400 返回") {
			t.Fatal("请求进度应写明无法改走 Build")
		}
		assertNoBuildOffCooldown(t, pool, buildOffA, buildOffB, buildOffC)
	})
	t.Run("改走 Build 时遇到", func(t *testing.T) {
		upstream := &buildOffUpstream{playgroundCap: true}
		service, pool, _ := buildOffService(t, upstream, channels, []string{buildOffA}, []string{buildOffA})
		_, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-cap-build")
		assertInputUnavailable(t, err)
		if !slices.Equal(calls, []string{buildOffA + "/playground", buildOffA + "/build"}) || elapsed >= buildOffNoBackoff {
			t.Fatalf("尝试 = %v 用时 %s，期望 Playground 超限后改走 Build 一次即结束", calls, elapsed)
		}
		assertNoBuildOffCooldown(t, pool, buildOffA)
	})
}

// TestBuildOnlyModelPaused 只能经 Build 调用的模型：Build 全局暂停时不发上游请求，直接返回只能经 Build 调用、官网已停用的 503；
// 还没有全局暂停时在 Build 上依次遇到，3 个账号后进入全局暂停并结束，不无限换号
func TestBuildOnlyModelPaused(t *testing.T) {
	channels := []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}
	accounts := []string{buildOffA, buildOffB, buildOffC, buildOffD}
	assertModelUnavailable := func(t *testing.T, err error) {
		t.Helper()
		var unavailable *aistudio.BuildGenerationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableModel || unavailable.Model != buildOffOnly ||
			unavailable.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("err = %v，期望只能经 Build 调用的 503", err)
		}
	}
	t.Run("全局暂停中", func(t *testing.T) {
		upstream := &buildOffUpstream{}
		service, _, _ := buildOffService(t, upstream, channels, accounts, accounts)
		pauseBuildGlobally(t, service, buildOffA, buildOffB, buildOffC)
		_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffOnly, true, "req-only-paused")
		assertModelUnavailable(t, err)
		if len(calls) != 0 {
			t.Fatalf("尝试 = %v，全局暂停中不应发上游请求", calls)
		}
	})
	t.Run("依次遇到后结束", func(t *testing.T) {
		upstream := &buildOffUpstream{}
		service, pool, _ := buildOffService(t, upstream, channels, accounts, accounts)
		_, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffOnly, false, "req-only-switch")
		assertModelUnavailable(t, err)
		want := []string{buildOffA + "/build", buildOffB + "/build", buildOffC + "/build"}
		if !slices.Equal(calls, want) || elapsed >= buildOffNoBackoff {
			t.Fatalf("尝试 = %v 用时 %s，期望 3 个账号后全局暂停并结束 %v", calls, elapsed, want)
		}
		if !pool.BuildGenerationPaused() {
			t.Fatal("应进入全局暂停")
		}
		assertNoBuildOffCooldown(t, pool, accounts...)
	})
}

// TestBuildNormalUnaffected 没有遇到 PROXY_GENERATION_DISABLED 时选号与通道顺序和原来一致：不加锁、不暂停、不写日志
func TestBuildNormalUnaffected(t *testing.T) {
	upstream := &buildOffUpstream{}
	upstream.recovered.Store(true)
	service, pool, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground},
		[]string{buildOffA, buildOffB}, []string{buildOffA, buildOffB})
	for _, model := range []string{buildOffShared, buildOffOnly} {
		text, err, calls, _ := runBuildOffRequest(t, service, upstream, model, true, "req-normal-"+model)
		if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) {
			t.Fatalf("%s：正文 = %q err = %v 尝试 = %v，期望照常走 A 的 Build", model, text, err, calls)
		}
	}
	if service.buildPauses().engaged.Load() || pool.BuildGenerationPaused() || runtimeLogCount(service, "Build 通道的生成") != 0 {
		t.Fatal("Build 正常时不应有任何暂停记录或日志")
	}
}

// TestBuildChannelStatus 状态接口在服务运行且启用 Build 时带 build_channel（全局暂停、开始时间、下次试探、暂停中的账号数），
// since 与 next_probe 只在全局暂停时输出；按账号暂停到期、等待试探的账号单独计数（awaiting_probe）；只启用 Playground 时没有
func TestBuildChannelStatus(t *testing.T) {
	upstream := &buildOffUpstream{}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild},
		[]string{buildOffA, buildOffB, buildOffC}, nil)
	service.catalogSync = newCatalogSyncer(service)
	service.egress = newEgressGuard(pool, service.requests)
	service.authRecheck = newAuthRecheck(service)
	service.state.Store(serviceRunning)
	admin := &runtimeAdmin{pool: pool, service: service, requests: service.requests, workers: service.workers}
	status, err := admin.Status(context.Background())
	if err != nil || status.BuildChannel == nil || status.BuildChannel.Paused || status.BuildChannel.PausedAccounts != 0 {
		t.Fatalf("正常时 build_channel = %+v err = %v", status.BuildChannel, err)
	}
	if encoded, _ := json.Marshal(status.BuildChannel); string(encoded) != `{"paused":false,"paused_accounts":0}` {
		t.Fatalf("正常时 build_channel JSON = %s，期望没有 since、next_probe 等字段", encoded)
	}
	// 按账号暂停到期后等待试探：不再算暂停中的账号，单独计数
	service.buildPauses().disabled(buildOffA, buildOffA, false)
	*now = now.Add(buildPauseInterval + time.Second)
	if channel := service.buildPauses().status(); channel.Paused || channel.PausedAccounts != 0 || channel.AwaitingProbe != 1 {
		t.Fatalf("按账号暂停到期后 build_channel = %+v，期望 1 个账号等待试探", channel)
	}
	status, _ = admin.Status(context.Background())
	if encoded, _ := json.Marshal(status.BuildChannel); string(encoded) != `{"paused":false,"paused_accounts":0,"awaiting_probe":1}` {
		t.Fatalf("等待试探时 build_channel JSON = %s", encoded)
	}
	// 清掉这条记录，再看全局暂停
	*now = now.Add(-buildPauseInterval - time.Second)
	tracker := service.buildPauses()
	tracker.mu.Lock()
	tracker.resetLocked()
	tracker.mu.Unlock()
	pool.ClearBuildPauses()
	pauseBuildGlobally(t, service, buildOffA, buildOffB, buildOffC)
	status, _ = admin.Status(context.Background())
	encoded, _ := json.Marshal(status.BuildChannel)
	if !status.BuildChannel.Paused || status.BuildChannel.PausedAccounts != 3 || !status.BuildChannel.Since.Equal(*now) ||
		!status.BuildChannel.NextProbe.Equal(now.Add(buildPauseInterval)) {
		t.Fatalf("全局暂停时 build_channel = %s", encoded)
	}
	for _, field := range []string{`"paused":true`, `"since":`, `"next_probe":`, `"paused_accounts":3`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("build_channel JSON 缺少 %s: %s", field, encoded)
		}
	}
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground})
	if status, _ = admin.Status(context.Background()); status.BuildChannel != nil {
		t.Fatalf("只启用 Playground 时不应有 build_channel: %+v", status.BuildChannel)
	}
}

// TestUpstreamChannelsHotReload UPSTREAM_CHANNELS 保存后立即生效，不要求重启生成服务：去掉 Build 后生成请求只走 Playground，
// 调整顺序后按新顺序选号；新启用 Build 时把还没有 Build 目录的账户排进后台同步
func TestUpstreamChannelsHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	t.Setenv("UPSTREAM_CHANNELS", "")
	os.Unsetenv("UPSTREAM_CHANNELS")
	upstream := &buildOffUpstream{}
	upstream.recovered.Store(true)
	service, pool, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild},
		[]string{buildOffA, buildOffB}, []string{buildOffA, buildOffB})
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	cfg.UpstreamChannels = []string{"playground", "build"}
	// 热更新同时应用路由策略：与测试账户池相同的 fill-first，选号结果只取决于通道顺序
	cfg.RoutingStrategy = "fill-first"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	service.catalog = emptyCatalog{}
	admin := &runtimeAdmin{lifecycle: service.lifecycle, pool: pool, service: service, requests: service.requests, workers: service.workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: service.lifecycle, configPath: path, activeManagement: cfg, requests: service.requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	update := func(channels ...string) {
		t.Helper()
		value := runtimeConfigDTO(manager.current.config)
		value.UpstreamChannels = channels
		updated, err := manager.UpdateRuntimeConfig(service.lifecycle, value)
		if err != nil {
			t.Fatal(err)
		}
		if updated.ServiceRestartRequired {
			t.Fatalf("上游通道改为 %v 后不应要求重启生成服务", channels)
		}
		if !slices.Equal(manager.current.config.UpstreamChannels, channels) {
			t.Fatalf("生效配置 = %v，期望 %v", manager.current.config.UpstreamChannels, channels)
		}
	}

	update("playground")
	if pool.BuildEnabled() {
		t.Fatal("去掉 Build 后应立即不再启用 Build")
	}
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-hot-1"); err != nil || !slices.Equal(calls, []string{buildOffA + "/playground"}) {
		t.Fatalf("只启用 Playground 时尝试 = %v err = %v", calls, err)
	}
	if runtimeLogCount(service, "上游通道=playground") != 1 {
		t.Fatal("热更新日志应写明上游通道")
	}

	update("build", "playground")
	if !pool.BuildEnabled() {
		t.Fatal("重新启用 Build 后应立即生效")
	}
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-hot-2"); err != nil || !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("Build 在前时尝试 = %v err = %v，期望按新顺序先走 Build", calls, err)
	}
}

// TestBuildChannelEnabledRequeuesCatalog 运行中新启用 Build 后，还没有 Build 目录的账户按“恢复时不一致”立即排进后台同步，
// 已有 Build 目录的账户不动
func TestBuildChannelEnabledRequeuesCatalog(t *testing.T) {
	service, pool := firstEventTestService(t, &channelRecorder{}, 0, buildOffA, buildOffB)
	if err := pool.SetBuildCatalog(buildOffB, []aistudio.Model{{ID: buildOffOnly, Methods: []string{"generateContent"}}}); err != nil {
		t.Fatal(err)
	}
	syncer := newCatalogSyncer(service)
	service.catalogSync = syncer
	syncer.mu.Lock()
	for _, accountID := range []string{buildOffA, buildOffB} {
		task := syncer.taskLocked(accountID)
		syncer.scheduleLocked(task, catalogPriorityExpired, time.Now().Add(time.Hour))
	}
	syncer.mu.Unlock()
	syncer.buildChannelEnabled()
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if task := syncer.tasks[buildOffA]; task.priority != catalogPriorityStale || task.due.After(time.Now()) {
		t.Fatalf("没有 Build 目录的账户任务 = %+v，期望立即按恢复时不一致同步", task)
	}
	if _, stale := syncer.stale[buildOffA]; !stale {
		t.Fatal("没有 Build 目录的账户应记为恢复时不一致")
	}
	if task := syncer.tasks[buildOffB]; task.priority != catalogPriorityExpired {
		t.Fatalf("已有 Build 目录的账户任务 = %+v，期望不动", task)
	}
}

// TestBuildPauseQuotaExhaustedKeeps429 Build 全局暂停中，Playground 能接该模型的账号额度都用完、号池里只剩 Build 目录有这个模型的账号：
// 按额度不足（429，带恢复时间）结束，与只启用 Playground 时一致；不说成“只能经 Build 调用”（503），也不去试暂停中的 Build
func TestBuildPauseQuotaExhaustedKeeps429(t *testing.T) {
	for _, test := range []struct {
		name     string
		channels []aistudio.Channel
		paused   bool
	}{
		{name: "只启用 Playground（对照）", channels: []aistudio.Channel{aistudio.ChannelPlayground}},
		{name: "两个通道、Build 全局暂停", channels: []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, paused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &buildOffUpstream{playgroundQuota: true}
			accounts := []string{buildOffA, buildOffB, buildOffC}
			// A、B 的 Playground 有该模型，C 只在 Build 目录有
			service, pool, _ := buildOffService(t, upstream, test.channels, accounts, []string{buildOffA, buildOffB})
			// 去掉预写的模型资格：额度 429 的冷却要能写入（预写的检查时间在将来，较早租约的冷却不写）
			for _, accountID := range accounts {
				if err := pool.ResetModelAccess(accountID); err != nil {
					t.Fatal(err)
				}
			}
			if test.paused {
				pauseBuildGlobally(t, service, accounts...)
			}
			_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-quota-paused")
			var quotaErr *aistudio.PrimaryQuotaError
			if !errors.As(err, &quotaErr) || quotaErr.HTTPStatus() != http.StatusTooManyRequests {
				t.Fatalf("err = %v（%T），期望额度不足 429", err, err)
			}
			var unavailable *aistudio.BuildGenerationUnavailableError
			if errors.As(err, &unavailable) {
				t.Fatalf("额度用完不应按 Build 不可用返回: %v", err)
			}
			if want := []string{buildOffA + "/playground", buildOffB + "/playground"}; !slices.Equal(calls, want) {
				t.Fatalf("尝试 = %v，期望 %v（不试暂停中的 Build）", calls, want)
			}
		})
	}
}

// TestBuildDisabledPrefersPlaygroundAfterBuild 在 Build 上遇到 PROXY_GENERATION_DISABLED、同账号没有 Playground 可用而换号后，
// 下一个账号先 Playground，不在它的 Build 上再白试一次
func TestBuildDisabledPrefersPlaygroundAfterBuild(t *testing.T) {
	upstream := &buildOffUpstream{}
	// A 只有 Build 能接，B 两个通道都能接；Build 在前
	service, _, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground},
		[]string{buildOffA, buildOffB}, []string{buildOffB})
	text, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-prefer-playground")
	if err != nil || text != "hello from playground" {
		t.Fatalf("正文 = %q err = %v", text, err)
	}
	if want := []string{buildOffA + "/build", buildOffB + "/playground"}; !slices.Equal(calls, want) {
		t.Fatalf("尝试 = %v，期望 %v（换号后先 Playground）", calls, want)
	}
	if elapsed >= buildOffNoBackoff {
		t.Fatalf("用时 %s，换号不应退避", elapsed)
	}
}

// TestBuildPauseWindow 全局判定只数 buildPauseWindow（10 分钟）内遇到的不同账号：跨过窗口的不算，窗口内凑满 3 个才全局暂停；
// 同一账号重复遇到只算一个
func TestBuildPauseWindow(t *testing.T) {
	pool, _ := testAccountPool(t, buildOffA, buildOffB, buildOffC, buildOffD)
	now := time.Now()
	tracker := newBuildPauseTracker(pool, nil)
	tracker.now = func() time.Time { return now }
	tracker.jitter = func(time.Duration) time.Duration { return 0 }
	step := func(after time.Duration, accountID string, wantGlobal bool) {
		t.Helper()
		now = now.Add(after)
		if got := tracker.disabled(accountID, accountID, false); got != wantGlobal {
			t.Fatalf("%s 遇到后全局暂停 = %t，期望 %t", accountID, got, wantGlobal)
		}
	}
	step(0, buildOffA, false)
	step(6*time.Minute, buildOffB, false)
	step(time.Minute, buildOffB, false)
	// A 在 11 分钟前，已不在窗口内：窗口内只有 B、C
	step(4*time.Minute, buildOffC, false)
	if pool.BuildGenerationPaused() {
		t.Fatal("跨过 10 分钟窗口的账号不应计入全局判定")
	}
	step(time.Minute, buildOffD, true)
	if !pool.BuildGenerationPaused() {
		t.Fatal("窗口内 3 个不同账号遇到后应全局暂停")
	}
}

// TestBuildProbeClaim 试探的认领：到期后先只给优先请求（不限定通道），降级判定与流式优先 Playground 的请求在到期超过 buildProbeRelax 后
// 才可以；每次只放行一个，没有结论（取不到 Build 账号）时 buildProbeRetry 后再放行；连续 buildProbeQueueAfter 次取不到 Build 账号后
// 试探改为排队，取得过租约（以其他原因失败或有了结论）后恢复只取立即可用的
func TestBuildProbeClaim(t *testing.T) {
	pool, _ := testAccountPool(t, buildOffA, buildOffB, buildOffC)
	now := time.Now()
	tracker := newBuildPauseTracker(pool, nil)
	tracker.now = func() time.Time { return now }
	tracker.jitter = func(time.Duration) time.Duration { return 0 }
	claim := func(preferred bool, allowed bool) bool {
		claimed, _ := tracker.claimProbe(preferred, allowed)
		return claimed
	}
	for _, accountID := range []string{buildOffA, buildOffB, buildOffC} {
		tracker.disabled(accountID, accountID, false)
	}
	due := now.Add(buildPauseInterval)
	if claim(true, true) {
		t.Fatal("没到试探时间不应放行")
	}
	now = due
	if claim(false, false) || claim(false, true) {
		t.Fatal("刚到期时只放行优先请求")
	}
	if claimed, queue := tracker.claimProbe(true, true); !claimed || queue || claim(true, true) {
		t.Fatal("到期后应放行一个优先请求（只取立即可用的），且同时只放行一个")
	}
	tracker.probeUnresolved(true)
	if status := tracker.status(); !status.NextProbe.Equal(now.Add(buildProbeRetry)) {
		t.Fatalf("没有结论的试探后下次试探 = %s，期望 %s 后", status.NextProbe, buildProbeRetry)
	}
	now = now.Add(buildProbeRetry + buildProbeRelax - time.Second)
	if claim(false, true) {
		t.Fatal("到期不满 buildProbeRelax 时不放行降级判定与流式优先 Playground 的请求")
	}
	now = now.Add(time.Second)
	if !claim(false, true) {
		t.Fatal("到期超过 buildProbeRelax 后应放行降级判定与流式优先 Playground 的请求")
	}
	if claim(false, false) {
		t.Fatal("固定账号等不能试探的请求从不放行")
	}

	// 连续 buildProbeQueueAfter 次没有取得 Build 租约（上面已有 1 次，这次认领的算第 2 次）后，之后的试探排队等 Build 租约
	tracker.probeUnresolved(true)
	for miss := 2; miss < buildProbeQueueAfter; miss++ {
		now = now.Add(buildProbeRetry)
		if claimed, queue := tracker.claimProbe(true, true); !claimed || queue {
			t.Fatalf("第 %d 次没有取得 Build 租约之前：认领 = %t 排队 = %t，期望只取立即可用的", miss+1, claimed, queue)
		}
		tracker.probeUnresolved(true)
	}
	now = now.Add(buildProbeRetry)
	if claimed, queue := tracker.claimProbe(true, true); !claimed || !queue {
		t.Fatalf("连续 %d 次没有取得 Build 租约后：认领 = %t 排队 = %t，期望排队试探", buildProbeQueueAfter, claimed, queue)
	}
	// 排队也没等到：仍排队；取得过租约（试探请求以其他原因失败）后恢复只取立即可用的
	tracker.probeUnresolved(true)
	now = now.Add(buildProbeRetry)
	if _, queue := tracker.claimProbe(true, true); !queue {
		t.Fatal("排队试探没等到 Build 租约后下一次仍应排队")
	}
	tracker.probeUnresolved(false)
	now = now.Add(buildProbeRetry)
	if claimed, queue := tracker.claimProbe(true, true); !claimed || queue {
		t.Fatalf("取得过 Build 租约后：认领 = %t 排队 = %t，期望只取立即可用的", claimed, queue)
	}
	// 试探失败（再次遇到）同样清零
	tracker.probeUnresolved(true)
	tracker.probeMisses = buildProbeQueueAfter
	now = now.Add(buildProbeRetry)
	if _, queue := tracker.claimProbe(true, true); !queue {
		t.Fatal("连续没有取得 Build 租约时应排队")
	}
	tracker.disabled(buildOffA, buildOffA, true)
	if tracker.probeMisses != 0 {
		t.Fatalf("试探有结论后连续次数 = %d，期望清零", tracker.probeMisses)
	}
}

// TestProbeQueueWait 排队试探的最长等待：默认 buildProbeQueueWait；主用模型不超过排队预算剩余的一半，太少时不排队
func TestProbeQueueWait(t *testing.T) {
	if got := probeQueueWait(nil); got != buildProbeQueueWait {
		t.Fatalf("没有排队预算时 = %s，期望 %s", got, buildProbeQueueWait)
	}
	if got := probeQueueWait(&leaseWaitBudget{limit: 4 * time.Second, startedAt: time.Now()}); got <= buildProbeQueueMin || got > 2*time.Second {
		t.Fatalf("排队预算剩余约 4s 时 = %s，期望不超过 2s", got)
	}
	if got := probeQueueWait(&leaseWaitBudget{limit: 10 * time.Minute, startedAt: time.Now()}); got != buildProbeQueueWait {
		t.Fatalf("排队预算充足时 = %s，期望 %s", got, buildProbeQueueWait)
	}
	if got := probeQueueWait(&leaseWaitBudget{limit: 600 * time.Millisecond, startedAt: time.Now()}); got != 0 {
		t.Fatalf("排队预算剩余不到 %s 的两倍时 = %s，期望不排队", buildProbeQueueMin, got)
	}
}

// TestBuildProbeWithoutBuildLeaseRetriesSoon 全局暂停中到了试探时间，却取不到现在就能用的 Build 账号（这里各账号的 Build 都在冷却）：
// 这次请求照常走 Playground，buildProbeRetry（1 分钟）后再放行试探，而不是等认领超时（10 分钟）
func TestBuildProbeWithoutBuildLeaseRetriesSoon(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC, buildOffD}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, []string{buildOffD})
	pauseBuildGlobally(t, service, buildOffA, buildOffB, buildOffC)
	buildScope := aistudio.ChannelCooldownScope(aistudio.ChannelBuild, buildOffShared)
	for _, accountID := range accounts {
		// 检查时间晚于预写的模型资格，冷却才会写入
		if err := pool.MarkCooldownIfGeneration(accountID, buildScope, pool.ModelAccessGeneration(accountID), time.Now().Add(2*time.Hour),
			time.Now().Add(3*time.Hour), "每日限额: test"); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(buildPauseInterval + time.Second)
	_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-no-lease")
	if err != nil || !slices.Equal(calls, []string{buildOffD + "/playground"}) {
		t.Fatalf("尝试 = %v err = %v，期望取不到 Build 账号时照常走 Playground", calls, err)
	}
	status := service.buildPauses().status()
	if status.Probing || !status.NextProbe.Equal(now.Add(buildProbeRetry)) {
		t.Fatalf("状态 = %+v，期望没有进行中的试探、%s 后再试探", status, buildProbeRetry)
	}
	for _, accountID := range accounts {
		if _, err := pool.ClearCooldownsMatching(accountID, func(key string, _ aistudio.CooldownState) bool { return key == buildScope }); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(buildProbeRetry / 2)
	if _, _, calls, _ = runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-wait"); !slices.Equal(calls, []string{buildOffD + "/playground"}) {
		t.Fatalf("没到下次试探时尝试 = %v，期望不走 Build", calls)
	}
	*now = now.Add(buildProbeRetry / 2)
	upstream.recovered.Store(true)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-retry")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) || pool.BuildGenerationPaused() {
		t.Fatalf("1 分钟后的试探：正文 = %q err = %v 尝试 = %v 全局暂停 = %t", text, err, calls, pool.BuildGenerationPaused())
	}
}

// TestBuildProbeRelaxedForPlaygroundFirstTraffic 只有流式优先 Playground（STREAM_PLAYGROUND_MODELS）的流量时：刚到试探时间不拿它们试探，
// 到期超过 buildProbeRelax 仍没有别的请求时放行其中一个走 Build，成功即解除暂停
func TestBuildProbeRelaxedForPlaygroundFirstTraffic(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	service.setStreamPlaygroundModels([]string{buildOffShared})
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Minute)
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-relax-1"); err != nil || !slices.Equal(calls, []string{buildOffA + "/playground"}) {
		t.Fatalf("刚到期时尝试 = %v err = %v，期望流式优先 Playground 的请求不拿来试探", calls, err)
	}
	*now = now.Add(buildProbeRelax)
	upstream.recovered.Store(true)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-relax-2")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("到期超过 %s 后：正文 = %q err = %v 尝试 = %v，期望放行试探并成功", buildProbeRelax, text, err, calls)
	}
	if pool.BuildGenerationPaused() || service.buildPauses().engaged.Load() {
		t.Fatal("试探成功应解除全局与各账号暂停")
	}
}

// TestBuildProbeRelaxedForDowngradeGuardTraffic 只有降级判定的模型（DOWNGRADE_GUARD_MODELS，默认 gemini-3.1-pro-preview）的流量时：
// 这类请求带 Playground 优先，刚到试探时间不拿它们试探；到期超过 buildProbeRelax 仍没有别的请求时放行其中一个走 Build，成功即解除暂停
func TestBuildProbeRelaxedForDowngradeGuardTraffic(t *testing.T) {
	saved := conversationMemory
	conversationMemory = &downgradeMemory{entries: make(map[string][]downgradeMemoryEntry)}
	t.Cleanup(func() { conversationMemory = saved })
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	guard := config.DefaultDowngradeGuard()
	guard.Models = []string{buildOffShared}
	service.setDowngradeGuard(guard)
	pauseBuildGlobally(t, service, accounts...)
	run := func(id string) []string {
		t.Helper()
		request := aistudio.GenerateRequest{
			ID: id, Model: buildOffShared,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
		}
		gate, rejected := service.prepareDowngradeGate(service.lifecycle, request, request.Contents)
		if gate == nil || rejected != nil {
			t.Fatalf("准备降级判定失败: gate=%v rejected=%v", gate, rejected)
		}
		diag := newGenerationDiagnostics(request, "未发送")
		diag.downgrade = gate
		before := len(upstream.called())
		requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
		defer requestCancel()
		events := make(chan aistudio.Event, 8)
		go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, diag)
		for range events {
		}
		return upstream.called()[before:]
	}
	*now = now.Add(buildPauseInterval + time.Minute)
	if calls := run("req-guard-relax-1"); !slices.Equal(calls, []string{buildOffA + "/playground"}) {
		t.Fatalf("刚到期时尝试 = %v，期望降级判定的请求照常走 Playground、不拿来试探", calls)
	}
	if status := service.buildPauses().status(); status.Probing || !pool.BuildGenerationPaused() {
		t.Fatalf("刚到期时状态 = %+v，期望没有认领试探", status)
	}
	*now = now.Add(buildProbeRelax)
	upstream.recovered.Store(true)
	if calls := run("req-guard-relax-2"); len(calls) == 0 || calls[0] != buildOffA+"/build" {
		t.Fatalf("到期超过 %s 后尝试 = %v，期望放行降级判定的请求走 Build 试探", buildProbeRelax, calls)
	}
	if !progressLogged(service, "这次请求作为试探改走 Build") || pool.BuildGenerationPaused() || service.buildPauses().engaged.Load() {
		t.Fatal("降级判定的请求试探成功应解除全局与各账号暂停")
	}
}

// TestBuildDisabledInputLimitAcrossAccounts 输入超过 Playground 上限、还没有全局暂停：改走 Build 遇到 PROXY_GENERATION_DISABLED 后
// 依次换账号的 Build 再试（不退避），第 3 个账号遇到即全局暂停，按写明上限的 400 结束
func TestBuildDisabledInputLimitAcrossAccounts(t *testing.T) {
	upstream := &buildOffUpstream{playgroundCap: true}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, accounts, accounts)
	_, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-cap-across")
	want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/build", buildOffC + "/build"}
	if !slices.Equal(calls, want) || elapsed >= buildOffNoBackoff {
		t.Fatalf("尝试 = %v 用时 %s，期望 %v 且不退避", calls, elapsed, want)
	}
	var unavailable *aistudio.BuildGenerationUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableInputLimit || unavailable.InputLimit != 131072 {
		t.Fatalf("err = %v，期望写明上限 131072 的输入超限 400", err)
	}
	if !pool.BuildGenerationPaused() {
		t.Fatal("3 个账号遇到后应全局暂停")
	}
	assertNoBuildOffCooldown(t, pool, accounts...)
}

// TestBuildDisabledInputLimitKeepsLimit 改走 Build 后中途以其他原因失败（A 的 Build 返回 503，清空了 inputLimitErr），随后遇到
// PROXY_GENERATION_DISABLED：最终 400 仍写明 Playground 的上限（131072），不按 503 返回
func TestBuildDisabledInputLimitKeepsLimit(t *testing.T) {
	channels := []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}
	assertLimit := func(t *testing.T, err error) {
		t.Helper()
		var unavailable *aistudio.BuildGenerationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableInputLimit || unavailable.InputLimit != 131072 ||
			unavailable.HTTPStatus() != http.StatusBadRequest || !strings.Contains(unavailable.PublicMessage(), "131072") {
			t.Fatalf("err = %v，期望写明上限 131072 的输入超限 400", err)
		}
	}
	t.Run("遇到时进入全局暂停", func(t *testing.T) {
		upstream := &buildOffUpstream{playgroundCap: true, buildFailing: []string{buildOffA}}
		service, _, _ := buildOffService(t, upstream, channels, []string{buildOffA, buildOffB, buildOffC, buildOffD}, []string{buildOffA})
		// C、D 之前遇到过：B 遇到即凑满 3 个账号
		service.buildPauses().disabled(buildOffC, buildOffC, false)
		service.buildPauses().disabled(buildOffD, buildOffD, false)
		_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-cap-midway-global")
		if want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", calls, want)
		}
		assertLimit(t, err)
	})
	t.Run("遇到后没有可用的 Build 账号", func(t *testing.T) {
		upstream := &buildOffUpstream{playgroundCap: true, buildFailing: []string{buildOffA}}
		service, _, _ := buildOffService(t, upstream, channels, []string{buildOffA, buildOffB}, []string{buildOffA})
		_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-cap-midway-account")
		if want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", calls, want)
		}
		assertLimit(t, err)
	})
	t.Run("其余账号的 Build 已暂停", func(t *testing.T) {
		// B 之前遇到过（按账号暂停中）：A 的 Build 返回 503 后选号因 Build 暂停没有候选，本请求没有遇到 PROXY_GENERATION_DISABLED，
		// 最后一次上游错误是 503，仍按写明上限的输入超限 400 结束
		upstream := &buildOffUpstream{playgroundCap: true, buildFailing: []string{buildOffA}}
		service, _, _ := buildOffService(t, upstream, channels, []string{buildOffA, buildOffB}, []string{buildOffA})
		service.buildPauses().disabled(buildOffB, buildOffB, false)
		_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, false, "req-cap-midway-paused")
		if want := []string{buildOffA + "/playground", buildOffA + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", calls, want)
		}
		assertLimit(t, err)
	})
}

// TestBuildProbeInputLimitDuringPause 全局暂停中到了试探时间：输入超过 Playground 上限的请求照常改走 Build，作为试探；
// 官网已恢复时在 Build 上完成并解除暂停（没到试探时间时按 400 结束，见 TestBuildDisabledInputLimit）
func TestBuildProbeInputLimitDuringPause(t *testing.T) {
	upstream := &buildOffUpstream{playgroundCap: true}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, accounts, accounts)
	// 流式优先 Playground 的请求刚到期时不拿来试探：第一次尝试照常走 Playground，超限后改走 Build 时才作为试探
	service.setStreamPlaygroundModels([]string{buildOffShared})
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Second)
	upstream.recovered.Store(true)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-cap-probe")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/playground", buildOffA + "/build"}) {
		t.Fatalf("正文 = %q err = %v 尝试 = %v，期望 Playground 超限后改走 Build 试探并完成", text, err, calls)
	}
	if !progressLogged(service, "这次请求作为试探改走 Build") || pool.BuildGenerationPaused() {
		t.Fatal("改走 Build 的再试应作为试探，成功后解除暂停")
	}
}

// TestBuildProbeDoesNotColdStart 全局暂停中的试探只取已启动 Worker 的 Build 账号：能经 Build 接的只有没有 Worker 的备用账号时，
// 不在请求路径上冷启动它（最多发起后台扩容），这次请求立即照常走 Playground，用户不多等冷启动
func TestBuildProbeDoesNotColdStart(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC, buildOffD}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, []string{buildOffD})
	pauseBuildGlobally(t, service, buildOffA, buildOffB, buildOffC)
	// A 没有 Worker（备用账号）；B、C、D 的 Build 都在冷却：试探能用的 Build 账号只有要冷启动的 A
	service.workers.accounts[buildOffA].warm.Store(false)
	service.workers.accounts[buildOffA].worker = nil
	release := make(chan struct{})
	launched := make(chan string, 4)
	service.workers.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launched <- accountID
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("测试不启动 Worker")
	}
	t.Cleanup(func() { close(release) })
	buildScope := aistudio.ChannelCooldownScope(aistudio.ChannelBuild, buildOffShared)
	for _, accountID := range []string{buildOffB, buildOffC, buildOffD} {
		if err := pool.MarkCooldownIfGeneration(accountID, buildScope, pool.ModelAccessGeneration(accountID), time.Now().Add(2*time.Hour),
			time.Now().Add(3*time.Hour), "每日限额: test"); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(buildPauseInterval + time.Second)
	_, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-cold")
	if err != nil || !slices.Equal(calls, []string{buildOffD + "/playground"}) || elapsed > time.Second {
		t.Fatalf("尝试 = %v err = %v 用时 %s，期望不冷启动、立即照常走 Playground", calls, err, elapsed)
	}
	if status := service.buildPauses().status(); status.Probing || !status.NextProbe.Equal(now.Add(buildProbeRetry)) {
		t.Fatalf("状态 = %+v，期望试探没有结论、%s 后再放行", status, buildProbeRetry)
	}
}

// TestBuildProbeSkipsModelsBuildCannotServe 到了试探时间，Build 接不了的模型（这里只在 Playground 目录里）的请求不拿来试探：
// 不认领、不把下次试探往后推，随后能经 Build 调用的请求照常试探
func TestBuildProbeSkipsModelsBuildCannotServe(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Second)
	due := service.buildPauses().status().NextProbe
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, testRuntimeModel, true, "req-probe-playground-only"); err != nil ||
		!slices.Equal(calls, []string{buildOffA + "/playground"}) {
		t.Fatalf("Playground 独有模型：尝试 = %v err = %v，期望不拿来试探", calls, err)
	}
	if status := service.buildPauses().status(); status.Probing || !status.NextProbe.Equal(due) {
		t.Fatalf("状态 = %+v，期望没有认领试探、下次试探仍为 %s", status, due)
	}
	upstream.recovered.Store(true)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-build-capable")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) || pool.BuildGenerationPaused() {
		t.Fatalf("能经 Build 调用的模型：正文 = %q err = %v 尝试 = %v，期望试探成功并解除暂停", text, err, calls)
	}
}

// holdAccountSlots 让每个账号的并发都被占满（测试先把单账号并发设为 1），返回释放函数（重复释放无害）
func holdAccountSlots(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) map[string]func() {
	t.Helper()
	releases := make(map[string]func(), len(accountIDs))
	for _, accountID := range accountIDs {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		releases[accountID] = func() { once.Do(func() { _ = lease.Release() }) }
		t.Cleanup(releases[accountID])
	}
	return releases
}

// waitDispatchQueued 等到有请求在调度队列里排队（选号已经开始等待账号空出槽位）
func waitDispatchQueued(t *testing.T, service *trackedService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		service.workers.dispatch.mu.Lock()
		queued := len(service.workers.dispatch.queues) > 0
		service.workers.dispatch.mu.Unlock()
		if queued {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("请求没有进入调度队列")
}

// runBuildOffWhileBusy 在各账号槽位都占满时执行一次生成：等请求进入调度队列后放开 free 的槽位，请求结束后放开其余的
func runBuildOffWhileBusy(t *testing.T, service *trackedService, pool *aistudio.AccountPool, upstream *buildOffUpstream, free string, accounts []string, id string) (string, error, []string) {
	t.Helper()
	releases := holdAccountSlots(t, pool, accounts...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitDispatchQueued(t, service)
		releases[free]()
	}()
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, id)
	<-done
	for _, release := range releases {
		release()
	}
	return text, err, calls
}

// TestBuildProbeQueuesWhenSaturated 全局暂停、试探到期，全部热 Build 账号的并发都占满（号池持续饱和，空出的槽位马上被排队的请求取走）：
// 只取立即可用账号的试探连续 buildProbeQueueAfter 次都没有结论（每次这个请求照常排队，在 Playground 完成）；之后的试探排队等 Build 租约，
// 空出槽位后在 Build 上完成试探并解除暂停，官网恢复后不会因为负载一直降不下来而迟迟不解除
func TestBuildProbeQueuesWhenSaturated(t *testing.T) {
	upstream := &buildOffUpstream{}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	pool.SetPerAccountConcurrency(1)
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Second)
	for miss := 1; miss <= buildProbeQueueAfter; miss++ {
		text, err, calls := runBuildOffWhileBusy(t, service, pool, upstream, buildOffA, accounts, fmt.Sprintf("req-saturated-%d", miss))
		if err != nil || text != "hello from playground" || !slices.Equal(calls, []string{buildOffA + "/playground"}) {
			t.Fatalf("第 %d 次：正文 = %q err = %v 尝试 = %v，期望试探取不到 Build 账号、照常排队后在 Playground 完成", miss, text, err, calls)
		}
		status := service.buildPauses().status()
		if status.Probing || !status.NextProbe.Equal(now.Add(buildProbeRetry)) || !status.Paused {
			t.Fatalf("第 %d 次后状态 = %+v，期望没有结论、%s 后再试探", miss, status, buildProbeRetry)
		}
		*now = now.Add(buildProbeRetry)
	}
	if service.buildPauses().probeMisses != buildProbeQueueAfter {
		t.Fatalf("连续没有取得 Build 租约 %d 次，期望 %d 次", service.buildPauses().probeMisses, buildProbeQueueAfter)
	}
	upstream.recovered.Store(true)
	text, err, calls := runBuildOffWhileBusy(t, service, pool, upstream, buildOffA, accounts, "req-saturated-queued")
	if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffA + "/build"}) {
		t.Fatalf("排队试探：正文 = %q err = %v 尝试 = %v，期望排队等到 A 的 Build 并完成", text, err, calls)
	}
	if pool.BuildGenerationPaused() || service.buildPauses().engaged.Load() || runtimeLogCount(service, "Build 通道的生成已恢复，解除暂停 | 范围=全部账号") != 1 {
		t.Fatal("排队试探成功应解除全局与各账号暂停并写一行 INFO")
	}
}

// TestBuildProbeOtherFailureRetriesSoon 试探请求在 Build 上以其他原因失败（这里 A 的 Build 返回 503）：说明不了 Build 是否恢复，
// 这次请求照常在 Playground 完成，buildProbeRetry（1 分钟）后再放行下一次试探，不等认领超时（buildProbeStale）
func TestBuildProbeOtherFailureRetriesSoon(t *testing.T) {
	upstream := &buildOffUpstream{buildFailing: []string{buildOffA}}
	accounts := []string{buildOffA, buildOffB, buildOffC}
	service, pool, now := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}, accounts, accounts)
	pauseBuildGlobally(t, service, accounts...)
	*now = now.Add(buildPauseInterval + time.Second)
	text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-probe-503")
	if err != nil || text != "hello from playground" || len(calls) < 2 || calls[0] != buildOffA+"/build" {
		t.Fatalf("正文 = %q err = %v 尝试 = %v，期望 A 的 Build 试探失败后在 Playground 完成", text, err, calls)
	}
	status := service.buildPauses().status()
	if status.Probing || !status.NextProbe.Equal(now.Add(buildProbeRetry)) || !pool.BuildGenerationPaused() {
		t.Fatalf("状态 = %+v，期望试探没有结论、%s 后再放行、仍全局暂停", status, buildProbeRetry)
	}
	if service.buildPauses().probeMisses != 0 {
		t.Fatal("试探取得过 Build 租约，不应算作没有取得租约")
	}
}

// TestBuildDisabledInputLimitBusyBuild 输入超过 Playground 上限、还没有全局暂停：A 改走 Build 遇到 PROXY_GENERATION_DISABLED 后，
// 其余能经 Build 的账号只是忙（没有立即可用的账号，不是因为暂停没有候选）：仍按写明上限（131072）的输入超限 400 结束，不按号池繁忙返回
func TestBuildDisabledInputLimitBusyBuild(t *testing.T) {
	upstream := &buildOffUpstream{playgroundCap: true}
	service, pool, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild},
		[]string{buildOffA, buildOffB}, []string{buildOffA})
	pool.SetPerAccountConcurrency(1)
	holdAccountSlots(t, pool, buildOffB)
	_, err, calls, elapsed := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-cap-busy")
	if want := []string{buildOffA + "/playground", buildOffA + "/build"}; !slices.Equal(calls, want) || elapsed >= buildOffNoBackoff {
		t.Fatalf("尝试 = %v 用时 %s，期望 %v 且不退避、不排队", calls, elapsed, want)
	}
	var unavailable *aistudio.BuildGenerationUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableInputLimit || unavailable.InputLimit != 131072 ||
		unavailable.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("err = %v（%T），期望写明上限 131072 的输入超限 400", err, err)
	}
	if pool.BuildGenerationPaused() {
		t.Fatal("只有 1 个账号遇到，不应全局暂停")
	}
}

// TestBuildSuccessOnOtherAccountKeepsPause 只有 A 按账号暂停时，没有暂停记录的 B 在 Build 上成功不解除 A 的暂停（官网可能只对 A 停用），
// 也不写恢复日志；A 自己到期后在 Build 上的结果才决定
func TestBuildSuccessOnOtherAccountKeepsPause(t *testing.T) {
	upstream := &buildOffUpstream{}
	// A 只有 Build 能接，B 两个通道都能接；Build 在前
	service, _, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground},
		[]string{buildOffA, buildOffB}, []string{buildOffB})
	if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-keep-1"); err != nil ||
		!slices.Equal(calls, []string{buildOffA + "/build", buildOffB + "/playground"}) {
		t.Fatalf("尝试 = %v err = %v，期望 A 遇到后换到 B 的 Playground", calls, err)
	}
	upstream.recovered.Store(true)
	for _, id := range []string{"req-keep-2", "req-keep-3"} {
		text, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, id)
		if err != nil || text != "hello from build" || !slices.Equal(calls, []string{buildOffB + "/build"}) {
			t.Fatalf("%s：正文 = %q err = %v 尝试 = %v，期望 A 仍暂停、B 的 Build 完成", id, text, err, calls)
		}
	}
	if status := service.buildPauses().status(); status.PausedAccounts != 1 || !service.buildPauses().engaged.Load() {
		t.Fatalf("状态 = %+v，期望 A 仍按账号暂停", status)
	}
	if runtimeLogCount(service, "Build 通道的生成已恢复") != 0 {
		t.Fatal("没有暂停记录的账号在 Build 上成功不应写恢复日志")
	}
}

// TestBuildDisabledNotEgressFailure PROXY_GENERATION_DISABLED 不计入出口的“其他失败”：与出口无关。状态码不看：
// 流尾状态若按协议错误码 12（UNIMPLEMENTED）换算为 501，仍不计
func TestBuildDisabledNotEgressFailure(t *testing.T) {
	for _, err := range []error{
		&aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusBadRequest, Code: 3, Message: buildOffStreamMessage},
		&aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusNotImplemented, Code: 12, Message: buildOffStreamMessage},
		fmt.Errorf("换号重试: %w", &aistudio.RPCError{Method: "ProxyUnaryCall", StatusCode: http.StatusNotImplemented, Code: 12, Message: buildOffUnaryMessage}),
	} {
		if egressCountsOther(err) {
			t.Fatalf("%v 不应计入出口的其他失败", err)
		}
	}
	// 对照：Playground 的同样状态码照常计入
	if !egressCountsOther(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusNotImplemented, Code: 12, Message: buildOffStreamMessage}) {
		t.Fatal("Playground 的 501 应照常计入出口的其他失败")
	}
}

// TestBuildAccountPauseRewritesPoolFlag 账号的暂停记录还在、账户池里却没有标记（账号移出后又加入，池里是新的账户）：它再遇到时把标记写回，
// 之后的请求直接走 Playground，不再每次先在 Build 上白失败一次；不重复写 WARN
func TestBuildAccountPauseRewritesPoolFlag(t *testing.T) {
	upstream := &buildOffUpstream{}
	service, pool, _ := buildOffService(t, upstream, []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground},
		[]string{buildOffA, buildOffB}, []string{buildOffA, buildOffB})
	if _, err, _, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-flag-1"); err != nil {
		t.Fatal(err)
	}
	// 模拟池里换成了没有标记的新账户
	if !pool.SetAccountBuildPaused(buildOffA, false) {
		t.Fatal("A 应带着暂停标记")
	}
	_, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, "req-flag-2")
	if err != nil || !slices.Equal(calls, []string{buildOffA + "/build", buildOffA + "/playground"}) {
		t.Fatalf("尝试 = %v err = %v，期望 A 在 Build 上再遇到一次后改走 Playground", calls, err)
	}
	for _, id := range []string{"req-flag-3", "req-flag-4"} {
		if _, err, calls, _ := runBuildOffRequest(t, service, upstream, buildOffShared, true, id); err != nil ||
			!slices.Equal(calls, []string{buildOffA + "/playground"}) {
			t.Fatalf("%s：尝试 = %v err = %v，期望标记已写回、直接走 Playground", id, calls, err)
		}
	}
	if warningCount(service, "该账号") != 1 {
		t.Fatal("同一账号再次遇到不应重复写 WARN")
	}
}

// TestBuildPauseStaleRecordsDropped 按账号暂停到期后一直没有 Build 结果（账号被删除、停用或之后都不再走 Build）：已不在账户池里的账号的
// 记录在到期时删除，仍在池里的到期后再过 buildPauseInterval 删除；全部删光后状态正常、请求路径恢复不加锁。全局暂停中到期的记录保留，
// 状态把遇到过的账号都算作暂停中（各账号不会单独试探），不算等待试探
func TestBuildPauseStaleRecordsDropped(t *testing.T) {
	t.Run("不在全局暂停", func(t *testing.T) {
		pool, _ := testAccountPool(t, buildOffA, buildOffB, buildOffC)
		now := time.Now()
		tracker := newBuildPauseTracker(pool, nil)
		tracker.now = func() time.Time { return now }
		tracker.jitter = func(time.Duration) time.Duration { return 0 }
		tracker.disabled(buildOffA, buildOffA, false)
		tracker.disabled(buildOffB, buildOffB, false)
		if _, err := pool.Remove(buildOffA, func(*aistudio.Account) error { return nil }); err != nil {
			t.Fatal(err)
		}
		now = now.Add(buildPauseInterval + time.Second)
		if status := tracker.status(); status.PausedAccounts != 0 || status.AwaitingProbe != 1 {
			t.Fatalf("到期后状态 = %+v，期望移出账户池的 A 已删除、B 等待试探", status)
		}
		// 别的账号在 Build 上成功不解除 B（没有它的结果），但到期后再过 buildPauseInterval 删除
		tracker.succeeded(buildOffC, buildOffC, false)
		now = now.Add(buildPauseInterval - 2*time.Second)
		if status := tracker.status(); status.AwaitingProbe != 1 {
			t.Fatalf("没满 %s 时状态 = %+v，期望 B 仍等待试探", buildPauseInterval, status)
		}
		now = now.Add(time.Second)
		if status := tracker.status(); status.PausedAccounts != 0 || status.AwaitingProbe != 0 || status.Paused {
			t.Fatalf("到期后又过 %s 状态 = %+v，期望记录删光", buildPauseInterval, status)
		}
		if tracker.engaged.Load() {
			t.Fatal("记录删光后请求路径应恢复不加锁")
		}
	})
	t.Run("全局暂停中", func(t *testing.T) {
		pool, _ := testAccountPool(t, buildOffA, buildOffB, buildOffC)
		now := time.Now()
		tracker := newBuildPauseTracker(pool, nil)
		tracker.now = func() time.Time { return now }
		tracker.jitter = func(time.Duration) time.Duration { return 0 }
		for _, accountID := range []string{buildOffA, buildOffB, buildOffC} {
			tracker.disabled(accountID, accountID, false)
		}
		// 31 分钟后的试探在 A 上再遇到：A 再暂停一段，B、C 各自的暂停到期
		now = now.Add(buildPauseInterval + time.Minute)
		if claimed, _ := tracker.claimProbe(true, true); !claimed {
			t.Fatal("应放行试探")
		}
		tracker.disabled(buildOffA, buildOffA, true)
		for _, after := range []time.Duration{0, 3 * buildPauseInterval} {
			now = now.Add(after)
			if status := tracker.status(); !status.Paused || status.PausedAccounts != 3 || status.AwaitingProbe != 0 {
				t.Fatalf("全局暂停中（再过 %s）状态 = %+v，期望 3 个遇到过的账号都算暂停中、没有等待试探", after, status)
			}
		}
	})
}

// TestBuildPauseForgetOnAccountRemoval 账号移出账户池（目录被移走、删除、批量删除）时删除它的按账号暂停记录：状态不再计入，
// 全部删光时请求路径恢复不加锁；它遇到过的时间仍算进全局判定（说明的是官网停用了 Build 的生成）
func TestBuildPauseForgetOnAccountRemoval(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, _ := deleteTestAdmin(t, buildOffA, buildOffB, buildOffC, buildOffD)
	tracker := admin.service.buildPauses()
	tracker.disabled(buildOffA, buildOffA, false)
	tracker.disabled(buildOffB, buildOffB, false)
	if err := admin.detachAccount(buildOffA); err != nil {
		t.Fatal(err)
	}
	if status := tracker.status(); status.PausedAccounts != 1 || !tracker.engaged.Load() {
		t.Fatalf("A 移出账户池后状态 = %+v，期望只剩 B", status)
	}
	if err := admin.DeleteAccount(context.Background(), buildOffB); err != nil {
		t.Fatal(err)
	}
	if status := tracker.status(); status.PausedAccounts != 0 || status.AwaitingProbe != 0 || tracker.engaged.Load() {
		t.Fatalf("B 删除后状态 = %+v，期望没有记录、请求路径不加锁", status)
	}
	if !tracker.disabled(buildOffC, buildOffC, false) || !admin.pool.BuildGenerationPaused() {
		t.Fatal("A、B 遇到过的时间仍在窗口内，C 遇到后应凑满 3 个账号进入全局暂停")
	}
	if _, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{buildOffC}}); err != nil {
		t.Fatal(err)
	}
	if status := tracker.status(); !status.Paused || status.PausedAccounts != 0 {
		t.Fatalf("批量删除 C 后状态 = %+v，期望仍全局暂停、没有账号记录", status)
	}
}
