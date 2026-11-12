package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// reimportTestState 返回可签名的认证状态：Cookie 值为 value，source 非空时带认证扩展（来源为 source、邮箱为 email），
// refresh 非空时带 Chrome OAuth 续签材料
func reimportTestState(t *testing.T, value, source, email, refresh string) aistudio.StorageState {
	t.Helper()
	state := authTestState(t, value, false)
	if source == "" {
		return state
	}
	extension := aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: source, Email: email}}
	if refresh != "" {
		extension.OAuth = &aistudio.ChromeOAuthMaterial{GaiaID: "1", RefreshToken: refresh}
	}
	if err := state.SetAuthExtension(extension); err != nil {
		t.Fatal(err)
	}
	return state
}

// saveTestStorage 在独占租约内把账户的认证状态换成 state
func saveTestStorage(t *testing.T, pool *aistudio.AccountPool, accountID string, state aistudio.StorageState) {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(state); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// diskAuth 读取磁盘上的认证状态，返回 SAPISID 的值与认证扩展
func diskAuth(t *testing.T, account *aistudio.Account) (string, aistudio.AuthExtension) {
	t.Helper()
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	for _, cookie := range state.Cookies {
		if cookie.Name == "SAPISID" {
			value = cookie.Value
		}
	}
	extension, _, err := state.AuthExtension()
	if err != nil {
		t.Fatal(err)
	}
	return value, extension
}

// TestKeepAuthExtension 新认证状态没有 OAuth、原有状态有时沿用原有的来源与 OAuth；新状态自己带 OAuth、原有状态没有 OAuth、
// 原有扩展的来源邮箱与新状态不一致时都不沿用
func TestKeepAuthExtension(t *testing.T) {
	const email = "a@example.com"
	cases := []struct {
		name    string
		next    aistudio.StorageState
		current aistudio.StorageState
		carried bool
		refresh string
	}{
		{"沿用", reimportTestState(t, "NEW", "camoufox", email, ""), reimportTestState(t, "OLD", "chrome", email, "old"), true, "old"},
		{"新状态没有扩展", reimportTestState(t, "NEW", "", "", ""), reimportTestState(t, "OLD", "chrome", email, "old"), true, "old"},
		{"新状态有 OAuth", reimportTestState(t, "NEW", "chrome", email, "fresh"), reimportTestState(t, "OLD", "chrome", email, "old"), false, "fresh"},
		{"原有没有 OAuth", reimportTestState(t, "NEW", "camoufox", email, ""), reimportTestState(t, "OLD", "camoufox", email, ""), false, ""},
		{"来源邮箱不一致", reimportTestState(t, "NEW", "camoufox", email, ""), reimportTestState(t, "OLD", "chrome", "b@example.com", "old"), false, ""},
	}
	for _, item := range cases {
		next := item.next
		carried, err := keepAuthExtension(&next, item.current)
		if err != nil || carried != item.carried {
			t.Fatalf("%s：carried=%t err=%v，期望 %t", item.name, carried, err, item.carried)
		}
		extension, _, _ := next.AuthExtension()
		refresh := ""
		if extension.OAuth != nil {
			refresh = extension.OAuth.RefreshToken
		}
		if refresh != item.refresh {
			t.Fatalf("%s：续签材料 = %q，期望 %q", item.name, refresh, item.refresh)
		}
		if item.carried && extension.Source.Browser != "chrome" {
			t.Fatalf("%s：沿用时来源应为原有的 chrome：%+v", item.name, extension.Source)
		}
	}
}

// TestLoginKeepsChromeOAuth Chrome 导入的账户在浏览器里重新登录：新登录状态只带 camoufox 来源，保存时沿用原有的 OAuth 续签材料，
// Cookie 换成新的；原先直接保存新状态，续签材料丢失，之后遇到 401 不能原地续签
func TestLoginKeepsChromeOAuth(t *testing.T) {
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com")
	account := accounts[0]
	saveTestStorage(t, admin.pool, account.ID, reimportTestState(t, "OLD", "chrome", account.ID, "chrome-refresh"))
	admin.login = &loginResultLogin{email: account.ID, state: reimportTestState(t, "NEW", "camoufox", account.ID, "")}
	if _, err := admin.LoginAccount(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	value, extension := diskAuth(t, account)
	if value != "NEW" {
		t.Fatalf("应保存新登录的 Cookie，SAPISID = %q", value)
	}
	if extension.OAuth == nil || extension.OAuth.RefreshToken != "chrome-refresh" || extension.Source.Browser != "chrome" {
		t.Fatalf("重新登录应沿用原有的 Chrome OAuth 续签材料：%+v", extension)
	}
	if !authRefreshMaterial(mustReload(t, admin.pool, account.ID)) {
		t.Fatal("重新登录后应仍能原地续签")
	}
	if !hasLogContaining(admin.requests, "账户登录 | 沿用原有的 Chrome OAuth 续签材料") {
		t.Fatal("缺少沿用续签材料的日志")
	}
}

// mustReload 在独占租约内重新读取账户的认证状态
func mustReload(t *testing.T, pool *aistudio.AccountPool, accountID string) aistudio.StorageState {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	state, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestCreateExistingAccountConflict 浏览器添加的账户已经存在：登录拿到邮箱后立即返回 409 account_exists（消息带邮箱，按状态说明：
// 需要登录时重新登录、停用的先启用、其他状态不用再添加），已有账户的认证文件不变、不就地覆盖；账户目录在磁盘上已存在、尚未进账户池时
// （store.Create 报 ErrAccountExists）同样返回 409，消息说明等目录扫描载入
func TestCreateExistingAccountConflict(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	account := accounts[0]
	before, _ := diskAuth(t, account)
	admin.login = &loginResultLogin{email: "A@Example.com", state: reimportTestState(t, "NEW", "camoufox", "a@example.com", "")}
	_, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{})
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != http.StatusConflict || operation.code != "account_exists" ||
		!strings.Contains(operation.message, "账户已存在: a@example.com，没有重复添加") ||
		!strings.Contains(operation.message, "停用的先启用") {
		t.Fatalf("已存在的账户应返回 409 account_exists，消息带邮箱并按状态说明，得到 %v", err)
	}
	if after, _ := diskAuth(t, account); after != before {
		t.Fatalf("已有账户的认证文件不应被覆盖：%q → %q", before, after)
	}
	if !hasLogContaining(admin.requests, "账户已在号池中，没有重复添加 | 账户=a@example.com") {
		t.Fatal("缺少账户已存在的日志")
	}

	// 磁盘上已有同邮箱的账户目录（还没进账户池）：store.Create 报 ErrAccountExists，同样映射为 409
	root := filepath.Dir(account.Directory)
	writeEnableTestAccount(t, root, "b@example.com", true)
	_, err = admin.addAccount(context.Background(), aistudio.DefaultAccountConfig("b@example.com"), authTestState(t, "NEW", false), "", nil)
	if !errors.As(err, &operation) || operation.code != "account_exists" ||
		!strings.Contains(operation.message, "账户目录已存在: b@example.com，没有重复添加：目录扫描会自动载入它") {
		t.Fatalf("账户目录已存在时应返回 409 account_exists 并说明等目录扫描载入，得到 %v", err)
	}
}

// chromeReimportStubs 换上测试用的 Chrome 导入：本机有 a 与 b 两个账号，导入结果带新的 Cookie 与 OAuth 续签材料，记录每次导入的参数
func chromeReimportStubs(t *testing.T, email func(id string) string) *[]chromeauth.ImportOptions {
	t.Helper()
	var mu sync.Mutex
	imports := &[]chromeauth.ImportOptions{}
	previousImport, previousRoot, previousDiscover := chromeImport, chromeImportRoot, chromeDiscover
	chromeImportRoot = func() (string, error) { return "root", nil }
	chromeDiscover = func(string) ([]chromeauth.Account, error) {
		return []chromeauth.Account{
			{ID: "Default/a", Profile: "Default", Email: "a@example.com", Importable: true},
			{ID: "Profile 1/b", Profile: "Profile 1", Email: "b@example.com", Importable: true},
			{ID: "Profile 2/c", Profile: "Profile 2", Email: "c@example.com"},
		}, nil
	}
	chromeImport = func(_ context.Context, options chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		mu.Lock()
		*imports = append(*imports, options)
		mu.Unlock()
		results := make([]chromeauth.ImportResult, 0, len(options.AccountIDs))
		for _, id := range options.AccountIDs {
			address := email(id)
			results = append(results, chromeauth.ImportResult{Email: address, State: reimportTestState(t, "NEW", "chrome", address, "chrome-new")})
		}
		return results, nil
	}
	t.Cleanup(func() {
		chromeImport, chromeImportRoot, chromeDiscover = previousImport, previousRoot, previousDiscover
	})
	return imports
}

// TestChromeReimportInPlace 已有账户从 Chrome 就地重新导入（D5-B）：导入候选标出已有账户；导入时用账户自己的出口签发 Cookie，
// 只覆盖 Cookie 与续签材料，账户配置（代理、语言、时区、启用）、Camoufox 指纹、Worker 回写的 AI Studio localStorage 与
// runtime-state.json 的资源绑定都保留，用旧 Cookie 的驻留 Worker 被关闭；填写的代理、语言、时区只用于新账户；同一批里的新账号照常新增
func TestChromeReimportInPlace(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com")
	account := accounts[0]
	// 驻留 Worker 用的是旧 Cookie，重新导入时要关掉；Worker 回写过 AI Studio 的 localStorage
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, account.ID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("前提：账户应有驻留 Worker")
	}
	withLocalStorage := reimportTestState(t, "OLD", "", "", "")
	withLocalStorage.Origins = []aistudio.StorageOrigin{
		{Origin: aistudio.BrowserLocalStorageOrigin, LocalStorage: []aistudio.StorageItem{{Name: "dismissed-tour", Value: "true"}}},
		{Origin: "https://accounts.google.com", LocalStorage: []aistudio.StorageItem{{Name: "other", Value: "old"}}},
	}
	saveTestStorage(t, admin.pool, account.ID, withLocalStorage)
	email := func(id string) string {
		return map[string]string{"Default/a": "a@example.com", "Profile 1/b": "b@example.com"}[id]
	}
	imports := chromeReimportStubs(t, email)

	// 账户原有的配置、指纹、资源绑定与认证状态（没有续签材料、需要登录）
	lease, err := admin.pool.AcquireAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	accountConfig := lease.Account().Config
	accountConfig.Proxy, accountConfig.Locale, accountConfig.Timezone = "http://203.0.113.7:3128", "ja-JP", "Asia/Tokyo"
	if err := lease.SaveConfig(accountConfig); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	fingerprint := []byte(`{"firefox_version":135,"config":{"screen.width":1920}}`)
	fingerprintPath := filepath.Join(account.Directory, "camoufox-fingerprint.json")
	if err := os.WriteFile(fingerprintPath, fingerprint, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := admin.pool.BindResourceKind("files/kept", account.ID, "file"); err != nil {
		t.Fatal(err)
	}
	if err := admin.pool.MarkAuthRequired(account.ID, "HTTP 401"); err != nil {
		t.Fatal(err)
	}

	profiles, err := admin.ChromeImportProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || !profiles[0].Existing || profiles[0].State != string(aistudio.AccountAuthRequired) || profiles[1].Existing {
		t.Fatalf("导入候选应列出已有账户（标 existing 与状态）与新账号，不列不可导入的：%+v", profiles)
	}

	imported, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{
		AccountIDs: []string{"Default/a", "Profile 1/b"}, Proxy: "http://198.51.100.1:8080", Locale: "en-US", Timezone: "UTC",
	})
	if err != nil || len(imported.Accounts) != 2 || len(imported.Failed) != 0 {
		t.Fatalf("导入结果 %+v（%v）", imported, err)
	}
	if len(*imports) != 2 || strings.Join((*imports)[0].AccountIDs, ",") != "Profile 1/b" || strings.Join((*imports)[1].AccountIDs, ",") != "Default/a" {
		t.Fatalf("新账号与已有账户应分开导入：%+v", *imports)
	}
	if (*imports)[1].Proxy != "http://203.0.113.7:3128" || (*imports)[0].Proxy != "http://198.51.100.1:8080" {
		t.Fatalf("已有账户应用自己的出口、新账号用填写的代理：%q / %q", (*imports)[1].Proxy, (*imports)[0].Proxy)
	}

	value, extension := diskAuth(t, account)
	if value != "NEW" || extension.OAuth == nil || extension.OAuth.RefreshToken != "chrome-new" {
		t.Fatalf("应覆盖 Cookie 与续签材料：SAPISID=%q 扩展=%+v", value, extension)
	}
	disk, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Origins) != 1 || disk.Origins[0].Origin != aistudio.BrowserLocalStorageOrigin ||
		len(disk.Origins[0].LocalStorage) != 1 || disk.Origins[0].LocalStorage[0].Name != "dismissed-tour" {
		t.Fatalf("应只沿用 AI Studio 的 localStorage，其他 origin 照新状态：%+v", disk.Origins)
	}
	if slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("重新导入应关闭用旧 Cookie 的驻留 Worker")
	}
	status := testStatus(t, admin.pool, account.ID)
	if status.Proxy != "http://203.0.113.7:3128" || status.Locale != "ja-JP" || status.Timezone != "Asia/Tokyo" || !status.Enabled {
		t.Fatalf("账户配置应保留：%+v", status)
	}
	if status.State == aistudio.AccountAuthRequired {
		t.Fatal("重新导入后不应仍是需要登录")
	}
	if content, err := os.ReadFile(fingerprintPath); err != nil || string(content) != string(fingerprint) {
		t.Fatalf("Camoufox 指纹应保留：%s（%v）", content, err)
	}
	runtimeState, err := os.ReadFile(account.RuntimePath)
	if err != nil || !strings.Contains(string(runtimeState), "files/kept") {
		t.Fatalf("runtime-state.json 的资源绑定应保留：%s（%v）", runtimeState, err)
	}
	if !hasLogContaining(admin.requests, "账户已就地重新导入 | 来源=Chrome 重新导入") {
		t.Fatal("缺少就地重新导入的日志")
	}
	created := testStatus(t, admin.pool, "b@example.com")
	if created.Proxy != "http://198.51.100.1:8080" || created.Locale != "en-US" || created.Timezone != "UTC" {
		t.Fatalf("新账号应按填写的环境新增：%+v", created)
	}
	var config aistudio.AccountConfig
	content, err := os.ReadFile(account.ConfigPath)
	if err != nil || json.Unmarshal(content, &config) != nil || config.Proxy != "http://203.0.113.7:3128" {
		t.Fatalf("account.json 不应被改写：%s", content)
	}
}

