package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 测试用的代理账号密码：断言它们不出现在错误、日志与 -h 输出里
const (
	testProxyUser     = "user-session-XYZ"
	testProxyPassword = "S3cr3t-PW"
)

// clearProxyEnv 去掉进程环境里的 PROXY，避免宿主机的值覆盖测试写的 .env
func clearProxyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PROXY", "")
	os.Unsetenv("PROXY")
}

// assertNoProxySecret 断言文本里没有测试代理的用户名和密码
func assertNoProxySecret(t *testing.T, where string, text string) {
	t.Helper()
	if strings.Contains(text, testProxyUser) || strings.Contains(text, testProxyPassword) {
		t.Fatalf("%s 含代理账号密码: %s", where, text)
	}
}

// assertNoProxySecretInLogs 断言管理页日志里没有测试代理的用户名和密码
func assertNoProxySecretInLogs(t *testing.T, registry *requestRegistry) {
	t.Helper()
	registry.mu.Lock()
	logs := append([]api.AdminLog(nil), registry.logs...)
	registry.mu.Unlock()
	for _, entry := range logs {
		assertNoProxySecret(t, "日志", entry.Message)
	}
}

// assertAdminError 断言错误为指定状态与代码的管理接口错误，返回其文案
func assertAdminError(t *testing.T, err error, status int, code string) string {
	t.Helper()
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != status || operation.code != code {
		t.Fatalf("应返回 %d %s，得到 %v", status, code, err)
	}
	return operation.message
}

// TestUpdateRuntimeConfigNormalizesProxy 管理页保存冒号写法的默认代理：响应与 .env 里都是规范值，读回来逐字节相同；
// 不带认证的旧值原样保存；不合法时返回 400 invalid_config，文案不含账号密码，.env 不变
func TestUpdateRuntimeConfigNormalizesProxy(t *testing.T) {
	clearProxyEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	readEnv := func() string {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	const want = "http://" + testProxyUser + ":p%40ss%3Aword%20x@1.2.3.4:8080"
	value := validRuntimeConfig()
	value.Proxy = " 1.2.3.4:8080:" + testProxyUser + ":p@ss:word x "
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Proxy != want {
		t.Fatalf("响应 proxy = %q，期望 %q", saved.Proxy, want)
	}
	if !strings.Contains(readEnv(), "\nPROXY="+want+"\n") {
		t.Fatalf(".env 应不加引号写入规范值:\n%s", readEnv())
	}
	if loaded, err := config.Load(path); err != nil || loaded.Proxy != want {
		t.Fatalf("读回 %q (%v)", loaded.Proxy, err)
	}
	// 页面把返回的规范值同步回表单后再保存：内容不变
	if again, err := admin.UpdateRuntimeConfig(ctx, saved); err != nil || again.Proxy != want {
		t.Fatalf("再次保存规范值得到 %q (%v)", again.Proxy, err)
	}

	value.Proxy = "HTTP://Proxy.example.com:8080/"
	if saved, err := admin.UpdateRuntimeConfig(ctx, value); err != nil || saved.Proxy != value.Proxy {
		t.Fatalf("不带认证的旧值应原样保存，得到 %q (%v)", saved.Proxy, err)
	}

	before := readEnv()
	for _, invalid := range []string{
		"ftp://" + testProxyUser + ":" + testProxyPassword + "@h:1",
		"socks5://" + testProxyUser + ":" + testProxyPassword + "@h",
		"http://" + testProxyUser + ":" + testProxyPassword + "@h:1/path",
	} {
		value.Proxy = invalid
		_, err := admin.UpdateRuntimeConfig(ctx, value)
		message := assertAdminError(t, err, http.StatusBadRequest, "invalid_config")
		assertNoProxySecret(t, "错误文案", message)
		if readEnv() != before {
			t.Fatalf("%q: 不合法时不应改写 .env", invalid)
		}
	}
	assertNoProxySecretInLogs(t, admin.requests)
}

// TestSameDataConfigNormalizesProxy 已保存的默认代理与生效值写法不同但上游相同时不误报需要重启；上游不同时报需要重启
func TestSameDataConfigNormalizesProxy(t *testing.T) {
	active := config.Default()
	active.Proxy = "http://u:p@1.2.3.4:8080"
	manager := &runtimeManager{activeManagement: active}

	dto := runtimeConfigDTO(active)
	for _, proxy := range []string{"http://u:p@1.2.3.4:8080", "1.2.3.4:8080:u:p", "u:p@1.2.3.4:8080", " http://u:p@1.2.3.4:8080/ "} {
		dto.Proxy = proxy
		if !sameDataConfig(dto, active, dataConfigOverrides{}) {
			t.Errorf("%q: 上游相同不应判为需要重启", proxy)
		}
		if manager.decorateRuntimeConfig(dto, active).ServiceRestartRequired {
			t.Errorf("%q: ServiceRestartRequired 应为假", proxy)
		}
	}
	for _, proxy := range []string{"1.2.3.4:8080:u:q", "socks5://u:p@1.2.3.4:8080", "", "http://1.2.3.4:8080"} {
		dto.Proxy = proxy
		if sameDataConfig(dto, active, dataConfigOverrides{}) {
			t.Errorf("%q: 上游不同应判为需要重启", proxy)
		}
	}
	// 命令行覆盖了代理时按覆盖值比较
	override := "http://u:p@1.2.3.4:8080"
	dto.Proxy = "http://other:pw@5.6.7.8:1"
	if !sameDataConfig(dto, active, dataConfigOverrides{proxy: &override}) {
		t.Fatal("命令行覆盖代理时应按覆盖值比较")
	}
	// 不带认证的旧值：写法与生效值逐字节一致时照旧为真
	active.Proxy = "HTTP://H:1/"
	dto = runtimeConfigDTO(active)
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("不带认证的旧值不应判为需要重启")
	}
}

