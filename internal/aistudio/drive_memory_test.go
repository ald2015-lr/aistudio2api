package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 账号没有 Drive 授权（GenerateAccessToken 401 / Code 16）的处理：内联附件改为内联发送、不冷却；按账号记住“Drive 不可用”（带认证代际与 TTL）；
// 其他需要 Drive 的路径只让这次尝试失败、不写全局冷却

// driveMissingCredential 为线上 GenerateAccessToken 401 的原文（不是 unauthorized_client）
const driveMissingCredential = "Request is missing required authentication credential. Expected OAuth 2 access token, login cookie or other valid authentication credential."

// driveTokenTransport 按账户返回 GenerateAccessToken 的结果：denied 中的账户返回 HTTP 401、协议 Code 16（文案为对应的值），
// login 中的账户返回签名 Cookie 失效（ErrAuthenticationRequired），其余返回 bearer token。Drive 上传读完数据后返回递增的文件 ID，
// 下载返回固定内容，删除直接成功。记录各账户换 token 的次数
type driveTokenTransport struct {
	mu      sync.Mutex
	denied  map[string]string
	login   map[string]bool
	tokens  map[string]int
	uploads int
}

func newDriveTokenTransport() *driveTokenTransport {
	return &driveTokenTransport{denied: make(map[string]string), login: make(map[string]bool), tokens: make(map[string]int)}
}

func (transport *driveTokenTransport) deny(accountID string, message string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.denied[accountID] = message
}

func (transport *driveTokenTransport) allow(accountID string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	delete(transport.denied, accountID)
}

func (transport *driveTokenTransport) tokenCalls(accountID string) int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.tokens[accountID]
}

func (transport *driveTokenTransport) Do(_ context.Context, request RPCRequest) (*RPCResponse, error) {
	if request.Method != "GenerateAccessToken" {
		return nil, fmt.Errorf("不应调用 %s", request.Method)
	}
	transport.mu.Lock()
	transport.tokens[request.AccountID]++
	message, denied := transport.denied[request.AccountID]
	login := transport.login[request.AccountID]
	transport.mu.Unlock()
	if login {
		return nil, fmt.Errorf("签名 AI Studio 请求: %w", ErrAuthenticationRequired)
	}
	header := http.Header{}
	header.Set("Content-Type", JSONProtobufContentType)
	if denied {
		body, err := json.Marshal([]any{16, message})
		if err != nil {
			return nil, err
		}
		return &RPCResponse{StatusCode: http.StatusUnauthorized, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
	return &RPCResponse{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`["bearer-token"]`))}, nil
}

func (transport *driveTokenTransport) UploadDrive(_ context.Context, _ string, _ string, request UploadRequest) (FileRef, error) {
	if _, err := io.Copy(io.Discard, request.Reader); err != nil {
		return FileRef{}, err
	}
	transport.mu.Lock()
	transport.uploads++
	id := fmt.Sprintf("drive-file-%d", transport.uploads)
	transport.mu.Unlock()
	return FileRef{ID: id, Name: request.Name, MIME: request.MIME}, nil
}

func (transport *driveTokenTransport) DownloadDrive(context.Context, string, string, string) (MediaStream, error) {
	return MediaStream{Body: io.NopCloser(strings.NewReader("hello")), MIME: "text/plain", Size: 5}, nil
}

func (transport *driveTokenTransport) DeleteDrive(context.Context, string, string, string) error {
	return nil
}

// driveTestService 返回 accountIDs 账户的号池服务，上游为 driveTokenTransport；账户目录带 ceilingModel（转写也能选中）
func driveTestService(t *testing.T, accountIDs ...string) (*PooledService, *driveTokenTransport) {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	model := Model{
		ID: ceilingModel, Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
	}
	for _, accountID := range accountIDs {
		account, publish, err := store.Create(DefaultAccountConfig(accountID), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{model}); err != nil {
			t.Fatal(err)
		}
	}
	transport := newDriveTokenTransport()
	client := &Client{transport: transport, catalogs: make(map[string]modelCatalog), tiers: make(map[string]BenefitTier)}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return service, transport
}

