package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// omniDriveModel 为测试用的 Interaction 模型（目录带 interactions_api，与 gemini-omni 相同）
const omniDriveModel = "gemini-omni-1.1-flash"

// omniDriveUpstream 模拟带 Drive 记忆的生成服务（aistudio.PooledService）：drive 为真的账号上传内联附件后改写为 Drive 引用并记为可用，
// 其余账号保留 inline data 并记为不可用（GenerateAccessToken 401）；DriveCandidates 按记忆报告候选状态（没试过的为未知）。
// Generate 对 Interaction 模型与 generateInteraction 一样按真实编码发送（PNG、JPEG 图片内联，仍是 inline data 的视频等返回参数错误），
// 记录每次调用的账号、是否带 inline data 与编码出的请求体
type omniDriveUpstream struct {
	mu         sync.Mutex
	accounts   []string
	drive      map[string]bool
	states     map[string]aistudio.DriveState
	calls      []string
	uploads    []string
	candidates int
	// uploadMIMEs 为每次上传收到的内联附件 MIME（按出现顺序），uploadData 为它们的数据
	uploadMIMEs [][]string
	uploadData  [][][]byte
	// contents 为每次 Generate 收到的内容，bodies 为 Interaction 模型编码出的 CreateInteractionStream 请求体
	contents [][]aistudio.Content
	bodies   [][]byte
}

// omniDriveDefaults 为 Omni 模型目录解析出的生成默认值（思考 High、输出上限 65536、默认分辨率 360p），与官网抓包的设置相同
var omniDriveDefaults = aistudio.GenerationDefaults{
	MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
	ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
}

func newOmniDriveUpstream(accounts []string, drive ...string) *omniDriveUpstream {
	upstream := &omniDriveUpstream{accounts: accounts, drive: make(map[string]bool), states: make(map[string]aistudio.DriveState)}
	for _, accountID := range drive {
		upstream.drive[accountID] = true
	}
	return upstream
}

func (upstream *omniDriveUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *omniDriveUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *omniDriveUpstream) remember(accountID string, state aistudio.DriveState) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.states[accountID] = state
}

func (upstream *omniDriveUpstream) UploadInlineMediaToLease(
	_ context.Context, lease *aistudio.AccountLease, contents []aistudio.Content, temporary *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	accountID := lease.Account().ID
	var mimes []string
	var data [][]byte
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.InlineData != nil {
				mimes = append(mimes, part.InlineData.MIME)
				data = append(data, part.InlineData.Data)
			}
		}
	}
	upstream.mu.Lock()
	ok := upstream.drive[accountID]
	upstream.uploads = append(upstream.uploads, accountID)
	upstream.uploadMIMEs = append(upstream.uploadMIMEs, mimes)
	upstream.uploadData = append(upstream.uploadData, data)
	if ok {
		upstream.states[accountID] = aistudio.DriveStateAvailable
	} else {
		upstream.states[accountID] = aistudio.DriveStateUnavailable
	}
	upstream.mu.Unlock()
	if temporary == nil {
		temporary = aistudio.NewTemporaryFileCopiesForTest(nil)
	}
	if !ok {
		return contents, temporary, nil
	}
	rewritten := make([]aistudio.Content, len(contents))
	for index, content := range contents {
		rewritten[index] = content
		rewritten[index].Parts = append([]aistudio.Part(nil), content.Parts...)
		for partIndex := range rewritten[index].Parts {
			part := &rewritten[index].Parts[partIndex]
			if part.InlineData != nil {
				part.File = &aistudio.FileRef{ID: "drive-" + accountID, MIME: part.InlineData.MIME}
				part.InlineData = nil
			}
		}
	}
	return rewritten, temporary, nil
}

func (upstream *omniDriveUpstream) DriveCandidates(_ context.Context, selection aistudio.AccountSelection) aistudio.DriveCandidates {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.candidates++
	var result aistudio.DriveCandidates
	for _, accountID := range upstream.accounts {
		switch upstream.states[accountID] {
		case aistudio.DriveStateAvailable:
			result.Available = append(result.Available, accountID)
		case aistudio.DriveStateUnavailable:
			result.Unavailable = append(result.Unavailable, accountID)
		default:
			result.Unknown = append(result.Unknown, accountID)
		}
	}
	return result
}

func (upstream *omniDriveUpstream) DriveSummary() aistudio.DriveCounts {
	candidates := upstream.DriveCandidates(context.Background(), aistudio.AccountSelection{})
	return aistudio.DriveCounts{Available: len(candidates.Available), Unavailable: len(candidates.Unavailable), Unknown: len(candidates.Unknown)}
}

func (upstream *omniDriveUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	inline := false
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			inline = inline || part.InlineData != nil
		}
	}
	var body []byte
	var encodeErr error
	if request.Model == omniDriveModel {
		body, _, encodeErr = aistudio.EncodeCreateInteractionStreamRequest(request, omniDriveDefaults)
	}
	upstream.mu.Lock()
	call := request.AccountID
	if inline {
		call += "/inline"
	}
	upstream.calls = append(upstream.calls, call)
	upstream.contents = append(upstream.contents, request.Contents)
	upstream.bodies = append(upstream.bodies, body)
	upstream.mu.Unlock()
	if encodeErr != nil {
		return nil, fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, encodeErr)
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (upstream *omniDriveUpstream) snapshot() ([]string, []string, int) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls), slices.Clone(upstream.uploads), upstream.candidates
}

// omniDriveService 返回 Worker 都已就绪、目录里有 Interaction 模型与普通文本模型的运行中生成服务（冷启动得到 stub Worker）
func omniDriveService(t *testing.T, upstream *omniDriveUpstream) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, upstream.accounts...)
	// 冷启动（Worker 未就绪的账号）换成 stub Worker，不连官网
	service.workers.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	for _, accountID := range upstream.accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
			{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return service, pool
}

// newAPIVideoRequest 为 new-api 经 Gemini 渠道转来的带视频请求：OpenAI 的 file、video_url 已被解码成 Gemini inlineData（视频在前、
// 文本在后，与官网发送视频时相同）。视频要先上传到账号的 Drive
func newAPIVideoRequest(id string, model string) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{
		ID: id, Model: model,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypmp42video")}}, {Text: "分析视频"},
		}}},
	}
}

// newAPIImageRequest 为 new-api 经 Gemini 渠道转来的带图请求：OpenAI 的 image_url 已被下载成 Gemini inlineData
func newAPIImageRequest(id string, model string) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{
		ID: id, Model: model,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "让图片动起来"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\nimage")}},
		}}},
	}
}

// runOmniDriveRequest 经 startGenerate 执行一次请求，返回正文、错误与耗时
func runOmniDriveRequest(t *testing.T, service *trackedService, request aistudio.GenerateRequest) (string, error, time.Duration) {
	t.Helper()
	startedAt := time.Now()
	events, err := service.Generate(service.lifecycle, request)
	if err != nil {
		return "", err, time.Since(startedAt)
	}
	var text string
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	return text, err, time.Since(startedAt)
}

// assertNoAccountCooldown 断言账号都没有冷却、租约都已释放
func assertNoAccountCooldown(t *testing.T, pool *aistudio.AccountPool) {
	t.Helper()
	assertLeasesReleased(t, pool)
	for _, status := range pool.Status() {
		if len(status.Cooldowns) > 0 {
			t.Fatalf("账号 %s 不应冷却: %+v", status.ID, status.Cooldowns)
		}
	}
}

// TestOmniDrivePrefersKnownDriveAccount new-api 风格的 Gemini inlineData 视频请求：部分账号记着 Drive 不可用、一个记着可用时，
// 直接选到可用的账号并成功；记着不可用的账号不取租约、不上传，也不调用 Generate
func TestOmniDrivePrefersKnownDriveAccount(t *testing.T) {
	accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com", "c-unknown@example.com", "z-drive@example.com"}
	upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("b-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
	service, pool := omniDriveService(t, upstream)
	for index := range 3 {
		text, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest(fmt.Sprintf("req-prefer-%d", index), omniDriveModel))
		if err != nil || text != "ok" {
			t.Fatalf("第 %d 次：text=%q err=%v", index, text, err)
		}
	}
	calls, uploads, candidates := upstream.snapshot()
	if !slices.Equal(calls, []string{"z-drive@example.com", "z-drive@example.com", "z-drive@example.com"}) {
		t.Fatalf("Generate 调用 = %v，期望都在记着 Drive 可用的账号上", calls)
	}
	if !slices.Equal(uploads, []string{"z-drive@example.com", "z-drive@example.com", "z-drive@example.com"}) || candidates != 3 {
		t.Fatalf("上传 = %v，候选查询 %d 次", uploads, candidates)
	}
	assertNoAccountCooldown(t, pool)

	// 没有记着可用的账号时：记着不可用的账号排除，选到状态未知的账号
	upstream = newOmniDriveUpstream(accounts, "z-drive@example.com")
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("b-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("c-unknown@example.com", aistudio.DriveStateUnavailable)
	service, pool = omniDriveService(t, upstream)
	text, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest("req-exclude", omniDriveModel))
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, []string{"z-drive@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，记着 Drive 不可用的账号应排除", calls, uploads)
	}
	assertNoAccountCooldown(t, pool)
}

