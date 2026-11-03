package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// omniLivePNG 为线上排查用的 64×64 PNG（136 字节，与线上 curl 测试的图片相同）
const omniLivePNG = "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAT0lEQVR42u3PQQkAAAgEsItz/fMYxgi+hcEKLNO+FgEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQGBywLzk8EPlvGqjQAAAABJRU5ErkJggg=="

// omniMediaBase 为测试媒体链接的站点前缀
const omniMediaBase = "https://ai.example.com"

// omniProductionModel 为线上 Omni 目录项的形状（与 aistudio 的 omniCatalogRow 相同：能力码 1、25、52、59 与 field 79 Interaction 配置，
// 方法 countTokens、generateContent，列出视频选项），没有 function_declarations
func omniProductionModel() aistudio.Model {
	return aistudio.Model{
		ID: omniDriveModel, Name: "Gemini Omni 1.1 Flash", Methods: []string{"countTokens", "generateContent"},
		InputTokenLimit: 131072, OutputTokenLimit: 65536,
		Capabilities: map[string]bool{
			"chat_model": true, "thinking": true, "thinking_level": true, "interaction_route": true, "interactions_api": true,
			"capability_code_1": true, "capability_code_25": true, "capability_code_52": true, "capability_code_59": true,
		},
		CapabilityOptions: map[string][]string{
			"video_aspect_ratios":      {"16:9", "9:16"},
			"video_output_resolutions": {"360p", "720p", "1080p", "4k"},
			"video_duration_range":     {"min=4", "max=15", "step=1", "default=10"},
			"video_frame_rates":        {"24fps"},
			"video_tasks":              {"text_to_video", "image_to_video", "reference_to_video", "edit", "extend"},
		},
	}
}

// omniProductionState 为贴近线上的号池：count 个账号的 Worker 都已就绪，前两个有 Drive 授权且记着可用、最后两个状态未知、其余记着
// Drive 不可用；目录里有线上形状的 Omni 与普通文本模型，Build 目录也列出两者；按 fill-first 选号。channels 为上游通道，
// 含 Build 时 Build 生成全局暂停（官网停用了 Build）。omni 为 Omni 目录项（权益限制等由调用方改）
type omniProductionState struct {
	service  *trackedService
	pool     *aistudio.AccountPool
	upstream *omniDriveUpstream
	accounts []string
	media    *mediastore.Store
	handler  http.Handler
}

func newOmniProductionState(t *testing.T, count int, channels []aistudio.Channel, omni aistudio.Model) *omniProductionState {
	t.Helper()
	accounts := make([]string, 0, count)
	for index := range count {
		accounts = append(accounts, fmt.Sprintf("omni-%03d@example.com", index))
	}
	upstream := newOmniDriveUpstream(accounts, accounts[0], accounts[1])
	for index, accountID := range accounts {
		switch {
		case index < 2:
			upstream.remember(accountID, aistudio.DriveStateAvailable)
		case index < count-2:
			upstream.remember(accountID, aistudio.DriveStateUnavailable)
		}
	}
	service, pool := omniDriveService(t, upstream)
	text := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{omni, text}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}}, {ID: testRuntimeModel, Methods: []string{"generateContent"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetUpstreamChannels(channels)
	pool.SetRoutingStrategy("fill-first")
	service.replaceModelSnapshot([]aistudio.Model{omni, text})
	if slices.Contains(channels, aistudio.ChannelBuild) && slices.Contains(channels, aistudio.ChannelPlayground) {
		pauseBuildGlobally(t, service, accounts[0], accounts[1], accounts[2])
	}
	dir := filepath.Join(t.TempDir(), "media")
	handler := api.NewHandler(service, api.Config{
		APIKey: "sk-test", MediaDir: dir, MediaLinks: func() (bool, string) { return true, omniMediaBase },
	})
	return &omniProductionState{
		service: service, pool: pool, upstream: upstream, accounts: accounts, media: mediastore.New(dir), handler: handler,
	}
}

// post 经公开 API 发送请求（OpenAI 用 Authorization，Gemini 用 x-goog-api-key，与线上 curl 相同），返回响应与用时
func (state *omniProductionState) post(t *testing.T, path string, body string) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1beta/") {
		request.Header.Set("x-goog-api-key", "sk-test")
	} else {
		request.Header.Set("Authorization", "Bearer sk-test")
	}
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	state.handler.ServeHTTP(recorder, request)
	return recorder, time.Since(startedAt)
}

// progress 返回请求进度里包含 text 的条目
func (state *omniProductionState) progress(text string) []string {
	state.service.requests.mu.Lock()
	defer state.service.requests.mu.Unlock()
	var matched []string
	for _, entry := range state.service.requests.logs {
		if entry.Event == "request.progress" && strings.Contains(entry.Message, text) {
			matched = append(matched, entry.Message)
		}
	}
	return matched
}

