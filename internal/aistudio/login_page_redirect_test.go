package aistudio

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// testLoginPageURL 为 Camoufox Worker 启动停在 Google 确认身份页的地址（照线上形态，参数为示例值）
const testLoginPageURL = "https://accounts.google.com/v3/signin/confirmidentifier?continue=https://aistudio.google.com/prompts/new_chat&flowName=GlifWebSignIn&flowEntry=ServiceLogin"

// testLoginPageError 返回与 camoufoxnative 启动停在登录页时相同形态的错误（哨兵 + " url=<地址>"）
func testLoginPageError() error {
	return fmt.Errorf("%w url=%s", camoufoxnative.ErrLoginPageRedirect, testLoginPageURL)
}

// TestDefinitiveAuthenticationFailureLoginPage Camoufox Worker 启动停在 Google 登录 / 确认身份页（ErrLoginPageRedirect）按登录失效处理：
// 文案与原来逐字相同；经中转包装、Join 清理错误、外层再包一层都认得；这次启动期间本机代理中转记到上游失败（ErrUpstream）时不算；
// 只有文案没有哨兵的错误不算（靠哨兵不靠文案）；servicerestricted 等拒绝页与地区说明页不算
func TestDefinitiveAuthenticationFailureLoginPage(t *testing.T) {
	login := testLoginPageError()
	if login.Error() != "隔离登录态失效 url="+testLoginPageURL {
		t.Fatalf("错误文案应与原来逐字相同，实际 %q", login.Error())
	}
	if ErrLoginPageRedirect != camoufoxnative.ErrLoginPageRedirect {
		t.Fatal("aistudio 的哨兵应与 camoufoxnative 的是同一个")
	}
	for name, err := range map[string]error{
		"原样":      login,
		"外层再包一层":  fmt.Errorf("WAA Worker 初始化: %w", login),
		"Join 清理": errors.Join(login, errors.New("释放账户浏览器缓存失败")),
	} {
		if !DefinitiveAuthenticationFailure(err) {
			t.Errorf("%s：应算登录失效: %v", name, err)
		}
		if AccessTokenAuthFailure(err) || OrgDisabledFailure(err) || upstreamAuthFailure(err) {
			t.Errorf("%s：不应算附件授权失败、组织停用或上游 401: %v", name, err)
		}
	}
	// 与 proxyrelay.Lease.WrapFailure 相同的包装：中转记到上游失败时附上说明并命中 ErrUpstream
	relayed := fmt.Errorf("%w；本机代理中转：上游代理拒绝了隧道：503 Service Unavailable（目标 accounts.google.com）%w", login, proxyrelay.ErrUpstream)
	if !errors.Is(relayed, ErrLoginPageRedirect) || DefinitiveAuthenticationFailure(relayed) {
		t.Fatalf("中转记到上游失败的登录页错误不应算登录失效: %v", relayed)
	}
	if !DefinitiveAuthenticationFailure(errors.Join(relayed, ErrAuthenticationRequired)) {
		t.Fatal("同时带签名 Cookie 失效等其他本地证据时照旧算登录失效")
	}
	if DefinitiveAuthenticationFailure(errors.New("隔离登录态失效 url=" + testLoginPageURL)) {
		t.Fatal("只有文案没有哨兵的错误不应算（识别靠哨兵）")
	}
	restricted := &camoufoxnative.ServiceNotAllowedError{URL: "accounts.google.com/info/servicerestricted", Kind: camoufoxnative.RestrictionService}
	region := &camoufoxnative.RegionNotSupportedError{URL: "ai.google.dev/gemini-api/docs/available-regions"}
	for _, err := range []error{restricted, region, &camoufoxnative.ServiceNotAllowedError{URL: "access.workspace.google.com/ServiceNotAllowed"}} {
		if DefinitiveAuthenticationFailure(err) || errors.Is(err, ErrLoginPageRedirect) {
			t.Fatalf("拒绝页与地区说明页不应算登录失效: %v", err)
		}
	}
	if !OrgDisabledFailure(restricted) {
		t.Fatal("servicerestricted 仍按组织停用一类处理")
	}
}