// TestOmniDriveMissingSwitchesAccount 状态都未知时选到没有 Drive 授权的账号：附件仍是 inline data，不调用 Generate，记一行请求进度后
// 换号，直到有 Drive 授权的账号成功。没有 Drive 的账号比连续失败上限还多，换号不冷却、不计入连续失败上限、不退避，运行日志不写账号切换
func TestOmniDriveMissingSwitchesAccount(t *testing.T) {
	accounts := []string{
		"a-nodrive@example.com", "b-nodrive@example.com", "c-nodrive@example.com", "d-nodrive@example.com",
		"e-nodrive@example.com", "f-nodrive@example.com", "z-drive@example.com",
	}
	upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
	service, pool := omniDriveService(t, upstream)
	text, err, elapsed := runOmniDriveRequest(t, service, newAPIVideoRequest("req-switch", omniDriveModel))
	if err != nil || text != "ok" {
		t.Fatalf("应换到有 Drive 授权的账号成功：text=%q err=%v", text, err)
	}
	calls, uploads, _ := upstream.snapshot()
	if !slices.Equal(calls, []string{"z-drive@example.com"}) {
		t.Fatalf("Generate 调用 = %v，没有 Drive 授权的账号不应调用 Generate", calls)
	}
	if len(uploads) != len(accounts) || uploads[len(uploads)-1] != "z-drive@example.com" {
		t.Fatalf("上传 = %v，期望逐个换号到有 Drive 授权的账号", uploads)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("换号不应退避，用了 %s", elapsed)
	}
	if !progressLogged(service, "账号没有 Drive 授权，Omni 的视频（及无法转为 PNG 的图片）无法发送，换有 Drive 授权的账号") {
		t.Fatal("应在请求进度里记下换号原因")
	}
	for _, message := range serviceLogMessages(service.requests) {
		if strings.Contains(message, "账号切换") || strings.Contains(message, "已连续") || strings.Contains(message, "附件改为内联") {
			t.Fatalf("不应写账号切换、连续失败或内联发送的日志: %q", message)
		}
	}
	assertNoAccountCooldown(t, pool)
	// 之后的请求排除已记着不可用的账号，直接选到有 Drive 授权的账号
	text, err, _ = runOmniDriveRequest(t, service, newAPIVideoRequest("req-switch-again", omniDriveModel))
	if calls, uploads, _ = upstream.snapshot(); err != nil || text != "ok" || len(uploads) != len(accounts)+1 || calls[len(calls)-1] != "z-drive@example.com" {
		t.Fatalf("第二次请求：text=%q err=%v 上传=%v", text, err, uploads)
	}
}

// TestOmniDriveNoAccountReturnsEnglish400 号池里没有有 Drive 授权的账号：全部记着不可用时不选号、直接返回 400 FAILED_PRECONDITION 的英文说明；
// 状态未知但都没有 Drive 授权时逐个试过后同样返回 400；指定账号没有 Drive 授权时返回 400（不能换号）；都不调用 Generate、不冷却
func TestOmniDriveNoAccountReturnsEnglish400(t *testing.T) {
	accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com", "c-nodrive@example.com"}
	for _, test := range []struct {
		name    string
		known   bool
		account string
		reason  aistudio.DriveRequiredReason
	}{
		{name: "全部记着不可用", known: true, reason: aistudio.DriveRequiredNoAccount},
		{name: "状态未知、逐个试过", reason: aistudio.DriveRequiredNoAccount},
		{name: "指定账号", account: "b-nodrive@example.com", reason: aistudio.DriveRequiredAccount},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newOmniDriveUpstream(accounts)
			if test.known {
				for _, accountID := range accounts {
					upstream.remember(accountID, aistudio.DriveStateUnavailable)
				}
			}
			service, pool := omniDriveService(t, upstream)
			request := newAPIVideoRequest("req-none", omniDriveModel)
			request.AccountID = test.account
			_, err, _ := runOmniDriveRequest(t, service, request)
			var driveRequired *aistudio.DriveRequiredError
			if !errors.As(err, &driveRequired) || driveRequired.Reason != test.reason || driveRequired.HTTPStatus() != http.StatusBadRequest ||
				driveRequired.RPCStatus() != "FAILED_PRECONDITION" || !strings.Contains(driveRequired.PublicMessage(), "requires an account with Google Drive authorized") {
				t.Fatalf("err = %#v，期望 400 FAILED_PRECONDITION 的需要 Drive 授权错误", err)
			}
			calls, uploads, _ := upstream.snapshot()
			if len(calls) != 0 {
				t.Fatalf("不应调用 Generate: %v", calls)
			}
			if wantUploads := map[bool]int{true: 0, false: 3}[test.known]; test.account == "" && len(uploads) != wantUploads {
				t.Fatalf("上传 = %v，期望 %d 次", uploads, wantUploads)
			}
			assertNoAccountCooldown(t, pool)
		})
	}
}

// TestOmniDriveCeilingReturns503 状态未知的账号都没有 Drive 授权、比每请求换号硬上限还多时：试满上限后按 503 结束（号池里可能还有没试过的账号）
func TestOmniDriveCeilingReturns503(t *testing.T) {
	accounts := make([]string, 0, generateSwitchCeiling+2)
	for index := range generateSwitchCeiling + 2 {
		accounts = append(accounts, fmt.Sprintf("nodrive-%02d@example.com", index))
	}
	upstream := newOmniDriveUpstream(accounts)
	service, pool := omniDriveService(t, upstream)
	_, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest("req-ceiling", omniDriveModel))
	var driveRequired *aistudio.DriveRequiredError
	if !errors.As(err, &driveRequired) || driveRequired.Reason != aistudio.DriveRequiredExhausted || driveRequired.Tried != generateSwitchCeiling ||
		driveRequired.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("err = %#v，期望试满 %d 个账号后按 503 结束", err, generateSwitchCeiling)
	}
	if calls, uploads, _ := upstream.snapshot(); len(calls) != 0 || len(uploads) != generateSwitchCeiling {
		t.Fatalf("Generate=%v 上传=%v，期望只上传 %d 次、不调用 Generate", calls, uploads, generateSwitchCeiling)
	}
	if !progressLogged(service, fmt.Sprintf("已尝试 %d 个账号，不再换号", generateSwitchCeiling)) {
		t.Fatal("应写每请求换号硬上限的进度")
	}
	assertNoAccountCooldown(t, pool)
}

// TestOmniDriveOtherRequestsUnaffected 纯文本的 Omni 请求与其他模型的带图请求不按 Drive 状态选号（只有图片的 Omni 请求见 omni_image_test.go）：不查询候选 Drive 状态，
// 记着 Drive 不可用的账号照常被选到；其他模型在没有 Drive 授权的账号上照旧改为内联发送
func TestOmniDriveOtherRequestsUnaffected(t *testing.T) {
	accounts := []string{"a-nodrive@example.com"}
	upstream := newOmniDriveUpstream(accounts)
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	service, _ := omniDriveService(t, upstream)
	text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
		ID: "req-text", Model: omniDriveModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个视频"}}}},
	})
	if err != nil || text != "ok" {
		t.Fatalf("纯文本 Omni 请求：text=%q err=%v", text, err)
	}
	text, err, _ = runOmniDriveRequest(t, service, newAPIImageRequest("req-other", testRuntimeModel))
	if err != nil || text != "ok" {
		t.Fatalf("其他模型的带图请求：text=%q err=%v", text, err)
	}
	calls, uploads, candidates := upstream.snapshot()
	if !slices.Equal(calls, []string{"a-nodrive@example.com", "a-nodrive@example.com/inline"}) || candidates != 0 {
		t.Fatalf("Generate 调用 = %v，候选查询 %d 次，期望照常选号、其他模型内联发送、不查询 Drive 状态", calls, candidates)
	}
	if !slices.Equal(uploads, []string{"a-nodrive@example.com"}) {
		t.Fatalf("上传 = %v", uploads)
	}
}

// TestOmniExternalMediaRejectedBeforeSelection Omni 请求里的外部地址（http(s) 图片链接）在选号前按 400 返回英文说明，不取租约、不调用 Generate
func TestOmniExternalMediaRejectedBeforeSelection(t *testing.T) {
	upstream := newOmniDriveUpstream([]string{"z-drive@example.com"}, "z-drive@example.com")
	service, _ := omniDriveService(t, upstream)
	_, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
		ID: "req-external", Model: omniDriveModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "让图片动起来"}, {ExternalMedia: &aistudio.ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}},
		}}},
	})
	if !errors.Is(err, aistudio.ErrInvalidArgument) || !strings.Contains(err.Error(), "external URLs are not supported") {
		t.Fatalf("err = %v", err)
	}
	if calls, uploads, candidates := upstream.snapshot(); len(calls)+len(uploads)+candidates != 0 {
		t.Fatalf("不应选号：Generate=%v 上传=%v 候选查询=%d", calls, uploads, candidates)
	}
}

// TestAdminStatusDriveCounts /api/status 带启用账户的 Drive 授权状态计数（可用、不可用、未知），换号学到的状态随之更新
func TestAdminStatusDriveCounts(t *testing.T) {
	accounts := []string{"a-nodrive@example.com", "b-unknown@example.com", "z-drive@example.com"}
	upstream := newOmniDriveUpstream(accounts, "b-unknown@example.com", "z-drive@example.com")
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
	service, pool := omniDriveService(t, upstream)
	service.catalogSync = newCatalogSyncer(service)
	service.egress = newEgressGuard(pool, service.requests)
	service.authRecheck = newAuthRecheck(service)
	admin := &runtimeAdmin{pool: pool, service: service, requests: service.requests, workers: service.workers}
	status, err := admin.Status(context.Background())
	if err != nil || status.Drive == nil || *status.Drive != (api.AdminDriveCounts{Available: 1, Unavailable: 1, Unknown: 1}) {
		t.Fatalf("drive = %+v err = %v", status.Drive, err)
	}
	if encoded, _ := json.Marshal(status.Drive); string(encoded) != `{"available":1,"unavailable":1,"unknown":1}` {
		t.Fatalf("drive JSON = %s", encoded)
	}
	upstream.remember("b-unknown@example.com", aistudio.DriveStateAvailable)
	if status, _ = admin.Status(context.Background()); *status.Drive != (api.AdminDriveCounts{Available: 2, Unavailable: 1}) {
		t.Fatalf("更新后 drive = %+v", status.Drive)
	}
}