// inlineImageContents 返回带一张内联图片的用户消息与这张图片的 Blob
func inlineImageContents() ([]Content, *Blob) {
	blob := &Blob{MIME: "image/png", Data: []byte{1, 2, 3}}
	return []Content{{Role: RoleUser, Parts: []Part{{Text: "看图"}, {InlineData: blob}}}}, blob
}

// assertNoCooldown 确认账户没有任何冷却（全局或模型级）、没有被标为需要登录
func assertNoCooldown(t *testing.T, pool *AccountPool, accountID string) {
	t.Helper()
	status, ok := pool.StatusOf(accountID)
	if !ok {
		t.Fatalf("账户 %s 不存在", accountID)
	}
	if status.State == AccountAuthRequired {
		t.Fatalf("账户 %s 不应被标为需要登录：%s", accountID, status.Message)
	}
	if pool.AccountCoolingDown(accountID, "") || len(status.Cooldowns) != 0 {
		t.Fatalf("账户 %s 不应有冷却：%+v", accountID, status.Cooldowns)
	}
}

// uploadInline 在 lease 上处理一次内联附件，返回改写后的内容与临时文件（调用方负责 Cleanup）
func uploadInline(t *testing.T, service *PooledService, lease *AccountLease) ([]Content, *Blob, *TemporaryFileCopies) {
	t.Helper()
	contents, blob := inlineImageContents()
	rewritten, temporary, err := service.UploadInlineMediaToLease(context.Background(), lease, contents, nil)
	if err != nil {
		t.Fatalf("内联附件处理返回错误: %v", err)
	}
	return rewritten, blob, temporary
}

// TestDriveUnavailableError GenerateAccessToken 的 401 / Code 16（含 unauthorized_client 与其他原文）都算 Drive 不可用；
// 登录失效（ErrAuthenticationRequired）、其他方法的 401 与 GenerateAccessToken 的其他错误不算
func TestDriveUnavailableError(t *testing.T) {
	for _, item := range []struct {
		name string
		err  error
		want bool
	}{
		{"缺少认证凭据", fmt.Errorf("发送: %w", DecodeRPCError("GenerateAccessToken", http.StatusUnauthorized, []byte(`[16,"`+driveMissingCredential+`"]`))), true},
		{"unauthorized_client", DecodeRPCError("GenerateAccessToken", http.StatusUnauthorized, []byte(`[16,"OAuth error: unauthorized_client."]`)), true},
		{"只有 HTTP 401", &RPCError{Method: "GenerateAccessToken", StatusCode: http.StatusUnauthorized}, true},
		{"签名 Cookie 缺失", fmt.Errorf("发送 AI Studio GenerateAccessToken: %w", ErrAuthenticationRequired), false},
		{"生成的 401", &RPCError{Method: "GenerateContent", StatusCode: http.StatusUnauthorized, Code: 16}, false},
		{"Drive 上传的 401", &RPCError{Method: "DriveUpload", StatusCode: http.StatusUnauthorized}, false},
		{"换 token 的 500", &RPCError{Method: "GenerateAccessToken", StatusCode: http.StatusInternalServerError, Code: 13}, false},
	} {
		if got := driveUnavailableError(item.err); got != item.want {
			t.Fatalf("%s: driveUnavailableError = %t，期望 %t（err=%v）", item.name, got, item.want, item.err)
		}
	}
}