// TestRequiresRebuildAfterLoadNormalized .env 写冒号写法，Load 之后与生效的规范值比较不需要重建；热更新字段照常比较
func TestRequiresRebuildAfterLoadNormalized(t *testing.T) {
	clearProxyEnv(t)
	directory := t.TempDir()
	load := func(name string, content string) config.Config {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	active := load("active.env", "PROXY=http://u:p%40x@1.2.3.4:8080\n")
	saved := load("saved.env", "PROXY=1.2.3.4:8080:u:p@x\n")
	if saved.Proxy != active.Proxy {
		t.Fatalf("规范值不同: %q %q", saved.Proxy, active.Proxy)
	}
	if requiresRebuild(saved, active) || !liveFieldsEqual(saved, active) {
		t.Fatal("同一上游的不同写法不应需要重建，也没有热更新变化")
	}
	changed := load("changed.env", "PROXY=1.2.3.4:8080:u:p@x\nWARM_WORKER_LIMIT=3\n")
	if requiresRebuild(changed, active) || liveFieldsEqual(changed, active) {
		t.Fatal("只改热更新字段时应走热更新")
	}
	other := load("other.env", "PROXY=1.2.3.4:8080:u:other\n")
	if !requiresRebuild(other, active) {
		t.Fatal("上游代理变化应需要重建")
	}
}

// captureStderr 运行 run 并返回期间写到标准错误的内容（flag 的 -h 输出写在这里）
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	var output []byte
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		output, _ = io.ReadAll(reader)
	}()
	func() {
		defer func() { os.Stderr = previous }()
		run()
	}()
	writer.Close()
	wait.Wait()
	reader.Close()
	return string(output)
}

