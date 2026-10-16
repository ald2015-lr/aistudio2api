package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 批量启用（POST /api/accounts/bulk-enable）：原先管理页逐个账户发 PUT，每启用一个都要写一次配置、合并一次全部账户的
// 模型目录、构建并推送一次全部账户的状态，账户上万时 O(N²)，一万多个账户要几个小时；等待账户空闲没有超时，
// 碰上正在验证的账户请求会一直挂着。现在一次请求处理一批：逐个账户只写 account.json，全部写完后只合并、推送一次，
// 没有模型目录的账户在后台限流同步。
// 批量启用是管理员的明确操作，与在账户页逐个启用一样：手动停用的账户也会启用，并清除它的手动停用记录
const (
	// bulkEnableConcurrency 为同时写回 account.json 的账户数
	bulkEnableConcurrency = 16
	// catalogBackfillConcurrency 为启用后在后台同步模型目录的并发数，批量启用与单个启用共用：一次启用上万个账户时
	// 目录请求不会挤满出口与代理，每个同步结果递增的目录版本也不会频繁让请求路径上的缓存失效
	catalogBackfillConcurrency = 16
)

var (
	// bulkEnableMaxPerCall 为单次请求最多启用的账户数，其余计入 remaining，管理页分批提交
	bulkEnableMaxPerCall = 1000
	// bulkEnableAcquireWait 为等待账户空闲的最长时间；账户正在验证、登录或处理请求时不等下去，报告 account_busy，稍后重试
	bulkEnableAcquireWait = 3 * time.Second
)

// bulkAccountEnabler 为管理接口 POST /api/accounts/bulk-enable 需要的能力；管理器把请求交给当前生成服务的管理端
type bulkAccountEnabler interface {
	EnableAccounts(context.Context, api.BulkEnableInput) (api.BulkEnableResult, error)
}

var (
	_ bulkAccountEnabler = (*runtimeManager)(nil)
	_ bulkAccountEnabler = (*runtimeAdmin)(nil)
)

// errAccountBusy 返回账户正忙、等不到空闲的错误
func errAccountBusy() error {
	return &adminOperationError{
		status: http.StatusConflict, code: "account_busy", message: "账户正忙（正在验证、登录或处理请求），稍后重试",
	}
}

// accountBusyError 判断错误是否为账户正忙
func accountBusyError(err error) bool {
	var operation *adminOperationError
	return errors.As(err, &operation) && operation.code == "account_busy"
}

// enableAccountConfig 只把账户改为启用并写回 account.json：代理、语言、时区不变，不重建固定出口与 Worker 配置
// （它们只取决于这些字段），不登记模型变化、不合并模型目录、不推送账户状态，由调用方在一批结束后统一处理；
// 清除账户的停用原因。账户最多等 bulkEnableAcquireWait 空闲，等不到返回 account_busy。返回是否由本次启用。
//
// automatic 为假时是管理员的明确操作（批量启用），手动停用的账户同样启用并清除记录；为真时是按默认启用的自动启用
// （认证文件更新后），在账户租约内再看一次停用记录，手动停用的不启用、返回 errAccountManuallyDisabled：
// 原先只在租约外判断，判断后、启用前被手动停用的账户仍被启用，手动停用记录也被清除
func (admin *runtimeAdmin) enableAccountConfig(ctx context.Context, accountID string, automatic bool) (bool, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, bulkEnableAcquireWait)
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		if acquireCtx.Err() != nil && ctx.Err() == nil {
			return false, errAccountBusy()
		}
		return false, accountOperationError(err)
	}
	accountConfig := lease.Account().Config
	if accountConfig.Enabled {
		return false, lease.Release()
	}
	if automatic && accountDisableNotes.keepDisabled(accountID) {
		return false, errors.Join(errAccountManuallyDisabled, lease.Release())
	}
	accountConfig.Enabled = true
	if err := lease.SaveConfig(accountConfig); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	accountDisableNotes.clear(accountID)
	return true, lease.Release()
}