// TestInlineMediaDriveUnavailableKeepsInline 内联附件换 token 返回 401 / Code 16（线上原文与 unauthorized_client 两种）：
// 返回改写前的 inline data Part、没有错误、不建临时文件，账户没有任何冷却、不标需要登录，并记住 Drive 不可用（有效期 6 小时）
func TestInlineMediaDriveUnavailableKeepsInline(t *testing.T) {
	for _, message := range []string{driveMissingCredential, "OAuth error: unauthorized_client."} {
		t.Run(message[:12], func(t *testing.T) {
			const accountID = "inline@example.com"
			service, transport := driveTestService(t, accountID)
			transport.deny(accountID, message)
			lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			rewritten, blob, temporary := uploadInline(t, service, lease)
			if len(rewritten) != 1 || len(rewritten[0].Parts) != 2 || rewritten[0].Parts[1].InlineData != blob || rewritten[0].Parts[1].File != nil ||
				rewritten[0].Parts[0].Text != "看图" {
				t.Fatalf("应原样保留 inline data Part，实际 %+v", rewritten)
			}
			if temporary.Count() != 0 {
				t.Fatalf("内联发送不应创建临时 Drive 文件：%d 个", temporary.Count())
			}
			assertNoCooldown(t, service.pool, accountID)
			if calls := transport.tokenCalls(accountID); calls != 1 {
				t.Fatalf("GenerateAccessToken 调用 %d 次，期望 1 次", calls)
			}
			service.drive.mu.Lock()
			entry, remembered := service.drive.entries[accountID]
			service.drive.mu.Unlock()
			if !remembered || entry.account != lease.Account() || entry.generation != service.pool.AuthGeneration(accountID) {
				t.Fatalf("应按当前认证代际记住 Drive 不可用：%+v（记住=%t）", entry, remembered)
			}
			if remaining := time.Until(entry.until); remaining <= driveUnavailableTTL-time.Minute || remaining > driveUnavailableTTL {
				t.Fatalf("记忆剩余 %s，期望约 %s", remaining, driveUnavailableTTL)
			}
		})
	}
}

