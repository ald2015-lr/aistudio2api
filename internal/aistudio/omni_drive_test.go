package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDriveMemoryStates Drive 授权状态的记忆：换到过 token 为可用，401 / Code 16 为不可用，两边互斥；认证代际变化、账户重新添加或
// 过期后为未知；管理验证清除不可用记录时不动可用记录
func TestDriveMemoryStates(t *testing.T) {
	var memory driveMemory
	now := time.Now()
	account := &Account{ID: "drive@example.com"}
	state := func(generation uint64, at time.Time) DriveState {
		return memory.states([]driveSnapshot{{account: account, generation: generation}}, at)[0]
	}
	if got := state(0, now); got != DriveStateUnknown {
		t.Fatalf("没有记录时 = %s，期望 unknown", got)
	}
	memory.rememberAvailable(account, 0, now)
	if got := state(0, now); got != DriveStateAvailable {
		t.Fatalf("换到 token 后 = %s，期望 available", got)
	}
	if got := state(0, now.Add(driveAvailableTTL)); got != DriveStateUnknown {
		t.Fatalf("可用记忆过期后 = %s，期望 unknown", got)
	}
	memory.rememberAvailable(account, 0, now)
	memory.remember(account, 0, now)
	if got := state(0, now); got != DriveStateUnavailable || len(memory.available) != 0 {
		t.Fatalf("记下不可用后 = %s（可用记录 %d 条），期望 unavailable 且删掉可用记录", got, len(memory.available))
	}
	memory.rememberAvailable(account, 0, now)
	if got := state(0, now); got != DriveStateAvailable || memory.count() != 0 {
		t.Fatalf("再次换到 token 后 = %s（不可用记录 %d 条），期望 available", got, memory.count())
	}
	if got := state(1, now); got != DriveStateUnknown {
		t.Fatalf("认证代际变化后 = %s，期望 unknown", got)
	}
	readded := &Account{ID: account.ID}
	memory.rememberAvailable(account, 0, now)
	if got := memory.states([]driveSnapshot{{account: readded}}, now)[0]; got != DriveStateUnknown {
		t.Fatalf("同 ID 重新添加的账户 = %s，期望 unknown", got)
	}
	memory.rememberAvailable(account, 0, now)
	memory.forget(account.ID)
	if got := state(0, now); got != DriveStateAvailable {
		t.Fatalf("清除不可用记录不应动可用记录，实际 %s", got)
	}
}

