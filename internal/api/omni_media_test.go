package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// omniMediaHandler 返回目录里有 Omni 视频模型、带媒体目录的公开 API，以及保存了两段视频与一张图片的媒体目录
func omniMediaHandler(t *testing.T) (http.Handler, *recordingService, *mediastore.Store) {
	t.Helper()
	service := &recordingService{modelListService: modelListService{models: omniAliasModels()}}
	handler, dir := newMediaHandler(t, service, &mediaSettingsStub{enabled: true, base: "https://ai.example.com"})
	return handler, service, mediastore.New(dir)
}

// TestRestoreMediaLinksOmni 目标模型是能输出视频的 Omni 时：new-api 回传的上一轮 assistant 正文里只剩本服务的视频链接（[video](…)），
// 恢复为视频媒体（Interaction 编码时移到随后的用户 step 上传发送，模型能看到原视频）；用户正文里粘贴的本服务视频与图片链接
// （裸地址或 Markdown）恢复为媒体附件；Gemini 原生 SDK 回传的 assistant 视频 fileData 同样恢复。同样的请求发给其他模型时与原先相同
func TestRestoreMediaLinksOmni(t *testing.T) {
	handler, service, store := omniMediaHandler(t)
	secondVideo := append(append([]byte(nil), testVideo...), 0x33)
	videoName, _ := store.Save(testVideo, "video/mp4")
	secondName, _ := store.Save(secondVideo, "video/mp4")
	imageName, _ := store.Save(testImage, "image/png")
	videoLink := "https://ai.example.com/media/" + videoName
	secondLink := "https://ai.example.com/media/" + secondName
	imageLink := "/media/" + imageName
	videoHash, secondHash, imageHash := sha256Hex(testVideo)[:8], sha256Hex(secondVideo)[:8], sha256Hex(testImage)[:8]
	chat := func(model string) string {
		return mustJSONText(t, map[string]any{"model": model, "messages": []any{
			map[string]any{"role": "user", "content": "做个海边视频"},
			map[string]any{"role": "assistant", "content": "\n[video](" + videoLink + ")\n"},
			map[string]any{"role": "user", "content": "把这个视频延长 5 秒 " + secondLink + "，参考 ![image](" + imageLink + ")"},
		}})
	}
	recorder := postJSON(t, handler, "/v1/chat/completions", chat("gemini-omni-1.1-flash-extend"), nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("Chat: %d %s", recorder.Code, recorder.Body.String())
	}
	contents := service.last.Contents
	if got := describeParts(contents[1].Parts); strings.Join(got, "|") != "text:\n|inline:video/mp4:"+videoHash+"|text:\n" {
		t.Fatalf("assistant 正文里的视频链接应恢复为视频: %q", got)
	}
	want := []string{"text:把这个视频延长 5 秒 ", "inline:video/mp4:" + secondHash, "text:，参考 ", "inline:image/png:" + imageHash}
	if got := describeParts(contents[2].Parts); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("用户正文里粘贴的链接 = %q，期望 %q", got, want)
	}
	// 交给生成服务的请求里：回传历史里恢复的视频带标记与原链接文本，这一轮用户消息里粘贴的链接与客户端附件一样不带
	if part := contents[1].Parts[1]; !part.RestoredLink || part.RestoredText != "[video]("+videoLink+")" {
		t.Fatalf("assistant 历史视频的恢复标记 = %v %q", part.RestoredLink, part.RestoredText)
	}
	for _, part := range contents[2].Parts {
		if part.RestoredLink {
			t.Fatalf("这一轮用户消息里的媒体不应带恢复标记: %+v", part)
		}
	}
	for _, note := range service.last.Notes {
		if strings.Contains(note, "没能恢复") {
			t.Fatalf("全部恢复时不应留下恢复失败的说明: %q", note)
		}
	}

	// 其他模型：assistant 视频链接与用户正文里的链接保持文本
	service.last = nil
	recorder = postJSON(t, handler, "/v1/chat/completions", chat("plain-chat"), nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("其他模型: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := describeParts(service.last.Contents[1].Parts); strings.Join(got, "|") != "text:\n[video]("+videoLink+")\n" {
		t.Fatalf("其他模型的 assistant 视频链接应保持文本: %q", got)
	}
	if got := describeParts(service.last.Contents[2].Parts); len(got) != 1 || !strings.HasPrefix(got[0], "text:把这个视频延长 5 秒 "+secondLink) {
		t.Fatalf("其他模型的用户正文应保持原样: %q", got)
	}

	// Gemini 原生 SDK 回传的历史：assistant 的视频 fileData 恢复为视频（同一文件的文本链接保持文本）
	gemini := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "做个视频"}}},
		map[string]any{"role": "model", "parts": []any{
			map[string]any{"text": "\n[video](" + videoLink + ")\n"},
			map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": videoLink}, "thoughtSignature": "sig-video"},
		}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "把天空换成晚霞"}}},
	}}
	recorder = postJSON(t, handler, "/v1beta/models/gemini-omni-1.1-flash:generateContent", mustJSONText(t, gemini), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Gemini: %d %s", recorder.Code, recorder.Body.String())
	}
	parts := service.last.Contents[1].Parts
	if got := describeParts(parts); strings.Join(got, "|") != "text:\n[video]("+videoLink+")\n|inline:video/mp4:"+videoHash || parts[1].ThoughtSignature != "sig-video" {
		t.Fatalf("assistant 视频 fileData 应恢复为视频并保留签名: %q", got)
	}
}