// TestParseFlagsProxyDefaultHidden .env 的代理带认证时 -h 与参数错误的输出不含账号密码；--proxy 的冒号写法规范化后作为覆盖，
// 不传时没有覆盖，--proxy "" 覆盖为直连；不合法时的错误不含账号密码
func TestParseFlagsProxyDefaultHidden(t *testing.T) {
	base := config.Default()
	base.Proxy = "http://" + testProxyUser + ":" + testProxyPassword + "@1.2.3.4:8080"

	for _, args := range [][]string{{"-h"}, {"-no-such-flag"}} {
		cfg := base
		var err error
		output := captureStderr(t, func() { _, err = parseFlags(args, &cfg) })
		if err == nil {
			t.Fatalf("%v 应返回错误", args)
		}
		if args[0] == "-h" && !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("-h 应返回 flag.ErrHelp，得到 %v", err)
		}
		if !strings.Contains(output, "-proxy") {
			t.Fatalf("%v 的输出应列出 -proxy:\n%s", args, output)
		}
		assertNoProxySecret(t, strings.Join(args, " ")+" 的输出", output)
	}

	cfg := base
	options, err := parseFlags([]string{"--proxy", "h.example.com:1:u:p@ss"}, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if options.overrides.proxy == nil || *options.overrides.proxy != "http://u:p%40ss@h.example.com:1" || cfg.Proxy != *options.overrides.proxy {
		t.Fatalf("--proxy 应规范化后作为覆盖，得到 %v %q", options.overrides.proxy, cfg.Proxy)
	}

	cfg = base
	if options, err = parseFlags(nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if options.overrides.proxy != nil || cfg.Proxy != base.Proxy {
		t.Fatalf("不传 --proxy 时不应覆盖，得到 %v %q", options.overrides.proxy, cfg.Proxy)
	}

	cfg = base
	if options, err = parseFlags([]string{"--proxy", ""}, &cfg); err != nil {
		t.Fatal(err)
	}
	if options.overrides.proxy == nil || *options.overrides.proxy != "" || cfg.Proxy != "" {
		t.Fatalf("--proxy \"\" 应覆盖为直连，得到 %v %q", options.overrides.proxy, cfg.Proxy)
	}

	cfg = base
	if options, err = parseFlags([]string{"--proxy", " HTTP://H:1/ "}, &cfg); err != nil || *options.overrides.proxy != "HTTP://H:1/" {
		t.Fatalf("不带认证的值应只去首尾空白，得到 %v (%v)", options.overrides.proxy, err)
	}

	cfg = base
	_, err = parseFlags([]string{"--proxy", "ftp://" + testProxyUser + ":" + testProxyPassword + "@h:1"}, &cfg)
	if err == nil {
		t.Fatal("不合法的 --proxy 应报错")
	}
	assertNoProxySecret(t, "错误", err.Error())

	// 忘了写 --proxy、把带账号密码的代理写成位置参数：错误只带去掉账号密码的 scheme://host:port
	cfg = base
	_, err = parseFlags([]string{"1.2.3.4:8080:" + testProxyUser + ":" + testProxyPassword}, &cfg)
	if err == nil || !strings.Contains(err.Error(), `未知参数 "http://1.2.3.4:8080"`) {
		t.Fatalf("位置参数应报未知参数，得到 %v", err)
	}
	assertNoProxySecret(t, "未知参数错误", err.Error())

	// 用户名:密码:host:port 顺序的位置参数：整体隐藏
	cfg = base
	_, err = parseFlags([]string{testProxyUser + ":" + testProxyPassword + ":gate.example.com:7777"}, &cfg)
	if err == nil || !strings.Contains(err.Error(), `未知参数 "<已隐藏>"`) {
		t.Fatalf("位置参数应报未知参数并隐藏，得到 %v", err)
	}
	assertNoProxySecret(t, "未知参数错误", err.Error())

	// --proxy 的值有空格又没加引号：shell 拆出的后半段是密码的一部分，传了 --proxy 时位置参数一律不回显
	cfg = base
	_, err = parseFlags([]string{"--proxy", "h.example:1:" + testProxyUser + ":pre", testProxyPassword}, &cfg)
	if err == nil || err.Error() != unknownArgumentAfterProxy {
		t.Fatalf("传了 --proxy 又有位置参数应报固定文案，得到 %v", err)
	}
	assertNoProxySecret(t, "未知参数错误", err.Error())
}

// proxyRecordingLogin 为测试用隔离登录：记录收到的代理，返回带登录签名 Cookie 的认证状态
type proxyRecordingLogin struct {
	mu      sync.Mutex
	proxies []string
	email   string
}

func (login *proxyRecordingLogin) Login(_ context.Context, request aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	login.mu.Lock()
	login.proxies = append(login.proxies, request.Proxy)
	login.mu.Unlock()
	cookies := make([]aistudio.StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{
			Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
		})
	}
	return aistudio.IsolatedLoginResult{
		Email: login.email, StorageState: aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}},
	}, nil
}

func (login *proxyRecordingLogin) Verify(
	context.Context, aistudio.IsolatedLoginRequest, aistudio.StorageState,
) (aistudio.LoginVerification, error) {
	return aistudio.LoginVerification{}, errors.New("测试中不验证")
}

func (login *proxyRecordingLogin) recorded() []string {
	login.mu.Lock()
	defer login.mu.Unlock()
	return append([]string(nil), login.proxies...)
}

// diskAccountProxy 读取账户目录中 account.json 的 proxy 字段
func diskAccountProxy(t *testing.T, directory string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	return value.Proxy
}

// accountDirectory 从账户目录根下按账户 ID 找到账户目录
func accountDirectory(t *testing.T, admin *runtimeAdmin, accountID string) string {
	t.Helper()
	accounts, _ := admin.store.LoadTolerant()
	for _, account := range accounts {
		if account.ID == accountID {
			return account.Directory
		}
	}
	t.Fatalf("账户 %s 不存在", accountID)
	return ""
}

