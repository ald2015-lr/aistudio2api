package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// interactionReplay 为返回固定响应的受保护传输：记录发出的请求体，按 status 与 body 返回 CreateInteractionStream 的响应
type interactionReplay struct {
	status int
	body   string
	sent   [][]byte
}

func (replay *interactionReplay) DoProtected(_ context.Context, _ GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	replay.sent = append(replay.sent, append([]byte(nil), rpc.Body...))
	header := http.Header{}
	header.Set("Content-Type", JSONProtobufContentType)
	return &RPCResponse{StatusCode: replay.status, Header: header, Body: io.NopCloser(strings.NewReader(replay.body))}, nil
}

// traceRecord 为排查记录里测试关心的字段
type traceRecord struct {
	Attempts []struct {
		Requests []struct {
			Method string       `json:"method"`
			URL    string       `json:"url"`
			Body   TracePayload `json:"body"`
		} `json:"requests"`
		UpstreamBytes int64  `json:"upstream_bytes"`
		UpstreamHead  string `json:"upstream_head"`
	} `json:"attempts"`
	Notes []string `json:"notes"`
}

// finishTrace 结束排查记录并解析出测试关心的字段
func finishTrace(t *testing.T, trace *Trace) traceRecord {
	t.Helper()
	data, err := trace.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var record traceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// noteWith 返回第一条含 fragment 的说明（去掉时间前缀）；没有时为空
func noteWith(notes []string, fragment string) string {
	for _, note := range notes {
		if index := strings.Index(note, fragment); index >= 0 {
			return note[index:]
		}
	}
	return ""
}

// TestGenerateInteractionTrace 经排查路由（/trace/）的 Interaction 请求与 GenerateContent 一样记下发往上游的 CreateInteractionStream 请求体
// （URL、方法、请求体与编码结果逐字节相同）与上游原始响应的开头；上游返回 HTTP 400 时错误响应体同样记下（在读出错误之前接上）；
// 另记 binding 的字节数与 sha256（OMNI_IMAGE_BINDING 开启时含图片 base64，关闭时只含文本）与请求结构摘要（没有请求信息观察者时写进 notes，
// 写明校验值是否含图片）；请求体与开关无关
func TestGenerateInteractionTrace(t *testing.T) {
	pngData := omniTestPNG(t)
	request := GenerateRequest{ID: "req-trace", Model: "gemini-omni-1.1-flash", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: pngData}}, {Text: "随机视频"}}},
	}}
	wantBody, _, err := EncodeCreateInteractionStreamRequest(request, omniTestDefaults())
	if err != nil {
		t.Fatal(err)
	}
	entry := modelEntry{model: omniTestModel(), defaults: omniTestDefaults()}
	bindingNote := func(prompt string) string {
		digest := sha256.Sum256([]byte(prompt))
		return fmt.Sprintf("CreateInteractionStream binding | 字节=%d | sha256=%s", len(prompt), hex.EncodeToString(digest[:]))
	}
	// OMNI_IMAGE_BINDING 开启（默认，没有挂开关时同样）时 binding 为图片 base64 与文本（请求体里图片在前），关闭时只含文本（与 a574e64 相同）
	textOnly := bindingNote("随机视频")
	bindingOn, bindingOff := true, false
	withImage := bindingNote(base64.StdEncoding.EncodeToString(pngData) + " 随机视频")

	interaction := make([]any, 13)
	interaction[1] = 3
	interaction[12] = []any{10, nil, nil, nil, 20, nil, nil, nil, 0, 30}
	final := make([]any, 20)
	final[19] = []any{interaction}
	stream, err := json.Marshal([]any{[]any{interactionDeltaEvent([]any{[]any{"好的"}}), final}, []any{0}})
	if err != nil {
		t.Fatal(err)
	}
	const rejected = `[3,"Request contains an invalid argument.",[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0].parts[0]","bad image"]]]]]]`
	for _, test := range []struct {
		name    string
		status  int
		body    string
		binding *bool
		want    string
		summary string
	}{
		{name: "成功", status: http.StatusOK, body: string(stream), want: withImage, summary: "校验值含图片=是（1 张）"},
		{name: "上游 400", status: http.StatusBadRequest, body: rejected, want: withImage, summary: "校验值含图片=是（1 张）"},
		{name: "开关开启", status: http.StatusOK, body: string(stream), binding: &bindingOn, want: withImage, summary: "校验值含图片=是（1 张）"},
		{name: "开关关闭", status: http.StatusOK, body: string(stream), binding: &bindingOff, want: textOnly, summary: "校验值含图片=关闭"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := &interactionReplay{status: test.status, body: test.body}
			client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: replay})
			if err != nil {
				t.Fatal(err)
			}
			trace := NewTrace()
			trace.StartAttempt("omni@example.com", "playground")
			ctx := ContextWithTrace(context.Background(), trace)
			if test.binding != nil {
				ctx = ContextWithInteractionImageBinding(ctx, *test.binding)
			}
			events, err := client.generateInteraction(ctx, request, entry)
			if test.status == http.StatusOK {
				if err != nil {
					t.Fatal(err)
				}
				for range events {
				}
			} else {
				var rpcError *RPCError
				if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest || rpcError.Code != 3 ||
					rpcError.Details != `[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0].parts[0]","bad image"]]]]]` {
					t.Fatalf("上游 400 应返回带 details 原文的 RPCError，得到 %#v", err)
				}
			}
			if len(replay.sent) != 1 || !bytes.Equal(replay.sent[0], wantBody) {
				t.Fatalf("发出的请求体与编码结果不同: %s", replay.sent)
			}
			record := finishTrace(t, trace)
			if len(record.Attempts) != 1 || len(record.Attempts[0].Requests) != 1 {
				t.Fatalf("排查记录应有一次尝试、一条发往上游的请求: %+v", record.Attempts)
			}
			wire := record.Attempts[0].Requests[0]
			var compact bytes.Buffer
			if err := json.Compact(&compact, wire.Body.JSON); err != nil {
				t.Fatal(err)
			}
			if wire.Method != "CreateInteractionStream" || wire.URL != MakerSuiteRPCBase+"CreateInteractionStream" ||
				compact.String() != string(wantBody) || wire.Body.Bytes != len(wantBody) {
				t.Fatalf("记下的请求 = %s %s %d 字节\n%s\n期望\n%s", wire.Method, wire.URL, wire.Body.Bytes, compact.String(), wantBody)
			}
			if head := record.Attempts[0].UpstreamHead; head != test.body || record.Attempts[0].UpstreamBytes != int64(len(test.body)) {
				t.Fatalf("上游原始响应 = %q（%d 字节），期望 %q", head, record.Attempts[0].UpstreamBytes, test.body)
			}
			if note := noteWith(record.Notes, "CreateInteractionStream binding"); note != test.want {
				t.Fatalf("binding 说明 = %q，期望 %q；notes=%v", note, test.want, record.Notes)
			}
			if note := noteWith(record.Notes, "Interaction 请求结构"); !strings.Contains(note, "step=1（用户[图片 1、文本 1]）") ||
				!strings.Contains(note, "| "+test.summary+" |") {
				t.Fatalf("请求结构摘要 = %q", note)
			}
		})
	}
}

