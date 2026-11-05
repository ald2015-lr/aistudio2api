package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// omniDeniedCall 为一次 Generate：账号、这次尝试挂着的 OMNI_IMAGE_BINDING、请求体里内联发送的图片数与调用时间
type omniDeniedCall struct {
	account string
	binding bool
	images  int
	at      time.Time
}

// omniDeniedUpstream 模拟官网对 Omni 请求返回 HTTP 403 / 协议错误码 7：denyImages 时带内联图片的请求一律拒绝（imageDenials 大于 0 时只拒绝前
// 这么多次，之后的调用在 then 不为空时返回 then），denyText 时纯文字请求一律拒绝，其余正常返回；unparsed 时拒绝的错误体不是状态数组
// （HTML 403 页面，协议错误码为 0）；first 不为空时第一次调用返回它（额度失败等）；hook 不为空时每次调用先在锁外执行它（call 为第几次调用，
// 从 1 起），返回的错误不为空时这次调用返回它（准备 WAA proof 失败等发出前的本地错误）。附件上传按账号没有 Drive 授权处理：内容原样返回，
// 附件照样内联发送；文件引用复制（文件绑定的请求）只记下目标账号，内容原样返回
type omniDeniedUpstream struct {
	mu           sync.Mutex
	denyImages   bool
	imageDenials int
	then         error
	denyText     bool
	unparsed     bool
	first        error
	hook         func(call int, request aistudio.GenerateRequest) error
	calls        []omniDeniedCall
	copies       []string
}

func (upstream *omniDeniedUpstream) CopyFileReferencesToLease(
	_ context.Context, lease *aistudio.AccountLease, contents []aistudio.Content,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	upstream.mu.Lock()
	upstream.copies = append(upstream.copies, lease.Account().ID)
	upstream.mu.Unlock()
	return contents, nil, nil
}

func (upstream *omniDeniedUpstream) UploadInlineMediaToLease(
	_ context.Context, _ *aistudio.AccountLease, contents []aistudio.Content, temporary *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	if temporary == nil {
		temporary = aistudio.NewTemporaryFileCopiesForTest(nil)
	}
	return contents, temporary, nil
}

func (upstream *omniDeniedUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *omniDeniedUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *omniDeniedUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	images := aistudio.InteractionInlineImageCount(request.Contents)
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, omniDeniedCall{
		account: request.AccountID, binding: aistudio.InteractionImageBindingFromContext(ctx), images: images, at: time.Now(),
	})
	deny := images > 0 && upstream.denyImages || images == 0 && upstream.denyText
	if deny && images > 0 && upstream.imageDenials > 0 {
		deny = len(upstream.calls) <= upstream.imageDenials
	}
	unparsed := upstream.unparsed
	var first error
	if len(upstream.calls) == 1 {
		first = upstream.first
	}
	if upstream.imageDenials > 0 && len(upstream.calls) > upstream.imageDenials && first == nil {
		first = upstream.then
	}
	call, hook := len(upstream.calls), upstream.hook
	upstream.mu.Unlock()
	if hook != nil {
		if hookErr := hook(call, request); hookErr != nil {
			return nil, hookErr
		}
	}
	if first != nil {
		return nil, first
	}
	if deny && unparsed {
		return nil, &aistudio.RPCError{
			Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Body: "<html><title>Error 403 (Forbidden)!!1</title></html>",
		}
	}
	if deny {
		return nil, &aistudio.RPCError{
			Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission",
		}
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (upstream *omniDeniedUpstream) snapshot() []omniDeniedCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// omniDeniedService 返回 Worker 都已就绪、目录里有 Omni 模型与普通文本模型（testRuntimeModel，没有 interactions_api）的运行中生成服务，
// 挂上 403 无权限账户登记表与出口判定器
func omniDeniedService(t *testing.T, upstream aistudio.Service, accounts ...string) (*trackedService, *aistudio.AccountPool, *permissionDeniedRegistry) {
	t.Helper()
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
	service.workers.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
			{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.egress = newEgressGuard(pool, service.requests)
	return service, pool, withPermissionDenied(t, service)
}

// omniImageRequest 为带一张 PNG 的 Omni 多轮请求（与线上“用户[文本 1、图片 1、文本 1]”相同的形状）；pngData 为 nil 时为纯文字
func omniImageRequest(id string, pngData []byte) aistudio.GenerateRequest {
	parts := []aistudio.Part{{Text: "参考这张图"}}
	if pngData != nil {
		parts = append(parts, aistudio.Part{InlineData: &aistudio.Blob{MIME: "image/png", Data: pngData}})
	}
	parts = append(parts, aistudio.Part{Text: "生成视频"})
	return aistudio.GenerateRequest{ID: id, Model: omniDriveModel, Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "你好"}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "你好，需要什么"}}},
		{Role: aistudio.RoleUser, Parts: parts},
	}}
}

// egressOtherCount 返回出口判定器记下的“其他失败”次数（全部出口、两个通道）
func egressOtherCount(guard *egressGuard) int {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	total := 0
	for _, state := range guard.states {
		total += state.history.sum(time.Now(), time.Hour, -1).other
	}
	return total
}

// forbiddenEventCount 返回多模型 403 暂停判定记下的 403 次数
func forbiddenEventCount(tracker *forbiddenTracker) int {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	total := 0
	for _, events := range tracker.events {
		total += len(events)
	}
	return total + len(tracker.models)
}

// TestOmniImageDeniedTriesTwoAccounts 带内联图片的 Omni 请求被官网以 HTTP 403 / 协议错误码 7 拒绝：只试 2 个账户（第一次 403 后换一个账户
// 再试一次，第二次仍 403 就结束，不再连换 5 个），账户不进入单模型冷却、不计入多模型 403 暂停、不登记为 403 无权限账户、不计入出口判定，
// 两次之间不退避；每次写一行请求日志（第 n/2 个账户、OMNI_IMAGE_BINDING 的值），对外按 InteractionImageRejectedError（400）结束
func TestOmniImageDeniedTriesTwoAccounts(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com", "d-omni@example.com", "e-omni@example.com"}
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	text, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied", pngData))
	var rejected *aistudio.InteractionImageRejectedError
	if text != "" || !errors.As(err, &rejected) {
		t.Fatalf("text=%q err=%v，期望以带图请求被拒绝结束", text, err)
	}
	if rejected.Tried != 2 || rejected.Images != 1 || !rejected.ImageBinding || rejected.Model != omniDriveModel ||
		rejected.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("对外错误 = %+v", rejected)
	}
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusForbidden {
		t.Fatalf("内部错误应带最后一次上游 403: %v", err)
	}
	calls := upstream.snapshot()
	if len(calls) != 2 || calls[0].account == calls[1].account || calls[0].images != 1 || !calls[0].binding || !calls[1].binding {
		t.Fatalf("Generate = %+v，期望在 2 个不同账户上各发一次、OMNI_IMAGE_BINDING 开启", calls)
	}
	// 计入连续失败时第一次换号前退避 200ms（generateRetryBackoff）
	if gap := calls[1].at.Sub(calls[0].at); gap >= generateRetryBackoff(1)*3/4 {
		t.Fatalf("两次之间不应退避，间隔 %s", gap)
	}
	assertNoAccountCooldown(t, pool)
	if entries := registry.list(); len(entries) != 0 {
		t.Fatalf("不应登记为 403 无权限账户: %+v", entries)
	}
	if count := forbiddenEventCount(service.forbidden); count != 0 {
		t.Fatalf("不应计入多模型 403 暂停与单模型 403 档位: %d", count)
	}
	if count := egressOtherCount(service.egress); count != 0 {
		t.Fatalf("不应计入出口判定: %d", count)
	}
	messages := requestLogMessages(service.requests, "req-image-denied")
	want := []string{
		imageDeniedNote(1, 2, imageDeniedSwitch, true, 1),
		imageDeniedNote(2, 2, imageDeniedLimitReached, true, 1),
	}
	var notes []string
	for _, message := range messages {
		if strings.HasPrefix(message, "视频模型带图请求被官网拒绝") {
			notes = append(notes, message)
		}
		if strings.Contains(message, "换号重试") || strings.Contains(message, "已连续") {
			t.Fatalf("不应按一般失败写换号或连续失败: %q", message)
		}
	}
	if !slices.Equal(notes, want) {
		t.Fatalf("请求日志 = %q\n期望 %q", notes, want)
	}
	if !strings.Contains(want[0], "第 1/2 个账户，换一个账户再试 | OMNI_IMAGE_BINDING=true | 图片=1 张") ||
		!strings.Contains(want[1], "第 2/2 个账户，不再换号，按 400 返回") {
		t.Fatalf("日志写法 = %q", want)
	}
	for _, message := range serviceLogMessages(service.requests) {
		if strings.Contains(message, "账号冷却") || strings.Contains(message, "账号自动暂停") || strings.Contains(message, "账号切换") {
			t.Fatalf("不应冷却、暂停或写账号切换: %q", message)
		}
	}
}