// TestPooledServiceDriveCandidates 选号范围内的 Drive 状态：附件上传换到 token 的账户为可用，GenerateAccessToken 401 的为不可用，
// 没试过的为未知；只看候选集（模型、号池），不看指定账号与排除列表；汇总计数覆盖全部启用账户
func TestPooledServiceDriveCandidates(t *testing.T) {
	accounts := []string{"drive-a@example.com", "drive-b@example.com", "nodrive-c@example.com", "unknown-d@example.com"}
	service, transport := driveTestService(t, accounts...)
	transport.deny("nodrive-c@example.com", driveMissingCredential)
	ctx := context.Background()
	for _, accountID := range accounts[:3] {
		lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		contents, _ := inlineImageContents()
		if _, copies, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil); err != nil {
			t.Fatalf("%s: %v", accountID, err)
		} else if err := copies.Cleanup(); err != nil {
			t.Fatal(err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	selection := AccountSelection{
		ModelID: ceilingModel, Method: "generateContent", AccountID: "drive-a@example.com",
		ExcludedAccountIDs: []string{"drive-b@example.com"},
	}
	got := service.DriveCandidates(ctx, selection)
	if !slices.Equal(got.Available, []string{"drive-a@example.com", "drive-b@example.com"}) ||
		!slices.Equal(got.Unavailable, []string{"nodrive-c@example.com"}) || !slices.Equal(got.Unknown, []string{"unknown-d@example.com"}) {
		t.Fatalf("候选 Drive 状态 = %+v", got)
	}
	if summary := service.DriveSummary(); summary != (DriveCounts{Available: 2, Unavailable: 1, Unknown: 1}) {
		t.Fatalf("汇总 = %+v", summary)
	}
	if got := service.DriveCandidates(ctx, AccountSelection{ModelID: "missing-model", Method: "generateContent"}); len(got.Available)+len(got.Unavailable)+len(got.Unknown) != 0 {
		t.Fatalf("没有账户支持的模型不应有候选: %+v", got)
	}
	// 重新登录（认证代际推进）后不再沿用记录
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: "drive-a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(testStorageState("NEW")); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := service.DriveCandidates(ctx, AccountSelection{ModelID: ceilingModel}); !slices.Equal(got.Available, []string{"drive-b@example.com"}) || len(got.Unknown) != 2 {
		t.Fatalf("重新登录后的候选 Drive 状态 = %+v", got)
	}
}

// TestDriveRequiredErrorMessages 需要 Drive 授权的错误：对外说明为英文、不含账号信息；没有可用账号与指定账号为 400 FAILED_PRECONDITION，
// 换号次数用完为 503；管理日志里的说明为中文
func TestDriveRequiredErrorMessages(t *testing.T) {
	for _, test := range []struct {
		err    *DriveRequiredError
		status int
		rpc    string
		want   string
	}{
		{err: &DriveRequiredError{Reason: DriveRequiredNoAccount, Model: "gemini-omni-1.1-flash", Unavailable: 958}, status: http.StatusBadRequest, rpc: "FAILED_PRECONDITION",
			want: "Video input for model gemini-omni-1.1-flash (and image input that cannot be sent inline as PNG or JPEG) requires an account with " +
				"Google Drive authorized, and no account in the pool has Google Drive authorization."},
		{err: &DriveRequiredError{Reason: DriveRequiredNoAccount, Model: "gemini-omni-1.1-flash", ImageHint: "contents[0]: image/webp images cannot be sent inline"},
			status: http.StatusBadRequest, rpc: "FAILED_PRECONDITION",
			want: "no account in the pool has Google Drive authorization. contents[0]: image/webp images cannot be sent inline."},
		{err: &DriveRequiredError{Reason: DriveRequiredAccount, Model: "gemini-omni-1.1-flash"}, status: http.StatusBadRequest, rpc: "FAILED_PRECONDITION",
			want: "the requested account has no Google Drive authorization"},
		{err: &DriveRequiredError{Reason: DriveRequiredExhausted, Model: "gemini-omni-1.1-flash", Tried: 8}, status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE",
			want: "none of the 8 accounts tried for this request has Google Drive authorization"},
	} {
		if test.err.HTTPStatus() != test.status || test.err.RPCStatus() != test.rpc || !strings.Contains(test.err.PublicMessage(), test.want) {
			t.Fatalf("%+v: %d %s %q", test.err, test.err.HTTPStatus(), test.err.RPCStatus(), test.err.PublicMessage())
		}
		for _, r := range test.err.PublicMessage() {
			if r >= 0x4e00 && r <= 0x9fff {
				t.Fatalf("对外说明应为英文: %q", test.err.PublicMessage())
			}
		}
		if !strings.Contains(test.err.Error(), "Drive 授权") {
			t.Fatalf("管理日志说明 = %q", test.err.Error())
		}
	}
}

// TestInteractionModelAndExternalMedia 按 Playground 模型目录（ID 或别名）判断 Interaction 模型；外部地址的图片与视频按参数错误返回英文说明
func TestInteractionModelAndExternalMedia(t *testing.T) {
	service, _ := driveTestService(t, "omni@example.com")
	omni := Model{
		ID: "gemini-omni-1.1-flash", Methods: []string{"generateContent"},
		Capabilities:      map[string]bool{"chat_model": true, "interactions_api": true},
		CapabilityOptions: map[string][]string{"aliases": {"omni-latest"}},
	}
	if err := service.pool.SetCatalog("omni@example.com", BenefitTierFree, []Model{omni, {ID: ceilingModel, Methods: []string{"generateContent"}}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"gemini-omni-1.1-flash": true, "models/omni-latest": true, ceilingModel: false, "": false, "missing": false} {
		if got := service.pool.InteractionModel(name); got != want {
			t.Fatalf("InteractionModel(%q) = %v，期望 %v", name, got, want)
		}
	}
	err := InteractionExternalMediaError([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
		{Role: RoleUser, Parts: []Part{{ExternalMedia: &ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}}}},
	})
	if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "contents[1]: this model only accepts inline or uploaded image and video input") {
		t.Fatalf("外部地址错误 = %v", err)
	}
	if err := InteractionExternalMediaError([]Content{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("x")}}}}}); err != nil {
		t.Fatalf("内联媒体不算外部地址: %v", err)
	}
}

// interactionDeltaEvent 构造一个 content delta 流事件（event field 11）
func interactionDeltaEvent(content []any) []any {
	event := make([]any, 11)
	event[10] = []any{nil, content}
	return event
}

