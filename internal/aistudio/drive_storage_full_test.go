package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Drive 存储空间已满（上游 Mag1cFall/AIStudio2API efda79b、e6f067a）：DriveUpload 返回 403、正文 reason 为 storageQuotaExceeded 时
// 附件全部保留 inline data 随生成请求内联发送，不冷却、不计入认证失败；按账号记住存储空间已满（与没有 Drive 授权分开），
// 换到 Drive token 不清除，上传成功时清除

// driveStorageFullBody 为 Drive v3 上传在存储空间已满时返回的错误正文（Drive API 公开文档的格式）
const driveStorageFullBody = `{"error":{"code":403,"message":"The user's Drive storage quota has been exceeded.",` +
	`"errors":[{"message":"The user's Drive storage quota has been exceeded.","domain":"usageLimits","reason":"storageQuotaExceeded"}]}}`

// driveFullTransport 在 driveTokenTransport 上按文件名决定 Drive 上传的结果：failures 里的文件名返回对应错误（读完请求体之后，同 multipart 上传），
// beforeBody 里的账户在读请求体之前返回对应错误（同可续传上传的 DriveUploadInit），其余照常上传成功
type driveFullTransport struct {
	*driveTokenTransport
	mu         sync.Mutex
	failures   map[string]error
	beforeBody map[string]error
	attempts   int
}

func (transport *driveFullTransport) fail(name string, err error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.failures[name] = err
}

func (transport *driveFullTransport) uploadAttempts() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.attempts
}

// errBlockUntilCanceled 作为 failures 的值时，这个文件的上传一直等到上传的 context 被取消（另一个附件失败后取消其余上传），返回取消错误
var errBlockUntilCanceled = errors.New("等到取消")

func (transport *driveFullTransport) UploadDrive(ctx context.Context, accountID string, token string, request UploadRequest) (FileRef, error) {
	transport.mu.Lock()
	transport.attempts++
	err := transport.failures[request.Name]
	early := transport.beforeBody[accountID]
	transport.mu.Unlock()
	if early != nil {
		return FileRef{}, early
	}
	if err == errBlockUntilCanceled {
		<-ctx.Done()
		return FileRef{}, fmt.Errorf("上传 Drive 文件: %w", ctx.Err())
	}
	if err != nil {
		if _, copyErr := io.Copy(io.Discard, request.Reader); copyErr != nil {
			return FileRef{}, copyErr
		}
		return FileRef{}, err
	}
	return transport.driveTokenTransport.UploadDrive(ctx, accountID, token, request)
}

// driveFullTestService 返回上游为 driveFullTransport 的号池服务（账户与 driveTestService 相同）
func driveFullTestService(t *testing.T, accountIDs ...string) (*PooledService, *driveFullTransport) {
	t.Helper()
	service, tokens := driveTestService(t, accountIDs...)
	transport := &driveFullTransport{driveTokenTransport: tokens, failures: make(map[string]error), beforeBody: make(map[string]error)}
	service.client.transport = transport
	return service, transport
}

// twoInlineImages 返回带两张内联图片的用户消息
func twoInlineImages() []Content {
	return []Content{{Role: RoleUser, Parts: []Part{
		{Text: "看图"}, {InlineData: &Blob{MIME: "image/png", Data: []byte{1, 2, 3}}}, {InlineData: &Blob{MIME: "image/png", Data: []byte{4, 5, 6}}},
	}}}
}

// driveStateOf 返回账户此刻在记忆里的 Drive 状态
func driveStateOf(service *PooledService, lease *AccountLease) DriveState {
	generation, _ := service.driveAuthGeneration(lease)
	return service.drive.states([]driveSnapshot{{account: lease.Account(), generation: generation}}, time.Now())[0]
}