// TestOmniDriveBusyAvailableQueues 记着 Drive 可用的账号在忙、另有一批状态未知（实际没有 Drive）的账号时：最多试一个 Worker 已就绪的
// 状态未知账号，之后排队等可用账号空出、在它上面成功，不把未知账号试满每请求换号硬上限后按 503 结束；主用模型排队超过预算时按 429 返回
func TestOmniDriveBusyAvailableQueues(t *testing.T) {
	accounts := []string{"z-drive@example.com"}
	for index := range generateSwitchCeiling + 1 {
		accounts = append(accounts, fmt.Sprintf("unknown-%02d@example.com", index))
	}
	t.Run("等可用账号空出", func(t *testing.T) {
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, pool := omniDriveService(t, upstream)
		pool.SetPerAccountConcurrency(1)
		release := holdLease(t, pool, "z-drive@example.com")
		const hold = 600 * time.Millisecond
		timer := time.AfterFunc(hold, release)
		defer timer.Stop()
		text, err, elapsed := runOmniDriveRequest(t, service, newAPIVideoRequest("req-busy", omniDriveModel))
		if err != nil || text != "ok" {
			t.Fatalf("应等记着 Drive 可用的账号空出后成功：text=%q err=%v", text, err)
		}
		calls, uploads, _ := upstream.snapshot()
		if !slices.Equal(calls, []string{"z-drive@example.com"}) {
			t.Fatalf("Generate 调用 = %v，期望只在记着 Drive 可用的账号上", calls)
		}
		if len(uploads) != 2 || !strings.HasPrefix(uploads[0], "unknown-") || uploads[1] != "z-drive@example.com" {
			t.Fatalf("上传 = %v，期望只试一个状态未知的账号、再在可用账号上上传", uploads)
		}
		if elapsed < hold-50*time.Millisecond {
			t.Fatalf("用了 %s，期望排队等可用账号空出（约 %s）", elapsed, hold)
		}
		assertNoAccountCooldown(t, pool)
	})
	t.Run("主用模型排队超过预算", func(t *testing.T) {
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, pool := omniDriveService(t, upstream)
		pool.SetPerAccountConcurrency(1)
		service.setPrimaryModelSettings([]string{omniDriveModel}, 300*time.Millisecond, nil, 0)
		release := holdLease(t, pool, "z-drive@example.com")
		defer release()
		_, err, elapsed := runOmniDriveRequest(t, service, newAPIVideoRequest("req-busy-primary", omniDriveModel))
		var busy *aistudio.CapacityBusyError
		var driveRequired *aistudio.DriveRequiredError
		if !errors.As(err, &busy) || errors.As(err, &driveRequired) {
			t.Fatalf("err = %v，期望号池繁忙（429），不是需要 Drive 授权", err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("用了 %s，期望在排队预算附近结束", elapsed)
		}
		if calls, uploads, _ := upstream.snapshot(); len(calls) != 0 || len(uploads) > 1 {
			t.Fatalf("Generate=%v 上传=%v，期望最多试一个状态未知的账号", calls, uploads)
		}
	})
}

// TestOmniDriveProbeOnlyWarmUnknown 记着 Drive 可用的账号在忙时试的状态未知账号只取 Worker 已就绪的：没有已就绪的状态未知账号时
// 不试（不为确认一个账号没有 Drive 冷启动浏览器），直接排队等可用账号
func TestOmniDriveProbeOnlyWarmUnknown(t *testing.T) {
	accounts := []string{"a-cold@example.com", "z-drive@example.com"}
	upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
	upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
	service, pool := omniDriveService(t, upstream)
	pool.SetPerAccountConcurrency(1)
	service.workers.accounts["a-cold@example.com"].warm.Store(false)
	release := holdLease(t, pool, "z-drive@example.com")
	timer := time.AfterFunc(300*time.Millisecond, release)
	defer timer.Stop()
	text, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest("req-cold", omniDriveModel))
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, []string{"z-drive@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，期望不试没有就绪 Worker 的状态未知账号", calls, uploads)
	}
}

// restoredMediaRequest 为第 2 轮请求：上一轮生成的视频（mime 为 video/mp4）或图片在 assistant 正文里只是本服务的链接，协议层已恢复为
// 媒体（RestoredLink），这一轮是纯文本追问。需要 Drive 的图片用 image/webp（PNG、JPEG 直接内联，见 omni_image_test.go）
func restoredMediaRequest(id string, mime string) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{
		ID: id, Model: omniDriveModel,
		Contents: []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个海边视频"}}},
			{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
				{Text: "好的：\n"},
				{InlineData: &aistudio.Blob{MIME: mime, Data: []byte("media")}, RestoredLink: true, RestoredText: restoredMediaLink(mime)},
				{Text: "\n"},
			}},
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "把它改成夜景"}}},
		},
	}
}

// restoredMediaLink 为 restoredMediaRequest 里恢复前的链接文本
func restoredMediaLink(mime string) string {
	if strings.HasPrefix(mime, "video/") {
		return "[video](https://ai.example.com/media/" + strings.Repeat("ab", 32) + ".mp4)"
	}
	return "![image](https://ai.example.com/media/" + strings.Repeat("cd", 32) + "." + strings.TrimPrefix(mime, "image/") + ")"
}

// restoredVideoRequest 为恢复出上一轮视频的第 2 轮请求（见 restoredMediaRequest）
func restoredVideoRequest(id string) aistudio.GenerateRequest {
	return restoredMediaRequest(id, "video/mp4")
}

// assertRestoredLinkText 断言 Generate 只调用一次、收到的 assistant 消息已换回链接文本并与前后文本合并成一段
func assertRestoredLinkText(t *testing.T, upstream *omniDriveUpstream, mime string) {
	t.Helper()
	want := "好的：\n" + restoredMediaLink(mime) + "\n"
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.contents) != 1 {
		t.Fatalf("Generate 调用 %d 次，期望 1 次", len(upstream.contents))
	}
	assistant := upstream.contents[0][1]
	if len(assistant.Parts) != 1 || assistant.Parts[0].Text != want || assistant.Parts[0].InlineData != nil {
		t.Fatalf("assistant 消息 = %+v，期望换回链接文本、与前后文本合并成一段 %q", assistant.Parts, want)
	}
}

// assertRestoredSentAsFile 断言 Generate 只调用一次、收到的 assistant 历史媒体已上传为 Drive 文件（模型能看到它）
func assertRestoredSentAsFile(t *testing.T, upstream *omniDriveUpstream) {
	t.Helper()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.contents) != 1 {
		t.Fatalf("Generate 调用 %d 次，期望 1 次", len(upstream.contents))
	}
	if parts := upstream.contents[0][1].Parts; len(parts) != 3 || parts[1].File == nil || parts[1].InlineData != nil {
		t.Fatalf("assistant 消息 = %+v，期望历史媒体以 Drive 文件发送", parts)
	}
}