// omniLiveRequests 为线上的四种单轮请求：只发文字（OpenAI），以及“文字 + 一张 PNG”的 OpenAI image_url data URI 与 Gemini 原生
// streamGenerateContent inlineData（与 new-api 转发的一样），另加一张 JPEG
func omniLiveRequests(t *testing.T) []struct{ name, path, body string } {
	_, jpegData, _ := omniTestImages(t)
	chatImage := func(mime, data string) string {
		return `{"model":"` + omniDriveModel + `","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"根据这张图生成视频"},` +
			`{"type":"image_url","image_url":{"url":"data:` + mime + `;base64,` + data + `"}}]}]}`
	}
	return []struct{ name, path, body string }{
		{name: "只发文字", path: "/v1/chat/completions",
			body: `{"model":"` + omniDriveModel + `","stream":true,"messages":[{"role":"user","content":"随机生成一个视频"}]}`},
		{name: "OpenAI 图片", path: "/v1/chat/completions", body: chatImage("image/png", omniLivePNG)},
		{name: "Gemini 图片", path: "/v1beta/models/" + omniDriveModel + ":streamGenerateContent?alt=sse",
			body: `{"contents":[{"role":"user","parts":[{"text":" 根据这张图生成视频"},{"inlineData":{"mimeType":"image/png","data":"` + omniLivePNG + `"}}]}]}`},
		{name: "OpenAI JPEG", path: "/v1/chat/completions", body: chatImage("image/jpeg", base64.StdEncoding.EncodeToString(jpegData))},
	}
}

// TestOmniImageOnlySelectsLikeTextInProduction 对照用例（基线 39ac8e0 上同样通过）：线上形状的号池（多数账号记着 Drive 不可用、两个可用、
// 两个未知，Build 生成全局暂停，Omni 目录项照线上形状）里，只带 PNG、JPEG 的 Omni 请求与只发文字的请求选号完全相同：都在 fill-first 的第一个
// 账号上成功，不查 Drive 状态、不上传、不换号，图片按官网内联发送，没有“选号没有候选”，各写一行同样的 Omni 选号诊断（临时）。选号条件与内容无关，
// 同一号池状态下文字与图片的结果相同，所以它不是线上“文字成功、图片失败”的复现：那次失败的根因尚未查明
func TestOmniImageOnlySelectsLikeTextInProduction(t *testing.T) {
	state := newOmniProductionState(t, 12, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
	for _, entry := range omniLiveRequests(t) {
		recorder, elapsed := state.post(t, entry.path, entry.body)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
			t.Fatalf("%s: 状态 %d（用时 %s）: %.400s", entry.name, recorder.Code, elapsed, recorder.Body.String())
		}
	}
	calls, uploads, candidates := state.upstream.snapshot()
	first := state.accounts[0]
	want := []string{first, first + "/inline", first + "/inline", first + "/inline"}
	if !slices.Equal(calls, want) || len(uploads) != 0 || candidates != 0 {
		t.Fatalf("Generate=%v 上传=%v Drive 候选查询=%d，期望文字与图片请求都在第一个账号上直接发送", calls, uploads, candidates)
	}
	if body := state.upstream.lastBody(t); !strings.Contains(body, `[null,[2,"`) {
		t.Fatalf("JPEG 应按枚举 2 内联发送: %.400s", body)
	}
	state.upstream.mu.Lock()
	pngBody := string(state.upstream.bodies[2])
	state.upstream.mu.Unlock()
	if !strings.Contains(pngBody, `[null,[1,"`+omniLivePNG+`"]]`) {
		t.Fatalf("Gemini inlineData 的 PNG 应按枚举 1 原字节内联发送: %.400s", pngBody)
	}
	if got := state.progress("Omni 图片按官网内联发送，不经 Drive | 图片=1 | 字节=136B"); len(got) != 2 {
		t.Fatalf("两次 PNG 请求都应记下按官网内联发送: %q", got)
	}
	if got := state.progress("选号没有候选"); len(got) != 0 {
		t.Fatalf("能选到账号时不应记没有候选: %q", got)
	}
	// 临时诊断：文字与图片请求都写一行 Omni 选号诊断，条件与候选集相同
	diagnostics := state.progress("Omni 选号诊断（临时） | 条件=无 | 限定=0 | 排除=0 | 候选索引=建立于")
	if len(diagnostics) != 4 {
		t.Fatalf("四次请求都应写 Omni 选号诊断: %q", diagnostics)
	}
	for _, line := range diagnostics {
		if !strings.Contains(line, "成员 12（可用 12、冷却 0）") {
			t.Fatalf("文字与图片请求的候选集应相同: %q", diagnostics)
		}
	}
	assertNoAccountCooldown(t, state.pool)
}