// TestDriveStorageQuotaExceeded 只认 Drive 上传（DriveUpload、DriveUploadInit、DriveUploadChunk）的 403 且正文 reason 为
// "storageQuotaExceeded"：别的状态码、别的 403 原因（限流、服务未启用）、别的方法与只在文字里提到的都不算
func TestDriveStorageQuotaExceeded(t *testing.T) {
	for _, item := range []struct {
		name string
		err  error
		want bool
	}{
		{"DriveUpload 403", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}, true},
		{"包装后的 DriveUploadInit 403", fmt.Errorf("上传: %w", &RPCError{Method: "DriveUploadInit", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}), true},
		{"DriveUploadChunk 403", &RPCError{Method: "DriveUploadChunk", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}, true},
		{"DriveUpload 400", &RPCError{Method: "DriveUpload", StatusCode: http.StatusBadRequest, Message: driveStorageFullBody}, false},
		{"DriveUpload 401", &RPCError{Method: "DriveUpload", StatusCode: http.StatusUnauthorized, Message: driveStorageFullBody}, false},
		{"DriveUpload 403 限流", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`}, false},
		{"DriveUpload 403 服务未启用", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: `{"error":{"errors":[{"reason":"serviceDisabled"}]}}`}, false},
		{"不带引号的文字", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: "storageQuotaExceeded"}, false},
		{"DriveDelete 403", &RPCError{Method: "DriveDelete", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}, false},
		{"生成的 403", &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: driveStorageFullBody}, false},
		{"非 RPC 错误", errors.New(driveStorageFullBody), false},
	} {
		if got := driveStorageQuotaExceeded(item.err); got != item.want {
			t.Fatalf("%s: driveStorageQuotaExceeded = %t，期望 %t", item.name, got, item.want)
		}
	}
}

// TestInlineMediaStorageFullKeepsInline Drive 上传返回 403 storageQuotaExceeded：附件全部保留 inline data（其中一张已上传成功也不引用，
// 它的副本留在临时文件里、随 Cleanup 删除），没有错误、没有冷却、不标需要登录，副本集合 DriveStorageFull 为真；按账号记住存储空间已满
// （不算没有 Drive 授权），之后的附件不再换 token、不再上传，直接内联
func TestInlineMediaStorageFullKeepsInline(t *testing.T) {
	const accountID = "storage-full@example.com"
	service, transport := driveFullTestService(t, accountID)
	transport.fail("inline-media-2", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody})
	ctx := context.Background()
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	contents := twoInlineImages()
	rewritten, temporary, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil)
	if err != nil {
		t.Fatalf("存储空间已满应改为内联发送、不报错，实际 %v", err)
	}
	for index := 1; index <= 2; index++ {
		if part := rewritten[0].Parts[index]; part.InlineData != contents[0].Parts[index].InlineData || part.File != nil {
			t.Fatalf("第 %d 个附件应保留 inline data，实际 %+v", index, part)
		}
	}
	if !temporary.DriveStorageFull() {
		t.Fatal("副本集合应报告 Drive 存储空间已满")
	}
	if temporary.Count() != 1 {
		t.Fatalf("已上传成功的一份应留在临时副本里等清理，实际 %d 份", temporary.Count())
	}
	if err := temporary.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if transport.deletes != 1 {
		t.Fatalf("Cleanup 应删除已上传的一份，实际删除 %d 次", transport.deletes)
	}
	assertNoCooldown(t, service.pool, accountID)
	if state := driveStateOf(service, lease); state != DriveStateStorageFull {
		t.Fatalf("应记住存储空间已满，实际 %s", state)
	}
	if counts := service.DriveSummary(); counts.StorageFull != 1 || counts.Unavailable != 0 {
		t.Fatalf("计数应把它算作空间已满、不算没有授权：%+v", counts)
	}
	if candidates := service.DriveCandidates(ctx, AccountSelection{ModelID: ceilingModel, Method: "generateContent"}); len(candidates.Unavailable) != 1 || len(candidates.Available) != 0 {
		t.Fatalf("Interaction 选号应把空间已满的账号当作传不上 Drive：%+v", candidates)
	}

	tokens, uploads := transport.tokenCalls(accountID), transport.uploadAttempts()
	rewritten, temporary, err = service.UploadInlineMediaToLease(ctx, lease, contents, nil)
	if err != nil || rewritten[0].Parts[1].InlineData == nil || rewritten[0].Parts[2].InlineData == nil || !temporary.DriveStorageFull() {
		t.Fatalf("记着空间已满时应直接内联：err=%v 内容=%+v", err, rewritten)
	}
	if transport.tokenCalls(accountID) != tokens || transport.uploadAttempts() != uploads {
		t.Fatal("记着空间已满时不应再换 token 或上传")
	}
}

// TestInlineMediaStorageFullMixedFailure 只有部分上传是空间已满、另有别的失败（限流 403、5xx）时按原规则返回错误；
// 其他 403（rateLimitExceeded）同样返回错误、不记空间已满
func TestInlineMediaStorageFullMixedFailure(t *testing.T) {
	for _, item := range []struct {
		name     string
		failures map[string]error
	}{
		{"空间已满加 500", map[string]error{
			"inline-media-1": &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody},
			"inline-media-2": &RPCError{Method: "DriveUpload", StatusCode: http.StatusInternalServerError, Message: "backend error"},
		}},
		{"限流 403", map[string]error{
			"inline-media-1": &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`},
		}},
	} {
		t.Run(item.name, func(t *testing.T) {
			const accountID = "mixed@example.com"
			service, transport := driveFullTestService(t, accountID)
			for name, err := range item.failures {
				transport.fail(name, err)
			}
			lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if _, _, err := service.UploadInlineMediaToLease(context.Background(), lease, twoInlineImages(), nil); err == nil {
				t.Fatal("不全是空间已满时应返回上传错误")
			}
			if state := driveStateOf(service, lease); state == DriveStateStorageFull {
				t.Fatal("不应记成空间已满")
			}
		})
	}
}