// TestRestoreMediaLinksFailureNotes 按规则该恢复却没能恢复的链接（文件已清理或不是本服务的链接、超出单次请求 64MB 上限）在该请求的
// 管理日志记一行说明，不再静默；其他模型的用户 fileData 恢复失败同样记录。全部恢复成功或不是候选的链接不记
func TestRestoreMediaLinksFailureNotes(t *testing.T) {
	store := mediastore.New(t.TempDir())
	newer := append(append([]byte(nil), testVideo...), bytes.Repeat([]byte{0x44}, 64)...)
	older := append(append([]byte(nil), testVideo...), bytes.Repeat([]byte{0x55}, 64)...)
	newerName, _ := store.Save(newer, "video/mp4")
	olderName, _ := store.Save(older, "video/mp4")
	missingName := strings.Repeat("e", 64) + ".mp4"
	contents := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个视频"}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "[video](https://ai.example.com/media/" + olderName + ")"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "接着这个 https://other.example.net/media/" + missingName}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "[video](https://ai.example.com/media/" + newerName + ")"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "再延长"}}},
	}
	notes := restoreMediaLinksWithin(store, contents, int64(len(newer)+len(older)/2), true)
	if len(notes) != 1 {
		t.Fatalf("说明 = %q，期望一行", notes)
	}
	for _, want := range []string{
		"没能恢复为媒体输入", "不在媒体目录（已清理或不是本服务的链接）=1（eeeeeeeeeeee….mp4）",
		"超出单次请求恢复上限 0MB=1（" + mediastore.Hash(olderName)[:12] + "….mp4，较新的消息优先）",
	} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("说明 %q 应含 %q", notes[0], want)
		}
	}
	if got := describeParts(contents[3].Parts); strings.Join(got, "|") != "inline:video/mp4:"+sha256Hex(newer)[:8] {
		t.Fatalf("较新的视频应优先恢复: %q", got)
	}
	if got := describeParts(contents[1].Parts); strings.Join(got, "|") != "text:[video](https://ai.example.com/media/"+olderName+")" {
		t.Fatalf("超出上限的较早视频应保持链接文本: %q", got)
	}

	// 其他模型：用户 fileData 指向已清理的文件，改为文本链接并记说明；assistant 的视频链接不是候选，不记
	other := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{File: &aistudio.FileRef{ID: "https://ai.example.com/media/" + missingName}}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "[video](https://ai.example.com/media/" + missingName + ")"}}},
	}
	notes = restoreMediaLinksWithin(store, other, maxRestoredMediaBytes, false)
	if len(notes) != 1 || !strings.Contains(notes[0], "不在媒体目录（已清理或不是本服务的链接）=1") {
		t.Fatalf("其他模型的说明 = %q", notes)
	}
	if notes = restoreMediaLinksWithin(store, []aistudio.Content{
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "[video](https://ai.example.com/media/" + missingName + ")"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "看 https://ai.example.com/media/" + missingName}}},
	}, maxRestoredMediaBytes, false); len(notes) != 0 {
		t.Fatalf("其他模型不恢复的链接不应留下说明: %q", notes)
	}
}