// TestOmniImageDeniedThenSucceeds 带图请求第一次 403、换一个账户成功：成功的请求照常返回，被拒的账户不冷却、不登记
func TestOmniImageDeniedThenSucceeds(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true, imageDenials: 1}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com", "c-omni@example.com")
	text, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-retry", pngData))
	calls := upstream.snapshot()
	if err != nil || text != "ok" || len(calls) != 2 || calls[0].account == calls[1].account {
		t.Fatalf("text=%q err=%v calls=%+v，期望换一个账户后成功", text, err, calls)
	}
	assertNoAccountCooldown(t, pool)
	if entries := registry.list(); len(entries) != 0 || forbiddenEventCount(service.forbidden) != 0 {
		t.Fatalf("被拒的账户不应登记: %+v", entries)
	}
}

// TestOmniImageDeniedPinnedAccount 指定账号的带图请求被 403 拒绝：不能换号，1/1 个账户后结束，同样不冷却、不登记
func TestOmniImageDeniedPinnedAccount(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	request := omniImageRequest("req-image-pinned", pngData)
	request.AccountID = "b-omni@example.com"
	_, err, _ := runOmniDriveRequest(t, service, request)
	var rejected *aistudio.InteractionImageRejectedError
	if !errors.As(err, &rejected) || rejected.Tried != 1 || len(upstream.snapshot()) != 1 {
		t.Fatalf("err=%v calls=%+v", err, upstream.snapshot())
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
	if messages := requestLogMessages(service.requests, "req-image-pinned"); !slices.Contains(messages, imageDeniedNote(1, 1, imageDeniedLimitReached, true, 1)) {
		t.Fatalf("请求日志 = %q", messages)
	}
}

// TestOmniTextDeniedKeepsRules 纯文字的 Omni 请求遇到 403 照原规则：每个账户都进入单模型 403 冷却、登记为 403 无权限账户、计入出口判定，
// 换号直到候选用完（3 个账户各试一次），不按带图请求的 2 个账户结束，也不写带图请求的日志
func TestOmniTextDeniedKeepsRules(t *testing.T) {
	accounts := []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"}
	upstream := &omniDeniedUpstream{denyText: true}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-text-denied", nil))
	var rejected *aistudio.InteractionImageRejectedError
	var rpcError *aistudio.RPCError
	if errors.As(err, &rejected) || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusForbidden {
		t.Fatalf("err=%v，期望按上游 403 原样结束", err)
	}
	if calls := upstream.snapshot(); len(calls) != len(accounts) || calls[0].images != 0 {
		t.Fatalf("Generate = %+v，期望 %d 个账户各一次", calls, len(accounts))
	}
	for _, accountID := range accounts {
		if _, _, ok := modelCooldownUntil(t, pool, accountID, omniDriveModel); !ok {
			t.Fatalf("账户 %s 应进入单模型 403 冷却", accountID)
		}
		if _, ok := permissionDeniedEntryOf(registry, accountID); !ok {
			t.Fatalf("账户 %s 应登记为 403 无权限账户", accountID)
		}
	}
	if egressOtherCount(service.egress) != len(accounts) {
		t.Fatalf("出口判定应记 %d 次其他失败，得到 %d", len(accounts), egressOtherCount(service.egress))
	}
	for _, message := range requestLogMessages(service.requests, "req-text-denied") {
		if strings.HasPrefix(message, "视频模型带图请求被官网拒绝") {
			t.Fatalf("纯文字请求不应写带图请求的日志: %q", message)
		}
	}
}

// TestOmniImageBindingHotConfig 视频模型带图校验值（OMNI_IMAGE_BINDING）保存后立即生效、不提示需要重启：写进 .env，读取时总是返回，
// 旧版页面不带该字段时沿用现值；保存后下一个请求的尝试立即挂上新值，带图 403 的日志写出当时的取值
func TestOmniImageBindingHotConfig(t *testing.T) {
	clearAppConfigEnv(t)
	t.Setenv("OMNI_IMAGE_BINDING", "")
	os.Unsetenv("OMNI_IMAGE_BINDING")
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, _ := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	manager, path := newMediaHotManager(t)
	generation := manager.current
	admin := generation.admin.(*runtimeAdmin)
	admin.service, admin.pool, generation.service = service, pool, service
	ctx := context.Background()

	if value := runtimeConfigDTO(config.Default()); value.OmniImageBinding == nil || !*value.OmniImageBinding {
		t.Fatalf("读取的默认值 = %v", value.OmniImageBinding)
	}
	value := runtimeConfigDTO(config.Default())
	off := false
	value.OmniImageBinding = &off
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OmniImageBinding == nil || *updated.OmniImageBinding || updated.ServiceRestartRequired || updated.ManagementRestartRequired {
		t.Fatalf("保存后返回 = %v，需要重启 = %v/%v", updated.OmniImageBinding, updated.ServiceRestartRequired, updated.ManagementRestartRequired)
	}
	if service.omniImageBinding() {
		t.Fatal("保存后应立即生效")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "OMNI_IMAGE_BINDING=false\n") {
		t.Fatalf(".env 缺少 OMNI_IMAGE_BINDING=false:\n%s", data)
	}
	if !hasLogContaining(manager.requests, "视频模型带图校验值设置已更新并立即生效", "开启=false") {
		t.Fatal("缺少生效日志")
	}
	_, err, _ = runOmniDriveRequest(t, service, omniImageRequest("req-binding-off", pngData))
	var rejected *aistudio.InteractionImageRejectedError
	calls := upstream.snapshot()
	if !errors.As(err, &rejected) || rejected.ImageBinding || len(calls) != 2 || calls[0].binding || calls[1].binding {
		t.Fatalf("保存后下一个请求应按关闭发送: err=%v calls=%+v", err, calls)
	}
	if messages := requestLogMessages(service.requests, "req-binding-off"); !slices.Contains(messages, imageDeniedNote(1, 2, imageDeniedSwitch, false, 1)) {
		t.Fatalf("日志应写出 OMNI_IMAGE_BINDING=false: %q", messages)
	}

	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.OmniImageBinding == nil || *read.OmniImageBinding || read.ServiceRestartRequired {
		t.Fatalf("读取配置: %v %v", read.OmniImageBinding, err)
	}
	// 旧版页面不带该字段：沿用现值
	legacy := read
	legacy.OmniImageBinding = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil || service.omniImageBinding() {
		t.Fatalf("旧版页面保存后应沿用现值（关闭）: %v", err)
	}
	on := true
	read.OmniImageBinding = &on
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil || !service.omniImageBinding() {
		t.Fatalf("重新开启应立即生效: %v", err)
	}
	before := len(upstream.snapshot())
	_, _, _ = runOmniDriveRequest(t, service, omniImageRequest("req-binding-on", pngData))
	if calls := upstream.snapshot()[before:]; len(calls) != 2 || !calls[0].binding || !calls[1].binding {
		t.Fatalf("重新开启后下一个请求应按开启发送: %+v", calls)
	}
}

// TestOmniImageBindingLiveFields OMNI_IMAGE_BINDING 是热更新字段：只有它不同时 liveFieldsEqual 为假、requiresRebuild 为假；生效配置比较包含它，
// 旧版页面不带这个字段时按生效值比较；测试直接构造的服务（零值）与线上默认一样开启
func TestOmniImageBindingLiveFields(t *testing.T) {
	active := config.Default()
	changed := active
	changed.OmniImageBinding = false
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("视频模型带图校验值变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含视频模型带图校验值")
	}
	dto.OmniImageBinding = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这个字段时按生效值比较")
	}
	service := &trackedService{}
	if !service.omniImageBinding() {
		t.Fatal("零值服务应按默认开启")
	}
	if !service.setOmniImageBinding(false) || service.omniImageBinding() || service.setOmniImageBinding(false) {
		t.Fatal("关闭应有变化且只报告一次")
	}
	if !service.setOmniImageBinding(true) || !service.omniImageBinding() {
		t.Fatal("重新开启应有变化")
	}
	if note := imageDeniedNote(1, 2, imageDeniedSwitch, true, 3); note != fmt.Sprintf(
		"视频模型带图请求被官网拒绝（没有权限），可能是校验值与官网不一致；不冷却、不登记账户，第 1/2 个账户，换一个账户再试 | OMNI_IMAGE_BINDING=true | 图片=%d 张", 3) {
		t.Fatalf("日志 = %q", note)
	}
}

