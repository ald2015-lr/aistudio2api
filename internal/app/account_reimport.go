package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// 已有账户的认证状态写回（重新登录与 Chrome 就地重新导入共用，对应上游 f27c6c4 的 replaceAccountState）：
//   - 新状态没有 Chrome OAuth 续签材料、账户原有的状态有时，沿用原有的来源与 OAuth（keepAuthExtension）：浏览器重新登录导出的状态
//     只带 camoufox 来源，原先直接保存会丢掉 Chrome 导入写入的续签材料，之后遇到 401 不能原地续签、直接转为需要登录；
//   - 上游另有 setCookieAuthUser（把 Cookie 的会话序号同步到 Worker）：本分叉的账户配置没有会话序号，x-goog-authuser 固定为 0，
//     也没有多会话的 Cookie 粘贴导入，不适用；上游“粘贴 Cookie 对话框的全选覆盖全部候选”同样没有对应界面。本分叉 Chrome 导入的全选
//     只选新账号与需要重新登录的已有账户（上游只把需要登录的已有账户留在候选里；本分叉全部列出，状态正常的要单独勾选，见 AccountsPanel.vue）。
// 浏览器添加账户不做就地更新：它用新生成的指纹登录，覆盖已有账户等于换掉账户的浏览器身份，已存在时返回 409（见 errAccountExists）

// chromeDiscover 列出本机 Chrome 的 Google 账号（导入候选与就地重新导入的分流用）；测试替换
var chromeDiscover = chromeauth.Discover

// errAccountExists 返回添加的账户已经存在、没有重复添加的错误（409 account_exists），消息带账户邮箱。loaded 为账户已在号池中
// （浏览器添加时登录的是已有账户）：按它的状态处理，需要登录时用“重新登录”（账户行只对需要登录的启用账户显示这个按钮），
// 停用的先启用，其他状态不需要再添加；否则是同邮箱的账户目录已在 auth 目录下、还没被目录扫描载入（store.Create 遇到已有账户文件，
// 浏览器添加与 Chrome 导入新账号都会遇到），它不在账户列表里，等目录扫描载入。原先一律提示“对该账户使用重新登录”，
// 多数状态下列表里没有这个按钮。管理页从消息里取出邮箱，按列表里该账户的状态显示本地化文案（见 AccountsPanel.vue 的 accountExistsNotice）
func errAccountExists(accountID string, loaded bool) error {
	accountID = strings.TrimSpace(accountID)
	message := fmt.Sprintf("账户已存在: %s，没有重复添加：状态为需要登录时在账户列表里对它使用重新登录，停用的先启用，其他状态不需要再添加", accountID)
	if !loaded {
		message = fmt.Sprintf("账户目录已存在: %s，没有重复添加：目录扫描会自动载入它，载入后需要登录时再对它使用重新登录", accountID)
	}
	return &adminOperationError{status: http.StatusConflict, code: "account_exists", message: message}
}

// keepAuthExtension 在新认证状态没有 Chrome OAuth 续签材料、账户原有状态有时，把原有的认证扩展（来源与 OAuth）写进新状态，
// 返回是否沿用。原有扩展读不出来、没有 OAuth，或来源邮箱与新状态的来源邮箱不一致时不沿用（不阻止保存新登录）
func keepAuthExtension(next *aistudio.StorageState, current aistudio.StorageState) (bool, error) {
	extension, _, err := next.AuthExtension()
	if err != nil {
		return false, err
	}
	if extension.OAuth != nil {
		return false, nil
	}
	previous, exists, err := current.AuthExtension()
	if err != nil || !exists || previous.OAuth == nil {
		return false, nil
	}
	nextEmail, previousEmail := strings.TrimSpace(extension.Source.Email), strings.TrimSpace(previous.Source.Email)
	if nextEmail != "" && previousEmail != "" && !strings.EqualFold(nextEmail, previousEmail) {
		return false, nil
	}
	if err := next.SetAuthExtension(previous); err != nil {
		return false, err
	}
	return true, nil
}

