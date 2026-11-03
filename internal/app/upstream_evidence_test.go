package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// omniTestBMP 返回 1×1 的 24 位 BMP（BITMAPFILEHEADER 加 40 字节的 BITMAPINFOHEADER 与一个像素）
func omniTestBMP() []byte {
	data := []byte{'B', 'M', 58, 0, 0, 0, 0, 0, 0, 0, 54, 0, 0, 0, 40, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 24, 0}
	data = append(data, make([]byte, 24)...)
	return append(data, 0xff, 0x00, 0x00, 0x00)
}

// TestOmniInlineImageFormatsSkipDrive 官网枚举表里的其他图片格式（WebP 3、HEIC 4、HEIF 5、BMP 8、TIFF 9）与 PNG、JPEG 一样按官网原字节内联：
// 号池里全部账号记着 Drive 不可用时也在第一个选到的账号上成功，不查询 Drive 状态、不调用上传、不换号、不冷却，请求体为 Content field 2
// [枚举, Base64]；MIME 为空或 octet-stream 的附件按文件头识别为图片同样如此；请求进度记下格式。同一请求里另有视频时照常按 Drive 选号、
// 只上传视频，WebP 在原位置内联
func TestOmniInlineImageFormatsSkipDrive(t *testing.T) {
	pngData, jpegData, _ := omniTestImages(t)
	encode := base64.StdEncoding.EncodeToString
	tiff := []byte("II*\x00\x08\x00\x00\x00tiff-data")
	heif := []byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1miaf\x00\x00\x00\x10meta-data-here")
	accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com"}
	for _, test := range []struct {
		name   string
		blob   aistudio.Blob
		enum   int
		format string
	}{
		{name: "WebP", blob: aistudio.Blob{MIME: "image/webp", Data: omniTestWebP}, enum: 3, format: "WebP"},
		{name: "HEIC", blob: aistudio.Blob{MIME: "image/heic", Data: omniTestHEIC}, enum: 4, format: "HEIC"},
		{name: "HEIF", blob: aistudio.Blob{MIME: "image/heif", Data: heif}, enum: 5, format: "HEIF"},
		{name: "BMP", blob: aistudio.Blob{MIME: "image/bmp", Data: omniTestBMP()}, enum: 8, format: "BMP"},
		{name: "TIFF", blob: aistudio.Blob{MIME: "image/tiff", Data: tiff}, enum: 9, format: "TIFF"},
		{name: "octet-stream 的 PNG", blob: aistudio.Blob{MIME: "application/octet-stream", Data: pngData}, enum: 1, format: "PNG"},
		{name: "空 MIME 的 JPEG", blob: aistudio.Blob{Data: jpegData}, enum: 2, format: "JPEG"},
		{name: "octet-stream 的 WebP", blob: aistudio.Blob{MIME: "application/octet-stream", Data: omniTestWebP}, enum: 3, format: "WebP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newOmniDriveUpstream(accounts)
			for _, accountID := range accounts {
				upstream.remember(accountID, aistudio.DriveStateUnavailable)
			}
			service, pool := omniDriveService(t, upstream)
			blob := test.blob
			text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
				ID: "req-inline-format", Model: omniDriveModel,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{InlineData: &blob}, {Text: "随机视频"}}}},
			})
			if err != nil || text != "ok" {
				t.Fatalf("应在没有 Drive 授权的账号上成功：text=%q err=%v", text, err)
			}
			calls, uploads, candidates := upstream.snapshot()
			if len(calls) != 1 || !strings.HasSuffix(calls[0], "/inline") || len(uploads) != 0 || candidates != 0 {
				t.Fatalf("Generate=%v 上传=%v 候选查询=%d，期望直接内联发送、不上传、不查 Drive 状态", calls, uploads, candidates)
			}
			want := omniTestCaptureBody(fmt.Sprintf(`[null,[%d,"%s"]],[["随机视频"]]`, test.enum, encode(test.blob.Data)), `[null,null,null,null,null,1]`)
			if body := upstream.lastBody(t); body != want {
				t.Fatalf("请求体 = %.300s\n期望 %.300s", body, want)
			}
			if !progressLogged(service, "Omni 图片按官网内联发送，不经 Drive | 图片=1 | 字节="+strconv.Itoa(len(test.blob.Data))+"B | 格式="+test.format+" 1") {
				t.Fatalf("请求进度应记下内联的图片与格式: %v", serviceLogMessages(service.requests))
			}
			if progressLogged(service, "Drive 授权") || progressLogged(service, "换号") {
				t.Fatal("只有这些图片的请求不应按 Drive 授权选号或换号")
			}
			assertNoAccountCooldown(t, pool)
		})
	}

	t.Run("WebP 与视频", func(t *testing.T) {
		withDrive := []string{"a-nodrive@example.com", "z-drive@example.com"}
		upstream := newOmniDriveUpstream(withDrive, "z-drive@example.com")
		upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
		upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
		service, _ := omniDriveService(t, upstream)
		text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
			ID: "req-webp-video", Model: omniDriveModel,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
				{InlineData: &aistudio.Blob{MIME: "image/webp", Data: omniTestWebP}},
				{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypisom\x00\x00\x00\x00isommp42")}},
				{Text: "参考视频"},
			}}},
		})
		if err != nil || text != "ok" {
			t.Fatalf("text=%q err=%v", text, err)
		}
		_, _, candidates := upstream.snapshot()
		upstream.mu.Lock()
		uploadMIMEs := slices.Clone(upstream.uploadMIMEs)
		upstream.mu.Unlock()
		if candidates == 0 || len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"video/mp4"}) {
			t.Fatalf("候选查询 %d 次，上传 %v，期望视频照官网先传 Drive（按 Drive 状态选号）、只上传视频", candidates, uploadMIMEs)
		}
		want := `[[[[[[null,[3,"` + encode(omniTestWebP) + `"]],[null,null,null,null,null,null,null,null,["drive-z-drive@example.com"]],[["参考视频"]]]]]]]`
		if body := upstream.lastBody(t); !strings.Contains(body, want) {
			t.Fatalf("请求体 = %.400s，期望含 %s", body, want)
		}
	})
}

