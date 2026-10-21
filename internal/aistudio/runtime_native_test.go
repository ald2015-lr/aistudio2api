package aistudio

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// 关闭失败后 runtime 停在 WorkerClosing：之后才排到的 Prepare 不能再生成 proof，也不能把状态改回 Busy/Ready，
// 否则后台清理重试认不出这个关不掉的 runtime
func TestNativeWorkerPrepareAfterFailedCloseKeepsClosing(t *testing.T) {
	var proofs atomic.Int32
	worker, _ := newStubWorker("a@example.com", &stubRuntime{closeFailures: 1, hooks: StubWorkerHooks{
		Proof: func(context.Context) (string, error) {
			proofs.Add(1)
			return "!proof", nil
		},
	}})
	request := ProtectedRequest{Body: []byte(`[null,"x"]`), ProofField: 1}
	prepared, err := worker.Prepare(context.Background(), request)
	if err != nil || string(prepared.Body) != `["!proof","x"]` {
		t.Fatalf("就绪 Worker 应写入 proof，实际 body=%s err=%v", prepared.Body, err)
	}
	if err := worker.Close(); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	if _, err := worker.Prepare(context.Background(), request); !errors.Is(err, errWorkerClosed) {
		t.Fatalf("关闭中的 Worker 应拒绝生成 proof，实际 %v", err)
	}
	if proofs.Load() != 1 {
		t.Fatalf("关闭中的 Worker 不应再生成 proof，实际生成 %d 次", proofs.Load())
	}
	if phase := worker.State().Phase; phase != WorkerClosing {
		t.Fatalf("Prepare 不应改写关闭中的状态，实际 %s", phase)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("第二次关闭应成功: %v", err)
	}
	if _, err := worker.Prepare(context.Background(), request); !errors.Is(err, errWorkerClosed) {
		t.Fatalf("已关闭的 Worker 应拒绝生成 proof，实际 %v", err)
	}
}

// nativeLoginRedirectText 为 Camoufox 启动时被跳转到 Google 登录页的原样错误文本（camoufoxnative.Worker.bootstrap 的格式）
const nativeLoginRedirectText = "隔离登录态失效 url=https://accounts.google.com/v3/signin/identifier?continue=https%3A%2F%2Faistudio.google.com%2Fprompts%2Fnew_chat&flowName=GlifWebSignIn"

// Camoufox 启动时被跳转到 Google 登录页（原样错误文本，不预先包装哨兵）：识别为登录失效，原文保留，跳转地址可取出用于展示
func TestClassifyNativeStartErrorLoginRedirect(t *testing.T) {
	original := fmt.Errorf("%s", nativeLoginRedirectText)
	err := classifyNativeStartError(original)
	if !errors.Is(err, ErrAuthenticationRequired) || !DefinitiveAuthenticationFailure(err) {
		t.Fatalf("登录页跳转应识别为登录失效，实际 %v", err)
	}
	if !errors.Is(err, original) || err.Error() != ErrAuthenticationRequired.Error()+": "+nativeLoginRedirectText {
		t.Fatalf("应保留原错误与原文，实际 %q", err.Error())
	}
	if got := NativeLoginRedirectURL(err); got != nativeLoginRedirectText[len("隔离登录态失效 url="):] {
		t.Fatalf("跳转地址 = %q", got)
	}
	if again := classifyNativeStartError(err); again != err {
		t.Fatal("已经是登录失效的错误应原样返回，不重复包装")
	}
}

// 跳转地址的主机名不是 accounts.google.com（含只在路径或参数里出现的）：不改变错误
func TestClassifyNativeStartErrorOtherHost(t *testing.T) {
	for _, text := range []string{
		"隔离登录态失效 url=https://aistudio.google.com/prompts/new_chat?next=accounts.google.com",
		"隔离登录态失效 url=https://accounts.google.com.example.net/signin",
		"隔离登录态失效 url=",
	} {
		original := errors.New(text)
		if err := classifyNativeStartError(original); err != original || DefinitiveAuthenticationFailure(err) {
			t.Fatalf("%q 不应识别为登录失效，实际 %v", text, err)
		}
		if got := NativeLoginRedirectURL(original); got != "" {
			t.Fatalf("%q 不应取出跳转地址，实际 %q", text, got)
		}
	}
}

// 登录页跳转与清理错误（关闭浏览器、释放缓存）用 errors.Join 合并在一起时仍能识别，地址截到换行为止
func TestClassifyNativeStartErrorJoinedWithCleanup(t *testing.T) {
	joined := errors.Join(errors.New(nativeLoginRedirectText), errors.New("释放账户浏览器缓存: unlock: bad file descriptor"))
	err := classifyNativeStartError(joined)
	if !errors.Is(err, ErrAuthenticationRequired) || !DefinitiveAuthenticationFailure(err) {
		t.Fatalf("合并了清理错误的登录页跳转应识别为登录失效，实际 %v", err)
	}
	if got := NativeLoginRedirectURL(err); got != nativeLoginRedirectText[len("隔离登录态失效 url="):] {
		t.Fatalf("跳转地址应截到换行为止，实际 %q", got)
	}
}

// 输入框未就绪且读不到页面地址（INIT_TIMEOUT 先于页面等待到期时的原样文本）：机器慢或号的问题都可能，不改变错误
func TestClassifyNativeStartErrorPageNotReady(t *testing.T) {
	original := fmt.Errorf("AI Studio 输入框未就绪 url=: %w", context.DeadlineExceeded)
	err := classifyNativeStartError(original)
	if err != original || DefinitiveAuthenticationFailure(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("输入框未就绪不应识别为登录失效，实际 %v", err)
	}
}