// imageDeniedNotes 取出请求日志里带图请求被 403 拒绝的那几行
func imageDeniedNotes(messages []string) []string {
	var notes []string
	for _, message := range messages {
		if strings.HasPrefix(message, "视频模型带图请求被官网拒绝") {
			notes = append(notes, message)
		}
	}
	return notes
}

// TestOmniImageDeniedSingleAccount 号池里只有 1 个启用账户的带图请求被 403 拒绝：换号次数已用完，第一次 403 的日志就写明没有其他可换的账户、
// 不再换号（不写“换一个账户再试”，也不再补写一行），对外 tried on 1 account，同样不冷却、不登记
func TestOmniImageDeniedSingleAccount(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com")
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-single", pngData))
	var rejected *aistudio.InteractionImageRejectedError
	if !errors.As(err, &rejected) || rejected.Tried != 1 || len(upstream.snapshot()) != 1 {
		t.Fatalf("err=%v calls=%+v", err, upstream.snapshot())
	}
	if !strings.Contains(rejected.PublicMessage(), "tried on 1 account") {
		t.Fatalf("对外说明 = %q", rejected.PublicMessage())
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 || egressOtherCount(service.egress) != 0 {
		t.Fatal("不应登记、计入多模型 403 暂停或出口判定")
	}
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-single"))
	want := []string{imageDeniedNote(1, 2, imageDeniedNoOther, true, 1)}
	if !slices.Equal(notes, want) {
		t.Fatalf("请求日志 = %q\n期望 %q", notes, want)
	}
	if !strings.Contains(want[0], "第 1/2 个账户，没有其他可换的账户，不再换号，按 400 返回（可以去掉图片重试） | OMNI_IMAGE_BINDING=true") {
		t.Fatalf("日志写法 = %q", want[0])
	}
}

// TestOmniImageDeniedNoOtherCandidate 第一次带图 403 后还能换号（号池里有 2 个启用账户），换号的选号却没有候选（另一个账户接不了 Omni）：
// 先写“换一个账户再试”，以这个 403 结束时补写一行“没有换到其他账户”（行尾附选号失败的原因），与对外的 tried on 1 account 一致
func TestOmniImageDeniedNoOtherCandidate(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-text@example.com")
	if err := pool.SetCatalog("b-text@example.com", aistudio.BenefitTierFree, []aistudio.Model{
		{ID: "gemini-text-test", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-no-other", pngData))
	var rejected *aistudio.InteractionImageRejectedError
	calls := upstream.snapshot()
	if !errors.As(err, &rejected) || rejected.Tried != 1 || len(calls) != 1 || calls[0].account != "a-omni@example.com" {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 {
		t.Fatal("不应登记或计入多模型 403 暂停")
	}
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-no-other", err, "")
	end := imageDeniedNotes(requestLogMessages(service.requests, "req-image-no-other"))[1]
	if !strings.Contains(end, "没有换到其他账户，不再换号，按 400 返回（共试 1 个账户，可以去掉图片重试） | OMNI_IMAGE_BINDING=true | 图片=1 张 | 原因=") {
		t.Fatalf("日志写法 = %q", end)
	}
	if note := imageDeniedEndNote(1, false, 2, ""); strings.Contains(note, "原因=") || !strings.HasSuffix(note, "| OMNI_IMAGE_BINDING=false | 图片=2 张") {
		t.Fatalf("没有选号失败原因时不写原因: %q", note)
	}
}

// assertImageDeniedEndedAfterSwitch 断言第一次带图 403 写了“换一个账户再试”、换号的选号却失败时，请求以带图被拒结束（400，tried on 1 account）：
// 不换成号池繁忙、全部冷却、额度不足等选号错误；请求日志在“换一个账户再试”之后补写一行“没有换到其他账户”，行尾附选号失败的原因
// （reason 不为空时要求就是它），不写“返回 429”的选号日志
func assertImageDeniedEndedAfterSwitch(t *testing.T, service *trackedService, requestID string, err error, reason string) {
	t.Helper()
	var rejected *aistudio.InteractionImageRejectedError
	if !errors.As(err, &rejected) || rejected.Tried != 1 || rejected.HTTPStatus() != http.StatusBadRequest ||
		!strings.Contains(rejected.PublicMessage(), "tried on 1 account") {
		t.Fatalf("err=%v，期望按带图被拒（400，试过 1 个账户）结束", err)
	}
	var busy *aistudio.CapacityBusyError
	var cooling *aistudio.AllCoolingError
	var quota *aistudio.PrimaryQuotaError
	var driveRequired *aistudio.DriveRequiredError
	if errors.As(err, &busy) || errors.As(err, &cooling) || errors.As(err, &quota) || errors.As(err, &driveRequired) {
		t.Fatalf("err=%v，不应换成选号错误", err)
	}
	messages := requestLogMessages(service.requests, requestID)
	notes := imageDeniedNotes(messages)
	endPrefix := imageDeniedEndNote(1, true, 1, "") + " | 原因="
	if len(notes) != 2 || notes[0] != imageDeniedNote(1, 2, imageDeniedSwitch, true, 1) ||
		!strings.HasPrefix(notes[1], endPrefix) || len(notes[1]) == len(endPrefix) {
		t.Fatalf("请求日志 = %q，期望先写换一个账户再试、再补写没有换到其他账户与原因", notes)
	}
	if reason != "" && notes[1] != imageDeniedEndNote(1, true, 1, reason) {
		t.Fatalf("补写的一行 = %q，期望原因为 %q", notes[1], reason)
	}
	for _, message := range messages {
		if strings.Contains(message, "返回 429") {
			t.Fatalf("不应按选号错误写返回 429: %q", message)
		}
	}
}

// TestOtherModelImageDeniedKeepsRules 不是 Interaction 模型的普通文本模型（目录没有 interactions_api）带内联图片的请求遇到 HTTP 403 /
// 协议错误码 7 照原规则：每个账户都进入单模型 403 冷却、登记为 403 无权限账户、计入多模型 403 暂停判定与出口判定，换号直到候选用完
// （3 个账户各试一次，不是 2 个），不按带图被拒（400）结束，也不写带图请求的日志。带图 403 的特殊处理只限 Interaction 模型
func TestOtherModelImageDeniedKeepsRules(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"}
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	request := omniImageRequest("req-other-image-denied", pngData)
	request.Model = testRuntimeModel
	_, err, _ := runOmniDriveRequest(t, service, request)
	var rejected *aistudio.InteractionImageRejectedError
	var rpcError *aistudio.RPCError
	if errors.As(err, &rejected) || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusForbidden || rpcError.Code != 7 {
		t.Fatalf("err=%v，期望按上游 403 原样结束", err)
	}
	calls := upstream.snapshot()
	if len(calls) != len(accounts) {
		t.Fatalf("Generate = %+v，期望 %d 个账户各一次", calls, len(accounts))
	}
	for _, call := range calls {
		if call.images != 1 {
			t.Fatalf("Generate = %+v，期望每次都带 1 张内联图片", calls)
		}
	}
	for _, accountID := range accounts {
		if _, _, ok := modelCooldownUntil(t, pool, accountID, testRuntimeModel); !ok {
			t.Fatalf("账户 %s 应进入单模型 403 冷却", accountID)
		}
		if _, ok := permissionDeniedEntryOf(registry, accountID); !ok {
			t.Fatalf("账户 %s 应登记为 403 无权限账户", accountID)
		}
	}
	if count := forbiddenEventCount(service.forbidden); count < len(accounts) {
		t.Fatalf("多模型 403 暂停判定应记下 %d 个账户的 403，得到 %d", len(accounts), count)
	}
	if egressOtherCount(service.egress) != len(accounts) {
		t.Fatalf("出口判定应记 %d 次其他失败，得到 %d", len(accounts), egressOtherCount(service.egress))
	}
	if notes := imageDeniedNotes(requestLogMessages(service.requests, "req-other-image-denied")); len(notes) != 0 {
		t.Fatalf("其他模型不应写带图请求被拒的日志: %q", notes)
	}
}

// omniDeniedDriveUpstream 在带 Drive 记忆的 omniDriveUpstream 之上对带内联图片的 Interaction 请求返回 HTTP 403 / 协议错误码 7。上传时只收到
// 视频等要传 Drive 的附件（能内联的图片在上传前拿掉、之后放回，见 holdInlineImages），所以图片照样内联发送
type omniDeniedDriveUpstream struct {
	*omniDriveUpstream
}

func (upstream *omniDeniedDriveUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if aistudio.InteractionInlineImageCount(request.Contents) == 0 {
		return upstream.omniDriveUpstream.Generate(ctx, request)
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/denied")
	upstream.mu.Unlock()
	return nil, &aistudio.RPCError{
		Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission",
	}
}

// TestOmniImageDeniedDriveExhausted Omni 请求带客户端视频（要 Drive）、一张 PNG 和文本，号池里只有一个记着 Drive 可用的账户、其余记着不可用：
// 它上传视频后发送、被官网以 403 / 协议错误码 7 拒绝，写“换一个账户再试”；换号的 Drive 选号用尽（可用账户都试过、没有未知账户）时
// 以这个 403 结束，按带图被拒的 400 返回并补写一行，不按号池繁忙 429 结束（被拒的账户不冷却，中转按 429 重发多半又落到它上面）
func TestOmniImageDeniedDriveExhausted(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-drive@example.com", "b-nodrive@example.com", "c-nodrive@example.com"}
	base := newOmniDriveUpstream(accounts, "a-drive@example.com")
	base.remember("a-drive@example.com", aistudio.DriveStateAvailable)
	base.remember("b-nodrive@example.com", aistudio.DriveStateUnavailable)
	base.remember("c-nodrive@example.com", aistudio.DriveStateUnavailable)
	service, pool, registry := omniDeniedService(t, &omniDeniedDriveUpstream{omniDriveUpstream: base}, accounts...)
	request := aistudio.GenerateRequest{ID: "req-image-denied-drive", Model: omniDriveModel, Contents: []aistudio.Content{{
		Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypmp42video")}},
			{InlineData: &aistudio.Blob{MIME: "image/png", Data: pngData}},
			{Text: "让视频里的人举起这张图"},
		},
	}}}
	_, err, _ := runOmniDriveRequest(t, service, request)
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-drive", err, "可能有 Drive 授权的账号本请求都试过或现在用不了")
	calls, uploads, _ := base.snapshot()
	if !slices.Equal(calls, []string{"a-drive@example.com/denied"}) || !slices.Equal(uploads, []string{"a-drive@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，期望只在记着 Drive 可用的账户上上传并发送一次", calls, uploads)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 || egressOtherCount(service.egress) != 0 {
		t.Fatal("不应登记、计入多模型 403 暂停或出口判定")
	}
}

// TestOmniImageDeniedSwitchQueueExceeded 主用模型的带图请求第一次 403 后换号，另一个账户一直在忙、排队超过预算：以这个 403 结束，
// 按带图被拒的 400 返回并补写一行（原因为排队超限），不按主用模型排队超限的 429 结束
func TestOmniImageDeniedSwitchQueueExceeded(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	pool.SetPerAccountConcurrency(1)
	service.setPrimaryModelSettings([]string{omniDriveModel}, 300*time.Millisecond, nil, 0)
	release := holdLease(t, pool, "b-omni@example.com")
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied-busy", pngData))
	release()
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-busy", err, "")
	if calls := upstream.snapshot(); len(calls) != 1 || calls[0].account != "a-omni@example.com" {
		t.Fatalf("Generate = %+v，期望只在空闲的账户上发送一次", calls)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
}

// TestOmniImageDeniedSwitchAllCooling 带图请求第一次 403 后换号，其余候选都在长时间冷却（选号返回全部冷却）：以这个 403 结束，
// 按带图被拒的 400 返回并补写一行，不按全部冷却结束；被拒的账户不冷却
func TestOmniImageDeniedSwitchAllCooling(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	if err := pool.MarkCooldownIfGeneration("b-omni@example.com", omniDriveModel, pool.ModelAccessGeneration("b-omni@example.com"),
		time.Now(), time.Now().Add(2*time.Hour), "每日限额"); err != nil {
		t.Fatal(err)
	}
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied-cooling", pngData))
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-cooling", err, "")
	if calls := upstream.snapshot(); len(calls) != 1 || calls[0].account != "a-omni@example.com" {
		t.Fatalf("Generate = %+v，期望只在没有冷却的账户上发送一次", calls)
	}
	if _, _, ok := modelCooldownUntil(t, pool, "a-omni@example.com", omniDriveModel); ok {
		t.Fatal("被拒的账户不应冷却")
	}
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
}

// TestOmniImageDeniedAfterQuotaFailure 带图请求先在一个账户上遇到额度失败（照原规则冷却、换号），换到的账户带图 403、写“换一个账户再试”，
// 再换号时其余候选都在冷却：最近一次尝试是带图 403，按带图被拒的 400 返回并补写一行，不按额度不足的 429 结束；带图 403 的账户不冷却
func TestOmniImageDeniedAfterQuotaFailure(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"}
	upstream := &omniDeniedUpstream{denyImages: true, first: dailyQuotaFailure()}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	if err := pool.MarkCooldownIfGeneration("c-omni@example.com", omniDriveModel, pool.ModelAccessGeneration("c-omni@example.com"),
		time.Now(), time.Now().Add(2*time.Hour), "每日限额"); err != nil {
		t.Fatal(err)
	}
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied-quota", pngData))
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-quota", err, "")
	calls := upstream.snapshot()
	if len(calls) != 2 || calls[0].account == calls[1].account {
		t.Fatalf("Generate = %+v，期望额度失败换号后带图 403、不再发送", calls)
	}
	if _, _, ok := modelCooldownUntil(t, pool, calls[1].account, omniDriveModel); ok {
		t.Fatalf("带图 403 的账户 %s 不应冷却", calls[1].account)
	}
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
}

// TestOmniImageUnparsed403KeepsRules 带图请求收到错误体不是状态数组的 403（HTML 错误页，协议错误码为 0）：与图片、校验值无关，
// 照原规则每个账户单模型 403 冷却、登记为 403 无权限账户、计入出口判定，换号直到候选用完，不按带图被拒（400）结束，也不写带图请求的日志
func TestOmniImageUnparsed403KeepsRules(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"}
	upstream := &omniDeniedUpstream{denyImages: true, unparsed: true}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-html-403", pngData))
	var rejected *aistudio.InteractionImageRejectedError
	var rpcError *aistudio.RPCError
	if errors.As(err, &rejected) || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusForbidden || rpcError.Code != 0 {
		t.Fatalf("err=%v，期望按上游 403 原样结束", err)
	}
	if calls := upstream.snapshot(); len(calls) != len(accounts) || calls[0].images != 1 {
		t.Fatalf("Generate = %+v，期望 %d 个账户各一次", calls, len(accounts))
	}
	for _, accountID := range accounts {
		if _, _, ok := modelCooldownUntil(t, pool, accountID, omniDriveModel); !ok {
			t.Fatalf("账户 %s 应进入单模型 403 冷却", accountID)
		}
		if _, ok := permissionDeniedEntryOf(registry, accountID); !ok {
			t.Fatalf("账户 %s 应登记为 403 无权限账户", accountID)
		}
	}
	if egressOtherCount(service.egress) != len(accounts) {
		t.Fatalf("出口判定应记 %d 次其他失败，得到 %d", len(accounts), egressOtherCount(service.egress))
	}
	if notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-html-403")); len(notes) != 0 {
		t.Fatalf("不应写带图请求被拒的日志: %q", notes)
	}
}

// TestInteractionImagePermissionDenied 带图被拒只认 HTTP 403 且协议错误码 7：错误体解析不出状态的 403（协议错误码 0）、其他状态码、
// 组织停用都不算；包装过的错误照样识别
func TestInteractionImagePermissionDenied(t *testing.T) {
	denied := &aistudio.RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 协议错误码 7", denied, true},
		{"包装过的 403 协议错误码 7", fmt.Errorf("生成失败: %w", denied), true},
		{"错误体不是状态数组的 403", &aistudio.RPCError{StatusCode: http.StatusForbidden, Body: "<html>403</html>"}, false},
		{"400 协议错误码 7", &aistudio.RPCError{StatusCode: http.StatusBadRequest, Code: 7}, false},
		{"组织停用", errors.Join(denied, aistudio.ErrOrgDisabled), false},
		{"不是上游错误", errors.New("403"), false},
	}
	for _, item := range cases {
		if got := interactionImagePermissionDenied(item.err); got != item.want {
			t.Fatalf("%s: 得到 %t，期望 %t", item.name, got, item.want)
		}
	}
	// isPermissionDenied 仍把协议错误码 0 的 403 算作无权限（纯文字与其他模型的原规则不变）
	if !isPermissionDenied(&aistudio.RPCError{StatusCode: http.StatusForbidden, Body: "<html>403</html>"}) {
		t.Fatal("isPermissionDenied 的原规则不应改变")
	}
}

// omniDeferredUpstream：alice 的第一次调用先占住同账号的另一个槽位（同账号还有请求在输出，失败的 Worker 一时关不了）再返回 WAA 运行态失效，
// 其余调用对带内联图片的请求返回 HTTP 403 / 协议错误码 7
type omniDeferredUpstream struct {
	mu     sync.Mutex
	holder *siblingHolder
	calls  []string
}

func (upstream *omniDeferredUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *omniDeferredUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *omniDeferredUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	first := len(upstream.calls) == 1
	upstream.mu.Unlock()
	if first && request.AccountID == resetTestAlice {
		upstream.holder.hold(request.AccountID)
		// 方法名取 GenerateContent：WAA 运行态失效按绑定模型的方法识别（DefinitiveWAARuntimeFailure），这里只为让 alice 的 Worker 要关闭
		return nil, waaRuntimeFailure("GenerateContent")
	}
	return nil, &aistudio.RPCError{
		Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission",
	}
}

func (upstream *omniDeferredUpstream) snapshot() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// TestOmniImageDeniedNoOtherSkipsDeferredReplay 第 1 个账户 Worker 失效、没等到关闭就换号，换到的第 2 个账户带图 403 时换号次数已用完：
// 日志写“没有其他可换的账户，不再换号”，请求就此按 400 结束（tried on 1 account），不再回到没等到关闭的原账号一直等它关闭后重放，
// 日志与实际去向一致
func TestOmniImageDeniedNoOtherSkipsDeferredReplay(t *testing.T) {
	upstream := &omniDeferredUpstream{}
	service, _ := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	upstream.holder = newSiblingHolder(service.pool)
	for _, accountID := range []string{resetTestAlice, resetTestBob} {
		if err := service.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pngData, _, _ := omniTestImages(t)
	request := omniImageRequest("req-image-deferred", pngData)
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events,
			newGenerationDiagnostics(request, "未发送"))
	}()
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	<-done
	sibling := upstream.holder.lease(t)
	defer func() {
		_ = sibling.Release()
		waitResetsSettled(t, service.workers)
	}()
	var rejected *aistudio.InteractionImageRejectedError
	if !errors.As(err, &rejected) || rejected.Tried != 1 {
		t.Fatalf("err=%v，期望第 2 个账户带图 403 后按 400 结束", err)
	}
	if calls := upstream.snapshot(); !slices.Equal(calls, []string{resetTestAlice, resetTestBob}) {
		t.Fatalf("上游调用 = %v，期望 alice 失败换到 bob 后结束，不回到 alice 重放", calls)
	}
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-deferred"))
	if want := []string{imageDeniedNote(1, 2, imageDeniedNoOther, true, 1)}; !slices.Equal(notes, want) {
		t.Fatalf("请求日志 = %q\n期望 %q", notes, want)
	}
	if workerResetLogged(service, "回到原账号等待 Worker 恢复") {
		t.Fatal("日志写了不再换号，不应回到原账号等待重放")
	}
}

// runOmniBoundRequest 以文件绑定的请求（resourceID 由 BindResource 绑在所属账户上，不固定账户）执行一次 generateWithRetry，返回请求的错误
func runOmniBoundRequest(t *testing.T, service *trackedService, request aistudio.GenerateRequest, resourceID string) error {
	t.Helper()
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, resourceID, events,
		newGenerationDiagnostics(request, "未发送"))
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	return err
}

// markOmniCooling 让账户的 Omni 模型进入 2 小时冷却（每日限额），选号时它不是候选
func markOmniCooling(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	if err := pool.MarkCooldownIfGeneration(accountID, omniDriveModel, pool.ModelAccessGeneration(accountID),
		time.Now(), time.Now().Add(2*time.Hour), "每日限额"); err != nil {
		t.Fatal(err)
	}
}

// TestOmniImageDeniedFileBoundCopyThenCooling 文件绑定的带图请求：所属账户带图 403、写“换一个账户再试”，换号的选号（带着资源、排除了所属账户）
// 没有候选，照常改为把文件复制到其他账户再试；复制文件后的选号其余候选全部冷却时以那次 403 结束，按带图被拒的 400 返回并补写一行（原因为
// 全部冷却），不按全部冷却的 429 结束。原先复制文件那一步把 err 换成了选号错误，之后的选号失败就按选号错误返回、也不补写结束行
func TestOmniImageDeniedFileBoundCopyThenCooling(t *testing.T) {
	const resourceID = "files/omni-image-bound"
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	if err := pool.BindResource(resourceID, "a-omni@example.com"); err != nil {
		t.Fatal(err)
	}
	markOmniCooling(t, pool, "b-omni@example.com")
	err := runOmniBoundRequest(t, service, omniImageRequest("req-image-denied-bound", pngData), resourceID)
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-bound", err, "")
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-denied-bound"))
	if !strings.Contains(notes[1], "原因=模型 "+omniDriveModel+" 的可用账户均在冷却") {
		t.Fatalf("结束行的原因应为复制文件后选号的全部冷却: %q", notes[1])
	}
	if calls := upstream.snapshot(); len(calls) != 1 || calls[0].account != "a-omni@example.com" {
		t.Fatalf("Generate = %+v，期望只在文件所属账户上发送一次", calls)
	}
	if _, _, ok := modelCooldownUntil(t, pool, "a-omni@example.com", omniDriveModel); ok {
		t.Fatal("被拒的账户不应冷却")
	}
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 {
		t.Fatal("不应登记或计入多模型 403 暂停")
	}
}

// omniImageVideoRequest 为带客户端视频（要 Drive）、一张 PNG 和文本的 Omni 请求
func omniImageVideoRequest(id string, pngData []byte) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{ID: id, Model: omniDriveModel, Contents: []aistudio.Content{{
		Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypmp42video")}},
			{InlineData: &aistudio.Blob{MIME: "image/png", Data: pngData}},
			{Text: "让视频里的人举起这张图"},
		},
	}}}
}

// TestOmniImageDeniedDriveMissingThenExhausted 带视频和 PNG 的 Omni 请求：A 记着 Drive 可用，B 状态未知（其实没有 Drive 授权），C 记着不可用。
// A 上传视频后发送、带图 403，写“换一个账户再试”；换到 B 上传后视频仍是 inline data（没有 Drive 授权，没有向上游发送）；再换号时 Drive 选号用尽。
// 中间这次没有到达上游的尝试不算换到了其他账户：以 A 的那次 403 结束，按带图被拒的 400 返回并补写一行，不按号池繁忙 429 结束（A 不冷却、
// 又是唯一记着 Drive 可用的账户，按 429 结束时中转重发多半又落到 A 上再 403）
func TestOmniImageDeniedDriveMissingThenExhausted(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-drive@example.com", "b-unknown@example.com", "c-nodrive@example.com"}
	base := newOmniDriveUpstream(accounts, "a-drive@example.com")
	base.remember("a-drive@example.com", aistudio.DriveStateAvailable)
	base.remember("c-nodrive@example.com", aistudio.DriveStateUnavailable)
	service, pool, registry := omniDeniedService(t, &omniDeniedDriveUpstream{omniDriveUpstream: base}, accounts...)
	_, err, _ := runOmniDriveRequest(t, service, omniImageVideoRequest("req-image-denied-nodrive", pngData))
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-nodrive", err, "可能有 Drive 授权的账号本请求都试过或现在用不了")
	calls, uploads, _ := base.snapshot()
	if !slices.Equal(calls, []string{"a-drive@example.com/denied"}) || !slices.Equal(uploads, []string{"a-drive@example.com", "b-unknown@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，期望 A 上传并发送一次、B 上传后没有 Drive 授权不发送", calls, uploads)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 || egressOtherCount(service.egress) != 0 {
		t.Fatal("不应登记、计入多模型 403 暂停或出口判定")
	}
}

// TestOmniImageDeniedDriveMissingSwitchCeiling 带视频和 PNG 的 Omni 请求：A 记着 Drive 可用、带图 403 后写“换一个账户再试”，其余都是状态未知、
// 其实没有 Drive 授权的账户，换到的 7 个都没有向上游发送，到每请求换号硬上限（8 个账户）：以 A 的那次 403 结束，按带图被拒的 400 返回并补写一行
// （原因写明到换号上限），不按“记着 Drive 可用的账号本请求试过”的号池繁忙 429 结束
func TestOmniImageDeniedDriveMissingSwitchCeiling(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-drive@example.com"}
	for index := range generateSwitchCeiling {
		accounts = append(accounts, fmt.Sprintf("u%d-unknown@example.com", index))
	}
	base := newOmniDriveUpstream(accounts, "a-drive@example.com")
	base.remember("a-drive@example.com", aistudio.DriveStateAvailable)
	service, pool, registry := omniDeniedService(t, &omniDeniedDriveUpstream{omniDriveUpstream: base}, accounts...)
	_, err, _ := runOmniDriveRequest(t, service, omniImageVideoRequest("req-image-denied-ceiling", pngData))
	reason := fmt.Sprintf("已尝试 %d 个账号，到每请求换号上限：", generateSwitchCeiling)
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-ceiling", err, "")
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-denied-ceiling"))
	if !strings.Contains(notes[1], "原因="+reason) {
		t.Fatalf("结束行的原因应写明到每请求换号上限: %q", notes[1])
	}
	calls, uploads, _ := base.snapshot()
	if !slices.Equal(calls, []string{"a-drive@example.com/denied"}) || len(uploads) != generateSwitchCeiling || uploads[0] != "a-drive@example.com" {
		t.Fatalf("Generate=%v 上传=%v，期望 A 发送一次、其余换到的账户只上传不发送", calls, uploads)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 {
		t.Fatal("不应登记或计入多模型 403 暂停")
	}
}

// TestOmniImageDeniedCoolingDownBeforeSend 号池里有 2 个账户：第一个带图 403、写“换一个账户再试”，换到的第二个在发出前已进入冷却（请求没有发到
// 上游），换号次数随之用完：以第一次的 403 结束，按带图被拒的 400 返回并补写一行，不按发出前已冷却的 429 结束
func TestOmniImageDeniedCoolingDownBeforeSend(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true, imageDenials: 1, then: &aistudio.AccountCoolingDownError{
		ModelID: omniDriveModel, Until: time.Now().Add(time.Hour), Reason: "每日限额: You exceeded your current quota",
	}}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied-cooldown", pngData))
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-cooldown", err, "")
	if errors.Is(err, aistudio.ErrAccountCoolingDown) {
		t.Fatalf("err=%v，不应按发出前已冷却结束", err)
	}
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-denied-cooldown"))
	if !strings.Contains(notes[1], "原因=换号次数已用完（号池里没有其他可试的账户）：账户已进入冷却") {
		t.Fatalf("结束行的原因应写明换号次数已用完与第二个账户发出前已冷却: %q", notes[1])
	}
	if calls := upstream.snapshot(); len(calls) != 2 || calls[0].account == calls[1].account {
		t.Fatalf("Generate = %+v，期望两个账户各一次", calls)
	}
	assertNoAccountCooldown(t, pool)
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
}

// TestOmniImageDeniedThenUpstreamFailure 带图 403 写“换一个账户再试”后，换到的账户真正把请求发到了上游、以别的错误失败（5xx、额度 429）：
// 那次 403 不再保留，之后换号的选号失败（其余候选都在冷却）照原规则按选号错误或额度不足返回，最终错误不包装成带图被拒的 400，也不补写结束行
func TestOmniImageDeniedThenUpstreamFailure(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	cases := []struct {
		name  string
		then  error
		check func(error) bool
	}{
		{"5xx", &aistudio.RPCError{
			Method: "CreateInteractionStream", StatusCode: http.StatusServiceUnavailable, Code: 14, Message: "The service is currently unavailable.",
		}, func(err error) bool {
			var cooling *aistudio.AllCoolingError
			return errors.As(err, &cooling)
		}},
		{"额度 429", dailyQuotaFailure(), func(err error) bool {
			var quota *aistudio.PrimaryQuotaError
			return errors.As(err, &quota)
		}},
	}
	for index, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			upstream := &omniDeniedUpstream{denyImages: true, imageDenials: 1, then: item.then}
			service, pool, _ := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com", "c-omni@example.com")
			markOmniCooling(t, pool, "c-omni@example.com")
			requestID := fmt.Sprintf("req-image-denied-then-%d", index)
			_, err, _ := runOmniDriveRequest(t, service, omniImageRequest(requestID, pngData))
			var rejected *aistudio.InteractionImageRejectedError
			if err == nil || errors.As(err, &rejected) || !item.check(err) {
				t.Fatalf("err=%v，期望照原规则按选号错误或额度不足结束，不包装成带图被拒", err)
			}
			if calls := upstream.snapshot(); len(calls) != 2 || calls[0].account == calls[1].account || calls[1].account == "c-omni@example.com" {
				t.Fatalf("Generate = %+v，期望在 A、B 上各发送一次", calls)
			}
			notes := imageDeniedNotes(requestLogMessages(service.requests, requestID))
			if want := []string{imageDeniedNote(1, 2, imageDeniedSwitch, true, 1)}; !slices.Equal(notes, want) {
				t.Fatalf("请求日志 = %q\n期望只有第一次的带图 403，不补写结束行", notes)
			}
		})
	}
}