// TestChromeReimportRejectsMismatchedEmail Chrome 导入结果的邮箱与已有账户不一致时拒绝（记入结果的失败项，错误码 invalid_account），
// 不改动账户的认证文件
func TestChromeReimportRejectsMismatchedEmail(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com")
	account := accounts[0]
	chromeReimportStubs(t, func(string) string { return "other@example.com" })
	before, _ := diskAuth(t, account)
	result, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{AccountIDs: []string{"Default/a"}})
	if err != nil || len(result.Accounts) != 0 || len(result.Failed) != 1 || result.Failed[0].Code != "invalid_account" ||
		result.Failed[0].ID != "Default/a" || result.Failed[0].Email != "a@example.com" {
		t.Fatalf("邮箱不一致应记为 invalid_account 失败项，得到 %+v（%v）", result, err)
	}
	if after, _ := diskAuth(t, account); after != before {
		t.Fatal("邮箱不一致时不应改动认证文件")
	}
}

// TestChromeReimportContinuesAfterFailure 就地重新导入逐个处理：排在前面的已有账户正忙（等不到独占租约）时记为失败项（account_busy），
// 后面的已有账户照常重新导入，结果里两者分别列出；接口不整批报错
func TestChromeReimportContinuesAfterFailure(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com", "b@example.com")
	admin.operationWait = 200 * time.Millisecond
	chromeReimportStubs(t, func(id string) string {
		return map[string]string{"Default/a": "a@example.com", "Profile 1/b": "b@example.com"}[id]
	})
	release := holdLease(t, admin.pool, "a@example.com")
	defer release()
	beforeA, _ := diskAuth(t, accounts[0])
	result, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{AccountIDs: []string{"Default/a", "Profile 1/b"}})
	if err != nil {
		t.Fatalf("单个账户失败不应让整批返回错误：%v", err)
	}
	if len(result.Failed) != 1 || result.Failed[0].Email != "a@example.com" || result.Failed[0].ID != "Default/a" ||
		result.Failed[0].Code != "account_busy" {
		t.Fatalf("正忙的账户应记为 account_busy 失败项：%+v", result.Failed)
	}
	if len(result.Accounts) != 1 || result.Accounts[0].ID != "b@example.com" {
		t.Fatalf("后面的已有账户应照常重新导入：%+v", result.Accounts)
	}
	if after, _ := diskAuth(t, accounts[0]); after != beforeA {
		t.Fatal("没有重新导入的账户认证文件不应改动")
	}
	if value, _ := diskAuth(t, accounts[1]); value != "NEW" {
		t.Fatalf("重新导入的账户应换上新 Cookie：%q", value)
	}
	if !hasLogContaining(admin.requests, "Chrome 就地重新导入失败") {
		t.Fatal("失败的账户应写一行日志")
	}
}
