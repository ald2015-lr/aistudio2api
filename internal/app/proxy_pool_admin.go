package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池的管理端接入：登录、验证、导入的出口租约，/api/config 的 proxy_pool 字段，“检查全部端口”与“改为跟随默认代理”

// runtimeManager 与 runtimeAdmin 实现 api 包按可选接口识别的 IP 池接口（POST /api/proxy/pool/check、
// POST /api/accounts/proxy/follow-default），不改 AdminService
var _ interface {
	CheckProxyPool(context.Context, api.ProxyPoolCheckInput) error
	FollowDefaultProxy(context.Context) (api.FollowDefaultProxyResult, error)
} = (*runtimeManager)(nil)

// ipPool 返回生成服务使用的 IP 池；测试构造的管理端没有 Worker 管理器时为 nil（IP 池的方法对 nil 安全）
func (admin *runtimeAdmin) ipPool() *proxyPool {
	if admin == nil {
		return nil
	}
	return admin.workers.ipPool()
}

// browserProxy 为登录、验证、导入申请出口：账户代理非空时直接用它（租约为 nil）；IP 池开启时申请独占租约（已有账户跟随它正在用的
// 出口：共用 Worker 的端口或会话号，Worker 仍走默认代理时用同一个上游），返回分到的上游；IP 池关闭时为直通租约，返回 PROXY。
// key 为租约键（已有账户为账户 ID，新账户登录为 "login:" + 随机串），调用方在浏览器类操作结束后 release（方法对 nil 安全）
func (admin *runtimeAdmin) browserProxy(key, accountID, kind, label, accountProxy string) (string, *proxyPoolLease) {
	if proxy := strings.TrimSpace(accountProxy); proxy != "" {
		return proxy, nil
	}
	fallback := admin.effectiveProxy("")
	lease := admin.ipPool().acquire(key, accountID, kind, label, fallback)
	if lease == nil {
		return fallback, nil
	}
	// 直通租约的 URL 为 fallback，或跟随的 Worker 实际使用的上游
	return lease.URL(), lease
}

// newLoginLeaseKey 返回新账户登录的租约键：登录成功、拿到邮箱之前没有账户 ID
func newLoginLeaseKey() string {
	return "login:" + newProxyPoolSession(rand.Reader, "", 16)
}

// chromeImport 为 Chrome 导入（Go 侧 OAuth），chromeImportRoot 为导入用的 Chrome 数据目录；测试替换
var (
	chromeImport     = chromeauth.Import
	chromeImportRoot = chromeauth.DefaultChromeRoot
)

// importChrome 从本机 Chrome 导入账号：IP 池开启且没有填账户代理时逐个账号导入，每个账号用自己的出口，一号一 IP；
// 否则整批一次（与以前相同）。claims 与 results 一一对应（整批导入时都是 nil）：导入只在 Go 侧做 OAuth、没有浏览器在用，
// 每个账号导入完就归还出口（不占着独占端口等整批添加结束），只记下端口号或会话号，调用方在账户新建成功后 adopt（记到账户名下，
// 之后的 Worker 回到这个 IP）。任何一个账号失败时整批失败（与以前相同）
func (admin *runtimeAdmin) importChrome(
	ctx context.Context, root string, accountProxy string, accountIDs []string,
) ([]chromeauth.ImportResult, []*proxyPoolClaim, error) {
	if strings.TrimSpace(accountProxy) != "" || !admin.ipPool().enabled() {
		proxy, lease := admin.browserProxy(newLoginLeaseKey(), "", proxyPoolKindImport, "auth", accountProxy)
		defer lease.release()
		results, err := chromeImport(ctx, chromeauth.ImportOptions{ChromeRoot: root, Proxy: proxy, AccountIDs: accountIDs})
		if err != nil {
			return nil, nil, err
		}
		return results, make([]*proxyPoolClaim, len(results)), nil
	}
	results := make([]chromeauth.ImportResult, 0, len(accountIDs))
	claims := make([]*proxyPoolClaim, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		imported, claim, err := admin.importChromeOne(ctx, root, accountID)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, imported...)
		for index := range imported {
			// 一次只导入一个账号，正常只有一个结果；万一有多个，出口只跟第一个走（只 adopt 一次），其余不记出口
			if index == 0 {
				claims = append(claims, claim)
			} else {
				claims = append(claims, nil)
			}
		}
	}
	return results, claims, nil
}

