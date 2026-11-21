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
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// verifyOtherModel 为单个模型需要验证测试里账户的另一个模型（照常服务）
const verifyOtherModel = "other-model"

// verifyModelUpstream 为按账户与模型返回需要验证账号 403 的假上游：denied[账户][模型] 为真时返回 verify-account 的 403，其余正常输出
type verifyModelUpstream struct {
	mu     sync.Mutex
	denied map[string]map[string]bool
	calls  []string
	// deniedAt 为每个账户最近一次返回 403 的时刻：晚于这次尝试取得租约、早于写入暂停（代表这期间取得租约的另一个请求）
	deniedAt map[string]time.Time
}

func newVerifyModelUpstream() *verifyModelUpstream {
	return &verifyModelUpstream{denied: make(map[string]map[string]bool), deniedAt: make(map[string]time.Time)}
}

func (upstream *verifyModelUpstream) deny(accountID string, modelID string) *verifyModelUpstream {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.denied[accountID] == nil {
		upstream.denied[accountID] = make(map[string]bool)
	}
	upstream.denied[accountID][modelID] = true
	return upstream
}

func (upstream *verifyModelUpstream) lastDenied(accountID string) time.Time {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return upstream.deniedAt[accountID]
}

func (upstream *verifyModelUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

func (upstream *verifyModelUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *verifyModelUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *verifyModelUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+request.Model)
	denied := upstream.denied[request.AccountID][strings.TrimPrefix(request.Model, "models/")]
	if denied {
		upstream.deniedAt[request.AccountID] = time.Now()
	}
	upstream.mu.Unlock()
	if denied {
		return nil, &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: verifyRequiredMessage}
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// verifyModelService 返回 forbiddenBad、forbiddenGood 两个都驻留 Worker、目录里有 testRuntimeModel 与 verifyOtherModel 的生成服务
// （两个通道都启用，Build 目录同样两个模型），挂上 403 登记表
func verifyModelService(t *testing.T, upstream aistudio.Service) (*trackedService, *aistudio.AccountPool, *permissionDeniedRegistry) {
	t.Helper()
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	registry := withPermissionDenied(t, service)
	models := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: verifyOtherModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	for _, accountID := range []string{forbiddenBad, forbiddenGood} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, models); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, models); err != nil {
			t.Fatal(err)
		}
	}
	preverifyRuntimeModelAccess(t, pool, forbiddenGood)
	return service, pool, registry
}

// TestModelVerifyReasonStable 单个模型需要验证的原因不能改动文字（已持久化的冷却按它逐字识别）：以 403 自动暂停的后缀结尾
// （认证文件更新、验证通过与其他 403 冷却一起清除），清除 403 记录也解除它，不是整户暂停，开头与整户暂停、阶梯冷却都不同
func TestModelVerifyReasonStable(t *testing.T) {
	want := "单个模型需要验证账号（verify-account），在官网验证后到管理页点验证或清除 403 记录；该模型已 1 " + forbiddenPauseReasonSuffix
	if modelVerifyRequiredReason != want {
		t.Fatalf("单个模型需要验证原因 = %q，期望 %q", modelVerifyRequiredReason, want)
	}
	cooldown := aistudio.CooldownState{Until: time.Now().Add(time.Hour), Reason: modelVerifyRequiredReason}
	if !forbiddenCooldown(testRuntimeModel, cooldown) || !verifyCooldown(testRuntimeModel, cooldown) || verifyRequiredPauseCooldown(testRuntimeModel, cooldown) {
		t.Fatal("应按 403 冷却清除、被清除 403 记录解除，但不是整户暂停")
	}
	accountHead, _, _ := strings.Cut(verifyRequiredPauseReason, "，")
	if strings.HasPrefix(modelVerifyRequiredReason, accountHead) || strings.HasPrefix(modelVerifyRequiredReason, forbiddenModelReasonPrefix) {
		t.Fatal("开头不能与整户暂停或阶梯冷却相同")
	}
	if verifyCooldown("", aistudio.CooldownState{Reason: forbiddenModelReasonPrefix + "x"}) {
		t.Fatal("阶梯冷却不算需要验证账号")
	}
}