// TestDecodeInteractionImageAndCitations 按上游移植的解码：content delta field 2 为图片（MIME 枚举 1 为 PNG）作为媒体输出，
// field 10 为引用（正文起止位置与来源网址），按网络顺序与正文、视频交错输出；未识别的图片 MIME 与缺少网址的引用按协议证据错误返回
func TestDecodeInteractionImageAndCitations(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nimage")
	video := []byte("\x00\x00\x00\x18ftypmp42video")
	interaction := make([]any, 13)
	interaction[1] = 3
	interaction[12] = []any{10, nil, nil, nil, 20, nil, nil, nil, 0, 30}
	final := make([]any, 20)
	final[19] = []any{interaction}
	citations := []any{[]any{
		[]any{0, 4, nil, []any{"https://example.com/a"}},
		[]any{5, 9, nil, []any{[]any{"https://example.com/b", "ignored"}}},
	}}
	body, err := json.Marshal([]any{[]any{
		interactionDeltaEvent([]any{[]any{"海浪 沙滩"}}),
		interactionDeltaEvent([]any{nil, []any{1, base64.StdEncoding.EncodeToString(image)}}),
		interactionDeltaEvent([]any{nil, nil, nil, nil, nil, nil, nil, nil, nil, citations}),
		interactionDeltaEvent([]any{nil, nil, nil, nil, []any{1, base64.StdEncoding.EncodeToString(video)}}),
		final,
	}, []any{0}})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if err := DecodeInteractionStream(bytes.NewReader(body), func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := make([]EventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	if !slices.Equal(kinds, []EventKind{EventText, EventMedia, EventCitation, EventCitation, EventMedia, EventUsage, EventFinish}) {
		t.Fatalf("事件顺序 = %v", kinds)
	}
	if media := events[1].Media; media == nil || media.MIME != "image/png" || !bytes.Equal(media.Data, image) {
		t.Fatalf("field 2 图片 = %+v", events[1].Media)
	}
	if got := *events[2].Citation; got != (Citation{URL: "https://example.com/a", Start: 0, End: 4}) {
		t.Fatalf("第一条引用 = %+v", got)
	}
	if got := *events[3].Citation; got != (Citation{URL: "https://example.com/b", Start: 5, End: 9}) {
		t.Fatalf("第二条引用 = %+v", got)
	}
	if media := events[4].Media; media == nil || media.MIME != "video/mp4" || !bytes.Equal(media.Data, video) {
		t.Fatalf("field 5 视频 = %+v", events[4].Media)
	}
	for name, content := range map[string][]any{
		"未识别的图片 MIME": {nil, []any{7, base64.StdEncoding.EncodeToString(image)}},
		"图片不是 Base64": {nil, []any{1, "%%%"}},
		"引用缺少网址":      {nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{[]any{[]any{0, 1, nil, []any{}}}}},
		"引用字段不足":      {nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{[]any{[]any{0, 1}}}},
	} {
		raw, _ := json.Marshal([]any{[]any{interactionDeltaEvent(content), final}, []any{0}})
		var evidence *ProtocolEvidenceError
		if err := DecodeInteractionStream(bytes.NewReader(raw), func(Event) error { return nil }); !errors.As(err, &evidence) {
			t.Fatalf("%s: 期望协议证据错误，实际 %v", name, err)
		}
	}
}

// TestDriveObserverAndRestore Drive 状态写盘与恢复：换 token 的结果按状态变化通知写盘（带认证文件的登录签名，同一状态 driveRecordRefresh
// 内不重复通知），管理验证清除时通知清除；新的生成服务只恢复登录签名相同、没过期的记录，已有记录的账户不覆盖，恢复不通知写盘。
// DriveCandidates 的短时缓存在状态变化（记忆版本）时立即失效
func TestDriveObserverAndRestore(t *testing.T) {
	accounts := []string{"a-drive@example.com", "b-nodrive@example.com", "c-relogin@example.com", "d-expired@example.com"}
	service, transport := driveTestService(t, accounts...)
	transport.deny("b-nodrive@example.com", driveMissingCredential)
	var records []DriveRecord
	service.SetDriveObserver(func(record DriveRecord) { records = append(records, record) })
	ctx := context.Background()
	selection := AccountSelection{ModelID: ceilingModel, Method: "generateContent"}
	if got := service.DriveCandidates(ctx, selection); len(got.Unknown) != len(accounts) {
		t.Fatalf("开始时候选 = %+v，期望全部未知", got)
	}
	upload := func(accountID string) {
		t.Helper()
		lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		contents, _ := inlineImageContents()
		_, copies, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil)
		if err != nil {
			t.Fatalf("%s: %v", accountID, err)
		}
		if err := copies.Cleanup(); err != nil {
			t.Fatal(err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	upload("a-drive@example.com")
	upload("b-nodrive@example.com")
	upload("a-drive@example.com")
	identity := LoginIdentity(testStorageState("OLD"))
	if len(records) != 2 || records[0].AccountID != "a-drive@example.com" || records[0].State != DriveStateAvailable || records[0].Identity != identity ||
		records[1].AccountID != "b-nodrive@example.com" || records[1].State != DriveStateUnavailable || records[1].Identity != identity {
		t.Fatalf("通知 = %+v，期望可用与不可用各一条、带登录签名，同一状态不重复通知", records)
	}
	if got := service.DriveCandidates(ctx, selection); !slices.Equal(got.Available, []string{"a-drive@example.com"}) ||
		!slices.Equal(got.Unavailable, []string{"b-nodrive@example.com"}) {
		t.Fatalf("状态变化后缓存应失效，候选 = %+v", got)
	}
	service.ForgetDriveUnavailable("b-nodrive@example.com")
	if len(records) != 3 || records[2].AccountID != "b-nodrive@example.com" || records[2].State != DriveStateUnknown {
		t.Fatalf("清除应通知写盘: %+v", records)
	}
	if got := service.DriveCandidates(ctx, selection); len(got.Unavailable) != 0 {
		t.Fatalf("清除后缓存应失效，候选 = %+v", got)
	}

	restoredService, _ := driveTestService(t, accounts...)
	var restoredRecords []DriveRecord
	restoredService.SetDriveObserver(func(record DriveRecord) { restoredRecords = append(restoredRecords, record) })
	lease, err := restoredService.pool.AcquireFor(ctx, AccountSelection{AccountID: "c-relogin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(testStorageState("NEW")); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	restored := restoredService.RestoreDriveRecords([]DriveRecord{
		{AccountID: "a-drive@example.com", State: DriveStateAvailable, CheckedAt: now.Add(-time.Hour), Identity: identity},
		{AccountID: "b-nodrive@example.com", State: DriveStateUnavailable, CheckedAt: now.Add(-time.Hour), Identity: identity},
		{AccountID: "c-relogin@example.com", State: DriveStateUnavailable, CheckedAt: now.Add(-time.Hour), Identity: identity},
		{AccountID: "d-expired@example.com", State: DriveStateUnavailable, CheckedAt: now.Add(-driveUnavailableTTL - time.Minute), Identity: identity},
		{AccountID: "missing@example.com", State: DriveStateAvailable, CheckedAt: now, Identity: identity},
		{AccountID: "a-drive@example.com", State: DriveStateUnavailable, CheckedAt: now, Identity: identity},
	})
	if restored != 2 || len(restoredRecords) != 0 {
		t.Fatalf("恢复 %d 条（通知 %+v），期望 2 条且不通知写盘", restored, restoredRecords)
	}
	got := restoredService.DriveCandidates(ctx, selection)
	if !slices.Equal(got.Available, []string{"a-drive@example.com"}) || !slices.Equal(got.Unavailable, []string{"b-nodrive@example.com"}) ||
		!slices.Equal(got.Unknown, []string{"c-relogin@example.com", "d-expired@example.com"}) {
		t.Fatalf("恢复后候选 = %+v，期望重新登录与过期的账户为未知，已恢复的记录不被后来的记录覆盖", got)
	}
}

// TestNoteDriveAuthorized 经本服务的登录流程登录、完成了 Drive 授权后记为可用：按当前认证代际记下、通知写盘（带登录签名），
// 原先记着不可用的账户改为可用；之后认证代际推进（再次更新认证文件）时记录失效；不在池中的账户不记
func TestNoteDriveAuthorized(t *testing.T) {
	accounts := []string{"login-a@example.com", "login-b@example.com"}
	service, transport := driveTestService(t, accounts...)
	transport.deny("login-b@example.com", driveMissingCredential)
	var records []DriveRecord
	service.SetDriveObserver(func(record DriveRecord) { records = append(records, record) })
	ctx := context.Background()
	selection := AccountSelection{ModelID: ceilingModel, Method: "generateContent"}
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: "login-b@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := inlineImageContents()
	if _, copies, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil); err != nil {
		t.Fatal(err)
	} else if err := copies.Cleanup(); err != nil {
		t.Fatal(err)
	}
	// 重新登录：保存新认证文件推进代际，随后记下 Drive 授权完成
	if err := lease.SaveStorageState(testStorageState("NEW")); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	records = nil
	service.NoteDriveAuthorized("login-b@example.com")
	service.NoteDriveAuthorized("missing@example.com")
	if got := service.DriveCandidates(ctx, selection); !slices.Equal(got.Available, []string{"login-b@example.com"}) ||
		!slices.Equal(got.Unknown, []string{"login-a@example.com"}) || len(got.Unavailable) != 0 {
		t.Fatalf("候选 = %+v，期望登录完成 Drive 授权的账户为可用", got)
	}
	if len(records) != 1 || records[0].AccountID != "login-b@example.com" || records[0].State != DriveStateAvailable ||
		records[0].Identity != LoginIdentity(testStorageState("NEW")) {
		t.Fatalf("通知 = %+v，期望写盘一条带新登录签名的可用记录", records)
	}
	lease, err = service.pool.AcquireFor(ctx, AccountSelection{AccountID: "login-b@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(testStorageState("NEWER")); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	// DriveCandidates 按选择条件短时缓存（见 driveCandidatesCacheTTL），这里看不缓存的汇总
	if summary := service.DriveSummary(); summary != (DriveCounts{Unknown: 2}) {
		t.Fatalf("认证代际推进后不应沿用记录: %+v", summary)
	}
}

// TestDriveCandidatesExcludeUnschedulable 候选集只含启用、就绪的账户：都需要登录时为空（app 层据此不按 Drive 状态限定，交给普通选号
// 报号池暂时不可用），汇总计数仍按启用账户统计
func TestDriveCandidatesExcludeUnschedulable(t *testing.T) {
	service, _ := driveTestService(t, "auth-a@example.com", "auth-b@example.com")
	for _, accountID := range []string{"auth-a@example.com", "auth-b@example.com"} {
		if err := service.pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	got := service.DriveCandidates(context.Background(), AccountSelection{ModelID: ceilingModel, Method: "generateContent"})
	if len(got.Available)+len(got.Unavailable)+len(got.Unknown) != 0 {
		t.Fatalf("需要登录的账户不应在候选集里: %+v", got)
	}
	if summary := service.DriveSummary(); summary != (DriveCounts{Unknown: 2}) {
		t.Fatalf("汇总 = %+v", summary)
	}
}

// TestInteractionModelTurnRestoredLink 从本服务链接恢复出来、带链接文本的模型消息媒体按链接文本算模型轮次：上一轮回复只有视频链接、
// 恢复后只剩视频时同样是多轮，视频任务返回官网提示，与换回链接文本后的结果一致；同一条消息里已有文本链接（RestoredText 为空）、
// 只带签名的恢复媒体不单独算；客户端回传的模型生成文件（没有恢复标记）照旧不算
func TestInteractionModelTurnRestoredLink(t *testing.T) {
	link := "[video](https://ai.example.com/media/" + strings.Repeat("ab", 32) + ".mp4)"
	model := Model{ID: "gemini-omni-1.1-flash", CapabilityOptions: map[string][]string{"video_tasks": {"edit", "extend"}}}
	contents := func(assistant ...Part) []Content {
		return []Content{
			{Role: RoleUser, Parts: []Part{{Text: "做个海边视频"}}},
			{Role: RoleAssistant, Parts: assistant},
			{Role: RoleUser, Parts: []Part{{Text: "把天空换成晚霞"}, {File: &FileRef{ID: "drive-video", MIME: "video/mp4"}}}},
		}
	}
	for _, test := range []struct {
		name      string
		assistant []Part
		modelTurn bool
	}{
		{name: "恢复的视频", assistant: []Part{{InlineData: &Blob{MIME: "video/mp4", Data: []byte("v")}, RestoredLink: true, RestoredText: link}}, modelTurn: true},
		{name: "上传后的恢复视频", assistant: []Part{{File: &FileRef{ID: "drive-restored", MIME: "video/mp4"}, RestoredLink: true, RestoredText: link}}, modelTurn: true},
		{name: "换回的链接文本", assistant: []Part{{Text: link}}, modelTurn: true},
		{name: "只带签名的恢复媒体", assistant: []Part{{InlineData: &Blob{MIME: "video/mp4", Data: []byte("v")}, RestoredLink: true, ThoughtSignature: "sig"}}},
		{name: "客户端回传的生成文件", assistant: []Part{{File: &FileRef{ID: "drive-generated", MIME: "video/mp4"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			history := contents(test.assistant...)
			if got := InteractionHasModelTurn(history); got != test.modelTurn {
				t.Fatalf("InteractionHasModelTurn = %t，期望 %t", got, test.modelTurn)
			}
			_, _, err := NormalizeVideoConfig(&VideoConfig{Task: "edit"}, model, history)
			if test.modelTurn && (err == nil || err.Error() != videoTaskFirstTurnMessage) {
				t.Fatalf("多轮对话指定任务应返回官网提示，得到 %v", err)
			}
			if !test.modelTurn && err != nil {
				t.Fatalf("不算模型轮次时任务应照常发送: %v", err)
			}
		})
	}
}