// importChromeOne 用一个出口租约导入一个 Chrome 账号：导入结束（成功或失败）就归还，成功时返回出口记录（账户新建成功后 adopt）
func (admin *runtimeAdmin) importChromeOne(
	ctx context.Context, root string, accountID string,
) ([]chromeauth.ImportResult, *proxyPoolClaim, error) {
	proxy, lease := admin.browserProxy(newLoginLeaseKey(), "", proxyPoolKindImport, "auth", "")
	imported, err := chromeImport(ctx, chromeauth.ImportOptions{ChromeRoot: root, Proxy: proxy, AccountIDs: []string{accountID}})
	if err != nil || len(imported) == 0 {
		lease.release()
		return imported, nil, err
	}
	return imported, lease.releaseClaim(), nil
}

// proxyPoolFromAPI 把接口里的 IP 池设置转成配置（按零值语义：端口为 0 时取默认值）
func proxyPoolFromAPI(value api.ProxyPoolConfig) config.ProxyPool {
	return config.ProxyPool{Mode: value.Mode, URL: value.URL, PortStart: value.PortStart, PortEnd: value.PortEnd}
}

// proxyPoolFromInput 规范化并校验管理页提交的 IP 池设置：端口为 0 表示输入框被清空，按无效端口报错（不按零值语义取默认值）；
// 错误文案与 .env 的校验相同，不含地址内容
func proxyPoolFromInput(value api.ProxyPoolConfig) (config.ProxyPool, error) {
	pool := proxyPoolFromAPI(value)
	if pool.PortStart == 0 {
		pool.PortStart = -1
	}
	if pool.PortEnd == 0 {
		pool.PortEnd = -1
	}
	return config.NormalizeProxyPool(pool)
}

// proxyPoolToAPI 返回 /api/config 的 IP 池设置：模式小写、地址为规范值（可能带账号密码，只在这个接口返回给管理页编辑）、端口为具体数字
func proxyPoolToAPI(pool config.ProxyPool) *api.ProxyPoolConfig {
	mode := strings.ToLower(strings.TrimSpace(pool.Mode))
	if mode == "" {
		mode = config.ProxyPoolOff
	}
	start, end := pool.Ports()
	return &api.ProxyPoolConfig{Mode: mode, URL: config.CanonicalProxyPoolURL(pool.URL), PortStart: start, PortEnd: end}
}

// CheckProxyPool 在当前生成服务上开始“检查全部端口”（服务停止时也能用）：input.Pool 为空时用当前生效的设置
// （IP 池热更新，保存后即生效）
func (manager *runtimeManager) CheckProxyPool(ctx context.Context, input api.ProxyPoolCheckInput) error {
	cfg := manager.generation().config.ProxyPool
	if input.Pool != nil {
		normalized, err := proxyPoolFromInput(*input.Pool)
		if err != nil {
			return invalidConfigError(err)
		}
		cfg = normalized
	}
	normalized, err := config.NormalizeProxyPool(cfg)
	if err != nil {
		return invalidConfigError(err)
	}
	if normalized.Mode != config.ProxyPoolStatic {
		return invalidConfigError(fmt.Errorf("检查全部端口只支持静态 IP（PROXY_POOL_MODE=static）"))
	}
	if manager.proxyPool == nil {
		return &adminOperationError{status: http.StatusServiceUnavailable, code: "proxy_pool_unavailable", message: "IP 池尚未就绪"}
	}
	// 检查在后台进行，跟随进程生命周期，不随这次管理请求结束；只走后台请求路径，不持有 proxyTestMu
	if !manager.proxyPool.startCheck(manager.lifecycle, normalized, manager.probeProxy) {
		return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_check_busy", message: "上一次检查还没有结束"}
	}
	return nil
}