// TestGenerateInteractionSummary 发送前经请求信息观察者报告一行请求结构摘要：按编码出的 step（相邻同角色合并、模型历史媒体移到用户 step 之后）
// 列出角色与 part 种类、数量（按发送顺序，连续同类合并计数：图片在前与文本在前的摘要不同），图片按格式汇总张数与字节数，Drive 引用数、文本段数与字节数、系统指令有无、思考等级、输出上限、视频设置、
// 视频格式（field 54 原样）、任务与请求体字节数；不含提示词正文、系统指令正文、图片数据与 Drive 文件 ID；不经请求说明观察者（WARN）报告
func TestGenerateInteractionSummary(t *testing.T) {
	pngData := omniTestPNG(t)
	capture := &interactionCapture{}
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: capture})
	if err != nil {
		t.Fatal(err)
	}
	entry := modelEntry{model: omniTestModel(), defaults: omniTestDefaults()}
	// binding 为挂在 context 上的 OMNI_IMAGE_BINDING，nil 时不挂（按默认开启）
	var binding *bool
	run := func(request GenerateRequest) (string, []string) {
		t.Helper()
		var infos, notes []string
		ctx := ContextWithRequestInfoObserver(context.Background(), func(message string) { infos = append(infos, message) })
		if binding != nil {
			ctx = ContextWithInteractionImageBinding(ctx, *binding)
		}
		ctx = ContextWithRequestNoteObserver(ctx, func(note string) { notes = append(notes, note) })
		if _, err := client.generateInteraction(ctx, request, entry); err == nil || errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("应发往上游（测试上游返回错误），得到 %v", err)
		}
		if len(infos) != 1 {
			t.Fatalf("应报告一行请求结构摘要，得到 %v", infos)
		}
		return infos[0], notes
	}

	first := GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/webp", Data: omniTestWebP}}, {Text: "随机视频"}}},
	}, Config: GenerationConfig{VideoConfig: &VideoConfig{DurationSeconds: 10, Task: "image_to_video"}}}
	summary, notes := run(first)
	want := fmt.Sprintf("Interaction 请求结构 | 方法=CreateInteractionStream | 模型=models/gemini-omni-1.1-flash | 多轮=否 | step=1（用户[图片 1、文本 1]） | "+
		"图片=1 张（WebP 1 张 %dB） | Drive 引用=0 | 校验值含图片=是（1 张） | 文本=1 段 12B | 系统指令=无 | 思考等级=4 | 输出上限=65536 | 视频设置=时长 10s、任务 image_to_video | "+
		"视频格式=[null,null,null,null,[10],1] | 任务=image_to_video（2） | 请求体=%dB", len(omniTestWebP), len(capture.bodies[0]))
	if summary != want || len(notes) != 0 {
		t.Fatalf("第一轮摘要 =\n%s\n期望\n%s\n说明 %v", summary, want, notes)
	}

	multi := GenerateRequest{Model: "gemini-omni-1.1-flash", System: "保持画风", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {InlineData: &Blob{MIME: "image/png", Data: pngData}}}},
		{Role: RoleUser, Parts: []Part{{File: &FileRef{ID: "drive-secret-id", MIME: "video/mp4"}}}},
		{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
	}}
	summary, _ = run(multi)
	want = fmt.Sprintf("Interaction 请求结构 | 方法=CreateInteractionStream | 模型=models/gemini-omni-1.1-flash | 多轮=是 | "+
		"step=3（用户[文本 1] 模型[文本 1] 用户[说明 1、图片 1、Drive 1、文本 1]） | 图片=1 张（PNG 1 张 %dB） | Drive 引用=1 | 校验值含图片=是（1 张） | 文本=3 段 27B | "+
		"系统指令=有 | 思考等级=4 | 输出上限=65536 | 视频设置=未设置 | 视频格式=[null,null,null,null,null,1] | 任务=未指定 | 请求体=%dB",
		len(pngData), len(capture.bodies[1]))
	if summary != want {
		t.Fatalf("多轮摘要 =\n%s\n期望\n%s", summary, want)
	}
	for _, secret := range []string{"画一只猫", "动起来", "保持画风", "drive-secret-id", base64.StdEncoding.EncodeToString(pngData)[:16]} {
		if strings.Contains(summary, secret) {
			t.Fatalf("摘要不应含提示词、系统指令、文件 ID 与图片数据（%q）: %s", secret, summary)
		}
	}
	// 校验值是否含图片：开关关闭为“关闭”，开启时没有内联图片（纯文字、只有 Drive 引用）为“否”，Drive 引用不计入
	for _, test := range []struct {
		binding *bool
		parts   []Part
		want    string
	}{
		{binding: &[]bool{false}[0], parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: pngData}}, {Text: "随机视频"}}, want: "校验值含图片=关闭"},
		{binding: &[]bool{true}[0], parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: pngData}}, {InlineData: &Blob{MIME: "image/webp", Data: omniTestWebP}}, {Text: "随机视频"}},
			want: "校验值含图片=是（2 张）"},
		{binding: &[]bool{true}[0], parts: []Part{{Text: "随机视频"}}, want: "校验值含图片=否"},
		{binding: &[]bool{false}[0], parts: []Part{{Text: "随机视频"}}, want: "校验值含图片=关闭"},
		{parts: []Part{{File: &FileRef{ID: "drive-only", MIME: "video/mp4"}}, {Text: "分析视频"}}, want: "校验值含图片=否"},
	} {
		binding = test.binding
		summary, _ := run(GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: []Content{{Role: RoleUser, Parts: test.parts}}})
		if !strings.Contains(summary, "| "+test.want+" |") {
			t.Fatalf("摘要应写明 %s: %s", test.want, summary)
		}
	}
	binding = nil

	// part 按发送顺序列出：文本在前、图片在后（new-api 从 OpenAI 格式转换时的常见顺序）与官网抓包的图片在前不同；连续同类合并计数，
	// 不连续的同类分开列
	for _, test := range []struct {
		parts []Part
		want  string
	}{
		{parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: pngData}}, {Text: "随机视频"}}, want: "step=1（用户[图片 1、文本 1]）"},
		{parts: []Part{{Text: "随机视频"}, {InlineData: &Blob{MIME: "image/png", Data: pngData}}}, want: "step=1（用户[文本 1、图片 1]）"},
		{parts: []Part{
			{InlineData: &Blob{MIME: "image/png", Data: pngData}}, {InlineData: &Blob{MIME: "image/webp", Data: omniTestWebP}}, {Text: "随机视频"},
		}, want: "step=1（用户[图片 2、文本 1]）"},
		{parts: []Part{
			{Text: "先看"}, {InlineData: &Blob{MIME: "image/png", Data: pngData}}, {Text: "再看"}, {File: &FileRef{ID: "drive-order", MIME: "video/mp4"}}, {Text: "生成"},
		}, want: "step=1（用户[文本 1、图片 1、文本 1、Drive 1、文本 1]）"},
	} {
		summary, _ := run(GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: []Content{{Role: RoleUser, Parts: test.parts}}})
		if !strings.Contains(summary, "| "+test.want+" |") {
			t.Fatalf("摘要应按发送顺序列出 part（%s）: %s", test.want, summary)
		}
	}

	edit := GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{File: &FileRef{ID: "drive-video", MIME: "video/mp4"}}, {Text: "把天空换成晚霞"}}},
	}, Config: GenerationConfig{VideoConfig: &VideoConfig{Task: "edit"}}}
	if summary, _ = run(edit); !strings.Contains(summary, "| 视频设置=任务 edit | 视频格式=无（编辑任务不带输出格式） | 任务=edit（4） |") {
		t.Fatalf("编辑任务摘要 = %s", summary)
	}
}