// TestOmniDriveRestoredLinksFallBack 需要 Drive 只是因为从本服务链接恢复出来的历史视频或无法转为 PNG 的图片（WebP）时，号池里没有可用的 Drive 授权账号
// 不让请求失败：全部记着不可用时不选号就换回链接文本、按普通请求成功；记着可用的账号都已过时、试满每请求换号硬上限时最后一个账号
// 换回链接文本发送；指定账号没有 Drive 时在它上面换回链接文本发送；都在请求进度里留一行说明。有空闲的 Drive 授权账号时上传发送。
// 客户端自己带的视频、指定了视频任务的请求照常按需要 Drive 授权返回 400（恢复的媒体是任务的输入，换回链接文本后任务没有输入）；
// 客户端自己带的 PNG 图片不需要 Drive，不影响换回链接文本
func TestOmniDriveRestoredLinksFallBack(t *testing.T) {
	for _, kind := range []struct{ name, mime string }{{name: "视频", mime: "video/mp4"}, {name: "WebP 图片", mime: "image/webp"}} {
		t.Run(kind.name+"/全部记着不可用", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			for _, accountID := range accounts {
				upstream.remember(accountID, aistudio.DriveStateUnavailable)
			}
			service, pool := omniDriveService(t, upstream)
			text, err, _ := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored", kind.mime))
			if err != nil || text != "ok" {
				t.Fatalf("第 2 轮应按链接文本成功：text=%q err=%v", text, err)
			}
			if calls, uploads, _ := upstream.snapshot(); len(calls) != 1 || strings.HasSuffix(calls[0], "/inline") || len(uploads) != 0 {
				t.Fatalf("Generate=%v 上传=%v，期望不上传、按链接文本发送一次", calls, uploads)
			}
			assertRestoredLinkText(t, upstream, kind.mime)
			if !progressLogged(service, "号池里没有可用的 Drive 授权账号，回传历史里恢复的 1 个媒体按链接文本发送") {
				t.Fatal("应在请求进度里记一行说明")
			}
			assertNoAccountCooldown(t, pool)
		})
		t.Run(kind.name+"/记着可用却都没有、试满上限", func(t *testing.T) {
			accounts := make([]string, 0, generateSwitchCeiling+2)
			for index := range generateSwitchCeiling + 2 {
				accounts = append(accounts, fmt.Sprintf("stale-%02d@example.com", index))
			}
			upstream := newOmniDriveUpstream(accounts)
			for _, accountID := range accounts {
				upstream.remember(accountID, aistudio.DriveStateAvailable)
			}
			service, pool := omniDriveService(t, upstream)
			text, err, _ := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored-ceiling", kind.mime))
			if err != nil || text != "ok" {
				t.Fatalf("应在第 %d 个账号上按链接文本成功：text=%q err=%v", generateSwitchCeiling, text, err)
			}
			if calls, uploads, _ := upstream.snapshot(); len(calls) != 1 || len(uploads) != generateSwitchCeiling || calls[0] != uploads[len(uploads)-1] {
				t.Fatalf("Generate=%v 上传=%v，期望试满 %d 个账号、最后一个按链接文本发送", calls, uploads, generateSwitchCeiling)
			}
			assertRestoredLinkText(t, upstream, kind.mime)
			if !progressLogged(service, fmt.Sprintf("已试 %d 个账号都没有 Drive 授权，回传历史里恢复的 1 个媒体按链接文本发送", generateSwitchCeiling)) {
				t.Fatal("应在请求进度里记一行说明")
			}
			assertNoAccountCooldown(t, pool)
		})
		t.Run(kind.name+"/指定账号没有 Drive", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
			upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
			service, pool := omniDriveService(t, upstream)
			request := restoredMediaRequest("req-restored-pinned", kind.mime)
			request.AccountID = "a-nodrive@example.com"
			text, err, _ := runOmniDriveRequest(t, service, request)
			if err != nil || text != "ok" {
				t.Fatalf("指定账号应按链接文本成功：text=%q err=%v", text, err)
			}
			if calls, _, _ := upstream.snapshot(); !slices.Equal(calls, []string{"a-nodrive@example.com"}) {
				t.Fatalf("Generate=%v，期望在指定账号上发送", calls)
			}
			assertRestoredLinkText(t, upstream, kind.mime)
			if !progressLogged(service, "指定账号没有 Drive 授权，回传历史里恢复的 1 个媒体按链接文本发送") {
				t.Fatal("应在请求进度里记一行说明")
			}
			assertNoAccountCooldown(t, pool)
		})
		t.Run(kind.name+"/可用账号空闲时上传发送", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
			upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
			service, _ := omniDriveService(t, upstream)
			text, err, _ := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored-drive", kind.mime))
			if err != nil || text != "ok" {
				t.Fatalf("text=%q err=%v", text, err)
			}
			if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, calls) {
				t.Fatalf("Generate=%v 上传=%v，期望在记着 Drive 可用的账号上把历史媒体上传发送", calls, uploads)
			}
			assertRestoredSentAsFile(t, upstream)
			if progressLogged(service, "按链接文本发送") {
				t.Fatal("有空闲的 Drive 授权账号时不应换回链接文本")
			}
		})
		t.Run(kind.name+"/客户端带视频照常 400", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			service, _ := omniDriveService(t, upstream)
			request := restoredMediaRequest("req-restored-client", kind.mime)
			request.Contents[2].Parts = append(request.Contents[2].Parts, aistudio.Part{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("video")}})
			_, err, _ := runOmniDriveRequest(t, service, request)
			var driveRequired *aistudio.DriveRequiredError
			if !errors.As(err, &driveRequired) || driveRequired.Reason != aistudio.DriveRequiredNoAccount {
				t.Fatalf("err = %v，客户端自己带的视频应按需要 Drive 授权返回 400", err)
			}
			if calls, _, _ := upstream.snapshot(); len(calls) != 0 {
				t.Fatalf("不应调用 Generate: %v", calls)
			}
		})
		t.Run(kind.name+"/客户端带 PNG 仍换回链接", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			service, _ := omniDriveService(t, upstream)
			request := restoredMediaRequest("req-restored-client-png", kind.mime)
			png := &aistudio.Blob{MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\nclient")}
			request.Contents[2].Parts = append(request.Contents[2].Parts, aistudio.Part{InlineData: png})
			text, err, _ := runOmniDriveRequest(t, service, request)
			if err != nil || text != "ok" {
				t.Fatalf("客户端的 PNG 不需要 Drive，恢复的媒体应换回链接文本后成功：text=%q err=%v", text, err)
			}
			assertRestoredLinkText(t, upstream, kind.mime)
			upstream.mu.Lock()
			sent := upstream.contents[0][2].Parts
			upstream.mu.Unlock()
			if len(sent) != 2 || sent[1].InlineData != png {
				t.Fatalf("这一轮消息 = %+v，客户端的 PNG 应原样内联发送", sent)
			}
		})
	}
	t.Run("指定视频任务照常 400", func(t *testing.T) {
		// 协议层只在对话没有模型轮次时保留任务；这里直接构造恢复媒体与任务同在的请求（例如上一条用户消息粘贴的链接），
		// 恢复的视频是 edit 的输入，换回链接文本后任务没有输入：按需要 Drive 授权返回，不换回链接文本、不出 first-turn 或缺少视频的 400
		accounts := []string{"a-nodrive@example.com"}
		upstream := newOmniDriveUpstream(accounts)
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		service, _ := omniDriveService(t, upstream)
		request := aistudio.GenerateRequest{
			ID: "req-restored-task", Model: omniDriveModel,
			Config: aistudio.GenerationConfig{VideoConfig: &aistudio.VideoConfig{Task: "edit"}},
			Contents: []aistudio.Content{
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{
					{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("video")}, RestoredLink: true, RestoredText: restoredMediaLink("video/mp4")},
				}},
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "把天空换成晚霞"}}},
			},
		}
		_, err, _ := runOmniDriveRequest(t, service, request)
		var driveRequired *aistudio.DriveRequiredError
		if !errors.As(err, &driveRequired) || driveRequired.Reason != aistudio.DriveRequiredNoAccount || driveRequired.HTTPStatus() != http.StatusBadRequest {
			t.Fatalf("err = %v，指定了视频任务时恢复的视频应按需要 Drive 授权返回 400", err)
		}
		if calls, _, _ := upstream.snapshot(); len(calls) != 0 || progressLogged(service, "按链接文本发送") {
			t.Fatalf("不应换回链接文本发送: %v", calls)
		}
	})
}

// TestOmniDriveRestoredImageLenient 只因恢复出来的无法转为 PNG 的历史图片（WebP）需要 Drive（例如出图后追问“谢谢，再来一个”）时不让请求久等：状态都未知时
// 只试一个 Worker 已就绪的账号，没有已就绪的状态未知账号时不为试 Drive 冷启动，记着可用的账号在忙时限时排队；之后换回链接文本发送
func TestOmniDriveRestoredImageLenient(t *testing.T) {
	const mime = "image/webp"
	t.Run("状态未知只试一个", func(t *testing.T) {
		accounts := make([]string, 0, generateSwitchCeiling+2)
		for index := range generateSwitchCeiling + 2 {
			accounts = append(accounts, fmt.Sprintf("nodrive-%02d@example.com", index))
		}
		upstream := newOmniDriveUpstream(accounts)
		service, pool := omniDriveService(t, upstream)
		text, err, _ := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored-unknown", mime))
		if err != nil || text != "ok" {
			t.Fatalf("应按链接文本成功：text=%q err=%v", text, err)
		}
		calls, uploads, _ := upstream.snapshot()
		if len(uploads) != 1 || len(calls) != 1 || calls[0] == uploads[0] {
			t.Fatalf("Generate=%v 上传=%v，期望只试一个状态未知的账号、换一个账号按链接文本发送", calls, uploads)
		}
		assertRestoredLinkText(t, upstream, mime)
		if !progressLogged(service, "没有记着 Drive 可用的账号，试过的状态未知账号也没有 Drive 授权，回传历史里恢复的 1 个媒体按链接文本发送") {
			t.Fatal("应在请求进度里记一行说明")
		}
		assertNoAccountCooldown(t, pool)
	})
	t.Run("没有已就绪的状态未知账号", func(t *testing.T) {
		accounts := []string{"a-cold@example.com", "b-nodrive@example.com"}
		upstream := newOmniDriveUpstream(accounts)
		upstream.remember("b-nodrive@example.com", aistudio.DriveStateUnavailable)
		service, _ := omniDriveService(t, upstream)
		service.workers.accounts["a-cold@example.com"].warm.Store(false)
		text, err, _ := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored-cold", mime))
		if err != nil || text != "ok" {
			t.Fatalf("应按链接文本成功：text=%q err=%v", text, err)
		}
		if calls, uploads, _ := upstream.snapshot(); len(uploads) != 0 || !slices.Equal(calls, []string{"b-nodrive@example.com"}) {
			t.Fatalf("Generate=%v 上传=%v，期望不为试 Drive 冷启动状态未知的账号、直接按链接文本在已启动的账号上发送", calls, uploads)
		}
		assertRestoredLinkText(t, upstream, mime)
		if !progressLogged(service, "没有记着 Drive 可用的账号，也没有 Worker 已就绪的状态未知账号可试") {
			t.Fatal("应在请求进度里记一行说明")
		}
	})
	t.Run("可用账号在忙限时排队后换回链接", func(t *testing.T) {
		previous := restoredDriveQueueWait
		restoredDriveQueueWait = 600 * time.Millisecond
		t.Cleanup(func() { restoredDriveQueueWait = previous })
		accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, pool := omniDriveService(t, upstream)
		pool.SetPerAccountConcurrency(1)
		release := holdLease(t, pool, "z-drive@example.com")
		defer release()
		text, err, elapsed := runOmniDriveRequest(t, service, restoredMediaRequest("req-restored-busy", mime))
		if err != nil || text != "ok" {
			t.Fatalf("应限时排队后按链接文本成功：text=%q err=%v", text, err)
		}
		if elapsed < restoredDriveQueueWait-50*time.Millisecond || elapsed > 5*time.Second {
			t.Fatalf("用了 %s，期望在记着 Drive 可用的账号上排队约 %s 后换回链接文本", elapsed, restoredDriveQueueWait)
		}
		if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"a-nodrive@example.com"}) || len(uploads) != 0 {
			t.Fatalf("Generate=%v 上传=%v，期望换回链接文本后按普通请求发送", calls, uploads)
		}
		assertRestoredLinkText(t, upstream, mime)
		if !progressLogged(service, "有 Drive 授权的账号现在都取不到") {
			t.Fatal("应在请求进度里记一行说明")
		}
	})
}

