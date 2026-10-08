package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type chromeCookieRefreshFunc func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error)

// authRuntimeRefresher 使用账户保存的 Chrome OAuth 材料原地续签
type authRuntimeRefresher struct {
	refresh        chromeCookieRefreshFunc
	reset          func(string) error
	prepareHeaders func(string) (func(bool), error)
	globalProxy    string
	requests       *requestRegistry
	// publish 在账户被标为需要登录后推送管理页账户状态，使用生成服务的合并推送；为空时不推送
	publish func()
	// waitLimit 为续签等待同账户其他请求结束的上限，为 0 时使用 authRefreshWaitLimit
	waitLimit time.Duration
}

// authRefreshWaitLimit 为续签等待同账户其他正常请求结束的上限。同账户的请求可能正在等待本次续签所在的
// Worker 启动结束（ensureWorker 的 waitForOpening），不设上限会互相等待；超时后不再等待，继续续签
const authRefreshWaitLimit = 12 * time.Second

// authRetryTransport 为普通 RPC 执行一次认证续签重试
type authRetryTransport struct {
	transport aistudio.RPCTransport
	refresher *authRuntimeRefresher
}

// authRetryProtectedTransport 为受保护 RPC 执行一次认证续签重试
type authRetryProtectedTransport struct {
	transport aistudio.ProtectedTransport
	refresher *authRuntimeRefresher
}

type bidiReleaseGate struct {
	mu        sync.Mutex
	once      sync.Once
	release   func() error
	requested bool
	committed bool
	abandoned bool
	err       error
}

func newBidiReleaseGate(release func() error) *bidiReleaseGate {
	return &bidiReleaseGate{release: release}
}

func (gate *bidiReleaseGate) Release() error {
	gate.mu.Lock()
	if gate.abandoned {
		gate.mu.Unlock()
		return nil
	}
	if !gate.committed {
		gate.requested = true
		gate.mu.Unlock()
		return nil
	}
	gate.mu.Unlock()
	return gate.releaseNow()
}

func (gate *bidiReleaseGate) Commit() error {
	gate.mu.Lock()
	gate.committed = true
	requested := gate.requested
	gate.mu.Unlock()
	if !requested {
		return nil
	}
	return gate.releaseNow()
}

func (gate *bidiReleaseGate) Abandon() {
	gate.mu.Lock()
	gate.abandoned = true
	gate.mu.Unlock()
}

func (gate *bidiReleaseGate) releaseNow() error {
	gate.once.Do(func() {
		if gate.release != nil {
			gate.err = gate.release()
		}
	})
	return gate.err
}

// UploadDrive 将 Drive 上传委托给同一认证传输
func (transport *authRetryTransport) UploadDrive(
	ctx context.Context,
	accountID string,
	token string,
	request aistudio.UploadRequest,
) (aistudio.FileRef, error) {
	drive, ok := transport.transport.(aistudio.DriveTransport)
	if !ok {
		return aistudio.FileRef{}, fmt.Errorf("transport 不支持 Drive 上传")
	}
	return drive.UploadDrive(ctx, accountID, token, request)
}

// DownloadDrive 将 Drive 下载委托给同一认证传输
func (transport *authRetryTransport) DownloadDrive(
	ctx context.Context,
	accountID string,
	token string,
	fileID string,
) (aistudio.MediaStream, error) {
	drive, ok := transport.transport.(aistudio.DriveTransport)
	if !ok {
		return aistudio.MediaStream{}, fmt.Errorf("transport 不支持 Drive 下载")
	}
	return drive.DownloadDrive(ctx, accountID, token, fileID)
}

// DeleteDrive 将 Drive 删除委托给同一认证传输
func (transport *authRetryTransport) DeleteDrive(
	ctx context.Context,
	accountID string,
	token string,
	fileID string,
) error {
	drive, ok := transport.transport.(aistudio.DriveTransport)
	if !ok {
		return fmt.Errorf("transport 不支持 Drive 删除")
	}
	return drive.DeleteDrive(ctx, accountID, token, fileID)
}