// TestRestoreMediaLinksNoteReachesRequest 协议层的恢复失败说明随请求交给生成服务（startGenerate 逐条写进该请求的管理日志）；
// Omni 下不能恢复的外部地址改为文本链接，不让整个请求因外部地址返回 400
func TestRestoreMediaLinksNoteReachesRequest(t *testing.T) {
	handler, service, _ := omniMediaHandler(t)
	missing := "https://ai.example.com/media/" + strings.Repeat("f", 64) + ".mp4"
	body := mustJSONText(t, map[string]any{"model": "gemini-omni-1.1-flash", "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "延长这个视频"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": missing}},
		}},
	}})
	recorder := postJSON(t, handler, "/v1/chat/completions", body, nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
	}
	if got := describeParts(service.last.Contents[0].Parts); strings.Join(got, "|") != "text:延长这个视频|text:[video]("+missing+")" {
		t.Fatalf("Omni 下不能恢复的外部地址应改为文本链接: %q", got)
	}
	found := false
	for _, note := range service.last.Notes {
		found = found || strings.Contains(note, "不在媒体目录（已清理或不是本服务的链接）=1")
	}
	if !found {
		t.Fatalf("请求说明 = %q，期望含恢复失败的说明", service.last.Notes)
	}
}

// TestUserMediaLinksBoundaries 用户正文里的裸地址只认完整的本服务媒体地址：带查询串、更长路径或接在英文单词后面的不算，
// 紧跟在中文后面的算（中文正文里常不加空格）
func TestUserMediaLinksBoundaries(t *testing.T) {
	name := strings.Repeat("a", 64) + ".mp4"
	for text, want := range map[string]int{
		"看 https://ai.example.com/media/" + name:                 1,
		"看/media/" + name:                                        1,
		"看 /media/" + name + "。":                                 1,
		"看 https://ai.example.com/media/" + name + "?x=1":        0,
		"看 https://ai.example.com/media/" + name + "/more":       0,
		"[video](https://ai.example.com/media/" + name + ") 和这个": 1,
		"(https://ai.example.com/media/" + name + ")":            1,
		"abc/media/" + name:                                      0,
	} {
		if got := len(userMediaLinks(text)); got != want {
			t.Fatalf("%q: 识别 %d 处，期望 %d", text, got, want)
		}
	}
	// 两个裸地址之间不是空白（中文逗号、顿号、“和”、半角逗号）时各自识别，范围不跨过前一个链接
	other := strings.Repeat("b", 64) + ".mp4"
	first, second := "https://ai.example.com/media/"+name, "https://ai.example.com/media/"+other
	for _, separator := range []string{"，", "、", "和", ",", " "} {
		text := "对比 " + first + separator + second + " 两个视频"
		links := userMediaLinks(text)
		if len(links) != 2 || links[0].name != name || links[1].name != other ||
			text[links[0].start:links[0].end] != first || text[links[1].start:links[1].end] != second {
			t.Fatalf("分隔符 %q：识别 %+v，期望两处各自完整的链接", separator, links)
		}
	}
}

// TestRestoreAdjacentUserMediaLinks 用户正文里相邻的两个本服务视频链接（中间是中文逗号等）都恢复为视频，链接之间的文字保留；
// 回传历史里恢复出来的媒体带 RestoredLink 与原链接文本（Omni 没有可用的 Drive 授权账号时换回它），最后一条用户消息（这一轮）里
// 恢复的与客户端自己带的附件都不带
func TestRestoreAdjacentUserMediaLinks(t *testing.T) {
	store := mediastore.New(t.TempDir())
	secondVideo := append(append([]byte(nil), testVideo...), 0x33)
	firstName, _ := store.Save(testVideo, "video/mp4")
	secondName, _ := store.Save(secondVideo, "video/mp4")
	first, second := "https://ai.example.com/media/"+firstName, "https://ai.example.com/media/"+secondName
	for _, separator := range []string{"，", "、", "和", ","} {
		for _, current := range []bool{false, true} {
			contents := []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
				{Text: "对比 " + first + separator + second + " 两个视频"},
				{InlineData: &aistudio.Blob{MIME: "image/png", Data: testImage}},
			}}}
			if !current {
				contents = append(contents,
					aistudio.Content{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "好的"}}},
					aistudio.Content{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "谢谢"}}})
			}
			if notes := restoreMediaLinksWithin(store, contents, maxRestoredMediaBytes, true); len(notes) != 0 {
				t.Fatalf("分隔符 %q：不应有恢复失败的说明 %q", separator, notes)
			}
			parts := contents[0].Parts
			want := []string{
				"text:对比 ", "inline:video/mp4:" + sha256Hex(testVideo)[:8], "text:" + separator,
				"inline:video/mp4:" + sha256Hex(secondVideo)[:8], "text: 两个视频", "inline:image/png:" + sha256Hex(testImage)[:8],
			}
			if got := describeParts(parts); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("分隔符 %q：恢复 = %q，期望 %q", separator, got, want)
			}
			history := !current
			if parts[1].RestoredLink != history || parts[3].RestoredLink != history || parts[5].RestoredLink ||
				history && (parts[1].RestoredText != first || parts[3].RestoredText != second) {
				t.Fatalf("分隔符 %q（这一轮=%v）：恢复标记 = %+v / %+v / %+v", separator, current, parts[1], parts[3], parts[5])
			}
		}
	}
	// assistant 正文里的 Markdown 视频链接与视频 fileData 同样带标记；同一条消息里已有文本链接的 fileData 不带链接文本
	contents := []aistudio.Content{
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "好的：[video](" + first + ")"},
			{File: &aistudio.FileRef{ID: second, MIME: "video/mp4"}},
		}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "再来一个"}}},
	}
	restoreMediaLinksWithin(store, contents, maxRestoredMediaBytes, true)
	parts := contents[0].Parts
	if len(parts) != 3 || !parts[1].RestoredLink || parts[1].RestoredText != "[video]("+first+")" ||
		!parts[2].RestoredLink || parts[2].RestoredText != mediaLinkText(second, secondName) {
		t.Fatalf("assistant 消息 = %+v", parts)
	}
}