// TestUpstreamClientErrorLine 上游 4xx 的证据行：方法、HTTP 状态、协议错误码、去掉内部标记并压成一行的文案、details 的全部类型与原文，
// 响应体不是状态数组时附原文；包在其他错误里的同样识别。不是上游 4xx（5xx、本服务自己的错误、没有错误）时不写
func TestUpstreamClientErrorLine(t *testing.T) {
	invalid := aistudio.DecodeRPCError("CreateInteractionStream", http.StatusBadRequest, []byte(
		`[3,"[original: beyond::dependency::INVALID_ARGUMENT] Request contains\n an invalid argument. (qos=CRITICAL_PLUS)",`+
			`[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0].parts[0]","bad image"]]]]]]`))
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "Interaction 400", err: fmt.Errorf("发送: %w", invalid),
			want: `上游返回 4xx | 方法=CreateInteractionStream | HTTP=400 | 协议错误码=3 | 文案=Request contains an invalid argument. | ` +
				`详情类型=google.rpc.BadRequest | 详情=[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0].parts[0]","bad image"]]]]]`},
		{name: "429", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded",
			DetailTypes: []string{"google.rpc.Help"}},
			want: `上游返回 4xx | 方法=GenerateContent | HTTP=429 | 协议错误码=8 | 文案=Quota exceeded | 详情类型=google.rpc.Help`},
		{name: "HTML 错误页", err: aistudio.DecodeRPCError("CreateInteractionStream", http.StatusNotFound, []byte("<html>\n<b>404</b></html>")),
			want: `上游返回 4xx | 方法=CreateInteractionStream | HTTP=404 | 文案=Not Found | 原文=<html> <b>404</b></html>`},
		{name: "Drive 上传 403", err: errors.Join(errors.New("上传"), &aistudio.RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden,
			Message: `{"error": {"code": 403, "message": "The user has exceeded their Drive storage quota"}}`}),
			want: `上游返回 4xx | 方法=DriveUpload | HTTP=403 | 文案={"error": {"code": 403, "message": "The user has exceeded their Drive storage quota"}}`},
		{name: "5xx 不写", err: &aistudio.RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusBadGateway, Message: "interaction failed"}},
		{name: "本服务的 Drive 错误不写", err: &aistudio.DriveRequiredError{Reason: aistudio.DriveRequiredNoAccount, Model: omniDriveModel}},
		{name: "参数错误不写", err: fmt.Errorf("%w: contents must not be empty", aistudio.ErrInvalidArgument)},
		{name: "没有错误"},
	} {
		line, ok := upstreamClientErrorLine(test.err)
		if line != test.want || ok != (test.want != "") {
			t.Fatalf("%s: 证据行 = %q（%v）\n期望 %q", test.name, line, ok, test.want)
		}
	}
	long := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Message: strings.Repeat("长", upstreamErrorMessageLimit+10)}
	if line, _ := upstreamClientErrorLine(long); !strings.HasSuffix(line, strings.Repeat("长", upstreamErrorMessageLimit)+"…") {
		t.Fatalf("超长文案应截断: %q", line[len(line)-20:])
	}
}