// TestSingleModelVerifyCoolsModelOnly 只在一个模型上返回需要验证账号：换号完成；只暂停这个模型（Playground 与 Build 两个键，30 天），
// 不写整户暂停、不写阶梯冷却、不释放 Worker，账户行照常就绪；这个模型的下一个请求不再选它；别的模型照常由它服务，成功后这个模型
// 仍暂停、登记仍在；对账户池按通道的成功清除同样碰不到它
func TestSingleModelVerifyCoolsModelOnly(t *testing.T) {
	upstream := newVerifyModelUpstream().deny(forbiddenBad, testRuntimeModel)
	service, pool, registry := verifyModelService(t, upstream)
	if text, err, _ := runAccountRequest(t, service, "req-verify-model", ""); err != nil || text != "hello" {
		t.Fatalf("应换到其他账户完成：text=%q err=%v 调用=%v", text, err, upstream.called())
	}
	if !slices.Contains(upstream.called(), forbiddenBad+"/"+testRuntimeModel) {
		t.Fatalf("用例要先落到 bad: %v", upstream.called())
	}
	playground, build := modelVerifyOf(t, pool, forbiddenBad, testRuntimeModel)
	for _, cooldown := range []aistudio.CooldownState{playground, build} {
		if cooldown.Reason != modelVerifyRequiredReason || time.Until(cooldown.Until) < verifyRequiredPause-time.Minute {
			t.Fatalf("该模型的两个键应写 30 天的单个模型需要验证冷却: playground=%+v build=%+v", playground, build)
		}
	}
	if cooldown, ok := globalPauseOf(t, pool, forbiddenBad); ok {
		t.Fatalf("不应写整户暂停: %+v", cooldown)
	}
	// 403 那次尝试取得租约之后、写入暂停之前取得租约的另一个请求在这个模型上成功：检查时间取写入时刻，它清不掉这个模型的暂停
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(forbiddenBad, testRuntimeModel, pool.ModelAccessGeneration(forbiddenBad), upstream.lastDenied(forbiddenBad)); err != nil {
		t.Fatal(err)
	}
	if playground, _ := modelVerifyOf(t, pool, forbiddenBad, testRuntimeModel); playground.Reason != modelVerifyRequiredReason {
		t.Fatalf("写入之前取得租约的成功不应清掉暂停: %+v", playground)
	}
	if !hasLogContaining(service.requests, "账号冷却 | 类型=单个模型需要验证 | 范围="+testRuntimeModel+" | 通道=playground、build | 恢复=验证通过、认证文件更新或清除 403 记录（最迟 ") {
		t.Fatalf("应写单个模型需要验证的冷却日志，日志 = %v", serviceLogMessages(service.requests))
	}
	for _, fragment := range []string{"账号暂停调度", "类型=无权限", "WAA Worker 释放", "冷却淘汰", "账号自动暂停"} {
		if hasLogContaining(service.requests, fragment) {
			t.Fatalf("单个模型需要验证不应出现“%s”，日志 = %v", fragment, serviceLogMessages(service.requests))
		}
	}
	if models := registry.verifyModels(forbiddenBad); !slices.Equal(models, []string{testRuntimeModel}) {
		t.Fatalf("登记的需要验证模型 = %v", models)
	}
	if reasons := pool.UnschedulableSet([]string{forbiddenBad}); len(reasons) != 0 {
		t.Fatalf("账户不应不可调度: %v", reasons)
	}
	if status := testStatus(t, pool, forbiddenBad); status.State != aistudio.AccountReady {
		t.Fatalf("账户行应照常就绪，实际 %s", status.State)
	}
	time.Sleep(200 * time.Millisecond)
	if !slices.Contains(service.workers.WarmAccountIDs(), forbiddenBad) {
		t.Fatalf("Worker 不应释放，驻留 = %v", service.workers.WarmAccountIDs())
	}
	// 这个模型的下一个请求不再落到 bad
	before := len(upstream.called())
	if text, err, _ := runAccountRequest(t, service, "req-verify-model-2", ""); err != nil || text != "hello" {
		t.Fatalf("第二个请求应由 good 完成: text=%q err=%v", text, err)
	}
	for _, call := range upstream.called()[before:] {
		if call == forbiddenBad+"/"+testRuntimeModel {
			t.Fatalf("暂停的模型不应再选 bad: %v", upstream.called())
		}
	}
	// 别的模型照常由 bad 服务；成功不解除这个模型的暂停
	if text, err, _ := runModelRequest(t, service, "req-verify-other", forbiddenBad, verifyOtherModel); err != nil || text != "hello" {
		t.Fatalf("bad 应照常服务其他模型: text=%q err=%v", text, err)
	}
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(forbiddenBad, verifyOtherModel, pool.ModelAccessGeneration(forbiddenBad), time.Now()); err != nil {
		t.Fatal(err)
	}
	if playground, build := modelVerifyOf(t, pool, forbiddenBad, testRuntimeModel); playground.Reason != modelVerifyRequiredReason || build.Reason != modelVerifyRequiredReason {
		t.Fatalf("其他模型成功后这个模型仍应暂停: %+v %+v", playground, build)
	}
	if models := registry.verifyModels(forbiddenBad); !slices.Equal(models, []string{testRuntimeModel}) {
		t.Fatalf("其他模型成功不应清除登记: %v", models)
	}
	if hasLogContaining(service.requests, "账户已通过验证") {
		t.Fatalf("其他模型成功不算通过验证，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestSecondVerifyModelPausesAccount 同一账户在第二个模型上也返回需要验证账号：整户暂停 30 天（检查时间取现在）、登记释放 Worker，
// “账号暂停调度”带需要验证的模型；之后在第三个模型上成功、但租约早于这次 403 时不解除（较晚的 403 为准），晚于时解除
func TestSecondVerifyModelPausesAccount(t *testing.T) {
	upstream := newVerifyModelUpstream().deny(forbiddenBad, testRuntimeModel).deny(forbiddenBad, verifyOtherModel)
	service, pool, registry := verifyModelService(t, upstream)
	if text, err, _ := runAccountRequest(t, service, "req-verify-first", ""); err != nil || text != "hello" {
		t.Fatalf("第一个模型应换号完成: text=%q err=%v", text, err)
	}
	early := time.Now()
	if text, err, _ := runModelRequest(t, service, "req-verify-second", "", verifyOtherModel); err != nil || text != "hello" {
		t.Fatalf("第二个模型应换号完成: text=%q err=%v 调用=%v", text, err, upstream.called())
	}
	pause, ok := globalPauseOf(t, pool, forbiddenBad)
	if !ok || pause.Reason != verifyRequiredPauseReason || time.Until(pause.Until) < verifyRequiredPause-time.Minute {
		t.Fatalf("第二个模型要求验证后应整户暂停 30 天: %+v（存在=%t）", pause, ok)
	}
	if !hasLogContaining(service.requests, "账号暂停调度 | 原因="+verifyRequiredPauseReason, "| 需要验证的模型="+verifyOtherModel+"、"+testRuntimeModel) {
		t.Fatalf("应写整户暂停与需要验证的模型，日志 = %v", serviceLogMessages(service.requests))
	}
	if !waitUntil(3*time.Second, func() bool {
		return sourceLogCount(service.requests, forbiddenBad, "WAA Worker 释放 | 原因=需要验证账号 | 登记后=") == 1
	}) {
		t.Fatalf("整户暂停应释放 Worker，日志 = %v", serviceLogMessages(service.requests))
	}
	// 403 那次尝试取得租约之后、写入暂停之前取得租约的另一个请求在其他模型上成功：池层按检查时间清不掉整户暂停
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(forbiddenBad, verifyOtherModel, pool.ModelAccessGeneration(forbiddenBad), upstream.lastDenied(forbiddenBad)); err != nil {
		t.Fatal(err)
	}
	if !pool.VerifyRequiredPaused(forbiddenBad) {
		t.Fatal("写入之前取得租约的成功不应清掉整户暂停")
	}
	// 403 之前就取得的租约在第三个模型上成功：不解除
	service.permissionDeniedSucceeded(forbiddenBad, "third-model", aistudio.ChannelPlayground, early.Add(-time.Second))
	if !pool.VerifyRequiredPaused(forbiddenBad) || len(registry.verifyModels(forbiddenBad)) != 2 {
		t.Fatal("早于 403 的租约成功不应解除整户暂停与登记")
	}
	// 之后取得的租约成功：账户已通过验证，全部解除
	service.permissionDeniedSucceeded(forbiddenBad, "third-model", aistudio.ChannelPlayground, time.Now())
	if pool.VerifyRequiredPaused(forbiddenBad) || len(registry.verifyModels(forbiddenBad)) != 0 {
		t.Fatal("晚于 403 的租约成功应解除整户暂停与登记")
	}
	if playground, _ := modelVerifyOf(t, pool, forbiddenBad, testRuntimeModel); playground.Reason == modelVerifyRequiredReason {
		t.Fatalf("单个模型的暂停一并清除: %+v", playground)
	}
}

// TestVerifySuccessRules 登记层的成功规则：单个模型时别的模型成功不算通过验证（只清普通 403 的通道），这个模型成功算；
// 两个及以上模型时任一模型成功算；租约早于最近一次需要验证账号的 403 时都不算，需要验证的条目保留
func TestVerifySuccessRules(t *testing.T) {
	registry := newPermissionDeniedRegistry("", nil)
	now := time.Now()
	verify := "AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: " + verifyRequiredMessage
	registry.record("one@example.com", "model-a", aistudio.ChannelPlayground, verify, "", now)
	registry.record("one@example.com", "model-b", aistudio.ChannelPlayground, "The caller does not have permission", "", now)
	if registry.succeededAt("one@example.com", "model-b", aistudio.ChannelPlayground, now.Add(time.Second)) {
		t.Fatal("单个模型时别的模型成功不算通过验证")
	}
	if entry, _ := permissionDeniedEntryOf(registry, "one@example.com"); len(entry.models) != 1 || entry.models[0].modelID != "model-a" {
		t.Fatalf("别的模型的普通 403 按通道清除、需要验证的保留: %+v", entry.models)
	}
	if registry.succeededAt("one@example.com", "model-a", aistudio.ChannelPlayground, now.Add(-time.Second)) {
		t.Fatal("早于 403 的租约在这个模型上成功不算")
	}
	if _, listed := permissionDeniedEntryOf(registry, "one@example.com"); !listed {
		t.Fatal("早于 403 的成功不应清除需要验证的条目")
	}
	if !registry.succeededAt("one@example.com", "models/MODEL-A", aistudio.ChannelBuild, now.Add(time.Second)) {
		t.Fatal("这个模型本身成功（不同写法、另一通道）算通过验证")
	}
	registry.record("two@example.com", "model-a", aistudio.ChannelPlayground, verify, "", now)
	registry.record("two@example.com", "model-b", aistudio.ChannelBuild, verify, "", now)
	if registry.succeededAt("two@example.com", "model-c", aistudio.ChannelPlayground, now.Add(-time.Second)) {
		t.Fatal("早于 403 的租约成功不算")
	}
	if !registry.succeededAt("two@example.com", "model-c", aistudio.ChannelPlayground, now.Add(time.Second)) {
		t.Fatal("两个模型时任一模型成功算")
	}
	// 同一模型的两个通道、大小写不同的写法只算一个
	registry.record("same@example.com", "model-a", aistudio.ChannelPlayground, verify, "", now)
	registry.record("same@example.com", "model-a", aistudio.ChannelBuild, verify, "", now)
	registry.record("same@example.com", "Model-A", aistudio.ChannelBuild, verify, "", now)
	if models := registry.verifyModels("same@example.com"); !slices.Equal(models, []string{"model-a"}) {
		t.Fatalf("同一模型只算一个: %v", models)
	}
}

// TestVerifySuccessReadLockOnly 单个模型需要验证的账户照常服务其他模型：别的模型成功、这个通道没有普通 403 条目时，succeededAt 只在读锁下
// 判断、不取写锁（持着读锁时照样返回，不与其他成功的请求串行）；要按通道清除普通 403 或通过验证时才取写锁
func TestVerifySuccessReadLockOnly(t *testing.T) {
	registry := newPermissionDeniedRegistry("", nil)
	now := time.Now()
	registry.record("one@example.com", "model-a", aistudio.ChannelPlayground, verifyRequiredMessage, "", now)
	registry.record("one@example.com", "model-b", aistudio.ChannelBuild, "The caller does not have permission", "", now)
	// wait 为持着读锁等它返回的时长：只走读锁的情况给足时间，要取写锁的情况持锁期间必然返回不了，短等即可
	succeeded := func(modelID string, channel aistudio.Channel, checkedAt time.Time, wait time.Duration) (bool, bool) {
		t.Helper()
		registry.mu.RLock()
		done := make(chan bool, 1)
		go func() { done <- registry.succeededAt("one@example.com", modelID, channel, checkedAt) }()
		select {
		case result := <-done:
			registry.mu.RUnlock()
			return result, true
		case <-time.After(wait):
			registry.mu.RUnlock()
			return <-done, false
		}
	}
	for _, item := range []struct {
		name      string
		modelID   string
		channel   aistudio.Channel
		checkedAt time.Time
	}{
		{name: "别的模型、没有登记的通道", modelID: "model-b", channel: aistudio.ChannelPlayground, checkedAt: now.Add(time.Second)},
		{name: "没有登记的模型", modelID: "model-c", channel: aistudio.ChannelPlayground, checkedAt: now.Add(time.Second)},
		{name: "这个模型、早于 403 的租约", modelID: "model-a", channel: aistudio.ChannelPlayground, checkedAt: now.Add(-time.Second)},
	} {
		if verified, readOnly := succeeded(item.modelID, item.channel, item.checkedAt, 5*time.Second); verified || !readOnly {
			t.Fatalf("%s：不改登记，应只走读锁（verified=%t 只读=%t）", item.name, verified, readOnly)
		}
	}
	if entry, _ := permissionDeniedEntryOf(registry, "one@example.com"); len(entry.models) != 2 {
		t.Fatalf("登记不应改变: %+v", entry.models)
	}
	if _, readOnly := succeeded("model-b", aistudio.ChannelBuild, now.Add(time.Second), 100*time.Millisecond); readOnly {
		t.Fatal("按通道清除普通 403 要取写锁")
	}
	if entry, _ := permissionDeniedEntryOf(registry, "one@example.com"); len(entry.models) != 1 || entry.models[0].modelID != "model-a" {
		t.Fatalf("普通 403 按通道清除、需要验证的保留: %+v", entry.models)
	}
	if verified, readOnly := succeeded("model-a", aistudio.ChannelBuild, now.Add(time.Second), 100*time.Millisecond); !verified || readOnly {
		t.Fatalf("这个模型本身成功算通过验证，要取写锁（verified=%t 只读=%t）", verified, readOnly)
	}
}

// TestVerifyPauseNotShortenedByAutoPause 同一账户 10 分钟内已有两次普通 403（两个模型）、再来一次需要验证账号（登记里已有另一个需要验证的
// 模型）：需要验证账号的 403 不计入多模型连续 403 的自动暂停，整户暂停仍是 30 天，不被 30 分钟的自动暂停挤掉
func TestVerifyPauseNotShortenedByAutoPause(t *testing.T) {
	upstream := newVerifyModelUpstream().deny(forbiddenBad, testRuntimeModel)
	service, pool, registry := verifyModelService(t, upstream)
	registry.record(forbiddenBad, "earlier-model", aistudio.ChannelPlayground, verifyRequiredMessage, "", time.Now().Add(-time.Hour))
	now := time.Now()
	service.forbidden.record(forbiddenBad, "plain-a", now)
	service.forbidden.record(forbiddenBad, "plain-b", now)
	if text, err, _ := runAccountRequest(t, service, "req-verify-auto", ""); err != nil || text != "hello" {
		t.Fatalf("应换号完成: text=%q err=%v", text, err)
	}
	pause, ok := globalPauseOf(t, pool, forbiddenBad)
	if !ok || pause.Reason != verifyRequiredPauseReason || time.Until(pause.Until) < verifyRequiredPause-time.Minute {
		t.Fatalf("整户暂停应为 30 天: %+v（存在=%t），日志 = %v", pause, ok, serviceLogMessages(service.requests))
	}
	if hasLogContaining(service.requests, "账号自动暂停") {
		t.Fatalf("需要验证账号的 403 不应触发多模型自动暂停，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestAllCandidatesModelVerifyReturns503 两个账户都只在这个模型上要求验证：这个模型的请求按 503 结束（Parked，不带重试时间），
// 不写 30 天的 Retry-After；其他模型照常成功
func TestAllCandidatesModelVerifyReturns503(t *testing.T) {
	upstream := newVerifyModelUpstream().deny(forbiddenBad, testRuntimeModel).deny(forbiddenGood, testRuntimeModel)
	service, pool, _ := verifyModelService(t, upstream)
	// preverifyRuntimeModelAccess 把 good 的检查时间写到了一小时后（测试替身），这里两个账户都要能写入
	if err := pool.ResetModelAccess(forbiddenGood); err != nil {
		t.Fatal(err)
	}
	_, err, _ := runAccountRequest(t, service, "req-verify-all", "")
	var cooling *aistudio.AllCoolingError
	if err == nil {
		t.Fatal("全部要求验证时应失败")
	}
	for _, accountID := range []string{forbiddenBad, forbiddenGood} {
		if playground, _ := modelVerifyOf(t, pool, accountID, testRuntimeModel); playground.Reason != modelVerifyRequiredReason {
			t.Fatalf("%s 应暂停这个模型: %+v 错误=%v 调用=%v 日志=%v", accountID, playground, err, upstream.called(), serviceLogMessages(service.requests))
		}
	}
	// 再来一个请求：选号即遇到候选全部只被需要验证挡住
	_, err, _ = runAccountRequest(t, service, "req-verify-all-2", "")
	if !errors.As(err, &cooling) || !cooling.Parked || !cooling.RetryAt().IsZero() || cooling.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("应返回 Parked 的全部冷却（503、不带重试时间），实际 %v", err)
	}
	if text, err, _ := runModelRequest(t, service, "req-verify-all-other", "", verifyOtherModel); err != nil || text != "hello" {
		t.Fatalf("其他模型应照常成功: text=%q err=%v", text, err)
	}
	if until, long := coolingDownLong(pool, &aistudio.AccountCoolingDownError{Until: time.Now().Add(verifyRequiredPause), Reason: modelVerifyRequiredReason},
		forbiddenBad, testRuntimeModel, time.Now()); long || !until.IsZero() {
		t.Fatal("需要验证账号的冷却不计每日类额度失败")
	}
	// 额度失败之后（调用方只在本请求有过额度失败时调用）换号遇到 Parked：按其余候选全部冷却结束，但不交出 Parked 的冷却（30 天兜底上限不进
	// Retry-After，见 TestQuotaThenVerifyParkedReturns429）
	if reason, parked, ok := primaryQuotaSelectionReason(cooling); !ok || reason != primaryQuotaReasonCooling || parked != nil {
		t.Fatalf("Parked 的全部冷却 = %q %v %t，期望按其余候选全部冷却、不带冷却错误", reason, parked, ok)
	}
}

// TestModelVerifyNoWorkerChurn 单个模型需要验证的冷却不让 Worker 被淘汰、清扫、轮换或补登记释放：这个模型即使成为全部驻留 Worker 上
// 的长时间冷却模型也不是热门冷却模型；状态的死槽位与不可调度都不计它
func TestModelVerifyNoWorkerChurn(t *testing.T) {
	warm := []string{partitionNormalA, partitionNormalB, partitionNormalC}
	manager, pool, requests := partitionTestManager(t, append(slices.Clone(warm), "normal-d@example.com"), nil, len(warm), len(warm)+1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	// 这个模型是分区的主用模型：死槽位、替补与淘汰的口径都会看它
	manager.setPrimaryModels([]string{testRuntimeModel}, nil)
	for _, accountID := range warm {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		manager.accounts[accountID].readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
		if written, _, err := pool.MarkModelVerifyCooldownIfGeneration(accountID, testRuntimeModel, pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(verifyRequiredPause)); err != nil || written != 2 {
			t.Fatalf("写入单个模型冷却: written=%d err=%v", written, err)
		}
	}
	if hot, counts := service.hotModelsIn(warm); len(hot) != 0 {
		t.Fatalf("单个模型需要验证不应让模型成为热门冷却模型: %v（%v）", hot, counts)
	}
	if added := service.registerUnschedulableWorkers(); added != 0 {
		t.Fatalf("不应补登记释放，新增 = %d", added)
	}
	if added := service.registerDeadWorkers(); added != 0 {
		t.Fatalf("不应补登记淘汰，新增 = %d", added)
	}
	if got := service.sweepDeadWorkers(ctx); got != 0 {
		t.Fatalf("不应冷却清扫，关闭 %d 个", got)
	}
	service.rotateCooledWorkers(ctx)
	if victim := manager.pickIdleVictim("", testRuntimeModel, victimDead, aistudio.PoolScopeNormal); victim != "" {
		t.Fatalf("不应按死槽位挑选淘汰对象: %s", victim)
	}
	service.retireCooledWorker(partitionNormalA, testRuntimeModel, aistudio.QuotaCooldown{Until: time.Now().Add(verifyRequiredPause), Kind: "单个模型需要验证", Reason: modelVerifyRequiredReason})
	manager.waitPrewarm()
	time.Sleep(300 * time.Millisecond)
	if got := sortedWarm(manager); !slices.Equal(got, warm) {
		t.Fatalf("驻留 Worker 不应变化: %v，日志 = %v", got, serviceLogMessages(requests))
	}
	for _, fragment := range []string{"补登记淘汰", "冷却淘汰", "冷却清扫", "冷却轮换", "排空", "WAA Worker 释放", "替补"} {
		if hasLogContaining(requests, fragment) {
			t.Fatalf("不应出现“%s”，日志 = %v", fragment, serviceLogMessages(requests))
		}
	}
	if manager.retirementCount() != 0 {
		t.Fatalf("不应有待淘汰登记: %d", manager.retirementCount())
	}
	counts := manager.deadWarmCounts(warm, pool.PartitionMembers())
	if counts[aistudio.PoolScopeNormal] != 0 {
		t.Fatalf("状态的死槽位不应计它: %+v", counts)
	}
	if unschedulable := pool.UnschedulableSet(warm); len(unschedulable) != 0 {
		t.Fatalf("不应不可调度: %v", unschedulable)
	}
	if adminCounts := adminAccountCounts(pool.StateCounts()); adminCounts.Cooldown != 0 {
		t.Fatalf("账户冷却计数不应计它: %+v", adminCounts)
	}
}

// TestModelVerifyClearedByVerifyAuthUpdateAndClear 单个模型需要验证的两个键：管理验证通过、认证文件更新、清除 403 记录都清除（清除 403 记录
// 计入 lifted，写单个模型的解除行），清除 403 记录不动普通“无权限（模型）”阶梯冷却
func TestModelVerifyClearedByVerifyAuthUpdateAndClear(t *testing.T) {
	for _, test := range []struct {
		name  string
		clear func(t *testing.T, service *trackedService)
		// ladderKept 为普通阶梯冷却应保留
		ladderKept bool
	}{
		{name: "验证通过", clear: func(_ *testing.T, service *trackedService) {
			service.clearPermissionDeniedState(partitionNormalA, "账户验证")
		}},
		{name: "认证文件更新", clear: func(_ *testing.T, service *trackedService) {
			service.forgetPermissionDenied(partitionNormalA, "外部更新")
		}},
		{name: "清除 403 记录", ladderKept: true, clear: func(t *testing.T, service *trackedService) {
			admin := &runtimeAdmin{pool: service.pool, service: service, requests: service.requests}
			result, err := admin.ClearPermissionDenied(context.Background(), api.PermissionDeniedClearInput{AccountIDs: []string{partitionNormalA}})
			if err != nil || result.Lifted != 1 {
				t.Fatalf("清除 403 记录应解除 1 个账户: %+v err=%v", result, err)
			}
			if !hasLogContaining(service.requests, "解除单个模型需要验证的暂停") {
				t.Fatalf("日志 = %v", serviceLogMessages(service.requests))
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := unschedulableTestService(ctx, manager, pool, requests)
			registry := withPermissionDenied(t, service)
			registry.record(partitionNormalA, testRuntimeModel, aistudio.ChannelPlayground, verifyRequiredMessage, "", time.Now())
			if _, _, err := pool.MarkModelVerifyCooldownIfGeneration(partitionNormalA, testRuntimeModel, pool.ModelAccessGeneration(partitionNormalA), time.Now(), time.Now().Add(verifyRequiredPause)); err != nil {
				t.Fatal(err)
			}
			if err := pool.MarkCooldownIfGeneration(partitionNormalA, "ladder-model", pool.ModelAccessGeneration(partitionNormalA), time.Now(), time.Now().Add(time.Hour), forbiddenModelReasonPrefix+"403"); err != nil {
				t.Fatal(err)
			}
			test.clear(t, service)
			if playground, build := modelVerifyOf(t, pool, partitionNormalA, testRuntimeModel); playground.Reason != "" || build.Reason != "" {
				t.Fatalf("两个键都应清除: %+v %+v", playground, build)
			}
			_, ladder := testStatus(t, pool, partitionNormalA).Cooldowns["ladder-model"]
			if ladder != test.ladderKept {
				t.Fatalf("阶梯冷却保留 = %t，期望 %t", ladder, test.ladderKept)
			}
		})
	}
}

// TestSystemicDropKeepsVerifyEntries 号池级 403 撤销登记时不撤销需要验证账号的条目（号池级判定本来就不计它们），普通条目照常撤销
func TestSystemicDropKeepsVerifyEntries(t *testing.T) {
	registry := newPermissionDeniedRegistry("", nil)
	since := time.Now().Add(-time.Minute)
	registry.record("verify@example.com", testRuntimeModel, aistudio.ChannelPlayground, verifyRequiredMessage, "", time.Now())
	registry.record("plain@example.com", testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", "", time.Now())
	if dropped := registry.dropSystemic(nil, aistudio.PoolScopeAll, testRuntimeModel, since); dropped != 1 {
		t.Fatalf("应只撤销普通条目，实际 %d", dropped)
	}
	if _, listed := permissionDeniedEntryOf(registry, "verify@example.com"); !listed {
		t.Fatal("需要验证账号的条目应保留")
	}
	if _, listed := permissionDeniedEntryOf(registry, "plain@example.com"); listed {
		t.Fatal("普通条目应撤销")
	}
}

// TestAdminCooldownVerifyKind 管理接口的冷却列表带需要验证账号的种类：整户暂停展开的每一条为 account，单个模型为 model，其他省略
func TestAdminCooldownVerifyKind(t *testing.T) {
	pool, _ := testAccountPool(t, "kind-account@example.com", "kind-model@example.com", "kind-plain@example.com")
	pauseVerifyRequired(t, pool, "kind-account@example.com")
	if _, _, err := pool.MarkModelVerifyCooldownIfGeneration("kind-model@example.com", testRuntimeModel, pool.ModelAccessGeneration("kind-model@example.com"), time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkCooldownIfGeneration("kind-plain@example.com", testRuntimeModel, pool.ModelAccessGeneration("kind-plain@example.com"), time.Now(), time.Now().Add(time.Hour), "每日限额: x"); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{pool: pool}
	cooldowns, err := admin.Cooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	for _, cooldown := range cooldowns {
		kinds[cooldown.AccountID] = append(kinds[cooldown.AccountID], cooldown.Channel+"="+cooldown.Verify)
	}
	slices.Sort(kinds["kind-model@example.com"])
	if !slices.Equal(kinds["kind-account@example.com"], []string{"playground=account"}) ||
		!slices.Equal(kinds["kind-model@example.com"], []string{"build=model", "playground=model"}) ||
		!slices.Equal(kinds["kind-plain@example.com"], []string{"playground="}) {
		t.Fatalf("种类 = %v", kinds)
	}
}

// watchCooldownEvents 订阅管理事件，返回收到的“cooldowns”推送（按到达顺序）
func watchCooldownEvents(t *testing.T, requests *requestRegistry) func() [][]api.AdminCooldown {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subscriber := requests.subscribe(ctx)
	events := requests.activateSubscriber(subscriber, nil, nil)
	var mu sync.Mutex
	var received [][]api.AdminCooldown
	go func() {
		for event := range events {
			if cooldowns, ok := event.Data.([]api.AdminCooldown); ok && event.Type == "cooldowns" {
				mu.Lock()
				received = append(received, cooldowns)
				mu.Unlock()
			}
		}
	}()
	return func() [][]api.AdminCooldown {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(received)
	}
}

// TestModelVerifyClearPublishesCooldowns 管理页“验证”通过清掉单个模型需要验证的暂停后立即推送冷却列表（不含这个账户的验证冷却）：
// 冷却列表平时只在请求结束后推送、管理页只重读账户与 403 登记，空闲时“403 单个模型需要验证”会一直按旧列表显示“该模型仍暂停”。
// 认证文件更新清掉时随账户列表的下一次合并推送一起推送
func TestModelVerifyClearPublishesCooldowns(t *testing.T) {
	const accountID = "verify-push@example.com"
	admin, _, _ := keepCatalogAdmin(t, accountID)
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.service.service = newProbeTestService(nil)
	registry := withPermissionDenied(t, admin.service)
	cooled := func() {
		t.Helper()
		registry.record(accountID, testRuntimeModel, aistudio.ChannelPlayground, verifyRequiredMessage, "", time.Now())
		if _, _, err := admin.pool.MarkModelVerifyCooldownIfGeneration(accountID, testRuntimeModel, admin.pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(verifyRequiredPause)); err != nil {
			t.Fatal(err)
		}
	}
	verifyFree := func(cooldowns []api.AdminCooldown) bool {
		for _, cooldown := range cooldowns {
			if cooldown.AccountID == accountID && cooldown.Verify != "" {
				return false
			}
		}
		return true
	}
	received := watchCooldownEvents(t, admin.requests)
	cooled()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if account, err := admin.VerifyAccount(ctx, accountID); err != nil || account.Authenticated == nil || !*account.Authenticated {
		t.Fatalf("验证应通过: %+v err=%v", account, err)
	}
	// 合并推送要 3 秒：1 秒内收到的只能是立即推送
	if !waitUntil(time.Second, func() bool {
		events := received()
		return len(events) > 0 && verifyFree(events[len(events)-1])
	}) {
		t.Fatalf("验证通过后应立即推送不含验证冷却的冷却列表，收到 %d 次", len(received()))
	}
	before := len(received())
	cooled()
	admin.service.forgetPermissionDenied(accountID, "认证文件更新")
	if !waitUntil(2*modelAccessPublishDelay, func() bool {
		events := received()
		return len(events) > before && verifyFree(events[len(events)-1])
	}) {
		t.Fatal("认证文件更新清掉验证冷却后应随合并推送推送冷却列表")
	}
}