// TestOmniDriveRestoredVideoNeedsDrive 恢复出来的上一轮视频（“把它改成夜景”这类编辑要让模型看到原视频）与客户端附件一样选号：
// 记着 Drive 可用的账号在忙时照常排队等它空出、没有热 Worker 时冷启动它，状态都未知时逐个试到有 Drive 的账号，都在上传后把视频发给模型；
// 只在换满每请求换号硬上限、记着可用的账号都在长时间冷却时换回链接文本。原先与只有图片时一样只等 10 秒、不冷启动，多半换回链接文本
func TestOmniDriveRestoredVideoNeedsDrive(t *testing.T) {
	t.Run("可用账号在忙时排队", func(t *testing.T) {
		previous := restoredDriveQueueWait
		restoredDriveQueueWait = 200 * time.Millisecond
		t.Cleanup(func() { restoredDriveQueueWait = previous })
		accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, pool := omniDriveService(t, upstream)
		pool.SetPerAccountConcurrency(1)
		release := holdLease(t, pool, "z-drive@example.com")
		const hold = 800 * time.Millisecond
		timer := time.AfterFunc(hold, release)
		defer timer.Stop()
		text, err, elapsed := runOmniDriveRequest(t, service, restoredVideoRequest("req-video-busy"))
		if err != nil || text != "ok" {
			t.Fatalf("应排队等记着 Drive 可用的账号空出后成功：text=%q err=%v", text, err)
		}
		if elapsed < hold-50*time.Millisecond {
			t.Fatalf("用了 %s，期望排队等可用账号空出（约 %s），不按只有图片时的限时换回链接文本", elapsed, hold)
		}
		if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, calls) {
			t.Fatalf("Generate=%v 上传=%v，期望在记着 Drive 可用的账号上把视频上传发送", calls, uploads)
		}
		assertRestoredSentAsFile(t, upstream)
		if progressLogged(service, "按链接文本发送") {
			t.Fatal("可用账号只是在忙时不应换回链接文本")
		}
		assertNoAccountCooldown(t, pool)
	})
	t.Run("可用账号没有热 Worker 时冷启动", func(t *testing.T) {
		accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, _ := omniDriveService(t, upstream)
		service.workers.accounts["z-drive@example.com"].warm.Store(false)
		text, err, _ := runOmniDriveRequest(t, service, restoredVideoRequest("req-video-cold"))
		if err != nil || text != "ok" {
			t.Fatalf("text=%q err=%v", text, err)
		}
		if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, calls) {
			t.Fatalf("Generate=%v 上传=%v，期望为记着 Drive 可用的账号启动 Worker、把视频上传发送", calls, uploads)
		}
		assertRestoredSentAsFile(t, upstream)
	})
	t.Run("状态未知时逐个试到有 Drive 的账号", func(t *testing.T) {
		accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com", "c-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		service, pool := omniDriveService(t, upstream)
		text, err, _ := runOmniDriveRequest(t, service, restoredVideoRequest("req-video-unknown"))
		if err != nil || text != "ok" {
			t.Fatalf("text=%q err=%v", text, err)
		}
		if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) || len(uploads) != len(accounts) {
			t.Fatalf("Generate=%v 上传=%v，期望逐个换号到有 Drive 的账号，不只试一个状态未知的账号就换回链接文本", calls, uploads)
		}
		assertRestoredSentAsFile(t, upstream)
		assertNoAccountCooldown(t, pool)
	})
	t.Run("状态未知都没有 Drive、试满上限", func(t *testing.T) {
		accounts := make([]string, 0, generateSwitchCeiling+2)
		for index := range generateSwitchCeiling + 2 {
			accounts = append(accounts, fmt.Sprintf("nodrive-%02d@example.com", index))
		}
		upstream := newOmniDriveUpstream(accounts)
		service, pool := omniDriveService(t, upstream)
		text, err, _ := runOmniDriveRequest(t, service, restoredVideoRequest("req-video-ceiling"))
		if err != nil || text != "ok" {
			t.Fatalf("应在第 %d 个账号上按链接文本成功：text=%q err=%v", generateSwitchCeiling, text, err)
		}
		if calls, uploads, _ := upstream.snapshot(); len(calls) != 1 || len(uploads) != generateSwitchCeiling || calls[0] != uploads[len(uploads)-1] {
			t.Fatalf("Generate=%v 上传=%v，期望试满 %d 个账号、最后一个按链接文本发送", calls, uploads, generateSwitchCeiling)
		}
		assertRestoredLinkText(t, upstream, "video/mp4")
		if !progressLogged(service, fmt.Sprintf("已试 %d 个账号都没有 Drive 授权，回传历史里恢复的 1 个媒体按链接文本发送", generateSwitchCeiling)) {
			t.Fatal("应在请求进度里记一行说明")
		}
		assertNoAccountCooldown(t, pool)
	})
	t.Run("可用账号都在长时间冷却", func(t *testing.T) {
		accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, pool := omniDriveService(t, upstream)
		if err := pool.MarkCooldownIfGeneration("z-drive@example.com", omniDriveModel, pool.ModelAccessGeneration("z-drive@example.com"),
			time.Now(), time.Now().Add(2*time.Hour), "每日限额"); err != nil {
			t.Fatal(err)
		}
		text, err, elapsed := runOmniDriveRequest(t, service, restoredVideoRequest("req-video-cooling"))
		if err != nil || text != "ok" {
			t.Fatalf("应换回链接文本按普通请求成功：text=%q err=%v", text, err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("用了 %s，可用账号长时间冷却时不应排队等它", elapsed)
		}
		if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"a-nodrive@example.com"}) || len(uploads) != 0 {
			t.Fatalf("Generate=%v 上传=%v，期望换回链接文本后在其他账号上发送", calls, uploads)
		}
		assertRestoredLinkText(t, upstream, "video/mp4")
		if !progressLogged(service, "记着 Drive 可用的账号都在冷却") {
			t.Fatal("应在请求进度里记一行说明")
		}
	})
}

// TestRestoredMediaAsLinks 换回链接文本：与恢复时拆开的前后文本合并、签名留在第一段；同一条消息里已有文本链接（RestoredText 为空）时
// 只留签名；只换回需要 Drive 的视频与无法转为 PNG 的图片（WebP），恢复出来的 PNG、JPEG 照样内联；没有恢复媒体的消息与客户端自己的附件
// 不动，原内容不改
func TestRestoredMediaAsLinks(t *testing.T) {
	video := &aistudio.Blob{MIME: "video/mp4", Data: []byte("v")}
	image := &aistudio.Blob{MIME: "image/webp", Data: []byte("p")}
	pngImage := &aistudio.Blob{MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\np")}
	contents := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "第一段"}, {Text: "第二段"}, {InlineData: image}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "看 ", ThoughtSignature: "sig-1"},
			{InlineData: video, RestoredLink: true, RestoredText: "[video](/media/a.mp4)"},
			{Text: " 和 "},
			{InlineData: image, RestoredLink: true, RestoredText: "![image](/media/b.webp)"},
			{Text: " 两个"},
			{InlineData: pngImage, RestoredLink: true, RestoredText: "![image](/media/d.png)"},
		}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "[video](/media/c.mp4)"},
			{InlineData: video, RestoredLink: true, ThoughtSignature: "sig-2"},
			{InlineData: video, RestoredLink: true},
		}},
	}
	original := slices.Clone(contents[1].Parts)
	got, replaced := restoredMediaAsLinks(contents)
	if replaced != 4 {
		t.Fatalf("换回 %d 个，期望 4 个", replaced)
	}
	if !slices.Equal(got[0].Parts, contents[0].Parts) {
		t.Fatalf("没有恢复媒体的消息不应改动: %+v", got[0].Parts)
	}
	if want := []aistudio.Part{
		{Text: "看 [video](/media/a.mp4) 和 ![image](/media/b.webp) 两个", ThoughtSignature: "sig-1"},
		{InlineData: pngImage, RestoredLink: true, RestoredText: "![image](/media/d.png)"},
	}; !slices.Equal(got[1].Parts, want) {
		t.Fatalf("assistant 消息 = %+v，期望 %+v（恢复的 PNG 照样内联）", got[1].Parts, want)
	}
	if want := []aistudio.Part{{Text: "[video](/media/c.mp4)"}, {ThoughtSignature: "sig-2"}}; !slices.Equal(got[2].Parts, want) {
		t.Fatalf("已有文本链接的消息 = %+v，期望 %+v", got[2].Parts, want)
	}
	if !slices.Equal(contents[1].Parts, original) {
		t.Fatal("不应改动原内容")
	}
}