// TestAttemptReachedUpstream 一次失败的尝试是否真正发到了上游按证据判断：没有调用 Generate 不算；报告过向上游发送（或拿到了上游事件流）、
// 或错误本身是上游的 RPC 错误才算；准备 WAA proof 失败等发出前的本地错误不算；发出前已冷却、Worker 已更新、Worker 初始化失败即使带着证据也不算
func TestAttemptReachedUpstream(t *testing.T) {
	upstream := &aistudio.RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusServiceUnavailable, Code: 14}
	prepare := fmt.Errorf("准备 fresh WAA proof: %w", errors.New("stub worker 不生成 proof"))
	cases := []struct {
		name      string
		generated bool
		sent      bool
		err       error
		want      bool
	}{
		{"没有调用 Generate", false, true, upstream, false},
		{"报告过发送", true, true, errors.New("连接被重置"), true},
		{"上游的 RPC 错误", true, false, upstream, true},
		{"包装后的上游 RPC 错误", true, false, fmt.Errorf("发送 AI Studio CreateInteractionStream: %w", upstream), true},
		{"准备 proof 失败", true, false, prepare, false},
		{"准备 proof 超时", true, false, fmt.Errorf("准备 fresh WAA proof: %w", context.DeadlineExceeded), false},
		{"发出前已冷却", true, true, &aistudio.AccountCoolingDownError{ModelID: omniDriveModel, Until: time.Now().Add(time.Hour)}, false},
		{"Worker 已更新", true, true, errAccountWorkerReplaced, false},
		{"Worker 初始化失败", true, true, &accountWorkerInitError{err: upstream}, false},
	}
	for _, item := range cases {
		if got := attemptReachedUpstream(item.generated, item.sent, item.err); got != item.want {
			t.Errorf("%s：attemptReachedUpstream = %t，期望 %t", item.name, got, item.want)
		}
	}
}