// TestDriveStorageFullMemory 记着空间已满的账号换到 Drive token 不清除记录（空间满与授权无关）；文件上传（Files API）成功时清除，
// 改记 Drive 可用；文件上传返回空间已满时同样记下；管理验证通过（ForgetDriveUnavailable）清除
func TestDriveStorageFullMemory(t *testing.T) {
	const accountID = "full-memory@example.com"
	service, transport := driveFullTestService(t, accountID)
	ctx := context.Background()
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	generation, _ := service.driveAuthGeneration(lease)
	service.noteDriveUpload(lease, generation, true, false, true)
	if state := driveStateOf(service, lease); state != DriveStateStorageFull {
		t.Fatalf("应记下空间已满，实际 %s", state)
	}
	if _, err := service.accessTokenForLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if state := driveStateOf(service, lease); state != DriveStateStorageFull {
		t.Fatalf("换到 Drive token 不应清除空间已满，实际 %s", state)
	}
	upload := func() error {
		_, _, err := service.uploadFileForLease(ctx, lease, UploadRequest{
			AccountID: accountID, Name: "notes.txt", MIME: "text/plain", Size: 5, Reader: strings.NewReader("hello"),
		})
		return err
	}
	if err := upload(); err != nil {
		t.Fatal(err)
	}
	if state := driveStateOf(service, lease); state != DriveStateAvailable {
		t.Fatalf("上传成功应清除空间已满、记为可用，实际 %s", state)
	}
	transport.fail("notes.txt", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody})
	if err := upload(); err == nil {
		t.Fatal("文件上传遇到空间已满应照旧返回错误")
	}
	if state := driveStateOf(service, lease); state != DriveStateStorageFull {
		t.Fatalf("文件上传返回空间已满时应记下，实际 %s", state)
	}
	service.ForgetDriveUnavailable(accountID)
	if state := driveStateOf(service, lease); state != DriveStateUnknown {
		t.Fatalf("管理验证通过应清除空间已满，实际 %s", state)
	}
}

// TestDriveStorageFullRecordRestore 空间已满的记录经写盘回调上报为 DriveStateStorageFull，重启后按原状态恢复
func TestDriveStorageFullRecordRestore(t *testing.T) {
	const accountID = "full-restore@example.com"
	service, _ := driveFullTestService(t, accountID)
	var records []DriveRecord
	service.SetDriveObserver(func(record DriveRecord) { records = append(records, record) })
	lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := service.driveAuthGeneration(lease)
	service.noteDriveUpload(lease, generation, true, false, true)
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != DriveStateStorageFull || records[0].Identity == "" {
		t.Fatalf("应上报一条空间已满的记录，实际 %+v", records)
	}
	restored, _ := driveFullTestService(t, accountID)
	if count := restored.RestoreDriveRecords(records); count != 1 {
		t.Fatalf("应恢复 1 条，实际 %d", count)
	}
	if counts := restored.DriveSummary(); counts.StorageFull != 1 {
		t.Fatalf("恢复后应记着空间已满：%+v", counts)
	}
}

// TestInlineMediaStorageFullCancelsOthers 一个附件空间已满、其余上传因此被取消（取消错误不算别的失败）：照样整批内联发送、不报错
func TestInlineMediaStorageFullCancelsOthers(t *testing.T) {
	const accountID = "full-cancel@example.com"
	service, transport := driveFullTestService(t, accountID)
	transport.fail("inline-media-1", errBlockUntilCanceled)
	transport.fail("inline-media-2", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody})
	lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	rewritten, temporary, err := service.UploadInlineMediaToLease(context.Background(), lease, twoInlineImages(), nil)
	if err != nil || rewritten[0].Parts[1].InlineData == nil || rewritten[0].Parts[2].InlineData == nil || !temporary.DriveStorageFull() {
		t.Fatalf("其余上传被取消时仍应整批内联：err=%v 内容=%+v", err, rewritten)
	}
	if temporary.Count() != 0 {
		t.Fatalf("没有上传成功的副本，实际 %d 份", temporary.Count())
	}
}