// get 经公开 API 发送 GET 请求，返回响应正文
func (state *omniProductionState) get(t *testing.T, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if strings.HasPrefix(path, "/v1beta/") {
		request.Header.Set("x-goog-api-key", "sk-test")
	} else {
		request.Header.Set("Authorization", "Bearer sk-test")
	}
	recorder := httptest.NewRecorder()
	state.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: 状态 %d: %s", path, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// TestOmniUnlistedNoCandidateExplained 号池目录列出了 Omni、这次选号却没有账号能接的两种长期状态：上游通道只启用 Build（Omni 不走 Build），
// 或 Omni 的权益限制（AccessModes）与账号权益不符。这两种状态下 GET /v1/models 与 /v1beta/models 都不列出 Omni，文字与图片请求同样立即失败
// （选号与内容无关，所以它们不是线上“文字成功、图片失败”的复现）。原先对外是官方措辞的 404 model_not_found，请求日志只有“没有符合条件的
// AI Studio 账户”；第一版改成 503 “is listed … try again later”，与模型列表不符、还会让中转反复重试。现在按 404 NOT_FOUND 返回写明原因类别的
// 英文说明（权益、启用的上游通道），不说稍后重试、不带 Retry-After；请求进度写一行核对摘要。不调用 Generate，不冷却
func TestOmniUnlistedNoCandidateExplained(t *testing.T) {
	proOnly := omniProductionModel()
	proOnly.AccessModes = []int64{3}
	for _, test := range []struct {
		name     string
		channels []aistudio.Channel
		omni     aistudio.Model
		reason   string
		summary  []string
	}{
		{name: "上游通道只有 Build", channels: []aistudio.Channel{aistudio.ChannelBuild}, omni: omniProductionModel(),
			reason:  "the enabled upstream channels (build) cannot serve this model",
			summary: []string{"模型=" + omniDriveModel, "可用通道=build", "目录列出=6", "所选通道不支持=6", "可接=0", "模型列表=不列出", "Playground=未启用"}},
		{name: "权益不符", channels: []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omni: proOnly,
			reason:  "no account in this pool has a subscription tier that allows this model",
			summary: []string{"可用通道=playground", "目录列出=6", "权益不符=6", "可接=0", "模型列表=不列出", "Build=启用、全局暂停"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newOmniProductionState(t, 6, test.channels, test.omni)
			for _, path := range []string{"/v1/models", "/v1beta/models"} {
				if body := state.get(t, path); strings.Contains(body, omniDriveModel) || !strings.Contains(body, testRuntimeModel) {
					t.Fatalf("%s 不应列出 %s: %.400s", path, omniDriveModel, body)
				}
			}
			want := "models/" + omniDriveModel + " is not available to the accounts in this pool: " + test.reason + ". Call ListModels to see the list of available models."
			var bodies []string
			for _, entry := range omniLiveRequests(t)[:3] {
				recorder, elapsed := state.post(t, entry.path, entry.body)
				body := recorder.Body.String()
				if recorder.Code != http.StatusNotFound || elapsed > time.Second || !strings.Contains(body, want) ||
					strings.Contains(body, "is listed") || strings.Contains(body, "try again later") || recorder.Header().Get("Retry-After") != "" {
					t.Fatalf("%s: 状态 %d（用时 %s）Retry-After=%q: %s，期望立即 404 与写明原因的说明", entry.name, recorder.Code, elapsed,
						recorder.Header().Get("Retry-After"), body)
				}
				if entry.path == "/v1/chat/completions" {
					if !strings.Contains(body, `"code":"model_not_found"`) {
						t.Fatalf("%s: OpenAI code 应为 model_not_found: %s", entry.name, body)
					}
					bodies = append(bodies, body)
				} else if !strings.Contains(body, `"status":"NOT_FOUND"`) {
					t.Fatalf("%s: Gemini status 应为 NOT_FOUND: %s", entry.name, body)
				}
			}
			if len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatalf("文字与图片请求的对外结果应相同: %q", bodies)
			}
			logged := state.progress("选号没有候选：模型不在这个号池的模型列表里，按 404 返回")
			if len(logged) != 3 {
				t.Fatalf("三次请求都应写核对摘要: %q", logged)
			}
			for _, line := range logged {
				for _, fragment := range test.summary {
					if !strings.Contains(line, fragment) {
						t.Fatalf("核对摘要 %q 缺少 %q", line, fragment)
					}
				}
			}
			if got := state.progress("候选索引与账户状态不一致"); len(got) != 0 {
				t.Fatalf("长期状态不应重试选号: %q", got)
			}
			// 指定账户的选号不核对、不包装：照原来的无账户错误返回（不说模型在列，也不说稍后重试）
			events, err := state.service.Generate(context.Background(), aistudio.GenerateRequest{
				ID: "pinned-" + test.name, Model: omniDriveModel, AccountID: state.accounts[0], Stream: true,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "随机生成一个视频"}}}},
			})
			final := err
			for event := range events {
				if event.Kind == aistudio.EventError {
					final = event.Err
				}
			}
			var noServing *aistudio.NoServingAccountError
			if !aistudio.NoCandidateFailure(final) || errors.As(final, &noServing) || len(state.progress("选号没有候选")) != 3 {
				t.Fatalf("指定账户的选号应照原来的无账户错误返回: %v", final)
			}
			if calls, uploads, _ := state.upstream.snapshot(); len(calls)+len(uploads) != 0 {
				t.Fatalf("Generate=%v 上传=%v，没有候选时不应发送", calls, uploads)
			}
			assertNoAccountCooldown(t, state.pool)
		})
	}
}