// newAuthRuntimeRefresher 创建生产环境认证续签器
func newAuthRuntimeRefresher(
	workers *accountWorkerManager,
	headers *accountHeaderProvider,
	requests *requestRegistry,
	globalProxy string,
) *authRuntimeRefresher {
	return &authRuntimeRefresher{
		refresh: chromeauth.Refresh, reset: workers.Reset, prepareHeaders: headers.prepareInvalidate,
		globalProxy: globalProxy,
		requests:    requests,
	}
}

func (provider *accountHeaderProvider) prepareInvalidate(accountID string) (func(bool), error) {
	provider.mu.RLock()
	account := provider.accounts[accountID]
	provider.mu.RUnlock()
	if account == nil {
		return nil, fmt.Errorf("账户固定出口不存在: %s", accountID)
	}
	account.mu.Lock()
	previous := account.headers.Clone()
	account.headers = nil
	return func(committed bool) {
		if !committed {
			account.headers = previous
		}
		account.mu.Unlock()
	}, nil
}

// markAuthenticationRequired 把当前租约账户标为需要登录并推送账户状态，返回保留原因的错误。
// 没有租约、请求已取消或错误不是登录失效时原样返回
func (refresher *authRuntimeRefresher) markAuthenticationRequired(ctx context.Context, cause error) error {
	lease, ok := aistudio.AccountLeaseFromContext(ctx)
	if !ok || ctx.Err() != nil || !aistudio.DefinitiveAuthenticationFailure(cause) {
		return cause
	}
	err := lease.MarkAuthenticationRequired(cause.Error())
	if refresher.publish != nil {
		refresher.publish()
	}
	return errors.Join(cause, err)
}

// Recover 为当前租约账户恢复一次登录：有续签材料时续签并重置 WAA runtime，成功返回 nil；
// 没有续签材料或续签失败时把账户标为需要登录，返回保留原因的错误。
// Worker 启动阶段已经处理过的登录失效（authHandled）不再恢复第二次
func (refresher *authRuntimeRefresher) Recover(ctx context.Context, cause error) error {
	var startup *accountWorkerInitError
	if errors.As(cause, &startup) && startup.authHandled {
		return cause
	}
	if refresher.Available(ctx) {
		err := refresher.Refresh(ctx)
		if err == nil {
			return nil
		}
		cause = errors.Join(cause, err)
	}
	return refresher.markAuthenticationRequired(ctx, cause)
}

// do 发送普通或受保护 RPC：登录失效（HTTP 401、协议 Code 16、登录页跳转、签名 Cookie 失效）时恢复一次并重放一次，
// 重放仍失效时把账户标为需要登录。带 WithoutAuthRecovery 标记的请求原样返回，不续签也不改账户状态
func (refresher *authRuntimeRefresher) do(
	ctx context.Context,
	method string,
	send func() (*aistudio.RPCResponse, error),
) (*aistudio.RPCResponse, error) {
	if refresher == nil || aistudio.AuthRecoveryDisabled(ctx) {
		return send()
	}
	for attempt := 0; ; attempt++ {
		response, err := send()
		if err == nil && authenticationFailed(response) {
			original, body, readErr := readAuthenticationFailure(method, response)
			if readErr != nil {
				return nil, readErr
			}
			if !aistudio.DefinitiveAuthenticationFailure(original) {
				// Drive 授权缺失等不表示登录失效的 401：还原响应正文，由调用方按原错误处理
				response.Body = io.NopCloser(bytes.NewReader(body))
				return response, nil
			}
			response, err = nil, original
		}
		if !aistudio.DefinitiveAuthenticationFailure(err) {
			return response, err
		}
		if attempt > 0 {
			return nil, refresher.markAuthenticationRequired(ctx, err)
		}
		if recoverErr := refresher.Recover(ctx, err); recoverErr != nil {
			return nil, recoverErr
		}
	}
}

// Do 在登录失效后恢复同一账户并重放一次请求
func (transport *authRetryTransport) Do(ctx context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return transport.refresher.do(ctx, request.Method, func() (*aistudio.RPCResponse, error) {
		return transport.transport.Do(ctx, request)
	})
}