// TestCreateAccountNormalizesProxy 添加账户：隔离登录收到的是规范化的上游代理（不是本机中转地址），account.json 里也是规范值；
// 不合法时返回 invalid_account，文案以“账户 proxy 无效: ”开头、不含账号密码，也不启动登录
func TestCreateAccountNormalizesProxy(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, _ := enableTestAdmin(t, "a@example.com")
	login := &proxyRecordingLogin{email: "new@example.com"}
	admin.login = login

	const want = "socks5://" + testProxyUser + ":pw%3A1@1.2.3.4:1080"
	created, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{
		Proxy: "socks5h://1.2.3.4:1080:" + testProxyUser + ":pw:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proxies := login.recorded(); len(proxies) != 1 || proxies[0] != want {
		t.Fatalf("隔离登录收到的代理 = %q，期望 %q", proxies, want)
	}
	if created.Proxy != want {
		t.Fatalf("返回的账户代理 = %q", created.Proxy)
	}
	if got := diskAccountProxy(t, accountDirectory(t, admin, created.ID)); got != want {
		t.Fatalf("account.json 的 proxy = %q", got)
	}

	_, err = admin.CreateAccount(context.Background(), api.AccountCreateInput{
		Proxy: "ftp://" + testProxyUser + ":" + testProxyPassword + "@h:1",
	})
	message := assertAdminError(t, err, http.StatusBadRequest, "invalid_account")
	if !strings.HasPrefix(message, "账户 proxy 无效: ") {
		t.Fatalf("文案应以“账户 proxy 无效: ”开头: %s", message)
	}
	assertNoProxySecret(t, "错误文案", message)
	if len(login.recorded()) != 1 {
		t.Fatal("代理不合法时不应启动隔离登录")
	}
	assertNoProxySecretInLogs(t, admin.requests)
}

// TestImportChromeAccountsRejectsProxyFirst 代理不合法时直接返回 invalid_account，不先做 Chrome 导入
func TestImportChromeAccountsRejectsProxyFirst(t *testing.T) {
	// 账户池、Worker 等都为空：只要先找 Chrome 目录或导入，就会得到别的错误或空指针
	admin := &runtimeAdmin{}
	for _, proxy := range []string{
		"ftp://" + testProxyUser + ":" + testProxyPassword + "@h:1",
		"h:1:" + testProxyUser,
		"http://us%3Aer:" + testProxyPassword + "@h:1",
	} {
		_, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{
			AccountIDs: []string{"Default/a@example.com"}, Proxy: proxy,
		})
		message := assertAdminError(t, err, http.StatusBadRequest, "invalid_account")
		if !strings.HasPrefix(message, "账户 proxy 无效: ") {
			t.Fatalf("%q: 文案应以“账户 proxy 无效: ”开头: %s", proxy, message)
		}
		assertNoProxySecret(t, "错误文案", message)
	}
}

// TestUpdateAccountNormalizesProxy 保存账户：冒号写法改写成规范值写入 account.json；onboarding 与页面回传规范值时内容不变；
// 不合法时返回 invalid_account，文案以“账户 proxy 无效: ”开头、不含账号密码，account.json 不变
func TestUpdateAccountNormalizesProxy(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	accountID := accounts[0].ID
	directory := accounts[0].Directory

	const want = "http://" + testProxyUser + ":p%23w@[2001:db8::1]:8080"
	input := manualDisableInput(accountID, true)
	input.Proxy = "[2001:db8::1]:8080:" + testProxyUser + ":p#w"
	updated, err := admin.UpdateAccount(context.Background(), accountID, input)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Proxy != want || diskAccountProxy(t, directory) != want {
		t.Fatalf("返回 %q，account.json %q，期望 %q", updated.Proxy, diskAccountProxy(t, directory), want)
	}
	// onboarding 与页面把返回的规范值原样回写：内容不变
	input.Proxy = updated.Proxy
	if again, err := admin.UpdateAccount(context.Background(), accountID, input); err != nil || again.Proxy != want {
		t.Fatalf("回写规范值得到 %q (%v)", again.Proxy, err)
	}

	for _, invalid := range []string{
		"socks5://" + testProxyUser + ":" + testProxyPassword + "@h",
		"http://" + testProxyUser + ":" + testProxyPassword + "@h:99999",
	} {
		input.Proxy = invalid
		_, err := admin.UpdateAccount(context.Background(), accountID, input)
		message := assertAdminError(t, err, http.StatusBadRequest, "invalid_account")
		if !strings.HasPrefix(message, "账户 proxy 无效: ") {
			t.Fatalf("%q: 文案应以“账户 proxy 无效: ”开头: %s", invalid, message)
		}
		assertNoProxySecret(t, "错误文案", message)
		if diskAccountProxy(t, directory) != want {
			t.Fatalf("%q: 不合法时不应改写 account.json", invalid)
		}
	}
}