// withSelectionFailure 在测试期间让 generateWithRetry 的选号按 fail 的结果失败（返回 nil 时照常选号），返回钩子被调用的次数
func withSelectionFailure(t *testing.T, fail func(aistudio.AccountSelection) error) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	hook := func(selection aistudio.AccountSelection) error {
		calls.Add(1)
		return fail(selection)
	}
	selectionFailureHook.Store(&hook)
	t.Cleanup(func() { selectionFailureHook.Store(nil) })
	return calls
}

// failOnce 返回只让 model 的前 times 次选号以裸的“没有候选”失败的钩子
func failOnce(model string, times int32) func(aistudio.AccountSelection) error {
	var failed atomic.Int32
	return func(selection aistudio.AccountSelection) error {
		if selection.ModelID == model && failed.Add(1) <= times {
			return aistudio.ErrNoEligibleAccount
		}
		return nil
	}
}

// TestOmniNoCandidateRecheckRetriesOnce 选号没有候选、逐个账户核对却还有候选时重试一次选号。这是兜底，不是线上“文字成功、图片失败”的复现：
// 线上日志的形状（同一秒内只有“请求开始”“Omni 图片按官网内联发送”“请求失败 404 没有符合条件的 AI Studio 账户”）在选号条件与内容无关的前提下
// 只能来自号池一侧的状态，其中“候选索引与账户状态不一致”是假设（没有找到会造成它的代码路径，aistudio 包内用人为清空候选集的测试覆盖作废与重建），
// 这里用包内测试钩子让第一次选号以裸的没有候选失败（账户池与候选索引不动，相当于核对前账户状态刚变化）。修复前同一秒内 404 model_not_found；
// 现在核对时逐个账户还有候选，按 ERROR 写一行核对摘要、重试一次选号（不占换号次数），请求在第一个账号上成功，成功时另写一行 Omni 选号诊断；
// 同样的情况下文字请求同样恢复（缓存期内沿用核对结果）；没有失败时不重试、不写摘要
func TestOmniNoCandidateRecheckRetriesOnce(t *testing.T) {
	state := newOmniProductionState(t, 12, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
	requests := omniLiveRequests(t)
	text, image := requests[0], requests[2]
	if recorder, _ := state.post(t, text.path, text.body); recorder.Code != http.StatusOK {
		t.Fatalf("文字请求: 状态 %d: %.400s", recorder.Code, recorder.Body.String())
	}
	state.pool.WaitBackground()
	calls := withSelectionFailure(t, failOnce(omniDriveModel, 1))
	recorder, elapsed := state.post(t, image.path, image.body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") || calls.Load() != 2 {
		t.Fatalf("选号没有候选、逐个账户核对还有时带图请求: 状态 %d（用时 %s，选号 %d 次）: %.400s，期望重试一次后成功",
			recorder.Code, elapsed, calls.Load(), recorder.Body.String())
	}
	logged := state.progress("选号没有候选，逐个账户核对却还有候选（候选索引与账户状态不一致，或核对前账户状态刚变化），重试一次选号")
	if len(logged) != 1 {
		t.Fatalf("应按 ERROR 写一行核对摘要: %q", logged)
	}
	for _, fragment := range []string{
		"模型=" + omniDriveModel, "可用通道=playground", "目录列出=12", "可接=12", "模型列表=列出", "核对候选=索引 12、逐个账户 12",
		"成员 12（可用 12、冷却 0）", "Build=启用、全局暂停",
	} {
		if !strings.Contains(logged[0], fragment) {
			t.Fatalf("核对摘要 %q 缺少 %q", logged[0], fragment)
		}
	}
	if strings.Contains(logged[0], "已作废候选索引") {
		t.Fatalf("候选索引有候选时不应作废: %q", logged[0])
	}
	state.service.requests.mu.Lock()
	levels := 0
	for _, entry := range state.service.requests.logs {
		if entry.Event == "request.progress" && entry.Level == "ERROR" && strings.Contains(entry.Message, "逐个账户核对却还有候选") {
			levels++
		}
	}
	state.service.requests.mu.Unlock()
	if levels != 1 {
		t.Fatalf("核对摘要应为 ERROR 级别，得到 %d 条", levels)
	}
	if got := state.progress("选号没有候选："); len(got) != 0 {
		t.Fatalf("重试成功时不应按没有候选结束: %q", got)
	}
	if got := state.progress("Omni 选号诊断（临时） | 条件=无 | 限定=0 | 排除=0 | 候选索引=建立于"); len(got) != 2 ||
		!strings.Contains(got[0], "成员 12（可用 12、冷却 0）") || !strings.Contains(got[1], "成员 12（可用 12、冷却 0）") {
		t.Fatalf("文字请求与重试取得租约的带图请求都应写一行同样的 Omni 选号诊断: %q", got)
	}

	// 文字请求在同样的情况下同样恢复（缓存期内沿用核对结果）
	state.pool.WaitBackground()
	withSelectionFailure(t, failOnce(omniDriveModel, 1))
	recorder, _ = state.post(t, text.path, text.body)
	retried := state.progress("逐个账户核对却还有候选")
	if recorder.Code != http.StatusOK || len(retried) != 2 || !strings.Contains(retried[1], "沿用") {
		t.Fatalf("同样情况下文字请求: 状态 %d: %.400s，核对摘要 %q", recorder.Code, recorder.Body.String(), retried)
	}
	// 没有失败时不重试、不写摘要
	selectionFailureHook.Store(nil)
	recorder, _ = state.post(t, image.path, image.body)
	if recorder.Code != http.StatusOK || len(state.progress("逐个账户核对却还有候选")) != 2 {
		t.Fatalf("没有失败时带图请求: 状态 %d，核对摘要 %q", recorder.Code, state.progress("逐个账户核对却还有候选"))
	}
	calls2, uploads, _ := state.upstream.snapshot()
	first := state.accounts[0]
	if want := []string{first, first + "/inline", first, first + "/inline"}; !slices.Equal(calls2, want) || len(uploads) != 0 {
		t.Fatalf("Generate=%v 上传=%v，期望都在第一个账号上直接发送", calls2, uploads)
	}
	if got := state.progress("Omni 选号诊断（临时）"); len(got) != 4 {
		t.Fatalf("四次 Omni 请求（文字与图片）都应写选号诊断: %q", got)
	}
	assertNoAccountCooldown(t, state.pool)
}

// noEligibleUpstream 是测试用的上游：每次生成都返回裸的“没有符合条件的账户”（发送前的租约校验不通过时就是这样），记录账号与通道
type noEligibleUpstream struct {
	channelRecorder
}

func (upstream *noEligibleUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	upstream.mu.Unlock()
	return nil, aistudio.ErrNoEligibleAccount
}

// TestNoCandidateRecheckKeepsOneShotRetries 选号没有候选后的核对与重试不打乱这次尝试的一次性条件，也不拿旧的核对结果包装后面的错误：
//   - 输入超过账号上限后改用更高权益账号再试（限定列表、只取立即可用的账号），这次选号没有候选：不重试（重试会按普通条件落到同样超限的
//     低权益账号上），按输入超限的原错误结束，只发过原来的两次；
//   - 第一次选号没有候选、重试取得租约，发送却以满足 NoCandidateFailure 的错误结束（发送前的租约校验）：照原来的错误返回，不按第一次的
//     核对结果包装成“模型在列、暂时没有账户能接”
func TestNoCandidateRecheckKeepsOneShotRetries(t *testing.T) {
	t.Run("更高权益账号的再试没有候选", func(t *testing.T) {
		upstream := &inputLimitUpstream{
			failing: map[string]bool{inputLimitA: true, inputLimitB: true}, status: http.StatusTooManyRequests, code: 8, message: inputCapMessage,
		}
		service, _ := inputLimitService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, map[string]inputLimitAccount{
			inputLimitA: {tier: aistudio.BenefitTierFree, playground: true, build: true},
			inputLimitB: {tier: aistudio.BenefitTierFree, playground: true},
			inputLimitU: {tier: aistudio.BenefitTierUltra, playground: true, build: true},
		})
		calls := withSelectionFailure(t, func(selection aistudio.AccountSelection) error {
			if selection.ModelID == streamTestModel && selection.AllowedAccountIDs != nil {
				return aistudio.ErrNoEligibleAccount
			}
			return nil
		})
		_, elapsed, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, service.lifecycle, true, nil)
		if !aistudio.InputTokenLimitError(err) {
			t.Fatalf("err = %v，期望按输入超限的原错误结束", err)
		}
		if got := upstream.called(); !slices.Equal(got, []string{inputLimitA + "/playground", inputLimitA + "/build"}) || calls.Load() != 3 {
			t.Fatalf("尝试 = %v（选号 %d 次），期望只发原来的两次、更高权益账号的再试没有候选时不重试", got, calls.Load())
		}
		if !progressLogged(service, "改用更高权益的账号再试一次") || !progressLogged(service, "输入超限后的再试没有立即可用的账号，按 400 返回") ||
			progressLogged(service, "逐个账户核对却还有候选") || progressLogged(service, "选号没有候选：") {
			t.Fatal("应按输入超限结束，不重试选号、不按没有候选结束")
		}
		if elapsed > 3*time.Second {
			t.Fatalf("用时 %s，输入超限不应排队等待", elapsed)
		}
	})
	t.Run("重试取得租约后以没有账户的错误结束", func(t *testing.T) {
		upstream := &noEligibleUpstream{}
		service, pool := firstEventTestService(t, upstream, 0, streamPlaygroundA, streamPlaygroundB)
		target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
		for _, accountID := range []string{streamPlaygroundA, streamPlaygroundB} {
			if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{target}); err != nil {
				t.Fatal(err)
			}
		}
		withSelectionFailure(t, failOnce(streamTestModel, 1))
		_, _, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, service.lifecycle, false, nil)
		var noServing *aistudio.NoServingAccountError
		if !errors.Is(err, aistudio.ErrNoEligibleAccount) || errors.As(err, &noServing) {
			t.Fatalf("err = %v，期望照原来的无账户错误返回，不按第一次选号的核对结果包装", err)
		}
		if len(upstream.called()) != 1 || !progressLogged(service, "逐个账户核对却还有候选") || progressLogged(service, "选号没有候选：") {
			t.Fatalf("尝试 = %v，期望重试一次选号后发送一次、不写没有候选的结束摘要", upstream.called())
		}
	})
}