// DoProtected 在登录失效后恢复同一账户并重放一次受保护请求
func (transport *authRetryProtectedTransport) DoProtected(
	ctx context.Context,
	request aistudio.GenerateRequest,
	rpc aistudio.RPCRequest,
) (*aistudio.RPCResponse, error) {
	return transport.refresher.do(ctx, rpc.Method, func() (*aistudio.RPCResponse, error) {
		return transport.transport.DoProtected(ctx, request, rpc)
	})
}

// OpenBidiProtected 在登录失效后恢复同一账户并重新建立 WebChannel
func (transport *authRetryProtectedTransport) OpenBidiProtected(
	ctx context.Context,
	request aistudio.BidiRequest,
	runtime aistudio.RequestContext,
	lease *aistudio.AccountLease,
	release func() error,
) (*aistudio.BidiSession, error) {
	bidiTransport, ok := transport.transport.(aistudio.BidiProtectedTransport)
	if !ok {
		return nil, fmt.Errorf("protected transport 不支持 BidiGenerateContent")
	}
	gate := newBidiReleaseGate(release)
	session, err := bidiTransport.OpenBidiProtected(ctx, request, runtime, lease, gate.Release)
	if err == nil {
		if releaseErr := gate.Commit(); releaseErr != nil {
			return nil, errors.Join(releaseErr, session.Close())
		}
		return session, nil
	}
	if !aistudio.DefinitiveAuthenticationFailure(err) || transport.refresher == nil || aistudio.AuthRecoveryDisabled(ctx) {
		return nil, errors.Join(err, gate.Commit())
	}
	gate.Abandon()
	if recoverErr := transport.refresher.Recover(ctx, err); recoverErr != nil {
		return nil, recoverErr
	}
	session, err = bidiTransport.OpenBidiProtected(ctx, request, runtime, lease, release)
	if aistudio.DefinitiveAuthenticationFailure(err) {
		err = transport.refresher.markAuthenticationRequired(ctx, err)
	}
	return session, err
}

// DoProtectedVideo 在登录失效后恢复同一账户并重放 Veo 请求
func (transport *authRetryProtectedTransport) DoProtectedVideo(
	ctx context.Context,
	request aistudio.VideoRequest,
	rpc aistudio.RPCRequest,
) (*aistudio.RPCResponse, error) {
	videoTransport, ok := transport.transport.(aistudio.VideoProtectedTransport)
	if !ok {
		return nil, fmt.Errorf("protected transport 不支持 GenerateVideo")
	}
	return transport.refresher.do(ctx, rpc.Method, func() (*aistudio.RPCResponse, error) {
		return videoTransport.DoProtectedVideo(ctx, request, rpc)
	})
}