// TestOmniImageDeniedCoolingDownThenSelectionFails 号池里有 3 个账户：第一个带图 403、写“换一个账户再试”，换到的第二个在发出前已进入冷却
// （没有发到上游，err 已是这次的 AccountCoolingDownError），再换号的选号失败：第三个在冷却（通用的选号错误分支）或被占满、主用模型排队超限
// （号池繁忙分支）。两条分支都把 err 换回那次 403：以它结束，按带图被拒的 400 返回并补写一行（原因为这次选号失败），不按账户已进入冷却的 429 结束
func TestOmniImageDeniedCoolingDownThenSelectionFails(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	const third = "c-omni@example.com"
	cases := []struct {
		name   string
		setup  func(t *testing.T, service *trackedService, pool *aistudio.AccountPool) (release func())
		reason string
	}{
		{"其余候选全部冷却", func(t *testing.T, _ *trackedService, pool *aistudio.AccountPool) func() {
			markOmniCooling(t, pool, third)
			return func() {}
		}, "模型 " + omniDriveModel + " 的可用账户均在冷却"},
		{"主用模型排队超限", func(t *testing.T, service *trackedService, pool *aistudio.AccountPool) func() {
			pool.SetPerAccountConcurrency(1)
			service.setPrimaryModelSettings([]string{omniDriveModel}, 300*time.Millisecond, nil, 0)
			return holdLease(t, pool, third)
		}, "号池繁忙"},
	}
	for index, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			upstream := &omniDeniedUpstream{denyImages: true, imageDenials: 1, then: &aistudio.AccountCoolingDownError{
				ModelID: omniDriveModel, Until: time.Now().Add(time.Hour), Reason: "每日限额: You exceeded your current quota",
			}}
			service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com", third)
			release := item.setup(t, service, pool)
			requestID := fmt.Sprintf("req-image-denied-cooling-select-%d", index)
			_, err, _ := runOmniDriveRequest(t, service, omniImageRequest(requestID, pngData))
			release()
			assertImageDeniedEndedAfterSwitch(t, service, requestID, err, "")
			if errors.Is(err, aistudio.ErrAccountCoolingDown) {
				t.Fatalf("err=%v，不应按换到的账户发出前已冷却结束", err)
			}
			notes := imageDeniedNotes(requestLogMessages(service.requests, requestID))
			if !strings.Contains(notes[1], "原因="+item.reason) {
				t.Fatalf("结束行的原因应为再换号的选号失败（%s）: %q", item.reason, notes[1])
			}
			calls := upstream.snapshot()
			if len(calls) != 2 || calls[0].account == calls[1].account || calls[0].account == third || calls[1].account == third {
				t.Fatalf("Generate = %+v，期望在前两个账户上各调用一次", calls)
			}
			if len(registry.list()) != 0 {
				t.Fatal("不应登记为 403 无权限账户")
			}
		})
	}
}