// EnableAccounts 批量启用账户：去重后只处理停用的账户，单次最多 bulkEnableMaxPerCall 个（其余计入 remaining），
// 按 bulkEnableConcurrency 并行写回 account.json；全部写完后登记一次模型变化、合并并推送一次快照，
// 再在后台为没有模型目录的账户同步目录。启用不等待目录：登录已经失效的账户由目录同步与请求标为需要登录
func (admin *runtimeAdmin) EnableAccounts(ctx context.Context, input api.BulkEnableInput) (api.BulkEnableResult, error) {
	if !input.AllDisabled && len(input.AccountIDs) == 0 {
		return api.BulkEnableResult{}, invalidAccount(fmt.Errorf("没有指定要启用的账户"))
	}
	startedAt := time.Now()
	result := api.BulkEnableResult{Failed: []api.BulkEnableFailure{}}
	// 一次加锁取得全部账户的轻量状态，不构建完整的账户状态
	summaries := admin.pool.LoginSummaries()
	enabledBefore := make(map[string]bool, len(summaries))
	targets := make([]string, 0)
	for _, summary := range summaries {
		enabledBefore[summary.ID] = summary.Enabled
		if input.AllDisabled && !summary.Enabled {
			targets = append(targets, summary.ID)
		}
	}
	if !input.AllDisabled {
		seen := make(map[string]struct{}, len(input.AccountIDs))
		for _, value := range input.AccountIDs {
			accountID := strings.TrimSpace(value)
			if _, duplicate := seen[accountID]; accountID == "" || duplicate {
				continue
			}
			seen[accountID] = struct{}{}
			enabled, exists := enabledBefore[accountID]
			switch {
			case !exists:
				result.Failed = append(result.Failed, bulkEnableFailure(accountID,
					accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, accountID))))
			case enabled:
				result.Skipped++
			default:
				targets = append(targets, accountID)
			}
		}
	}
	if len(targets) > bulkEnableMaxPerCall {
		result.Remaining = len(targets) - bulkEnableMaxPerCall
		targets = targets[:bulkEnableMaxPerCall]
	}

	var resultMu sync.Mutex
	enabledIDs := make([]string, 0, len(targets))
	busy := 0
	jobs := make(chan string)
	var workers sync.WaitGroup
	for range min(bulkEnableConcurrency, len(targets)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for accountID := range jobs {
				enabled, err := admin.enableAccountConfig(ctx, accountID, false)
				resultMu.Lock()
				switch {
				case err != nil:
					result.Failed = append(result.Failed, bulkEnableFailure(accountID, err))
					if accountBusyError(err) {
						busy++
					}
				case enabled:
					enabledIDs = append(enabledIDs, accountID)
				default:
					// 处理前已被其他操作启用
					result.Skipped++
				}
				resultMu.Unlock()
			}
		}()
	}
	for _, accountID := range targets {
		jobs <- accountID
	}
	close(jobs)
	workers.Wait()

	result.Enabled = len(enabledIDs)
	sort.Slice(result.Failed, func(left, right int) bool { return result.Failed[left].ID < result.Failed[right].ID })
	if len(enabledIDs) > 0 {
		sort.Strings(enabledIDs)
		// 逐个写入时没有登记模型变化，写完后登记一次，合并刷新并推送一次快照
		admin.service.markModelsChanged()
		admin.syncModelCache()
		admin.startCatalogBackfill(enabledIDs)
	}
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"批量启用账户 | 启用=%d | 原本已启用=%d | 失败=%d | 其中账户正忙=%d | 未处理=%d | 耗时=%s",
		result.Enabled, result.Skipped, len(result.Failed), busy, result.Remaining, time.Since(startedAt).Round(time.Millisecond),
	))
	return result, nil
}

// bulkEnableFailure 把一个账户的错误转换为批量启用结果中的失败项
func bulkEnableFailure(accountID string, err error) api.BulkEnableFailure {
	failure := api.BulkEnableFailure{ID: accountID, Message: strings.TrimSpace(err.Error())}
	var operation *adminOperationError
	if errors.As(err, &operation) {
		failure.Code = operation.code
	}
	return failure
}

// catalogBackfillSlots 返回启用后同步模型目录的共用并发名额
func (admin *runtimeAdmin) catalogBackfillSlots() chan struct{} {
	admin.catalogSlotsOnce.Do(func() {
		admin.catalogSlots = make(chan struct{}, catalogBackfillConcurrency)
	})
	return admin.catalogSlots
}

// startCatalogBackfill 在后台为刚启用、还没有模型目录的账户同步目录：停用账户不参与启动时的目录同步，
// 启用后没有目录就不能预热，也接不到模型请求
func (admin *runtimeAdmin) startCatalogBackfill(accountIDs []string) {
	missing := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if status, ok := admin.pool.StatusOf(accountID); ok && status.Enabled && len(status.Models) == 0 {
			missing = append(missing, accountID)
		}
	}
	if len(missing) == 0 {
		return
	}
	go admin.backfillCatalogs(admin.lifecycle, missing)
}

// backfillCatalogs 同步一批账户的模型目录。批量启用与单个启用共用 catalogBackfillSlots，连续提交多批时并发不会叠加；
// 本批全部同步完后合并应用一次目录并推送，不逐个账户重算全量目录。同步失败的账户与启动时一样进入重试集合
func (admin *runtimeAdmin) backfillCatalogs(ctx context.Context, accountIDs []string) {
	startedAt := time.Now()
	slots := admin.catalogBackfillSlots()
	var refreshed, failed atomic.Int32
	var refreshes sync.WaitGroup
	defer refreshes.Wait()
	for _, accountID := range accountIDs {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		refreshes.Add(1)
		go func(accountID string) {
			defer refreshes.Done()
			defer func() { <-slots }()
			models, err := admin.service.refreshAccountModelCatalog(ctx, accountID)
			switch {
			case err != nil:
				failed.Add(1)
			case len(models) > 0:
				refreshed.Add(1)
			}
		}(accountID)
	}
	refreshes.Wait()
	if ctx.Err() != nil {
		return
	}
	if refreshed.Load() > 0 {
		admin.service.modelSyncMu.Lock()
		admin.service.applyCachedModelCatalog()
		admin.service.modelSyncMu.Unlock()
	}
	admin.service.publishModelAccess()
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"启用后的模型目录同步完成 | 账户=%d | 非空=%d | 失败=%d | 耗时=%s",
		len(accountIDs), refreshed.Load(), failed.Load(), time.Since(startedAt).Round(time.Millisecond),
	))
}