// keepBrowserLocalStorage 在新认证状态没有 AI Studio 的 localStorage（aistudio.BrowserLocalStorageOrigin）、账户原有状态有时，
// 把原有的这一个 origin 写进新状态（其他 origin 照新状态），返回是否沿用。这份 localStorage 是 Worker 启动后回写的页面设置、
// 已关闭的引导等（见 aistudio.AccountPool.MergeBrowserLocalStorage），与登录会话无关；Chrome 导入得到的状态没有任何 origin，
// 原样保存会把它清空，下一次 Worker 启动像新账户一样从空的 localStorage 开始
func keepBrowserLocalStorage(next *aistudio.StorageState, current aistudio.StorageState) bool {
	for _, origin := range next.Origins {
		if origin.Origin == aistudio.BrowserLocalStorageOrigin {
			return false
		}
	}
	for _, origin := range current.Origins {
		if origin.Origin == aistudio.BrowserLocalStorageOrigin && len(origin.LocalStorage) > 0 {
			next.Origins = append(slices.Clone(next.Origins), aistudio.StorageOrigin{
				Origin: origin.Origin, LocalStorage: slices.Clone(origin.LocalStorage),
			})
			return true
		}
	}
	return false
}

// replaceAccountState 在账户的独占租约内用新的认证状态替换认证文件：沿用原有的 Chrome OAuth 续签材料（见 keepAuthExtension），
// fingerprintDirectory 非空时先把那里的 Camoufox 指纹写回账户目录（重新登录时是账户原有指纹的副本）。fingerprintDirectory 为空时是
// 就地重新导入：指纹不变，并沿用 Worker 回写的 AI Studio localStorage（见 keepBrowserLocalStorage），只覆盖 Cookie 与续签材料。
// 返回是否沿用了续签材料。重新登录与就地重新导入共用
func replaceAccountState(lease *aistudio.AccountLease, state aistudio.StorageState, fingerprintDirectory string) (bool, error) {
	account := lease.Account()
	if fingerprintDirectory != "" {
		if err := camoufoxnative.PersistAccountFingerprint(fingerprintDirectory, account.Directory); err != nil {
			return false, err
		}
	}
	carried := false
	// 原有状态读不出来（文件损坏）时不沿用，新登录照常保存
	if current, err := lease.ReloadStorageState(); err == nil {
		if carried, err = keepAuthExtension(&state, current); err != nil {
			return false, err
		}
		if fingerprintDirectory == "" {
			keepBrowserLocalStorage(&state, current)
		}
	}
	return carried, lease.SaveStorageState(state)
}

// chromeReimport 为一个就地重新导入的 Chrome 账号：chromeID 为 Chrome 侧的账号 ID，accountID 为号池里同一邮箱的账户
type chromeReimport struct {
	chromeID  string
	accountID string
}

// splitChromeImport 按 Chrome 账号的邮箱把要导入的账号分成新账户与已有账户：邮箱已是号池里的账户时就地重新导入，
// 其余（含本机已经找不到的 ID，交给导入报错）按新账户导入
func (admin *runtimeAdmin) splitChromeImport(root string, chromeIDs []string) ([]string, []chromeReimport, error) {
	discovered, err := chromeDiscover(root)
	if err != nil {
		return nil, nil, err
	}
	emails := make(map[string]string, len(discovered))
	for _, account := range discovered {
		emails[account.ID] = strings.ToLower(strings.TrimSpace(account.Email))
	}
	fresh := make([]string, 0, len(chromeIDs))
	existing := make([]chromeReimport, 0)
	reimported := make(map[string]struct{})
	for _, chromeID := range chromeIDs {
		email := emails[chromeID]
		if email != "" {
			if _, ok := admin.pool.StatusOf(email); ok {
				// 同一个账户只重新导入一次（同一邮箱出现在多个 Profile 或 ID 重复提交）
				if _, duplicate := reimported[email]; !duplicate {
					reimported[email] = struct{}{}
					existing = append(existing, chromeReimport{chromeID: chromeID, accountID: email})
				}
				continue
			}
		}
		fresh = append(fresh, chromeID)
	}
	return fresh, existing, nil
}