// TestDriveStateMemoryPersists Drive 授权状态写盘与恢复：上报的可用、不可用记录写进 drive-accounts.json，重新打开后交给生成服务恢复；
// 清除的记录、过期的记录与没有登录签名的记录不写、不恢复；之后的上报继续写盘
func TestDriveStateMemoryPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), driveStateFileName)
	now := time.Now()
	memory := openDriveStateMemory(path, nil)
	memory.observe(aistudio.DriveRecord{AccountID: "a@example.com", State: aistudio.DriveStateAvailable, CheckedAt: now, Identity: "id-a"})
	memory.observe(aistudio.DriveRecord{AccountID: "b@example.com", State: aistudio.DriveStateUnavailable, CheckedAt: now.Add(-time.Hour), Identity: "id-b"})
	memory.observe(aistudio.DriveRecord{AccountID: "c@example.com", State: aistudio.DriveStateUnavailable, CheckedAt: now, Identity: "id-c"})
	memory.observe(aistudio.DriveRecord{AccountID: "c@example.com", State: aistudio.DriveStateUnknown})
	memory.observe(aistudio.DriveRecord{AccountID: "d@example.com", State: aistudio.DriveStateUnavailable, CheckedAt: now.Add(-driveStateKeep - time.Minute), Identity: "id-d"})
	memory.observe(aistudio.DriveRecord{AccountID: "e@example.com", State: aistudio.DriveStateAvailable, CheckedAt: now})
	if err := memory.close(); err != nil {
		t.Fatal(err)
	}
	reopened := openDriveStateMemory(path, nil)
	store := &driveRecordRecorder{}
	reopened.attach(store)
	slices.SortFunc(store.restored, func(left, right aistudio.DriveRecord) int { return strings.Compare(left.AccountID, right.AccountID) })
	want := []aistudio.DriveRecord{
		{AccountID: "a@example.com", State: aistudio.DriveStateAvailable, CheckedAt: time.Unix(now.Unix(), 0), Identity: "id-a"},
		{AccountID: "b@example.com", State: aistudio.DriveStateUnavailable, CheckedAt: time.Unix(now.Add(-time.Hour).Unix(), 0), Identity: "id-b"},
	}
	if len(store.restored) != len(want) {
		t.Fatalf("恢复 = %+v，期望 %+v", store.restored, want)
	}
	for index := range want {
		if got := store.restored[index]; got.AccountID != want[index].AccountID || got.State != want[index].State ||
			!got.CheckedAt.Equal(want[index].CheckedAt) || got.Identity != want[index].Identity {
			t.Fatalf("恢复第 %d 条 = %+v，期望 %+v", index, got, want[index])
		}
	}
	if store.observer == nil {
		t.Fatal("恢复后应设置写盘回调")
	}
	store.observer(aistudio.DriveRecord{AccountID: "a@example.com", State: aistudio.DriveStateUnavailable, CheckedAt: now, Identity: "id-a"})
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}
	if entry := openDriveStateMemory(path, nil).entries["a@example.com"]; entry.State != "u" {
		t.Fatalf("之后的上报应继续写盘，实际 %+v", entry)
	}
}

// driveRecordRecorder 记录交给生成服务恢复的 Drive 状态与写盘回调
type driveRecordRecorder struct {
	restored []aistudio.DriveRecord
	observer func(aistudio.DriveRecord)
}

func (store *driveRecordRecorder) SetDriveObserver(observer func(aistudio.DriveRecord)) {
	store.observer = observer
}

func (store *driveRecordRecorder) RestoreDriveRecords(records []aistudio.DriveRecord) int {
	store.restored = append(store.restored, records...)
	return len(records)
}

// driveCandidatesStub 按固定结果报告候选的 Drive 状态
type driveCandidatesStub struct{ result aistudio.DriveCandidates }

func (stub driveCandidatesStub) DriveCandidates(context.Context, aistudio.AccountSelection) aistudio.DriveCandidates {
	return stub.result
}

// TestDrivePlanSelectionLists Drive 选号的限定与排除列表：可用账号少于其余账号时限定到可用账号，多于其余账号时改为排除记着不可用与
// 状态未知的账号（列表不随号池增长）；先试的状态未知账号只取 Worker 已就绪的，选择条件保留本请求原有的排除；只因恢复出来的无法转为 PNG 的历史图片
// 需要 Drive、没有可用账号时只试已就绪的状态未知账号，没有就换回链接文本，恢复出视频时照常选号；候选集为空时不限定；
// 用尽时只剩记着不可用的账号为 400，其余为 429
func TestDrivePlanSelectionLists(t *testing.T) {
	ids := func(prefix string, count int) []string {
		result := make([]string, 0, count)
		for index := range count {
			result = append(result, fmt.Sprintf("%s-%02d@example.com", prefix, index))
		}
		return result
	}
	base := aistudio.AccountSelection{ModelID: omniDriveModel, Method: "generateContent", ExcludedAccountIDs: []string{"tried@example.com"}}
	attempted := map[string]struct{}{"tried@example.com": {}}
	warm := func() []string { return []string{"c-unknown-01@example.com", "a-avail-00@example.com"} }

	few := &driveSelection{source: driveCandidatesStub{aistudio.DriveCandidates{
		Available: ids("a-avail", 2), Unavailable: ids("b-nodrive", 3), Unknown: ids("c-unknown", 3),
	}}}
	plan := few.plan(context.Background(), base, attempted, warm)
	if !plan.available || !slices.Equal(plan.selection.AllowedAccountIDs, ids("a-avail", 2)) ||
		!slices.Equal(plan.selection.ExcludedAccountIDs, base.ExcludedAccountIDs) {
		t.Fatalf("可用账号少时应限定到可用账号: %+v", plan.selection)
	}
	if !slices.Equal(plan.probe, []string{"c-unknown-01@example.com"}) ||
		!slices.Equal(plan.probeSelection.AllowedAccountIDs, plan.probe) || !slices.Equal(plan.probeSelection.ExcludedAccountIDs, base.ExcludedAccountIDs) {
		t.Fatalf("先试的状态未知账号 = %v，选择条件 %+v", plan.probe, plan.probeSelection)
	}

	many := &driveSelection{source: driveCandidatesStub{aistudio.DriveCandidates{
		Available: ids("a-avail", 6), Unavailable: ids("b-nodrive", 2), Unknown: ids("c-unknown", 3),
	}}}
	plan = many.plan(context.Background(), base, attempted, warm)
	wantExcluded := slices.Concat(base.ExcludedAccountIDs, ids("b-nodrive", 2), ids("c-unknown", 3))
	if !plan.available || plan.selection.AllowedAccountIDs != nil || !slices.Equal(plan.selection.ExcludedAccountIDs, wantExcluded) {
		t.Fatalf("可用账号多时应排除其余账号: %+v", plan.selection)
	}
	if !slices.Equal(plan.probeSelection.ExcludedAccountIDs, base.ExcludedAccountIDs) {
		t.Fatalf("先试状态未知账号的选择条件不应排除它们: %+v", plan.probeSelection)
	}
	many.probes = driveUnknownProbeLimit
	if plan = many.plan(context.Background(), base, attempted, warm); len(plan.probe) != 0 {
		t.Fatalf("试过 %d 个状态未知账号后不应再试: %v", driveUnknownProbeLimit, plan.probe)
	}

	restored := &driveSelection{restoredOnly: true, lenient: true, source: driveCandidatesStub{aistudio.DriveCandidates{
		Unavailable: ids("b-nodrive", 2), Unknown: ids("c-unknown", 3),
	}}}
	if plan = restored.plan(context.Background(), base, attempted, warm); plan.fallback || plan.available ||
		!slices.Equal(plan.probe, []string{"c-unknown-01@example.com"}) {
		t.Fatalf("只因恢复的媒体需要 Drive 时应只试已就绪的状态未知账号: %+v", plan)
	}
	restored.probes = driveUnknownProbeLimit
	if plan = restored.plan(context.Background(), base, attempted, warm); !plan.fallback {
		t.Fatalf("试过状态未知账号后应换回链接文本: %+v", plan)
	}

	// 恢复的媒体里有视频：与客户端附件一样在状态未知的账号里照常选号，不只试已就绪的、不换回链接文本
	video := &driveSelection{restoredOnly: true, source: driveCandidatesStub{aistudio.DriveCandidates{
		Unavailable: ids("b-nodrive", 2), Unknown: ids("c-unknown", 3),
	}}}
	if plan = video.plan(context.Background(), base, attempted, warm); plan.fallback || plan.exhausted || len(plan.probe) != 0 ||
		plan.selection.AllowedAccountIDs != nil || !slices.Equal(plan.selection.ExcludedAccountIDs, slices.Concat(base.ExcludedAccountIDs, ids("b-nodrive", 2))) {
		t.Fatalf("恢复出视频时应在状态未知的账号里照常选号: %+v", plan)
	}

	// 候选集为空（账号都需要登录、不可调度）：不按 Drive 状态限定，交给普通选号报原有的错误
	empty := &driveSelection{source: driveCandidatesStub{}}
	if plan = empty.plan(context.Background(), base, attempted, warm); plan.exhausted || plan.available || plan.fallback ||
		plan.selection.AllowedAccountIDs != nil || !slices.Equal(plan.selection.ExcludedAccountIDs, base.ExcludedAccountIDs) {
		t.Fatalf("候选集为空时不应限定选号或判为用尽: %+v", plan)
	}

	// 用尽时的错误：只剩记着不可用的账号为 400 FAILED_PRECONDITION；状态未知的账号本请求都试过时为号池繁忙 429
	onlyUnavailable := &driveSelection{source: driveCandidatesStub{aistudio.DriveCandidates{Unavailable: ids("b-nodrive", 2)}}}
	if plan = onlyUnavailable.plan(context.Background(), base, attempted, warm); !plan.exhausted {
		t.Fatalf("只剩记着不可用的账号时应判为用尽: %+v", plan)
	}
	var driveRequired *aistudio.DriveRequiredError
	if err := onlyUnavailable.exhaustedError(omniDriveModel, aistudio.PoolScopeNormal); !errors.As(err, &driveRequired) ||
		driveRequired.Reason != aistudio.DriveRequiredNoAccount {
		t.Fatalf("err = %v，期望号池里没有 Drive 授权的账号", err)
	}
	unknownTried := &driveSelection{source: driveCandidatesStub{aistudio.DriveCandidates{
		Unavailable: ids("b-nodrive", 2), Unknown: []string{"tried@example.com"},
	}}}
	if plan = unknownTried.plan(context.Background(), base, attempted, warm); !plan.exhausted {
		t.Fatalf("状态未知的账号都试过时应判为用尽: %+v", plan)
	}
	var busy *aistudio.CapacityBusyError
	if err := unknownTried.exhaustedError(omniDriveModel, aistudio.PoolScopeNormal); !errors.As(err, &busy) || errors.As(err, &driveRequired) {
		t.Fatalf("err = %v，状态未知的账号本请求试过时应按号池繁忙返回，不说号池里没有 Drive 授权的账号", err)
	}
}