// replaceWorkerGeneration 模拟账户的 Worker 刚被替换（线上 errAccountWorkerReplaced 出现时 Worker 版本已经变了）
func replaceWorkerGeneration(manager *accountWorkerManager, accountID string) {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	account.generation.Add(1)
}

// TestOmniImageDeniedWorkerUnavailableThenExhausted 号池里有 2 个账户：第一个带图 403、写“换一个账户再试”，换到的第二个返回 Worker 已更新
// （errAccountWorkerReplaced，Worker 版本已变，在同账号重放后仍是它）或 Worker 初始化失败（accountWorkerInitError），都没有向上游发出，
// 换号次数随之用完：以第一次的 403 结束，按带图被拒的 400 返回并补写一行（原因写明换号次数已用完与那次尝试的错误），不按 Worker 的错误结束
func TestOmniImageDeniedWorkerUnavailableThenExhausted(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	initErr := &accountWorkerInitError{err: errors.New("启动浏览器超时")}
	cases := []struct {
		name    string
		failure func(service *trackedService, accountID string) error
		cause   error
		reason  string
	}{
		{"Worker 已更新", func(service *trackedService, accountID string) error {
			replaceWorkerGeneration(service.workers, accountID)
			return errAccountWorkerReplaced
		}, errAccountWorkerReplaced, "WAA worker 已更新"},
		{"Worker 初始化失败", func(*trackedService, string) error { return initErr }, initErr, "启动浏览器超时"},
	}
	for index, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			upstream := &omniDeniedUpstream{denyImages: true}
			service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com")
			upstream.hook = func(call int, request aistudio.GenerateRequest) error {
				if call == 1 {
					return nil
				}
				return item.failure(service, request.AccountID)
			}
			requestID := fmt.Sprintf("req-image-denied-worker-%d", index)
			_, err, _ := runOmniDriveRequest(t, service, omniImageRequest(requestID, pngData))
			assertImageDeniedEndedAfterSwitch(t, service, requestID, err, "")
			if errors.Is(err, item.cause) {
				t.Fatalf("err=%v，不应按换到的账户的 Worker 错误结束", err)
			}
			notes := imageDeniedNotes(requestLogMessages(service.requests, requestID))
			if !strings.Contains(notes[1], "原因=换号次数已用完（号池里没有其他可试的账户）：") || !strings.Contains(notes[1], item.reason) {
				t.Fatalf("结束行的原因应写明换号次数已用完与 %q: %q", item.reason, notes[1])
			}
			calls := upstream.snapshot()
			if len(calls) < 2 {
				t.Fatalf("Generate = %+v，期望两个账户各至少一次", calls)
			}
			for _, call := range calls[1:] {
				if call.account == calls[0].account {
					t.Fatalf("Generate = %+v，带图 403 的账户不应再试", calls)
				}
			}
			assertNoAccountCooldown(t, pool)
			if len(registry.list()) != 0 {
				t.Fatal("不应登记为 403 无权限账户")
			}
		})
	}
}