// evidenceUpstream 为 Interaction 模型的测试上游：Generate 先经请求信息观察者报告一行结构摘要（与 aistudio.generateInteraction 相同），
// 再按 midStream 在首个事件之前或输出一段正文之后返回上游 400；记录每次调用的账号
type evidenceUpstream struct {
	mu        sync.Mutex
	err       error
	midStream bool
	calls     []string
}

func (upstream *evidenceUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *evidenceUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *evidenceUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	aistudio.ReportRequestInfo(ctx, "Interaction 请求结构 | 方法=CreateInteractionStream | 测试")
	if !upstream.midStream {
		return nil, upstream.err
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "部分"}
	events <- aistudio.Event{Kind: aistudio.EventError, Err: upstream.err}
	close(events)
	return events, nil
}

// TestUpstreamClientErrorLogged 上游返回 4xx 时在该请求的管理日志（来源为执行账号）记一行 WARN 证据：首个事件之前的 400（不换号，只有一行）与
// 已输出内容后流内的 400（附“已输出内容后”）；发送前的请求结构摘要以 INFO 记在同一请求、同一账号名下（不是 WARN 的请求说明）
func TestUpstreamClientErrorLogged(t *testing.T) {
	rejected := aistudio.DecodeRPCError("CreateInteractionStream", http.StatusBadRequest, []byte(
		`[3,"Request contains an invalid argument.",[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0]","bad"]]]]]]`))
	want := `上游返回 4xx | 方法=CreateInteractionStream | HTTP=400 | 协议错误码=3 | 文案=Request contains an invalid argument. | ` +
		`详情类型=google.rpc.BadRequest | 详情=[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0]","bad"]]]]]`
	for _, test := range []struct {
		name      string
		midStream bool
		suffix    string
	}{
		{name: "首个事件之前"},
		{name: "已输出内容后", midStream: true, suffix: " | 已输出内容后"},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := []string{"a-evidence@example.com", "b-evidence@example.com"}
			upstream := &evidenceUpstream{err: rejected, midStream: test.midStream}
			service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
			_, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
				ID: "req-evidence", Model: testRuntimeModel,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
			})
			var rpcError *aistudio.RPCError
			if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest {
				t.Fatalf("err = %v，期望上游 400", err)
			}
			upstream.mu.Lock()
			calls := slices.Clone(upstream.calls)
			upstream.mu.Unlock()
			if len(calls) != 1 {
				t.Fatalf("上游调用 = %v，400 不应换号", calls)
			}
			service.requests.mu.Lock()
			var evidence, summaries []string
			for _, entry := range service.requests.logs {
				if entry.Event != "request.progress" || entry.Request == nil || entry.Request.ID != "req-evidence" || entry.Source != calls[0] {
					continue
				}
				switch {
				case strings.HasPrefix(entry.Message, "上游返回 4xx"):
					evidence = append(evidence, entry.Level+" "+entry.Message)
				case strings.HasPrefix(entry.Message, "Interaction 请求结构"):
					summaries = append(summaries, entry.Level+" "+entry.Message)
				}
			}
			service.requests.mu.Unlock()
			if !slices.Equal(evidence, []string{"WARN " + want + test.suffix}) {
				t.Fatalf("证据行 = %q\n期望 %q", evidence, "WARN "+want+test.suffix)
			}
			if !slices.Equal(summaries, []string{"INFO Interaction 请求结构 | 方法=CreateInteractionStream | 测试"}) {
				t.Fatalf("请求结构摘要 = %q，期望以 INFO 记在执行账号名下", summaries)
			}
		})
	}

	// 经排查路由（/trace/）的请求：请求结构摘要与 4xx 证据行经请求进度写进排查记录的 timeline（与请求日志同一行、来源为执行账号），
	// 生成服务有请求信息观察者，摘要不走 notes 的回退分支
	accounts := []string{"a-evidence-trace@example.com", "b-evidence-trace@example.com"}
	upstream := &evidenceUpstream{err: rejected}
	service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
	trace := aistudio.NewTrace()
	// 排查路由在鉴权通过后标记，Generate 只为标记过的排查记录同步请求进度
	trace.Activate()
	events, err := service.Generate(aistudio.ContextWithTrace(service.lifecycle, trace), aistudio.GenerateRequest{
		ID: "req-evidence-trace", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	})
	if err == nil {
		for event := range events {
			if event.Kind == aistudio.EventError {
				err = event.Err
			}
		}
	}
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest {
		t.Fatalf("排查路由: err = %v，期望上游 400", err)
	}
	data, err := trace.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Notes    []string                      `json:"notes"`
		Timeline []aistudio.TraceTimelineEntry `json:"timeline"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	upstream.mu.Lock()
	calls := slices.Clone(upstream.calls)
	upstream.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("排查路由: 上游调用 = %v", calls)
	}
	var timeline []string
	for _, entry := range record.Timeline {
		if entry.Source == calls[0] && (strings.HasPrefix(entry.Message, "Interaction 请求结构") || strings.HasPrefix(entry.Message, "上游返回 4xx")) {
			timeline = append(timeline, entry.Level+" "+entry.Message)
		}
	}
	if !slices.Equal(timeline, []string{"INFO Interaction 请求结构 | 方法=CreateInteractionStream | 测试", "WARN " + want}) {
		t.Fatalf("排查记录 timeline = %q，期望请求结构摘要与 4xx 证据行", timeline)
	}
	for _, note := range record.Notes {
		if strings.Contains(note, "Interaction 请求结构") {
			t.Fatalf("有请求信息观察者时摘要只进 timeline，不再写 notes: %q", record.Notes)
		}
	}
}

// TestCleanupErrorDoesNotSwitchAccount 不固定账号的请求：附件上传成功、生成以不可换号的错误（非 RPC 的连接错误）失败、随后删除临时 Drive 副本时
// Drive 返回 429、404 或 403：是否换号只看这次尝试本身的错误，清理错误是可换号的 RPC 状态也不换号（原先按并入清理错误后的错误判断，换到另一个账号
// 再失败一次）；清理的 403 不计入多模型连续 403 的自动暂停计数（原先按并入清理错误后的错误计数）；清理错误照旧并入返回的错误，清理的 4xx
// 不写上游 4xx 证据（不是这次尝试本身的错误）
func TestCleanupErrorDoesNotSwitchAccount(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusNotFound, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			accounts := []string{"a-cleanup@example.com", "b-cleanup@example.com"}
			upstream := &cleanupRateUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}, cleanupErr: driveDeleteError(status)}
			service, _ := rateLadderService(t, upstream, accounts...)
			service.state.Store(serviceRunning)
			service.dataContext = context.Background()
			request := aistudio.GenerateRequest{
				ID: "req-cleanup-switch", Model: rateLadderModel,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
					{Text: "hi"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}},
				}}},
			}
			_, err, _ := runOmniDriveRequest(t, service, request)
			var rpcError *aistudio.RPCError
			if err == nil || !strings.Contains(err.Error(), "connection reset by peer") || !errors.As(err, &rpcError) || rpcError.Method != "DriveDelete" {
				t.Fatalf("请求错误 = %v，期望连接错误并带上清理时的 Drive 错误", err)
			}
			if calls := upstream.called(); len(calls) != 1 {
				t.Fatalf("上游生成调用 = %v，清理错误不应让请求换号", calls)
			}
			if progressLogged(service, "上游返回 4xx") {
				t.Fatalf("清理时的 Drive 错误不应记成上游 4xx 证据: %v", serviceLogMessages(service.requests))
			}
			if status == http.StatusForbidden && !isPermissionDenied(err) {
				t.Fatalf("前提：并入清理错误后的错误 %v 会被识别为 403 无权限", err)
			}
			service.forbidden.mu.Lock()
			recorded := len(service.forbidden.events[accounts[0]]) + len(service.forbidden.events[accounts[1]])
			service.forbidden.mu.Unlock()
			if recorded != 0 {
				t.Fatalf("清理时的 Drive 错误不应计入连续 403 的自动暂停计数：记录 %d 次", recorded)
			}
		})
	}
}