// TestLoginPageMarkSkipsAuthGate 登录页错误是本地证据：租约路径直接标需要登录，不经“疑似登录失效”的 gate
func TestLoginPageMarkSkipsAuthGate(t *testing.T) {
	accountID := "login-page@example.com"
	pool := testPoolWithAccount(t, accountID)
	gateCalls := 0
	pool.SetAuthFailureGate(func(*AccountLease, error) bool {
		gateCalls++
		return false
	})
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	marked, err := lease.MarkAuthenticationFailure(fmt.Errorf("WAA Worker 初始化: %w", testLoginPageError()))
	if err != nil || !marked || gateCalls != 0 {
		t.Fatalf("登录页错误应直接标记且不经 gate: marked=%t err=%v gate=%d", marked, err, gateCalls)
	}
	if status, _ := pool.StatusOf(accountID); status.State != AccountAuthRequired || status.Message != "WAA Worker 初始化: 隔离登录态失效 url="+testLoginPageURL {
		t.Fatalf("账户应为需要登录、原因保留原文: %+v", status)
	}
}

// TestCamoufoxLoginPageErrorWrapsSentinel 守住 camoufoxnative/worker.go 唯一一处登录页判断（openPromptPage 里
// strings.Contains(pageURL, "accounts.google.com") 的分支）返回的错误包着 ErrLoginPageRedirect，文案为“隔离登录态失效 url=<地址>”。
// 本包与 app 的测试都自己构造这种错误，丢掉这里的 %w（重构、合并冲突）时它们照样通过，线上启动落到登录页的账号又回到
// 一直就绪、被反复冷启动；camoufoxnative 的启动要真浏览器或整套假 BiDi，这里按源码核对这一处（其余页面判断与顺序由 camoufoxnative 的测试覆盖）
func TestCamoufoxLoginPageErrorWrapsSentinel(t *testing.T) {
	if camoufoxnative.ErrLoginPageRedirect.Error() != "隔离登录态失效" {
		t.Fatalf("哨兵文案应为“隔离登录态失效”，实际 %q", camoufoxnative.ErrLoginPageRedirect)
	}
	path := filepath.Join("..", "camoufoxnative", "worker.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var openPromptPage *ast.FuncDecl
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Name.Name == "openPromptPage" && function.Recv != nil {
			openPromptPage = function
		}
	}
	if openPromptPage == nil {
		t.Fatalf("%s 里没有 openPromptPage（改名或挪走时同步改这个测试）", path)
	}
	selector := func(expression ast.Expr, pkg string, name string) bool {
		call, ok := expression.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		ident, ok := call.X.(*ast.Ident)
		return ok && ident.Name == pkg && call.Sel.Name == name
	}
	stringLiteral := func(expression ast.Expr) (string, bool) {
		literal, ok := expression.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(literal.Value)
		return value, err == nil
	}
	branches, wrapped := 0, 0
	ast.Inspect(openPromptPage.Body, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		condition, ok := branch.Cond.(*ast.CallExpr)
		if !ok || !selector(condition.Fun, "strings", "Contains") || len(condition.Args) != 2 {
			return true
		}
		if host, ok := stringLiteral(condition.Args[1]); !ok || host != "accounts.google.com" {
			return true
		}
		branches++
		for _, statement := range branch.Body.List {
			result, ok := statement.(*ast.ReturnStmt)
			if !ok || len(result.Results) == 0 {
				continue
			}
			call, ok := result.Results[len(result.Results)-1].(*ast.CallExpr)
			if !ok || !selector(call.Fun, "fmt", "Errorf") || len(call.Args) != 3 {
				continue
			}
			format, ok := stringLiteral(call.Args[0])
			sentinel, isIdent := call.Args[1].(*ast.Ident)
			if ok && format == "%w url=%s" && isIdent && sentinel.Name == "ErrLoginPageRedirect" {
				wrapped++
			}
		}
		return true
	})
	if branches != 1 || wrapped != 1 {
		t.Fatalf("openPromptPage 的登录页分支应恰有一处、返回 fmt.Errorf(\"%%w url=%%s\", ErrLoginPageRedirect, pageURL)：分支 %d 处，包哨兵 %d 处", branches, wrapped)
	}
}