// TestNoCandidateReportSkippedWhenSwitchExhausted 换号用尽（能接这个模型的账号都试过、都失败）后的选号没有候选：请求按上一次尝试的错误结束，
// 核对结果用不上，不调用 NoCandidateReport（原先每个换号用尽的请求都在账户池锁内遍历全部账户、逐个账户核对一遍，结果随后被丢掉）。
// 用核对结果的缓存判断：请求结束后同一选择条件的核对是新做的（没有沿用）
func TestNoCandidateReportSkippedWhenSwitchExhausted(t *testing.T) {
	upstream := &inputLimitUpstream{
		failing: map[string]bool{inputLimitA: true, inputLimitB: true}, status: http.StatusInternalServerError, code: 13, message: "internal error",
	}
	other := "limit-c@example.com"
	service, pool := inputLimitService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground}, map[string]inputLimitAccount{
		inputLimitA: {tier: aistudio.BenefitTierFree, playground: true},
		inputLimitB: {tier: aistudio.BenefitTierFree, playground: true},
		other:       {tier: aistudio.BenefitTierFree},
		inputLimitU: {tier: aistudio.BenefitTierUltra},
	})
	_, _, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, service.lifecycle, false, nil)
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v，期望按上一次尝试的上游错误结束", err)
	}
	if got := upstream.called(); len(got) != 2 {
		t.Fatalf("尝试 = %v，期望两个能接的账号各试一次", got)
	}
	report := pool.NoCandidateReport(aistudio.AccountSelection{
		ModelID: streamTestModel, Method: "generateContent", ExcludedAccountIDs: []string{inputLimitB, inputLimitA},
	})
	if report.CachedAge != 0 || report.Excluded != 2 || report.Recheckable() {
		t.Fatalf("核对 = %+v，换号用尽的选号不应核对（应没有缓存的核对结果）", report)
	}
	if progressLogged(service, "选号没有候选") {
		t.Fatal("换号用尽不应写没有候选的摘要")
	}
}