// TestDecodeRPCErrorDetailsAndBody 上游错误的状态 details 原文压缩后记进 RPCError.Details（超过 1200 字节截断并注明总字节数），
// 错误响应体不是状态数组（HTML 错误页等）时开头记进 RPCError.Body（空白压成一个空格），供请求日志的 4xx 证据行使用
func TestDecodeRPCErrorDetailsAndBody(t *testing.T) {
	rpcError := DecodeRPCError("CreateInteractionStream", http.StatusBadRequest, []byte(`[3, "Request contains an invalid argument.",
		[["type.googleapis.com/google.rpc.BadRequest", [[["contents[0].parts[1]", "Unsupported MIME type"]]]],
		 ["type.googleapis.com/google.rpc.DebugInfo", [[], "x"]]]]`))
	if rpcError.Code != 3 || rpcError.Message != "Request contains an invalid argument." || rpcError.Body != "" ||
		rpcError.Details != `[["type.googleapis.com/google.rpc.BadRequest",[[["contents[0].parts[1]","Unsupported MIME type"]]]],["type.googleapis.com/google.rpc.DebugInfo",[[],"x"]]]` ||
		strings.Join(rpcError.DetailTypes, ",") != "google.rpc.BadRequest,google.rpc.DebugInfo" {
		t.Fatalf("状态数组: %+v", rpcError)
	}
	if wrapped := DecodeRPCError("GenerateContent", http.StatusForbidden, []byte(`[null,[7,"denied"]]`)); wrapped.Details != "" || wrapped.Body != "" || wrapped.Code != 7 {
		t.Fatalf("没有 details 的流式封装: %+v", wrapped)
	}
	html := DecodeRPCError("CreateInteractionStream", http.StatusBadRequest, []byte("<html>\n  <body>Error 400 (Bad Request)</body>\n</html>"))
	if html.Message != "Bad Request" || html.Body != "<html> <body>Error 400 (Bad Request)</body> </html>" || html.Details != "" {
		t.Fatalf("HTML 错误页: %+v", html)
	}
	if object := DecodeRPCError("CreateInteractionStream", http.StatusNotFound, []byte(`{"error":{"code":404}}`)); object.Body != `{"error":{"code":404}}` {
		t.Fatalf("不是状态数组的 JSON: %+v", object)
	}
	long := DecodeRPCError("CreateInteractionStream", http.StatusBadRequest,
		[]byte(`[3,"bad",[["type.googleapis.com/google.rpc.BadRequest","`+strings.Repeat("长", 600)+`"]]]`))
	if !strings.HasSuffix(long.Details, "…（已截断，共 1850 字节）") || len(long.Details) > unparsedDetailLimit+64 {
		t.Fatalf("超长 details = %d 字节 %q", len(long.Details), long.Details[len(long.Details)-40:])
	}
}