// driveRequiredService 让生成直接返回给定的错误
type driveRequiredService struct {
	modelListService
	err error
}

func (service *driveRequiredService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	return nil, service.err
}

// TestDriveRequiredPublicErrors 需要 Drive 授权与 Interaction 附件的英文说明原样返回客户端，不换成通用的
// “Request contains an invalid argument.”：号池里没有 Drive 授权的账号为 400 FAILED_PRECONDITION，换号次数用完为 503
func TestDriveRequiredPublicErrors(t *testing.T) {
	noAccount := &aistudio.DriveRequiredError{Reason: aistudio.DriveRequiredNoAccount, Model: "gemini-omni-1.1-flash", Unavailable: 958}
	for _, test := range []struct {
		err     error
		status  int
		rpc     string
		message string
	}{
		{err: noAccount, status: http.StatusBadRequest, rpc: "FAILED_PRECONDITION", message: noAccount.PublicMessage()},
		{err: fmt.Errorf("包装: %w", &aistudio.DriveRequiredError{Reason: aistudio.DriveRequiredExhausted, Model: "gemini-omni-1.1-flash", Tried: 8}),
			status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE", message: "none of the 8 accounts tried for this request has Google Drive authorization"},
		{err: aistudio.InteractionExternalMediaError([]aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{ExternalMedia: &aistudio.ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}},
		}}}), status: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: "external URLs are not supported"},
		{err: fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, "contents[0]: video and file input for this model must be uploaded to Google Drive first, which requires an account with Google Drive authorized"),
			status: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: "must be uploaded to Google Drive first"},
	} {
		public := publicErrorFor(test.err, "gemini-omni-1.1-flash")
		if public.Status != test.status || public.RPC != test.rpc || !strings.Contains(public.Message, test.message) || public.Message == officialInvalidMessage {
			t.Fatalf("%v -> %+v", test.err, public)
		}
	}
	for _, path := range []string{"/v1beta/models/gemini-omni-1.1-flash:generateContent", "/v1/chat/completions"} {
		handler, _ := newMediaHandler(t, &driveRequiredService{modelListService: modelListService{models: omniAliasModels()}, err: noAccount}, &mediaSettingsStub{})
		body := `{"contents":[{"role":"user","parts":[{"text":"分析视频"},{"inlineData":{"mimeType":"video/mp4","data":"AAAAGGZ0eXBtcDQy"}}]}]}`
		if strings.HasPrefix(path, "/v1/chat") {
			body = `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":[{"type":"text","text":"分析视频"},{"type":"file","file":{"file_data":"data:video/mp4;base64,AAAAGGZ0eXBtcDQy","filename":"clip.mp4"}}]}]}`
		}
		recorder := postJSON(t, handler, path, body, nil)
		var response struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusBadRequest ||
			response.Error.Message != noAccount.PublicMessage() {
			t.Fatalf("%s: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if strings.HasPrefix(path, "/v1beta") && response.Error.Status != "FAILED_PRECONDITION" {
			t.Fatalf("Gemini 错误状态 = %q", response.Error.Status)
		}
	}
}
