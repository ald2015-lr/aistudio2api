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

// TestDefinitiveAuthenticationFailureScope 只有用 Cookie 认证的 MakerSuite RPC 返回 401 / Code 16 才算登录失效；
// Drive* 的 Bearer 401 与 GenerateAccessToken 的 401 都不算，后者另按 AccessTokenAuthFailure 处理
func TestDefinitiveAuthenticationFailureScope(t *testing.T) {
	unauthorized := func(method string) error {
		return &RPCError{Method: method, StatusCode: http.StatusUnauthorized, Code: 16, Message: "Request is missing required authentication credential."}
	}
	for method, want := range map[string]bool{
		"GenerateContent": true, "GetAiStudioBenefitTier": true, "ListModels": true, "CountTokens": true,
		"DriveUpload": false, "DriveUploadInit": false, "DriveDownload": false, "DriveDelete": false,
		"GenerateAccessToken": false,
	} {
		if got := DefinitiveAuthenticationFailure(fmt.Errorf("发送 AI Studio %s: %w", method, unauthorized(method))); got != want {
			t.Fatalf("%s 401: DefinitiveAuthenticationFailure = %t，期望 %t", method, got, want)
		}
	}
	if !DefinitiveAuthenticationFailure(&RPCError{Method: "GenerateContent", StatusCode: http.StatusOK, Code: 16}) {
		t.Fatal("协议错误码 16 应算登录失效")
	}
	if !DefinitiveAuthenticationFailure(fmt.Errorf("%w url=https://accounts.google.com/", ErrAuthenticationRequired)) {
		t.Fatal("登录页跳转应算登录失效")
	}
	if !AccessTokenAuthFailure(unauthorized("GenerateAccessToken")) {
		t.Fatal("GenerateAccessToken 的 401 应按附件授权失败处理")
	}
	if AccessTokenAuthFailure(&RPCError{Method: "GenerateAccessToken", StatusCode: http.StatusUnauthorized, Code: 16, Message: "OAuth error: unauthorized_client."}) {
		t.Fatal("Drive 授权缺失不是附件授权失败（不冷却、退回内联附件）")
	}
	if AccessTokenAuthFailure(unauthorized("GenerateContent")) || AccessTokenAuthFailure(unauthorized("DriveUpload")) {
		t.Fatal("其他方法的 401 不是附件授权失败")
	}
}