// TestTranscribeDriveStorageFullNoCooldown 转写上传时 Drive 存储空间已满（403 storageQuotaExceeded）：与没有 Drive 授权一样不写冷却
// （原先落到其余失败、写 30 秒全局冷却，连带挡住这个账户所有模型的生成，冷却到期后下一个转写又选中它再冷却），本请求换号时排除它，
// 记下存储空间已满；别的请求照常能取得它
func TestTranscribeDriveStorageFullNoCooldown(t *testing.T) {
	accounts := []string{"t-0@example.com", "t-1@example.com"}
	service, transport := driveFullTestService(t, accounts...)
	full := &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}
	transport.fail("a.wav", full)
	audio := []byte("RIFF0000WAVE")
	_, err := service.Transcribe(context.Background(), TranscriptionRequest{
		Model: ceilingModel, Name: "a.wav", MIME: "audio/wav", Size: int64(len(audio)), Reader: strings.NewReader(string(audio)),
	})
	if !driveStorageQuotaExceeded(err) {
		t.Fatalf("两个账户 Drive 都满时应以存储空间已满结束，实际 %v", err)
	}
	if attempts := transport.uploadAttempts(); attempts != 2 {
		t.Fatalf("上传 %d 次，期望每个账户各 1 次（换号排除已失败的账户）", attempts)
	}
	for _, accountID := range accounts {
		assertNoCooldown(t, service.pool, accountID)
		lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID, ModelID: ceilingModel, Method: "generateContent"})
		if err != nil {
			t.Fatalf("%s：Drive 空间已满的账户不应被其他请求避开: %v", accountID, err)
		}
		if state := driveStateOf(service, lease); state != DriveStateStorageFull {
			t.Fatalf("%s：Drive 状态 = %s，期望记下存储空间已满", accountID, state)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestUploadFileDriveStorageFullSwitches 文件 API 自动选号的上传：选中的账户 Drive 存储空间已满、还没读请求体（可续传上传的
// DriveUploadInit 返回 403 storageQuotaExceeded）时换下一个候选完成上传，失败的账户不写冷却、记下存储空间已满，之后的上传把它排在最后；
// 已经读了请求体（multipart 上传）时不能重读，照旧返回这个错误
func TestUploadFileDriveStorageFullSwitches(t *testing.T) {
	const fullID, okID = "a-full@example.com", "b-ok@example.com"
	service, transport := driveFullTestService(t, fullID, okID)
	transport.beforeBody[fullID] = &RPCError{Method: "DriveUploadInit", StatusCode: http.StatusForbidden, Message: driveStorageFullBody}
	upload := func(name string) (FileRef, error) {
		data := []byte("hello")
		return service.UploadFile(context.Background(), UploadRequest{
			Name: name, MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: strings.NewReader(string(data)),
		})
	}
	file, err := upload("a.txt")
	if err != nil {
		t.Fatalf("应换到 %s 完成上传: %v", okID, err)
	}
	if owner, _, err := service.pool.fileReferenceMetadata(context.Background(), file.ID, ""); err != nil || owner != okID {
		t.Fatalf("文件应绑定到 %s，实际 %s（err=%v）", okID, owner, err)
	}
	if attempts := transport.uploadAttempts(); attempts != 2 {
		t.Fatalf("上传 %d 次，期望空间已满的账户 1 次、换号后 1 次", attempts)
	}
	assertNoCooldown(t, service.pool, fullID)
	// 之后的上传把空间已满的账户排在最后，不再先在它上面白试一次
	if _, err := upload("b.txt"); err != nil || transport.uploadAttempts() != 3 {
		t.Fatalf("第二次上传：err=%v 上传次数=%d，期望直接在 %s 上传 1 次", err, transport.uploadAttempts(), okID)
	}

	// 只有这一个候选、已经读了请求体：不能换号重读，照旧返回存储空间已满
	single, singleTransport := driveFullTestService(t, fullID)
	singleTransport.fail("c.txt", &RPCError{Method: "DriveUpload", StatusCode: http.StatusForbidden, Message: driveStorageFullBody})
	data := []byte("hello")
	if _, err := single.UploadFile(context.Background(), UploadRequest{
		Name: "c.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: strings.NewReader(string(data)),
	}); !driveStorageQuotaExceeded(err) {
		t.Fatalf("读了请求体之后应照旧返回存储空间已满，实际 %v", err)
	}
	assertNoCooldown(t, single.pool, fullID)
}