// TestTemporaryCopiesCleanupNotFound 删除临时 Drive 副本时 DriveDelete 返回 404（文件已被外部删除或回收）按已删除处理：照常解绑资源、不返回错误；
// 解绑时资源映射已不存在（已经解绑过）同样算成功；其他错误（DriveDelete 500、别的方法的 404）照旧返回错误、保留绑定
func TestTemporaryCopiesCleanupNotFound(t *testing.T) {
	for _, test := range []struct {
		name        string
		deleteErr   error
		unbindFirst bool
		wantErr     bool
	}{
		{name: "删除成功"},
		{name: "DriveDelete 404 视为已删除", deleteErr: &RPCError{Method: "DriveDelete", StatusCode: http.StatusNotFound, Message: "File not found"}},
		{name: "资源已解绑", unbindFirst: true},
		{name: "404 且资源已解绑", deleteErr: &RPCError{Method: "DriveDelete", StatusCode: http.StatusNotFound}, unbindFirst: true},
		{name: "DriveDelete 500 保留绑定", deleteErr: &RPCError{Method: "DriveDelete", StatusCode: http.StatusInternalServerError}, wantErr: true},
		{name: "别的方法的 404 不算", deleteErr: &RPCError{Method: "DriveDownload", StatusCode: http.StatusNotFound}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, transport := driveTestService(t, "cleanup@example.com")
			transport.deleteErr = test.deleteErr
			ctx := context.Background()
			lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: "cleanup@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Release() }()
			contents, _ := inlineImageContents()
			rewritten, copies, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil)
			if err != nil || copies.Count() != 1 || rewritten[0].Parts[1].File == nil {
				t.Fatalf("上传应改写为 Drive 引用: %+v err=%v", rewritten, err)
			}
			fileID := rewritten[0].Parts[1].File.ID
			bound := func() bool {
				service.pool.mu.Lock()
				defer service.pool.mu.Unlock()
				_, exists := service.pool.resources[fileID]
				return exists
			}
			if !bound() {
				t.Fatal("上传后应绑定资源")
			}
			if test.unbindFirst {
				if err := service.pool.UnbindResource(fileID); err != nil {
					t.Fatal(err)
				}
			}
			err = copies.Cleanup()
			if (err != nil) != test.wantErr {
				t.Fatalf("Cleanup = %v，期望出错=%v", err, test.wantErr)
			}
			if bound() != test.wantErr {
				t.Fatalf("清理后资源绑定=%v，期望 %v", bound(), test.wantErr)
			}
			if transport.deletes != 1 {
				t.Fatalf("DriveDelete 调用 %d 次，期望 1 次", transport.deletes)
			}
		})
	}
}