// TestMarkAuthenticationFailureGate 上游 401 先交给账户池的 gate：gate 返回 false 时不改账户状态；登录页跳转等本地证据不经 gate 直接标记；
// 没有 gate 时照旧直接标记
func TestMarkAuthenticationFailureGate(t *testing.T) {
	accountID := "gate@example.com"
	pool := testPoolWithAccount(t, accountID)
	ctx := context.Background()
	upstream := &RPCError{Method: "GenerateContent", StatusCode: http.StatusUnauthorized, Code: 16, Message: "invalid"}
	var gateCalls []error
	pool.SetAuthFailureGate(func(lease *AccountLease, cause error) bool {
		if lease.Account().ID != accountID {
			t.Errorf("gate 收到的租约账户 = %s", lease.Account().ID)
		}
		gateCalls = append(gateCalls, cause)
		return false
	})
	lease, err := pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	marked, err := lease.MarkAuthenticationFailure(upstream)
	if err != nil || marked {
		t.Fatalf("gate 拦下时应不标记: marked=%t err=%v", marked, err)
	}
	if status, _ := pool.StatusOf(accountID); status.State == AccountAuthRequired {
		t.Fatal("gate 拦下后账户不应变成需要登录")
	}
	if len(gateCalls) != 1 || !errors.Is(gateCalls[0], upstream) {
		t.Fatalf("gate 调用 = %v", gateCalls)
	}
	// 本地证据（登录页跳转）不经 gate
	local := fmt.Errorf("%w url=https://accounts.google.com/v3/signin", ErrAuthenticationRequired)
	marked, err = lease.MarkAuthenticationFailure(local)
	if err != nil || !marked || len(gateCalls) != 1 {
		t.Fatalf("登录页跳转应直接标记且不经 gate: marked=%t err=%v gate=%d", marked, err, len(gateCalls))
	}
	if status, _ := pool.StatusOf(accountID); status.State != AccountAuthRequired {
		t.Fatalf("登录页跳转后状态 = %s，期望需要登录", status.State)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	// 没有 gate：上游 401 直接标记（原行为）
	other := testPoolWithAccount(t, "plain@example.com")
	plain, err := other.AcquireFor(ctx, AccountSelection{AccountID: "plain@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Release()
	if marked, err := plain.MarkAuthenticationFailure(upstream); err != nil || !marked {
		t.Fatalf("没有 gate 时应直接标记: marked=%t err=%v", marked, err)
	}
}

// TestMergeStateCookies 浏览器 Cookie 按 (name, domain, path) 合并：新值覆盖旧值、新增的追加、浏览器没导出的旧 Cookie 保留，过期的旧 Cookie 丢掉
func TestMergeStateCookies(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	previous := []StateCookie{
		{Name: "SID", Value: "old", Domain: ".google.com", Path: "/"},
		{Name: "SID", Value: "other-path", Domain: ".google.com", Path: "/x"},
		{Name: "KEEP", Value: "kept", Domain: ".google.com", Path: "/", Expires: float64(now.Add(time.Hour).Unix())},
		{Name: "GONE", Value: "expired", Domain: ".google.com", Path: "/", Expires: float64(now.Add(-time.Hour).Unix())},
	}
	fresh := []StateCookie{
		{Name: "SID", Value: "new", Domain: ".GOOGLE.com", Path: "/"},
		{Name: "__Secure-1PSIDTS", Value: "rotated", Domain: ".google.com", Path: "/"},
	}
	merged := mergeStateCookies(previous, fresh, now)
	got := make([]string, 0, len(merged))
	for _, cookie := range merged {
		got = append(got, cookie.Name+cookie.Path+"="+cookie.Value)
	}
	want := "SID/=new,SID/x=other-path,KEEP/=kept,__Secure-1PSIDTS/=rotated"
	if strings.Join(got, ",") != want {
		t.Fatalf("合并结果 = %s，期望 %s", strings.Join(got, ","), want)
	}
}

// TestMergeBrowserCookiesGeneration Worker 启动后写回浏览器 Cookie：认证代际没变时合并写入（内存与磁盘），代际变化（期间保存过新登录）时不写，
// 与磁盘相同时不写文件
func TestMergeBrowserCookiesGeneration(t *testing.T) {
	accountID := "merge@example.com"
	pool := testPoolWithAccount(t, accountID)
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	generation := pool.AuthGeneration(accountID)
	written, err := pool.MergeBrowserCookies(accountID, generation+1, testStorageState("STALE").Cookies)
	if err != nil || written {
		t.Fatalf("代际不符时不应写入: written=%t err=%v", written, err)
	}
	if value := diskSAPISID(t, account.StoragePath); value != "OLD" {
		t.Fatalf("磁盘 SAPISID = %q，期望保持 OLD", value)
	}
	written, err = pool.MergeBrowserCookies(accountID, generation, testStorageState("FRESH").Cookies)
	if err != nil || !written {
		t.Fatalf("代际相同应写入: written=%t err=%v", written, err)
	}
	if value := diskSAPISID(t, account.StoragePath); value != "FRESH" {
		t.Fatalf("磁盘 SAPISID = %q，期望 FRESH", value)
	}
	updated, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := updated.StorageState.CookieValue("SAPISID", aiStudioOrigin+"/", time.Now()); value != "FRESH" {
		t.Fatalf("内存 SAPISID = %q，期望 FRESH", value)
	}
	if written, err := pool.MergeBrowserCookies(accountID, generation, testStorageState("FRESH").Cookies); err != nil || written {
		t.Fatalf("与磁盘相同时不应再写: written=%t err=%v", written, err)
	}
}

// TestVerificationPagePath 验证页地址只保留 host+path，查询参数（账号、回跳地址）与片段不进日志与原因
func TestVerificationPagePath(t *testing.T) {
	for raw, want := range map[string]string{
		"https://accounts.google.com/v3/signin/identifier?continue=https%3A%2F%2Faistudio.google.com&Email=a%40b.com#x": "accounts.google.com/v3/signin/identifier",
		"https://Accounts.Google.com/":                      "accounts.google.com",
		"https://accounts.google.com/speedbump/idvreenable": "accounts.google.com/speedbump/idvreenable",
		"":           "",
		"not a url":  "",
		"::bad::url": "",
	} {
		if got := VerificationPagePath(raw); got != want {
			t.Fatalf("VerificationPagePath(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

// accessTokenTransport 让 GenerateAccessToken 返回 401（Code 16，不是 unauthorized_client），记录调用
type accessTokenTransport struct {
	mu    sync.Mutex
	calls []string
}

func (transport *accessTokenTransport) Do(_ context.Context, request RPCRequest) (*RPCResponse, error) {
	transport.mu.Lock()
	transport.calls = append(transport.calls, request.Method)
	transport.mu.Unlock()
	header := http.Header{}
	header.Set("Content-Type", JSONProtobufContentType)
	return &RPCResponse{
		StatusCode: http.StatusUnauthorized, Header: header,
		Body: io.NopCloser(strings.NewReader(`[16,"Request is missing required authentication credential."]`)),
	}, nil
}

func (transport *accessTokenTransport) UploadDrive(context.Context, string, string, UploadRequest) (FileRef, error) {
	return FileRef{}, errors.New("不应上传")
}

func (transport *accessTokenTransport) DownloadDrive(context.Context, string, string, string) (MediaStream, error) {
	return MediaStream{}, errors.New("不应下载")
}

func (transport *accessTokenTransport) DeleteDrive(context.Context, string, string, string) error {
	return nil
}

// TestInlineMediaAccessTokenUnauthorizedCools 带图请求换 token 时 GenerateAccessToken 返回 401：只让本次附件尝试失败，
// 账户写模型无关的短冷却（不超过 ShortCooldownMax），不标需要登录
func TestInlineMediaAccessTokenUnauthorizedCools(t *testing.T) {
	accountID := "token@example.com"
	pool := testPoolWithAccount(t, accountID)
	client := &Client{transport: &accessTokenTransport{}, catalogs: make(map[string]modelCatalog), tiers: make(map[string]BenefitTier)}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lease, err := pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	contents := []Content{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte{1, 2, 3}}}}}}
	_, _, uploadErr := service.UploadInlineMediaToLease(ctx, lease, contents, nil)
	if !AccessTokenAuthFailure(uploadErr) || DefinitiveAuthenticationFailure(uploadErr) {
		t.Fatalf("应以 GenerateAccessToken 授权失败结束，实际 %v", uploadErr)
	}
	status, _ := pool.StatusOf(accountID)
	if status.State == AccountAuthRequired {
		t.Fatal("GenerateAccessToken 的 401 不应把账户标为需要登录")
	}
	if !pool.AccountCoolingDown(accountID, "") {
		t.Fatal("GenerateAccessToken 的 401 后账户应有全局短冷却")
	}
	pool.mu.Lock()
	cooldown := pool.byID[accountID].runtime.Cooldowns[globalCooldownKey]
	pool.mu.Unlock()
	if remaining := time.Until(cooldown.Until); remaining <= 0 || remaining > ShortCooldownMax {
		t.Fatalf("冷却剩余 %s，期望不超过 %s", remaining, ShortCooldownMax)
	}
	if !strings.Contains(cooldown.Reason, "GenerateAccessToken") {
		t.Fatalf("冷却原因 = %q", cooldown.Reason)
	}
}

// TestAuthRequiredAccountIDs 只返回启用且需要登录的账户
func TestAuthRequiredAccountIDs(t *testing.T) {
	pool := testPoolWithAccount(t, "need@example.com")
	if ids := pool.AuthRequiredAccountIDs(); len(ids) != 0 {
		t.Fatalf("就绪账户不应列出: %v", ids)
	}
	if err := pool.MarkAuthRequired("need@example.com", "测试"); err != nil {
		t.Fatal(err)
	}
	if ids := pool.AuthRequiredAccountIDs(); len(ids) != 1 || ids[0] != "need@example.com" {
		t.Fatalf("需要登录账户 = %v", ids)
	}
}