// TestOmniMultiTurnMediaLinkImageTools new-api 经 Gemini 渠道转来的多轮请求：历史里上一轮的模型回复只剩本服务 /media/ 视频链接，
// 这一轮带一张 PNG，请求还带 new-api 的 save_memory、delete_memory 两个函数工具。原先函数声明在发送前按 Omni 目录校验（没有
// function_declarations），中文原因被换成通用措辞，对外 400 “Request contains an invalid argument.”。现在 Omni 请求去掉工具并在请求进度里
// 记一行，按普通请求成功：本地媒体文件还在时历史视频恢复为媒体、交给有 Drive 授权的账号上传后以文件引用发送；文件已清理时按链接文本发送
// （记一行没能恢复）。两种情况下这一轮的 PNG 都按官网内联发送；别的模型的函数工具不受影响
func TestOmniMultiTurnMediaLinkImageTools(t *testing.T) {
	video := []byte("\x00\x00\x00\x18ftypmp42omni-history-video")
	request := func(link string) string {
		return `{"contents":[` +
			`{"role":"user","parts":[{"text":"生成一个海边视频"}]},` +
			`{"role":"model","parts":[{"text":"好的：\n[video](` + link + `)\n"}]},` +
			`{"role":"user","parts":[{"text":"参考这张图把视频改成夜景"},{"inlineData":{"mimeType":"image/png","data":"` + omniLivePNG + `"}}]}],` +
			`"tools":[{"functionDeclarations":[` +
			`{"name":"save_memory","description":"保存记忆","parameters":{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}},` +
			`{"name":"delete_memory","description":"删除记忆","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}]}]}`
	}
	path := "/v1beta/models/" + omniDriveModel + ":streamGenerateContent?alt=sse"
	toolsNote := "CreateInteractionStream 不带工具，已忽略 2 个函数声明与 0 个 Google 工具"
	for _, test := range []struct {
		name  string
		saved bool
	}{
		{name: "本地媒体文件还在", saved: true},
		{name: "本地媒体文件已清理", saved: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newOmniProductionState(t, 8, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
			link := omniMediaBase + "/media/" + strings.Repeat("ef", 32) + ".mp4"
			if test.saved {
				name, err := state.media.Save(video, "video/mp4")
				if err != nil {
					t.Fatal(err)
				}
				link = omniMediaBase + "/media/" + name
			}
			recorder, _ := state.post(t, path, request(link))
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
				t.Fatalf("状态 %d: %.400s", recorder.Code, recorder.Body.String())
			}
			calls, uploads, _ := state.upstream.snapshot()
			state.upstream.mu.Lock()
			sentTools := slices.Clone(state.upstream.tools)
			uploadMIMEs := slices.Clone(state.upstream.uploadMIMEs)
			state.upstream.mu.Unlock()
			if len(calls) != 1 || len(sentTools) != 1 || len(sentTools[0].Functions) != 0 {
				t.Fatalf("Generate=%v 工具=%+v，期望发送一次、不带函数工具", calls, sentTools)
			}
			if got := state.progress(toolsNote); len(got) != 1 {
				t.Fatalf("应记一行忽略的工具: %q", got)
			}
			body := state.upstream.lastBody(t)
			if !strings.Contains(body, `[null,[1,"`+omniLivePNG+`"]]`) {
				t.Fatalf("这一轮的 PNG 应按官网内联发送: %.600s", body)
			}
			if test.saved {
				// 恢复出的历史视频只因回传历史需要 Drive：交给记着 Drive 可用的账号上传（图片不上传），以文件引用发送
				if !slices.Equal(calls, []string{state.accounts[0] + "/inline"}) || !slices.Equal(uploads, []string{state.accounts[0]}) ||
					len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"video/mp4"}) ||
					!strings.Contains(body, `["drive-`+state.accounts[0]+`"]`) {
					t.Fatalf("Generate=%v 上传=%v（%v），期望历史视频在有 Drive 授权的账号上传后以文件引用发送: %.600s", calls, uploads, uploadMIMEs, body)
				}
				return
			}
			if len(uploads) != 0 || !strings.Contains(body, "[video]("+link+")") ||
				len(state.progress("回传历史里本服务的媒体链接没能恢复为媒体输入")) != 1 {
				t.Fatalf("上传=%v，文件已清理时应按链接文本发送并记一行没能恢复: %.600s", uploads, body)
			}
		})
	}

	// 别的模型的函数工具照常交给生成服务
	state := newOmniProductionState(t, 4, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
	recorder, _ := state.post(t, "/v1beta/models/"+testRuntimeModel+":generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"save_memory","parameters":{"type":"object"}}]}]}`)
	state.upstream.mu.Lock()
	sentTools := slices.Clone(state.upstream.tools)
	state.upstream.mu.Unlock()
	if recorder.Code != http.StatusOK || len(sentTools) != 1 || len(sentTools[0].Functions) != 1 || len(state.progress("CreateInteractionStream 不带工具")) != 0 {
		t.Fatalf("其他模型的工具不应被去掉: 状态 %d 工具=%+v", recorder.Code, sentTools)
	}
	if got := state.progress("Omni 选号诊断"); len(got) != 0 {
		t.Fatalf("其他模型不写 Omni 选号诊断: %q", got)
	}
}

// TestOmniMultiTurnToolHistoryImage 同一段对话先用别的模型调用过 save_memory（Chatbox 等每次都带 save_memory、delete_memory），再切到 Omni 并带
// 一张 PNG：经 new-api 的 Gemini 渠道时历史里有 functionCall 与 functionResponse，OpenAI 格式时有 tool_calls 与 role=tool 的消息。原先只去掉
// 了工具声明，两种格式都已选到账号，却在编码时按 400 “contents[1]: this model only accepts text, image and video parts” 结束。现在历史里的
// 调用与结果改为文本（写明函数名与 JSON），tool 消息改为用户消息，请求进度记一行；请求发送一次并成功，这一轮的 PNG 照常内联，不带函数工具
func TestOmniMultiTurnToolHistoryImage(t *testing.T) {
	functions := `{"name":"save_memory","description":"保存记忆","parameters":{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}},` +
		`{"name":"delete_memory","description":"删除记忆","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}`
	for _, test := range []struct {
		name, path, body string
		result           string
	}{
		{
			name: "Gemini（new-api 转发）", path: "/v1beta/models/" + omniDriveModel + ":streamGenerateContent?alt=sse",
			body: `{"contents":[` +
				`{"role":"user","parts":[{"text":"记住我喜欢猫"}]},` +
				`{"role":"model","parts":[{"functionCall":{"name":"save_memory","args":{"content":"喜欢猫"}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"save_memory","response":{"result":"saved"}}}]},` +
				`{"role":"model","parts":[{"text":"好的，记住了"}]},` +
				`{"role":"user","parts":[{"text":"根据这张图生成视频"},{"inlineData":{"mimeType":"image/png","data":"` + omniLivePNG + `"}}]}],` +
				`"tools":[{"functionDeclarations":[` + functions + `]}]}`,
			result: `Tool result save_memory: {\"result\":\"saved\"}`,
		},
		{
			name: "OpenAI", path: "/v1/chat/completions",
			body: `{"model":"` + omniDriveModel + `","stream":true,"messages":[` +
				`{"role":"user","content":"记住我喜欢猫"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"save_memory","arguments":"{\"content\":\"喜欢猫\"}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"saved"},` +
				`{"role":"assistant","content":"好的，记住了"},` +
				`{"role":"user","content":[{"type":"text","text":"根据这张图生成视频"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + omniLivePNG + `"}}]}],` +
				`"tools":[{"type":"function","function":` + strings.Replace(functions, `},{"name"`, `}},{"type":"function","function":{"name"`, 1) + `}]}`,
			result: `Tool result save_memory: {\"result\":\"saved\"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newOmniProductionState(t, 8, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
			recorder, _ := state.post(t, test.path, test.body)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
				t.Fatalf("状态 %d: %.600s", recorder.Code, recorder.Body.String())
			}
			calls, uploads, _ := state.upstream.snapshot()
			state.upstream.mu.Lock()
			sentTools := slices.Clone(state.upstream.tools)
			sent := slices.Clone(state.upstream.contents)
			state.upstream.mu.Unlock()
			if !slices.Equal(calls, []string{state.accounts[0] + "/inline"}) || len(uploads) != 0 || len(sentTools) != 1 || len(sentTools[0].Functions) != 0 {
				t.Fatalf("Generate=%v 上传=%v 工具=%+v，期望在第一个账号上发送一次、不带函数工具", calls, uploads, sentTools)
			}
			for _, content := range sent[0] {
				if content.Role == aistudio.RoleTool {
					t.Fatalf("发送的内容不应有 tool 消息: %+v", sent[0])
				}
				for _, part := range content.Parts {
					if part.FunctionCall != nil || part.FunctionResult != nil {
						t.Fatalf("发送的内容不应有函数调用或结果: %+v", sent[0])
					}
				}
			}
			body := state.upstream.lastBody(t)
			for _, fragment := range []string{
				`Tool call save_memory: {\"content\":\"喜欢猫\"}`, test.result, "好的，记住了", `[null,[1,"` + omniLivePNG + `"]]`,
			} {
				if !strings.Contains(body, fragment) {
					t.Fatalf("请求体缺少 %q: %.900s", fragment, body)
				}
			}
			if got := state.progress("Omni 模型不调用工具：对话历史里的 2 个函数调用、函数结果与代码执行已改为文本发送"); len(got) != 1 {
				t.Fatalf("应记一行历史改为文本: %q", got)
			}
			if got := state.progress("CreateInteractionStream 不带工具，已忽略 2 个函数声明与 0 个 Google 工具"); len(got) != 1 {
				t.Fatalf("应记一行忽略的工具: %q", got)
			}
		})
	}
}