// FollowDefaultProxy 在当前生成服务上把账户代理与默认代理相同的账户改为跟随默认代理
func (manager *runtimeManager) FollowDefaultProxy(ctx context.Context) (api.FollowDefaultProxyResult, error) {
	admin, ok := manager.generation().admin.(*runtimeAdmin)
	if !ok {
		return api.FollowDefaultProxyResult{}, fmt.Errorf("改为跟随默认代理尚未就绪")
	}
	return admin.FollowDefaultProxy(ctx)
}

// FollowDefaultProxy 把自己的代理与 PROXY 规范值相同（因此不使用 IP 池）的账户改为跟随默认代理：逐个账户最多等
// bulkEnableAcquireWait 空闲，清空 account.json 的 proxy；运行中的 Worker 不重启（补登记直通租约，Go 侧在浏览器关闭前仍走 PROXY），
// 关闭后改用 IP 池。账户正忙或 Worker 正在启动的跳过，计入 skipped
func (admin *runtimeAdmin) FollowDefaultProxy(ctx context.Context) (api.FollowDefaultProxyResult, error) {
	startedAt := time.Now()
	global := config.CanonicalProxy(admin.config.Proxy)
	result := api.FollowDefaultProxyResult{}
	if global == "" {
		return result, nil
	}
	var failures []error
	for _, accountID := range admin.ipPool().pinnedDefault() {
		if ctx.Err() != nil {
			break
		}
		changed, running, err := admin.followDefaultProxy(ctx, accountID, global)
		switch {
		case accountBusyError(err) || errors.Is(err, errFollowDefaultBusy):
			result.Skipped++
		case err != nil:
			failures = append(failures, err)
		case changed:
			result.Changed++
			if running {
				result.Running++
			}
		}
	}
	if result.Changed > 0 {
		admin.service.publishModelAccessNow()
	}
	level := "INFO"
	if result.Skipped > 0 || len(failures) > 0 {
		level = "WARN"
	}
	message := fmt.Sprintf("账户改为跟随默认代理 | 账户=%d | 运行中的 Worker=%d（关闭后改用 IP 池）| 跳过=%d | 耗时=%s",
		result.Changed, result.Running, result.Skipped, time.Since(startedAt).Round(time.Millisecond))
	if len(failures) > 0 {
		message += fmt.Sprintf(" | 失败=%d | 错误=%s", len(failures), strings.TrimSpace(failures[0].Error()))
	}
	admin.requests.log("service", level, message)
	if len(failures) > 0 && result.Changed == 0 && result.Skipped == 0 {
		return result, failures[0]
	}
	return result, nil
}

// followDefaultProxy 处理一个账户：在账户租约内确认它的代理仍等于 PROXY，清空后写回；返回是否改了与是否有运行中的 Worker
func (admin *runtimeAdmin) followDefaultProxy(ctx context.Context, accountID string, global string) (bool, bool, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, bulkEnableAcquireWait)
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		if acquireCtx.Err() != nil && ctx.Err() == nil {
			return false, false, errAccountBusy()
		}
		return false, false, accountOperationError(err)
	}
	accountConfig := lease.Account().Config
	if strings.TrimSpace(accountConfig.Proxy) == "" || config.CanonicalProxy(accountConfig.Proxy) != global {
		// 列出之后已被其他操作改掉
		return false, false, lease.Release()
	}
	accountConfig.Proxy = ""
	running, err := admin.workers.followDefaultProxy(accountID, func() error { return lease.SaveConfig(accountConfig) })
	if err != nil {
		return false, false, errors.Join(err, lease.Release())
	}
	// 账户代理清空：重算外部占用、pinned 与默认出口（有运行中的 Worker 时绑定仍是它的直通租约）
	admin.ipPool().refresh(admin.pool, accountID)
	return true, running, lease.Release()
}

// enabled 判断 IP 池当前是否开启（nil 为关闭）
func (pool *proxyPool) enabled() bool {
	if pool == nil {
		return false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.settings.cfg.Enabled()
}