// Refresh 续签当前租约账户并保存新的 storage state
func (refresher *authRuntimeRefresher) Refresh(ctx context.Context) error {
	lease, ok := aistudio.AccountLeaseFromContext(ctx)
	if !ok {
		return fmt.Errorf("认证续签缺少账户租约")
	}
	endRefresh, ok := lease.BeginAuthRefresh()
	if !ok {
		return fmt.Errorf("%w: 账户存在活动生成", aistudio.ErrAccountLeased)
	}
	defer endRefresh()
	account := lease.Account()
	if err := refresher.waitForOtherRequests(ctx, lease); err != nil {
		refresher.requests.log(account.Config.Label, "WARN", "账户认证续签放弃 | 错误="+err.Error())
		return err
	}
	startedAt := time.Now()
	refresher.requests.log(account.Config.Label, "INFO", "账户认证续签 | 1/2 | 刷新 Cookie")
	err := lease.RefreshStorageState(func(state *aistudio.StorageState) error {
		extension, exists, err := state.AuthExtension()
		if err != nil {
			return err
		}
		if !exists || extension.OAuth == nil {
			return fmt.Errorf("账户 %s 缺少 Chrome OAuth 续签材料", account.ID)
		}
		cookies, err := refresher.refresh(ctx, *extension.OAuth, account.EffectiveProxy(refresher.globalProxy))
		if err != nil {
			return fmt.Errorf("续签账户 %s: %w", account.ID, err)
		}
		state.Cookies = cookies
		// 续签结果必须能生成授权头才提交，否则会用不可用的 Cookie 覆盖原登录状态
		if _, err := aistudio.NewSigner().Sign(*state); err != nil {
			return fmt.Errorf("续签账户 %s 的 Cookie 无法签名: %w", account.ID, err)
		}
		return nil
	}, func() (func(bool), error) {
		refresher.requests.log(account.Config.Label, "INFO", "账户认证续签 | 2/2 | 重置协议运行时")
		if err := refresher.reset(account.ID); err != nil {
			return nil, fmt.Errorf("重置账户 %s runtime: %w", account.ID, err)
		}
		finish, err := refresher.prepareHeaders(account.ID)
		if err != nil {
			return nil, fmt.Errorf("刷新账户 %s 公共头: %w", account.ID, err)
		}
		return finish, nil
	})
	if err != nil {
		wrapped := fmt.Errorf("保存账户 %s 认证状态: %w", account.ID, err)
		refresher.requests.log(account.Config.Label, "ERROR", fmt.Sprintf(
			"账户认证续签失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), wrapped.Error(),
		))
		return wrapped
	}
	refresher.requests.log(account.Config.Label, "INFO", fmt.Sprintf(
		"账户认证续签完成 | 耗时=%s",
		time.Since(startedAt).Round(time.Millisecond),
	))
	return nil
}

// waitForOtherRequests 在有上限的时间内等待同账户其他正常请求结束（续签会重置它们正在使用的 WAA runtime）。
// 超过上限不再等待，返回 nil 继续续签：放弃续签会让有续签材料的账户被标为需要登录、退出调度，之后不会再自动恢复。
// 只有请求本身取消时返回错误
func (refresher *authRuntimeRefresher) waitForOtherRequests(ctx context.Context, lease *aistudio.AccountLease) error {
	limit := refresher.waitLimit
	if limit <= 0 {
		limit = authRefreshWaitLimit
	}
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := lease.WaitForAuthRefresh(waitCtx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		refresher.requests.log(lease.Account().Config.Label, "WARN", fmt.Sprintf(
			"账户认证续签 | 同账户其他请求 %s 内没有结束，不再等待", limit,
		))
	}
	return nil
}

// Available 返回当前租约账户是否保存了 Chrome OAuth 续签材料
func (refresher *authRuntimeRefresher) Available(ctx context.Context) bool {
	lease, ok := aistudio.AccountLeaseFromContext(ctx)
	if !ok {
		return false
	}
	state, err := lease.ReloadStorageState()
	if err != nil {
		return false
	}
	return authRefreshMaterial(state)
}

// authRefreshMaterial 返回认证状态是否带有可原地续签的 Chrome OAuth 材料
func authRefreshMaterial(state aistudio.StorageState) bool {
	extension, exists, err := state.AuthExtension()
	return err == nil && exists && extension.OAuth != nil
}

func authenticationFailed(response *aistudio.RPCResponse) bool {
	return response != nil && response.Body != nil && response.StatusCode == http.StatusUnauthorized
}

// readAuthenticationFailure 读取并关闭认证失败响应，返回原始原因与响应正文
func readAuthenticationFailure(method string, response *aistudio.RPCResponse) (*aistudio.RPCError, []byte, error) {
	body, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		return nil, nil, fmt.Errorf("读取认证失败响应: %w", err)
	}
	return aistudio.DecodeRPCError(method, response.StatusCode, body), body, nil
}

var _ aistudio.RPCTransport = (*authRetryTransport)(nil)
var _ aistudio.DriveTransport = (*authRetryTransport)(nil)
var _ aistudio.ProtectedTransport = (*authRetryProtectedTransport)(nil)
var _ aistudio.VideoProtectedTransport = (*authRetryProtectedTransport)(nil)
var _ aistudio.BidiProtectedTransport = (*authRetryProtectedTransport)(nil)