// TestOmniVideoStillNeedsDriveInProduction 线上形状的号池里，客户端自己带视频（与 PNG）的 Omni 请求仍按 Drive 规则选号：只在记着 Drive
// 可用的账号上发送，视频上传、图片不上传；号池里只剩记着 Drive 不可用的账号时按 400 FAILED_PRECONDITION 结束（不是没有候选的 503）
func TestOmniVideoStillNeedsDriveInProduction(t *testing.T) {
	state := newOmniProductionState(t, 8, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}, omniProductionModel())
	videoData := base64.StdEncoding.EncodeToString([]byte("\x00\x00\x00\x18ftypmp42client-video"))
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + videoData + `"}},` +
		`{"inlineData":{"mimeType":"image/png","data":"` + omniLivePNG + `"}},{"text":"按这张图延续视频"}]}]}`
	path := "/v1beta/models/" + omniDriveModel + ":streamGenerateContent?alt=sse"
	recorder, _ := state.post(t, path, body)
	calls, uploads, candidates := state.upstream.snapshot()
	state.upstream.mu.Lock()
	uploadMIMEs := slices.Clone(state.upstream.uploadMIMEs)
	state.upstream.mu.Unlock()
	if recorder.Code != http.StatusOK || !slices.Equal(calls, []string{state.accounts[0] + "/inline"}) || !slices.Equal(uploads, []string{state.accounts[0]}) ||
		candidates == 0 || len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"video/mp4"}) {
		t.Fatalf("状态 %d Generate=%v 上传=%v（%v）候选查询=%d，期望视频交给记着 Drive 可用的账号上传、图片不上传", recorder.Code, calls, uploads, uploadMIMEs, candidates)
	}

	// 号池里只剩记着 Drive 不可用的账号
	for _, accountID := range state.accounts {
		state.upstream.remember(accountID, aistudio.DriveStateUnavailable)
	}
	recorder, _ = state.post(t, path, body)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "FAILED_PRECONDITION") ||
		!strings.Contains(recorder.Body.String(), "requires an account with Google Drive authorized") {
		t.Fatalf("状态 %d: %s，期望 400 FAILED_PRECONDITION 与需要 Drive 授权的说明", recorder.Code, recorder.Body.String())
	}
	if got := state.progress("选号没有候选"); len(got) != 0 {
		t.Fatalf("需要 Drive 的结束不应记成没有候选: %q", got)
	}
	if calls, _, _ := state.upstream.snapshot(); len(calls) != 1 {
		t.Fatalf("Generate=%v，没有 Drive 授权的账号时不应发送", calls)
	}
}
