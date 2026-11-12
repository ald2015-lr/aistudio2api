package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// visibleLoginTimeout 为可见隔离登录（管理页添加账户、重新登录与 setup 登录）从启动浏览器到导出认证状态的期限：
// 用户要在浏览器里手动登录、完成两步验证，固定 30 分钟，不跟随 REQUEST_TIMEOUT（默认 5 分钟，调小后来不及完成两步验证）。
// 无头验证（Verify）仍用创建驱动时给的时限：它占着账户的独占租约，不需要更长
const visibleLoginTimeout = 30 * time.Minute

// NativeLoginDriver 通过纯 Go WebDriver BiDi 完成隔离登录；timeout 只用于无头验证，可见登录固定为 visibleLoginTimeout
type NativeLoginDriver struct {
	camoufox string
	timeout  time.Duration
}

var _ IsolatedLoginDriver = (*NativeLoginDriver)(nil)

// NewNativeLoginDriver 创建纯 Go Camoufox 登录驱动；timeout 为无头验证的时限（调用方传 REQUEST_TIMEOUT），
// 可见登录的时限固定为 visibleLoginTimeout
func NewNativeLoginDriver(camoufoxPath string, timeout time.Duration) (*NativeLoginDriver, error) {
	camoufoxPath = strings.TrimSpace(camoufoxPath)
	if camoufoxPath == "" {
		return nil, errors.New("缺少 Camoufox 路径")
	}
	absolute, err := filepath.Abs(camoufoxPath)
	if err != nil {
		return nil, fmt.Errorf("解析 Camoufox 路径: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("读取 Camoufox: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("Camoufox 路径是目录")
	}
	if timeout <= 0 {
		return nil, errors.New("Camoufox 登录超时必须为正数")
	}
	return &NativeLoginDriver{camoufox: absolute, timeout: timeout}, nil
}

// Login 启动可见隔离 Camoufox 并导出认证状态
func (driver *NativeLoginDriver) Login(ctx context.Context, request IsolatedLoginRequest) (IsolatedLoginResult, error) {
	if driver == nil {
		return IsolatedLoginResult{}, errors.New("纯 Go Camoufox 登录驱动未初始化")
	}
	options, relay, err := driver.relayOptions(request, "登录", true)
	if err != nil {
		return IsolatedLoginResult{}, err
	}
	// camoufoxnative.Login 返回前会在 defer 里 session.Close()，但 Close 的错误被 errors.Join 进返回值，
	// 驱动分不清浏览器是否已退出；这里照常释放，监听器按活动判定关闭，浏览器还在发连接就不会关
	defer relay.Release()
	startedAt := time.Now()
	result, err := camoufoxnative.Login(ctx, options)
	if err != nil {
		return IsolatedLoginResult{}, relay.WrapFailure(err, startedAt, proxyrelay.GoogleTarget)
	}
	var state StorageState
	if err := json.Unmarshal(result.StorageStateJSON, &state); err != nil {
		return IsolatedLoginResult{}, fmt.Errorf("解析隔离登录状态: %w", err)
	}
	if err := state.Validate(); err != nil {
		return IsolatedLoginResult{}, err
	}
	if err := state.SetAuthExtension(AuthExtension{
		Source: AuthSource{Browser: "camoufox", Email: result.Email},
	}); err != nil {
		return IsolatedLoginResult{}, err
	}
	return IsolatedLoginResult{
		StorageState: state, Email: result.Email, VerifiedAt: result.VerifiedAt, DriveError: result.DriveError,
	}, nil
}

// Verify 使用无头隔离 Camoufox 验证已有认证状态
func (driver *NativeLoginDriver) Verify(ctx context.Context, request IsolatedLoginRequest, state StorageState) (LoginVerification, error) {
	if driver == nil {
		return LoginVerification{}, errors.New("纯 Go Camoufox 登录驱动未初始化")
	}
	if err := state.Validate(); err != nil {
		return LoginVerification{}, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return LoginVerification{}, fmt.Errorf("编码隔离验证状态: %w", err)
	}
	options, relay, err := driver.relayOptions(request, "验证", false)
	if err != nil {
		return LoginVerification{}, err
	}
	// 释放时机同 Login：监听器按活动判定关闭
	defer relay.Release()
	startedAt := time.Now()
	verification, err := camoufoxnative.Verify(ctx, options, encoded)
	if err != nil {
		return LoginVerification{}, relay.WrapFailure(err, startedAt, proxyrelay.GoogleTarget)
	}
	return LoginVerification{
		Authenticated: verification.Authenticated,
		VerifiedAt:    verification.VerifiedAt,
		Reason:        verification.Reason,
		PageURL:       VerificationPagePath(verification.PageURL),
		OrgDisabled:   verification.OrgDisabled,
	}, nil
}

// relayOptions 生成 Camoufox 登录参数：带账号密码的代理换成本机中转地址，调用方负责释放租约（对 nil 安全）。
// operation 与账户 ID 组成中转日志里的使用者，新账户与 setup 登录还没有账户 ID，记为“新账户”
func (driver *NativeLoginDriver) relayOptions(
	request IsolatedLoginRequest, operation string, visible bool,
) (camoufoxnative.LoginOptions, *proxyrelay.Lease, error) {
	owner := operation + ":" + request.AccountID
	if request.AccountID == "" || request.AccountID == "new" || request.AccountID == "setup" {
		owner = operation + ":新账户"
	}
	relay, proxy, err := acquireProxyRelay(request.Proxy, owner)
	if err != nil {
		return camoufoxnative.LoginOptions{}, nil, err
	}
	options := driver.options(request, visible)
	options.Proxy = proxy
	return options, relay, nil
}

// options 生成 Camoufox 登录参数：visible 为可见登录，时限固定为 visibleLoginTimeout；无头验证用驱动的 timeout
func (driver *NativeLoginDriver) options(request IsolatedLoginRequest, visible bool) camoufoxnative.LoginOptions {
	timeout := driver.timeout
	if visible {
		timeout = visibleLoginTimeout
	}
	return camoufoxnative.LoginOptions{
		ExecutablePath:      driver.camoufox,
		Directory:           request.Directory,
		Locale:              request.Locale,
		Timezone:            request.Timezone,
		Proxy:               request.Proxy,
		Timeout:             timeout,
		OnboardingCompleted: request.OnboardingCompleted,
	}
}