// reimportChromeAccount 从本机 Chrome 就地重新导入已有账户：用账户自己的出口（账户代理，或 IP 池里它的端口）签发 Cookie，
// 导入结束就归还出口、不 adopt；邮箱必须与账户一致、认证状态必须能签名，然后交给 updateImportedAccount
func (admin *runtimeAdmin) reimportChromeAccount(ctx context.Context, root string, target chromeReimport) (api.AdminAccount, error) {
	status, ok := admin.pool.StatusOf(target.accountID)
	if !ok {
		return api.AdminAccount{}, accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, target.accountID))
	}
	proxy, lease := admin.browserProxy(status.ID, status.ID, proxyPoolKindImport, status.Label, status.Proxy)
	imported, err := chromeImport(ctx, chromeauth.ImportOptions{ChromeRoot: root, Proxy: proxy, AccountIDs: []string{target.chromeID}})
	lease.release()
	if err != nil {
		return api.AdminAccount{}, err
	}
	if len(imported) != 1 || !strings.EqualFold(strings.TrimSpace(imported[0].Email), status.ID) {
		return api.AdminAccount{}, invalidAccount(fmt.Errorf("Chrome 账号邮箱与账户不一致: %s", status.ID))
	}
	if _, err := aistudio.NewSigner().Sign(imported[0].State); err != nil {
		return api.AdminAccount{}, fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	return admin.updateImportedAccount(ctx, status.ID, imported[0].State, "Chrome 重新导入")
}

// updateImportedAccount 就地替换已有账户的认证状态（Cookie 与 Chrome OAuth 续签材料）：保留账户配置（启用状态、代理、语言、时区）、
// Camoufox 指纹、AI Studio 的 localStorage、IP 池出口，以及 runtime-state.json 里的资源绑定、冷却与模型资格。步骤与认证文件更新
// （reloadAccountCredentials）一致：先取消启停的后台收尾，在独占租约内关闭旧 Worker（它用的是旧 Cookie）、写回认证状态、标为就绪，
// 清除 403 无权限记录与冷却（一并清除组织停用状态），释放后清掉疑似失效与登录复核的记录，再同步一次模型目录。
// 被取消的收尾在结束时（成功或失败）按原阶段重新开始：停用的账户照常重建固定出口、显示阶段，启用的账户没有目录时同步一次
func (admin *runtimeAdmin) updateImportedAccount(
	ctx context.Context, accountID string, state aistudio.StorageState, source string,
) (api.AdminAccount, error) {
	startedAt := time.Now()
	stage := admin.cancelTransition(accountID)
	resume := stage
	defer func() { admin.resumeTransition(accountID, resume) }()
	lease, err := admin.acquireAccountForOperation(ctx, accountID)
	if err != nil {
		return api.AdminAccount{}, err
	}
	account := lease.Account()
	label := account.Config.Label
	if err := admin.workers.Reset(account.ID); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	if _, err := replaceAccountState(lease, state, ""); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	if err := admin.service.changeModels(func() error {
		return admin.pool.MarkReady(account.ID)
	}); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	admin.service.forgetPermissionDenied(account.ID, source)
	if err := lease.Release(); err != nil {
		return api.AdminAccount{}, err
	}
	admin.service.noteLoginRestored(account.ID)
	admin.syncAccountModelCatalog(ctx, account)
	// 在返回的账户视图之前恢复收尾，账户行照常显示阶段
	admin.resumeTransition(account.ID, resume)
	resume = ""
	admin.requests.log(label, "INFO", fmt.Sprintf(
		"账户已就地重新导入 | 来源=%s | 保留指纹、资源绑定与运行态 | 耗时=%s",
		source, time.Since(startedAt).Round(time.Millisecond),
	))
	return admin.account(account.ID)
}