// TestOmniDriveManyAvailableExcludesOthers 记着 Drive 可用的账号多于其余账号时选号改为排除其余账号：可用账号都在忙时同样只试一个
// 状态未知的账号，之后排队等可用账号空出，不选到另一个空闲的状态未知账号
func TestOmniDriveManyAvailableExcludesOthers(t *testing.T) {
	drive := []string{"z1-drive@example.com", "z2-drive@example.com", "z3-drive@example.com"}
	accounts := append([]string{"a-unknown@example.com", "b-unknown@example.com"}, drive...)
	upstream := newOmniDriveUpstream(accounts, drive...)
	for _, accountID := range drive {
		upstream.remember(accountID, aistudio.DriveStateAvailable)
	}
	service, pool := omniDriveService(t, upstream)
	pool.SetPerAccountConcurrency(1)
	releases := make([]func(), 0, len(drive))
	for _, accountID := range drive {
		releases = append(releases, holdLease(t, pool, accountID))
	}
	const hold = 400 * time.Millisecond
	timer := time.AfterFunc(hold, releases[0])
	defer timer.Stop()
	text, err, elapsed := runOmniDriveRequest(t, service, newAPIVideoRequest("req-many", omniDriveModel))
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	calls, uploads, _ := upstream.snapshot()
	if !slices.Equal(calls, []string{"z1-drive@example.com"}) || !slices.Equal(uploads, []string{"a-unknown@example.com", "z1-drive@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，期望试一个状态未知的账号后排队等记着 Drive 可用的账号", calls, uploads)
	}
	if elapsed < hold-50*time.Millisecond {
		t.Fatalf("用了 %s，期望排队等可用账号空出", elapsed)
	}
	for _, release := range releases[1:] {
		release()
	}
	assertNoAccountCooldown(t, pool)
}

// TestOmniDriveCeilingWithAvailableReturns429 换满每请求换号硬上限时号池里还有记着 Drive 可用的账号（这里的记录都已过时）：
// 按号池繁忙返回 429（带 Retry-After），不按“换过的账号都没有 Drive 授权”的 503 结束；不冷却
func TestOmniDriveCeilingWithAvailableReturns429(t *testing.T) {
	accounts := make([]string, 0, generateSwitchCeiling+2)
	for index := range generateSwitchCeiling + 2 {
		accounts = append(accounts, fmt.Sprintf("stale-%02d@example.com", index))
	}
	upstream := newOmniDriveUpstream(accounts)
	for _, accountID := range accounts {
		upstream.remember(accountID, aistudio.DriveStateAvailable)
	}
	service, pool := omniDriveService(t, upstream)
	_, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest("req-ceiling-available", omniDriveModel))
	var busy *aistudio.CapacityBusyError
	var driveRequired *aistudio.DriveRequiredError
	if !errors.As(err, &busy) || errors.As(err, &driveRequired) || busy.Until.IsZero() {
		t.Fatalf("err = %#v，期望号池繁忙的 429", err)
	}
	if calls, uploads, _ := upstream.snapshot(); len(calls) != 0 || len(uploads) != generateSwitchCeiling {
		t.Fatalf("Generate=%v 上传=%v，期望只上传 %d 次、不调用 Generate", calls, uploads, generateSwitchCeiling)
	}
	assertNoAccountCooldown(t, pool)
}

// TestDriveStatesSavedOnStop 停止服务时立即写出 Drive 授权状态记录（不等 30 秒的合并写盘），重建运行时的新一代读到最新的记录
func TestDriveStatesSavedOnStop(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	path := filepath.Join(t.TempDir(), driveStateFileName)
	service.driveStates = openDriveStateMemory(path, requests)
	t.Cleanup(func() { _ = service.driveStates.close() })
	service.driveStates.observe(aistudio.DriveRecord{
		AccountID: partitionNormalA, State: aistudio.DriveStateAvailable, CheckedAt: time.Now(), Identity: "id-a",
	})
	if entry, ok := openDriveStateMemory(path, requests).entries[partitionNormalA]; ok {
		t.Fatalf("停止前还没到合并写盘的时间，文件里不应已有记录: %+v", entry)
	}
	modelRefreshDone := make(chan struct{})
	close(modelRefreshDone)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	if entry := openDriveStateMemory(path, requests).entries[partitionNormalA]; entry.State != "a" || entry.Identity != "id-a" {
		t.Fatalf("停止服务后新一代应读到记录，实际 %+v", entry)
	}
}