// TestOmniImageDeniedUltraCoolingDownLong Ultra 主用模型的带图请求（号池里 2 个 Ultra 账户）：第一个带图 403、写“换一个账户再试”，换到的第二个
// 在发出前已处在长时间冷却（Ultra 主用模型把它记为一次每日类额度失败，没有发到上游），换号次数随之用完：以第一次的 403 结束，按带图被拒的 400
// 返回并补写一行，不按额度不足（PrimaryQuotaError，429）结束
func TestOmniImageDeniedUltraCoolingDownLong(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	accounts := []string{"a-ultra@example.com", "b-ultra@example.com"}
	upstream := &omniDeniedUpstream{denyImages: true, imageDenials: 1, then: &aistudio.AccountCoolingDownError{
		ModelID: omniDriveModel, Until: time.Now().Add(2 * time.Hour), Reason: "每日限额: You exceeded your current quota",
	}}
	service, pool, registry := omniDeniedService(t, upstream, accounts...)
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setUltraPrimaryModels(service, []string{omniDriveModel}, 10*time.Second)
	request := omniImageRequest("req-image-denied-ultra", pngData)
	events, err := service.Generate(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), request)
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	assertImageDeniedEndedAfterSwitch(t, service, "req-image-denied-ultra", err, "")
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-denied-ultra"))
	if !strings.Contains(notes[1], "原因=换号次数已用完（号池里没有其他可试的账户）：账户已进入冷却") {
		t.Fatalf("结束行的原因应写明换号次数已用完与第二个账户发出前已冷却: %q", notes[1])
	}
	if calls := upstream.snapshot(); len(calls) != 2 || calls[0].account == calls[1].account {
		t.Fatalf("Generate = %+v，期望两个账户各一次", calls)
	}
	if len(registry.list()) != 0 {
		t.Fatal("不应登记为 403 无权限账户")
	}
}