// TestInlineMediaDriveMemory 记着 Drive 不可用的账户之后的内联附件不再调 GenerateAccessToken；认证代际变化（保存新的登录）或 TTL 过期后
// 重新探测；换 token 成功（文件上传等任何路径）清除记忆，之后的内联附件照常上传为 Drive 文件；ForgetDriveUnavailable 同样清除
func TestInlineMediaDriveMemory(t *testing.T) {
	const accountID = "memory@example.com"
	service, transport := driveTestService(t, accountID)
	transport.deny(accountID, driveMissingCredential)
	ctx := context.Background()
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	expectInline := func(step string, wantCalls int) {
		t.Helper()
		rewritten, blob, _ := uploadInline(t, service, lease)
		if rewritten[0].Parts[1].InlineData != blob {
			t.Fatalf("%s：应保留 inline data，实际 %+v", step, rewritten[0].Parts[1])
		}
		if calls := transport.tokenCalls(accountID); calls != wantCalls {
			t.Fatalf("%s：GenerateAccessToken 累计调用 %d 次，期望 %d 次", step, calls, wantCalls)
		}
		assertNoCooldown(t, service.pool, accountID)
	}
	expireMemory := func() {
		service.drive.mu.Lock()
		entry := service.drive.entries[accountID]
		entry.until = time.Now().Add(-time.Second)
		service.drive.entries[accountID] = entry
		service.drive.mu.Unlock()
	}
	expectInline("第一次（401）", 1)
	expectInline("第二次（记忆命中）", 1)
	expectInline("第三次（记忆命中）", 1)
	// 保存新的登录状态（重新登录、更新认证文件；续签同样推进代际）：记录失效，重新探测
	if err := lease.SaveStorageState(testStorageState("NEW")); err != nil {
		t.Fatal(err)
	}
	expectInline("认证代际变化后", 2)
	expectInline("新代际记忆命中", 2)
	expireMemory()
	expectInline("TTL 过期后", 3)

	// 别的路径换 token 成功（指定这个账户的文件上传）清除记忆：之后的内联附件重新探测并上传为 Drive 文件
	transport.allow(accountID)
	data := []byte("hello")
	if _, err := service.UploadFile(ContextWithAccountLease(ctx, lease), UploadRequest{
		AccountID: accountID, Name: "a.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if service.drive.count() != 0 {
		t.Fatal("换 token 成功后应清除 Drive 不可用记忆")
	}
	rewritten, _, temporary := uploadInline(t, service, lease)
	if part := rewritten[0].Parts[1]; part.InlineData != nil || part.File == nil || part.File.ID == "" {
		t.Fatalf("Drive 可用时应改写为 Drive 文件引用，实际 %+v", part)
	}
	if temporary.Count() != 1 {
		t.Fatalf("应创建 1 个临时 Drive 文件，实际 %d", temporary.Count())
	}
	if err := temporary.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if calls := transport.tokenCalls(accountID); calls != 5 {
		t.Fatalf("GenerateAccessToken 累计调用 %d 次，期望 5 次（上传 1 次、内联 1 次）", calls)
	}

	// 人工恢复（管理验证通过）清除记忆
	transport.deny(accountID, driveMissingCredential)
	expectInline("再次 401", 6)
	expectInline("记忆命中", 6)
	service.ForgetDriveUnavailable(accountID)
	expectInline("ForgetDriveUnavailable 之后", 7)
}

// TestDriveMemoryAccountIdentity 账户删除后以同一 ID 重新添加是另一个对象（认证代际从 0 重新计）：不沿用旧对象的记录；
// 代际不同、过期的记录同样不算，并被清掉
func TestDriveMemoryAccountIdentity(t *testing.T) {
	var memory driveMemory
	now := time.Now()
	original := &Account{ID: "same@example.com"}
	readded := &Account{ID: "same@example.com"}
	memory.remember(original, 0, now)
	if !memory.unavailable(original, 0, now) {
		t.Fatal("同一账户对象、同一代际应命中")
	}
	if memory.unavailable(readded, 0, now) {
		t.Fatal("重新添加的账户对象不应沿用旧记录")
	}
	if memory.count() != 0 {
		t.Fatal("对不上的记录应被清掉")
	}
	memory.remember(original, 3, now)
	if memory.unavailable(original, 4, now) || memory.count() != 0 {
		t.Fatal("认证代际变化后不应命中")
	}
	memory.remember(original, 3, now)
	if memory.unavailable(original, 3, now.Add(driveUnavailableTTL)) || memory.count() != 0 {
		t.Fatal("过期后不应命中")
	}
}

// TestDriveMemoryPrune 条目达到清理门槛时清掉过期的条目，门槛随剩余条目数翻倍（不会每次写入都扫描全部条目）
func TestDriveMemoryPrune(t *testing.T) {
	var memory driveMemory
	now := time.Now()
	for index := range driveMemoryPruneMin {
		memory.remember(&Account{ID: fmt.Sprintf("old-%d@example.com", index)}, 0, now.Add(-driveUnavailableTTL))
	}
	memory.remember(&Account{ID: "new@example.com"}, 0, now)
	if got := memory.count(); got != 1 {
		t.Fatalf("清理后剩 %d 条，期望只剩新写入的 1 条", got)
	}
	if memory.pruneAt != 0 {
		t.Fatalf("清理后门槛 = %d，期望按清理后的 0 条计（下次仍以 driveMemoryPruneMin 为准）", memory.pruneAt)
	}
}

// TestInlineMediaLoginFailureStillAuth 换 token 时签名 Cookie 失效（ErrAuthenticationRequired）仍是登录失效：返回错误、按认证失败标记账户，
// 不按 Drive 不可用改为内联，也不记 Drive 记忆
func TestInlineMediaLoginFailureStillAuth(t *testing.T) {
	const accountID = "login@example.com"
	service, transport := driveTestService(t, accountID)
	transport.mu.Lock()
	transport.login[accountID] = true
	transport.mu.Unlock()
	lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	contents, _ := inlineImageContents()
	rewritten, _, uploadErr := service.UploadInlineMediaToLease(context.Background(), lease, contents, nil)
	if rewritten != nil || !errors.Is(uploadErr, ErrAuthenticationRequired) || !DefinitiveAuthenticationFailure(uploadErr) {
		t.Fatalf("签名 Cookie 失效应按登录失效返回错误，实际 contents=%v err=%v", rewritten, uploadErr)
	}
	if status, _ := service.pool.StatusOf(accountID); status.State != AccountAuthRequired {
		t.Fatalf("账户应被标为需要登录，实际 %s", status.State)
	}
	if service.drive.count() != 0 {
		t.Fatal("登录失效不应记成 Drive 不可用")
	}
}

// TestUploadFileDriveUnavailableSwitches 文件 API 自动选号的上传：第一个账户换 token 401 时这次尝试失败、换到下一个账户完成上传；
// 失败的账户不写任何冷却，别的请求照常能取得它，只记 Drive 不可用；之后的上传先试没有记录的账户。
// 下载 Drive 文件时换 token 401 同样只让这次下载失败，账户不冷却
func TestUploadFileDriveUnavailableSwitches(t *testing.T) {
	const denied, granted = "a-denied@example.com", "b-granted@example.com"
	service, transport := driveTestService(t, denied, granted)
	transport.deny(denied, driveMissingCredential)
	upload := func() FileRef {
		t.Helper()
		data := []byte("hello")
		file, err := service.UploadFile(context.Background(), UploadRequest{
			Name: "a.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: bytes.NewReader(data),
		})
		if err != nil {
			t.Fatalf("上传失败: %v", err)
		}
		return file
	}
	file := upload()
	if owner, _, err := service.pool.fileReferenceMetadata(context.Background(), file.ID, ""); err != nil || owner != granted {
		t.Fatalf("文件应绑定到 %s，实际 %s（err=%v）", granted, owner, err)
	}
	if transport.tokenCalls(denied) != 1 || transport.tokenCalls(granted) != 1 {
		t.Fatalf("换 token 次数 %s=%d %s=%d，期望各 1 次", denied, transport.tokenCalls(denied), granted, transport.tokenCalls(granted))
	}
	assertNoCooldown(t, service.pool, denied)
	if service.drive.count() != 1 {
		t.Fatalf("应只记住 %s 的 Drive 不可用，实际 %d 条", denied, service.drive.count())
	}
	// 别的请求（如纯文字生成）照常能取得这个账户
	lease, err := service.pool.AcquireFor(context.Background(), AccountSelection{AccountID: denied, ModelID: ceilingModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("Drive 不可用的账户不应被其他请求避开: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	// 之后的上传先试没有记录的账户：不再在 Drive 不可用的账户上白试一次
	upload()
	if transport.tokenCalls(denied) != 1 || transport.tokenCalls(granted) != 2 {
		t.Fatalf("第二次上传换 token 次数 %s=%d %s=%d，期望 %s 不再被先试", denied, transport.tokenCalls(denied), granted,
			transport.tokenCalls(granted), denied)
	}

	transport.deny(granted, driveMissingCredential)
	if _, err := service.DownloadFile(context.Background(), file.ID); !AccessTokenAuthFailure(err) {
		t.Fatalf("下载应以 GenerateAccessToken 授权失败结束，实际 %v", err)
	}
	assertNoCooldown(t, service.pool, granted)
	if service.drive.count() != 2 {
		t.Fatalf("下载失败后应记住两个账户的 Drive 不可用，实际 %d 条", service.drive.count())
	}
}

// TestTranscribeDriveUnavailableNoCooldown 转写上传时换 token 401：这次尝试失败、换号时在本请求内排除失败的账户（不会选回它），
// 账户不写冷却（原先写 30 秒全局冷却），只记 Drive 不可用
func TestTranscribeDriveUnavailableNoCooldown(t *testing.T) {
	accounts := []string{"t-0@example.com", "t-1@example.com"}
	service, transport := driveTestService(t, accounts...)
	for _, accountID := range accounts {
		transport.deny(accountID, driveMissingCredential)
	}
	audio := []byte("RIFF0000WAVE")
	_, err := service.Transcribe(context.Background(), TranscriptionRequest{
		Model: ceilingModel, Name: "a.wav", MIME: "audio/wav", Size: int64(len(audio)), Reader: bytes.NewReader(audio),
	})
	if !AccessTokenAuthFailure(err) {
		t.Fatalf("两个账户都没有 Drive 授权时应以 GenerateAccessToken 授权失败结束，实际 %v", err)
	}
	for _, accountID := range accounts {
		if calls := transport.tokenCalls(accountID); calls != 1 {
			t.Fatalf("%s 换 token %d 次，期望 1 次（换号排除已失败的账户）", accountID, calls)
		}
		assertNoCooldown(t, service.pool, accountID)
	}
	if service.drive.count() != 2 {
		t.Fatalf("应记住两个账户的 Drive 不可用，实际 %d 条", service.drive.count())
	}
}

// TestInlineDataPartEncoding 改为内联发送时 Playground 的 GenerateContent 按 Part field 3（Blob：MIME、Base64 数据）编码 inline data，
// 与原先 unauthorized_client 的回退相同；整段内容（文字加内联图片）能正常编码
func TestInlineDataPartEncoding(t *testing.T) {
	data := []byte{1, 2, 3}
	wire, err := encodePart(Part{InlineData: &Blob{MIME: "image/png", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	blob, ok := wire[2].([]any)
	if len(wire) != 3 || wire[0] != nil || wire[1] != nil || !ok || len(blob) != 2 ||
		blob[0] != "image/png" || blob[1] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("inline data wire = %#v", wire)
	}
	if _, err := encodePart(Part{InlineData: &Blob{MIME: "image/png"}}); err == nil {
		t.Fatal("没有数据的 inline data 应报错")
	}
	contents, _ := inlineImageContents()
	if encoded, err := encodeContents(contents); err != nil || len(encoded) != 1 {
		t.Fatalf("带内联图片的内容编码失败: %v（%#v）", err, encoded)
	}
}

// TestDeleteFileDriveUnavailableNoCooldown 删除 Drive 文件时换 token 返回 401：这次删除失败、文件绑定保留，账户不写任何冷却，
// 别的请求照常取得它，只记 Drive 不可用；Drive 恢复后删除成功并清除记忆
func TestDeleteFileDriveUnavailableNoCooldown(t *testing.T) {
	const accountID = "delete@example.com"
	service, transport := driveTestService(t, accountID)
	ctx := context.Background()
	data := []byte("hello")
	file, err := service.UploadFile(ctx, UploadRequest{
		Name: "a.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	transport.deny(accountID, driveMissingCredential)
	if err := service.DeleteFile(ctx, file.ID); !AccessTokenAuthFailure(err) || DefinitiveAuthenticationFailure(err) {
		t.Fatalf("删除应以 GenerateAccessToken 授权失败结束（不算登录失效），实际 %v", err)
	}
	assertNoCooldown(t, service.pool, accountID)
	if service.drive.count() != 1 {
		t.Fatalf("删除失败后应记住 Drive 不可用，实际 %d 条", service.drive.count())
	}
	if _, err := service.FileMetadata(ctx, file.ID); err != nil {
		t.Fatalf("删除失败时文件绑定应保留: %v", err)
	}
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accountID, ModelID: ceilingModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("Drive 不可用的账户不应被其他请求避开: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	transport.allow(accountID)
	if err := service.DeleteFile(ctx, file.ID); err != nil {
		t.Fatalf("Drive 恢复后删除失败: %v", err)
	}
	if service.drive.count() != 0 {
		t.Fatal("换 token 成功后应清除 Drive 不可用记忆")
	}
}

// TestCopyFileReferencesDriveUnavailableNoCooldown 跨账户复制文件引用时目标账户换 token 返回 401：这次尝试失败（返回错误，由生成按原有逻辑换号），
// 目标账户不写冷却、不标需要登录，只记 Drive 不可用，释放后别的请求照常取得它；文件所属账户不受影响
func TestCopyFileReferencesDriveUnavailableNoCooldown(t *testing.T) {
	const owner, target = "a-owner@example.com", "b-target@example.com"
	service, transport := driveTestService(t, owner, target)
	ctx := context.Background()
	data := []byte("hello")
	file, err := service.UploadFile(ctx, UploadRequest{
		AccountID: owner, Name: "a.txt", MIME: "text/plain", Purpose: "user_data", Size: int64(len(data)), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	transport.deny(target, driveMissingCredential)
	lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: target})
	if err != nil {
		t.Fatal(err)
	}
	contents := []Content{{Role: RoleUser, Parts: []Part{{Text: "看文件"}, {File: &FileRef{ID: file.ID, Name: file.Name, MIME: file.MIME}}}}}
	rewritten, copies, copyErr := service.CopyFileReferencesToLease(ctx, lease, contents)
	if releaseErr := lease.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if rewritten != nil || copies != nil || !AccessTokenAuthFailure(copyErr) || DefinitiveAuthenticationFailure(copyErr) {
		t.Fatalf("目标账户没有 Drive 授权时复制应失败（不算登录失效），实际 contents=%v err=%v", rewritten, copyErr)
	}
	assertNoCooldown(t, service.pool, target)
	assertNoCooldown(t, service.pool, owner)
	service.drive.mu.Lock()
	_, remembered := service.drive.entries[target]
	entries := len(service.drive.entries)
	service.drive.mu.Unlock()
	if !remembered || entries != 1 {
		t.Fatalf("应只记住目标账户的 Drive 不可用：记住=%t 条目=%d", remembered, entries)
	}
	again, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: target, ModelID: ceilingModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("Drive 不可用的账户不应被其他请求避开: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestDriveMemoryConcurrentAccess 同一账户的两个租约并发处理内联附件，期间一个租约保存新登录（推进认证代际），另有人清除记忆、
// 按记忆排序上传候选：记忆的锁与账户池 mu 不嵌套，-race 下没有数据竞争、不死锁，账户始终没有冷却
func TestDriveMemoryConcurrentAccess(t *testing.T) {
	accounts := []string{"c-0@example.com", "c-1@example.com"}
	service, transport := driveTestService(t, accounts...)
	for _, accountID := range accounts {
		transport.deny(accountID, driveMissingCredential)
	}
	ctx := context.Background()
	leases := make([]*AccountLease, 0, 2)
	for range 2 {
		lease, err := service.pool.AcquireFor(ctx, AccountSelection{AccountID: accounts[0]})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	var wait sync.WaitGroup
	for index, lease := range leases {
		wait.Add(1)
		go func(index int, lease *AccountLease) {
			defer wait.Done()
			for round := range 40 {
				contents, blob := inlineImageContents()
				rewritten, _, err := service.UploadInlineMediaToLease(ctx, lease, contents, nil)
				if err != nil || rewritten[0].Parts[1].InlineData != blob {
					t.Errorf("租约 %d 第 %d 次：err=%v，期望保留 inline data", index, round, err)
					return
				}
				if index == 1 && round%10 == 0 {
					if err := lease.SaveStorageState(testStorageState(fmt.Sprintf("NEW-%d", round))); err != nil {
						t.Errorf("保存登录状态失败: %v", err)
						return
					}
				}
			}
		}(index, lease)
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for range 40 {
			service.ForgetDriveUnavailable(accounts[0])
			if ordered := service.preferDriveCandidates(accounts); len(ordered) != len(accounts) {
				t.Errorf("候选排序丢了账户：%v", ordered)
				return
			}
		}
	}()
	wait.Wait()
	for _, lease := range leases {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	assertNoCooldown(t, service.pool, accounts[0])
	if calls := transport.tokenCalls(accounts[0]); calls == 0 || calls > 80 {
		t.Fatalf("GenerateAccessToken 调用 %d 次，期望在 1 到 80 次之间（记忆命中时跳过）", calls)
	}
}