// TestOmniDriveEmptyCandidatesUsesPoolErrors 候选集为空（Omni 账号都需要登录，DriveCandidates 不含不可调度的账号）时，带视频请求不误报
// “号池里没有 Drive 授权的账号”的 400，与纯文本请求一样按号池暂时不可用返回 503（中转据此稍后重试）
func TestOmniDriveEmptyCandidatesUsesPoolErrors(t *testing.T) {
	accounts := []string{"a-login@example.com", "b-login@example.com"}
	upstream := newOmniDriveUpstream(accounts, "b-login@example.com")
	service, pool := omniDriveService(t, upstream)
	for _, accountID := range accounts {
		if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	// 与 aistudio.DriveCandidates 一致：需要登录的账号不在候选集里
	upstream.mu.Lock()
	upstream.accounts = nil
	upstream.mu.Unlock()
	for _, request := range []aistudio.GenerateRequest{
		newAPIVideoRequest("req-empty-video", omniDriveModel),
		{ID: "req-empty-text", Model: omniDriveModel, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个视频"}}}}},
	} {
		done := make(chan error, 1)
		go func() {
			_, err, _ := runOmniDriveRequest(t, service, request)
			done <- err
		}()
		var err error
		select {
		case err = <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("%s 没有结束", request.ID)
		}
		var notReady *aistudio.AccountsNotReadyError
		var driveRequired *aistudio.DriveRequiredError
		if !errors.As(err, &notReady) || notReady.HTTPStatus() != http.StatusServiceUnavailable || errors.As(err, &driveRequired) {
			t.Fatalf("%s: err = %#v，期望号池暂时不可用的 503，不是需要 Drive 授权", request.ID, err)
		}
	}
	if calls, uploads, _ := upstream.snapshot(); len(calls)+len(uploads) != 0 {
		t.Fatalf("Generate=%v 上传=%v，不应取得租约", calls, uploads)
	}
}

// failingDriveUpstream 在 omniDriveUpstream 之上让 failures 里账号的前几次 Generate 先执行 beforeFail（模拟同账号还有请求在输出），
// 再返回协议 Code 5（WAA 运行态失效，要关闭 Worker）
type failingDriveUpstream struct {
	*omniDriveUpstream
	failMu     sync.Mutex
	failures   map[string]int
	beforeFail func(string)
}

func (upstream *failingDriveUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.failMu.Lock()
	fail := upstream.failures[request.AccountID] > 0
	if fail {
		upstream.failures[request.AccountID]--
	}
	hook := upstream.beforeFail
	upstream.failMu.Unlock()
	if !fail {
		return upstream.omniDriveUpstream.Generate(ctx, request)
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/fail")
	upstream.mu.Unlock()
	if hook != nil {
		hook(request.AccountID)
	}
	return nil, waaRuntimeFailure("GenerateContent")
}

// startOmniDriveRequest 在后台经 generateWithRetry 执行一次请求，返回结果通道
func startOmniDriveRequest(service *trackedService, request aistudio.GenerateRequest) <-chan resetTestResult {
	results := make(chan resetTestResult, 1)
	startedAt := time.Now()
	go func() {
		requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
		defer requestCancel()
		events := make(chan aistudio.Event, 8)
		done := make(chan struct{})
		go func() {
			defer close(done)
			service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events,
				newGenerationDiagnostics(request, "未发送"))
		}()
		var result resetTestResult
		for event := range events {
			switch event.Kind {
			case aistudio.EventText:
				result.text += event.Text
			case aistudio.EventError:
				result.err = event.Err
			}
		}
		<-done
		result.elapsed = time.Since(startedAt)
		results <- result
	}()
	return results
}

// TestOmniDriveExhaustedReturnsToDeferredAccount 号池里只有一个有 Drive 授权的账号：它的 Worker 失败、短等待内没关闭完就换号，
// 换号时 Drive 候选已用尽：回到这个账号，等 Worker 关闭结束后在它上面重放成功，不按“有 Drive 授权的账号都试过”的 429 结束
// （与普通请求换号没有可用账号时相同）
func TestOmniDriveExhaustedReturnsToDeferredAccount(t *testing.T) {
	base := newOmniDriveUpstream([]string{resetTestAlice, resetTestBob}, resetTestAlice)
	base.remember(resetTestAlice, aistudio.DriveStateAvailable)
	base.remember(resetTestBob, aistudio.DriveStateUnavailable)
	upstream := &failingDriveUpstream{omniDriveUpstream: base, failures: map[string]int{resetTestAlice: 1}}
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	for _, accountID := range []string{resetTestAlice, resetTestBob} {
		if err := service.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	holder := newSiblingHolder(service.pool)
	upstream.failMu.Lock()
	upstream.beforeFail = holder.hold
	upstream.failMu.Unlock()

	results := startOmniDriveRequest(service, newAPIVideoRequest("req-drive-deferred", omniDriveModel))
	sibling := holder.lease(t)
	deadline := time.Now().Add(5 * time.Second)
	for !progressLogged(service, "可能有 Drive 授权的账号本请求都试过，回到原账号等待 Worker 恢复后重放") {
		select {
		case result := <-results:
			t.Fatalf("Drive 候选用尽时应回到原账号等待，请求却结束了: text=%q err=%v", result.text, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Drive 候选用尽时应回到原账号等待 Worker 恢复")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	result := awaitResetTestResult(t, results, 10*time.Second)
	if result.err != nil || result.text != "ok" {
		t.Fatalf("text=%q err=%v，期望回到原账号重放成功", result.text, result.err)
	}
	calls, uploads, _ := base.snapshot()
	if !slices.Equal(calls, []string{resetTestAlice + "/fail", resetTestAlice}) || !slices.Equal(uploads, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("Generate=%v 上传=%v，期望在 alice 上失败后重放，不选记着 Drive 不可用的 bob", calls, uploads)
	}
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
}

// TestOmniDriveAvailableUnusableUsesUnknown 记着 Drive 可用的账号现在用不了（刚被标为需要登录，候选状态还没跟上）时，改在状态未知的账号里
// 选号（这里要冷启动它），在有 Drive 的账号上成功：不按选号失败结束、不冷却，也不在用不了的可用账号上反复重选（限时内返回）
func TestOmniDriveAvailableUnusableUsesUnknown(t *testing.T) {
	accounts := []string{"a-gone@example.com", "u-drive@example.com"}
	upstream := newOmniDriveUpstream(accounts, "u-drive@example.com")
	upstream.remember("a-gone@example.com", aistudio.DriveStateAvailable)
	service, pool := omniDriveService(t, upstream)
	if err := pool.MarkAuthRequired("a-gone@example.com", "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	// 状态未知的账号没有就绪的 Worker：不在“可用账号忙时先试一个”里试到它，只能经“可用账号用不了、改在状态未知的账号里选”取到
	service.workers.accounts["u-drive@example.com"].warm.Store(false)
	done := make(chan resetTestResult, 1)
	go func() {
		text, err, elapsed := runOmniDriveRequest(t, service, newAPIVideoRequest("req-available-unusable", omniDriveModel))
		done <- resetTestResult{text: text, err: err, elapsed: elapsed}
	}()
	var result resetTestResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("请求 10 秒内没有结束（在用不了的可用账号上反复重选）")
	}
	if result.err != nil || result.text != "ok" {
		t.Fatalf("text=%q err=%v，期望改在状态未知的账号上成功", result.text, result.err)
	}
	if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"u-drive@example.com"}) || !slices.Equal(uploads, calls) {
		t.Fatalf("Generate=%v 上传=%v，期望在状态未知、有 Drive 的账号上发送", calls, uploads)
	}
	for _, status := range pool.Status() {
		if len(status.Cooldowns) > 0 {
			t.Fatalf("账号 %s 不应冷却: %+v", status.ID, status.Cooldowns)
		}
	}
}

// driveNoteRecorder 为测试用生成服务：记录 NoteDriveAuthorized 的账号，不生成
type driveNoteRecorder struct {
	mu    sync.Mutex
	noted []string
}

func (recorder *driveNoteRecorder) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (recorder *driveNoteRecorder) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (recorder *driveNoteRecorder) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	return nil, errors.New("不生成")
}

func (recorder *driveNoteRecorder) NoteDriveAuthorized(accountID string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.noted = append(recorder.noted, accountID)
}

func (recorder *driveNoteRecorder) notedAccounts() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.noted)
}

// driveResultLogin 为测试用隔离浏览器：登录返回同一邮箱与可签名的认证状态，DriveError 为 Drive 授权的结果
type driveResultLogin struct {
	loginResultLogin
	driveError string
}

func (login *driveResultLogin) Login(ctx context.Context, request aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	result, err := login.loginResultLogin.Login(ctx, request)
	result.DriveError = login.driveError
	return result, err
}

// TestLoginNotesDriveAuthorized 经本服务的登录流程重新登录、完成了 Drive 授权时，保存新认证文件后把账号记为 Drive 可用
// （gemini-omni 的视频输入及无法转为 PNG 的图片随即优先交给它）；Drive 授权失败时不记；添加账户同样如此
func TestLoginNotesDriveAuthorized(t *testing.T) {
	admin, _, accounts := keepCatalogAdmin(t, "login-drive@example.com")
	accountID := accounts[0].ID
	recorder := &driveNoteRecorder{}
	admin.service.service = recorder
	login := &driveResultLogin{loginResultLogin: loginResultLogin{email: accountID, state: authTestState(t, "NEW", false)}, driveError: "GenerateAccessToken 返回 HTTP 401"}
	admin.login = login
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.LoginAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if noted := recorder.notedAccounts(); len(noted) != 0 {
		t.Fatalf("Drive 授权失败时不应记为可用: %v", noted)
	}
	login.driveError = ""
	if _, err := admin.LoginAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if noted := recorder.notedAccounts(); !slices.Equal(noted, []string{accountID}) {
		t.Fatalf("记为 Drive 可用的账号 = %v，期望 %s", noted, accountID)
	}

	// 添加账户：登录完成 Drive 授权、账户进入账户池之后同样记为可用
	isolateDisableNotes(t)
	creator, _, _ := enableTestAdmin(t, "existing@example.com")
	created := &driveNoteRecorder{}
	creator.service.service = created
	creator.login = &proxyRecordingLogin{email: "new-drive@example.com"}
	account, err := creator.CreateAccount(context.Background(), api.AccountCreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if noted := created.notedAccounts(); !slices.Equal(noted, []string{account.ID}) || account.ID != "new-drive@example.com" {
		t.Fatalf("记为 Drive 可用的账号 = %v，期望新账户 %s", noted, account.ID)
	}
}

// TestOmniDriveRestoredOnlyLinkFallbackEncodes 上一轮回复只有本服务的视频链接（恢复后 assistant 消息只剩视频）、号池里没有 Drive 授权的账号
// （omniDriveUpstream 按真实编码发送）：
// 协议层按多轮忽略了后缀任务（VideoConfig 不带任务），换回链接文本后按真实编码发送成功，不出 first-turn 的 400；请求里仍带着任务时
// （协议层之外构造的请求）按需要 Drive 授权返回，同样不是 first-turn 或缺少视频的 400
func TestOmniDriveRestoredOnlyLinkFallbackEncodes(t *testing.T) {
	accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com"}
	base := newOmniDriveUpstream(accounts)
	for _, accountID := range accounts {
		base.remember(accountID, aistudio.DriveStateUnavailable)
	}
	service, _ := omniDriveService(t, base)
	link := restoredMediaLink("video/mp4")
	request := func(id string, video *aistudio.VideoConfig) aistudio.GenerateRequest {
		return aistudio.GenerateRequest{
			ID: id, Model: omniDriveModel, Config: aistudio.GenerationConfig{VideoConfig: video},
			Contents: []aistudio.Content{
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个海边视频"}}},
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
					{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("video")}, RestoredLink: true, RestoredText: link},
				}},
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "把天空换成晚霞"}}},
			},
		}
	}
	text, err, _ := runOmniDriveRequest(t, service, request("req-link-only", &aistudio.VideoConfig{AspectRatio: "9:16"}))
	if err != nil || text != "ok" {
		t.Fatalf("换回链接文本后应按真实编码发送成功：text=%q err=%v", text, err)
	}
	base.mu.Lock()
	sent := base.contents[0][1].Parts
	base.mu.Unlock()
	if len(sent) != 1 || sent[0].Text != link {
		t.Fatalf("assistant 消息 = %+v，期望换回链接文本 %q", sent, link)
	}
	_, err, _ = runOmniDriveRequest(t, service, request("req-link-only-task", &aistudio.VideoConfig{Task: "edit"}))
	var driveRequired *aistudio.DriveRequiredError
	if !errors.As(err, &driveRequired) || strings.Contains(err.Error(), "first turn") || strings.Contains(err.Error(), "requires an input video") {
		t.Fatalf("err = %v，带任务时应按需要 Drive 授权返回", err)
	}
}