// failStubWorker 让账户当前的 stub Worker 准备 proof 失败（stub 不生成 proof），Worker 随之进入失败状态（与线上 NativeWorker.Prepare 相同），
// 返回 WorkerProtectedTransport 写法的错误
func failStubWorker(service *trackedService, accountID string) error {
	worker := currentWorker(service.workers, accountID)
	if worker == nil {
		return fmt.Errorf("账户 %s 没有 Worker", accountID)
	}
	if _, err := worker.Prepare(context.Background(), aistudio.ProtectedRequest{}); err != nil {
		return fmt.Errorf("准备 fresh WAA proof: %w", err)
	}
	return errors.New("stub Worker 应准备失败")
}

// coolOmniAccount 让账户的 Omni 模型进入 2 小时冷却（在上游替身里调用，不能用 t.Fatal）
func coolOmniAccount(pool *aistudio.AccountPool, accountID string) error {
	return pool.MarkCooldownIfGeneration(accountID, omniDriveModel, pool.ModelAccessGeneration(accountID),
		time.Now(), time.Now().Add(2*time.Hour), "每日限额")
}

// TestOmniImageDeniedPrepareFailedBeforeSend 带图 403 写“换一个账户再试”后，换到的账户在发出前就失败（准备 WAA proof 失败或超时，没有向上游
// 发送），同时它被冷却。这次尝试不算换到了其他账户：之后 Worker 重建后的重放选号失败、其余候选全部冷却，或重放选号失败后循环正常结束，
// 都以第一次的 403 结束，按带图被拒的 400 返回并补写一行（行尾带原因），不按全部冷却等选号错误（429）结束。
// 原先只要调用了 Generate、错误不是发出前已冷却等三种就算到达了上游，清掉了保留的 403
func TestOmniImageDeniedPrepareFailedBeforeSend(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	cases := []struct {
		name     string
		accounts []string
		cooling  string
		failure  func(service *trackedService, accountID string) error
		replay   bool
		reason   string
	}{
		{"Worker 失败后重放选号失败、其余候选全部冷却", []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"},
			"c-omni@example.com", failStubWorker, true, "模型 " + omniDriveModel + " 的可用账户均在冷却"},
		{"准备 proof 超时、其余候选全部冷却", []string{"a-omni@example.com", "b-omni@example.com", "c-omni@example.com"},
			"c-omni@example.com", func(*trackedService, string) error {
				return fmt.Errorf("准备 fresh WAA proof: %w", context.DeadlineExceeded)
			}, false, "模型 " + omniDriveModel + " 的可用账户均在冷却"},
		{"Worker 失败后重放选号失败、循环正常结束", []string{"a-omni@example.com", "b-omni@example.com"},
			"", failStubWorker, true, ""},
	}
	for index, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			upstream := &omniDeniedUpstream{denyImages: true}
			service, pool, registry := omniDeniedService(t, upstream, item.accounts...)
			if item.cooling != "" {
				markOmniCooling(t, pool, item.cooling)
			}
			upstream.hook = func(call int, request aistudio.GenerateRequest) error {
				if call != 2 {
					return nil
				}
				// 换到的账户：先被冷却（同账号别的请求刚遇到额度 429 等），再在发出前失败
				if err := coolOmniAccount(pool, request.AccountID); err != nil {
					return err
				}
				return item.failure(service, request.AccountID)
			}
			requestID := fmt.Sprintf("req-image-denied-prepare-%d", index)
			_, err, _ := runOmniDriveRequest(t, service, omniImageRequest(requestID, pngData))
			waitResetsSettled(t, service.workers)
			assertImageDeniedEndedAfterSwitch(t, service, requestID, err, "")
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v，不应按换到的账户准备 proof 的错误结束", err)
			}
			notes := imageDeniedNotes(requestLogMessages(service.requests, requestID))
			if item.reason != "" && !strings.Contains(notes[1], "原因="+item.reason) {
				t.Fatalf("结束行的原因应为 %q: %q", item.reason, notes[1])
			}
			if replayed := workerResetLogged(service, "WAA Worker 重建，重放当前请求"); replayed != item.replay {
				t.Fatalf("Worker 重建后重放 = %t，期望 %t", replayed, item.replay)
			}
			calls := upstream.snapshot()
			if len(calls) != 2 || calls[0].account == calls[1].account || calls[0].account == item.cooling || calls[1].account == item.cooling {
				t.Fatalf("Generate = %+v，期望在没有冷却的两个账户上各调用一次（重放选号失败，不再调用）", calls)
			}
			if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 {
				t.Fatal("不应登记或计入多模型 403 暂停")
			}
		})
	}
}

// TestOmniImageDeniedWorkerFailedSwitches 带图 403 的那次尝试里该账户的 Worker 正好被判为失败（同账号其他请求准备 proof 失败等）：日志写了
// “换一个账户再试”，就真的换到另一个账户，不在原账户重建 Worker 后重放（原先在原账户重放，第二次 403 被算成“第 2/2 个账户”，对外 tried on 2
// accounts，其实只试了 1 个），也不回到它等 Worker 关闭后重放；它的失败 Worker 照常在后台关闭
func TestOmniImageDeniedWorkerFailedSwitches(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	upstream := &omniDeniedUpstream{denyImages: true}
	service, pool, registry := omniDeniedService(t, upstream, "a-omni@example.com", "b-omni@example.com", "c-omni@example.com")
	var prepareErr error
	upstream.hook = func(call int, request aistudio.GenerateRequest) error {
		if call == 1 {
			prepareErr = failStubWorker(service, request.AccountID)
		}
		return nil
	}
	_, err, _ := runOmniDriveRequest(t, service, omniImageRequest("req-image-denied-worker-failed", pngData))
	waitResetsSettled(t, service.workers)
	if prepareErr == nil || !strings.Contains(prepareErr.Error(), "stub worker 不生成 proof") {
		t.Fatalf("第一个账户的 Worker 应准备失败: %v", prepareErr)
	}
	var rejected *aistudio.InteractionImageRejectedError
	if !errors.As(err, &rejected) || rejected.Tried != 2 {
		t.Fatalf("err=%v，期望在 2 个不同账户上带图 403 后按 400 结束", err)
	}
	calls := upstream.snapshot()
	if len(calls) != 2 || calls[0].account == calls[1].account {
		t.Fatalf("Generate = %+v，期望第二次在另一个账户上，不在原账户重放", calls)
	}
	notes := imageDeniedNotes(requestLogMessages(service.requests, "req-image-denied-worker-failed"))
	want := []string{imageDeniedNote(1, 2, imageDeniedSwitch, true, 1), imageDeniedNote(2, 2, imageDeniedLimitReached, true, 1)}
	if !slices.Equal(notes, want) {
		t.Fatalf("请求日志 = %q\n期望 %q", notes, want)
	}
	if workerResetLogged(service, "WAA Worker 重建，重放当前请求") || workerResetLogged(service, "回到原账号等待 Worker 恢复") ||
		workerResetLogged(service, "换号重试") {
		t.Fatal("带图 403 的账户不应在原账户重建后重放、回到原账户等待或按没等到关闭写换号重试")
	}
	if _, _, ok := modelCooldownUntil(t, pool, calls[0].account, omniDriveModel); ok {
		t.Fatal("被拒的账户不应冷却")
	}
	if len(registry.list()) != 0 || forbiddenEventCount(service.forbidden) != 0 {
		t.Fatal("不应登记或计入多模型 403 暂停")
	}
}
