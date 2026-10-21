package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 批量删除（POST /api/accounts/bulk-delete）：管理页“403 无权限账户”卡片的一键删除，把列表里的账户连同认证目录一起删除。
// 原先只能在账户页逐个删除，每删除一个都要合并一次全部账户的模型目录、构建并推送一次全部账户的状态（syncModelCache），
// 账户上万时删几十个就要很久；正在处理请求的账户直接返回 account_busy。现在一次请求处理一批：
//   - 每个账户的删除步骤与单个删除相同（removeAccount），整批结束后只登记一次模型变化、合并并推送一次快照，
//     新账户自动处理记录也只写一次文件；
//   - 开始前为整批账户登记独占等待（aistudio.AccountReservation，与后台关闭失败 Worker 用的是同一个原语）：登记后账户不再发出
//     新租约，进行中的请求照常结束，空闲后在同一次加锁内转为独占删除（AccountReservation.Remove），中间没有空档。
//     整批共用 bulkDeleteBusyWait 的等待上限，等不到的报告 account_busy，登记随之撤销、账户恢复原状。登记按次计数，
//     Remove 返回时撤销，没交给删除的由 defer 撤销，不会留下永久不接请求的账户，也不改动排空等其他标记；
//   - 号池里已经没有的账户（已被删除、目录被移走后移出，或从未载入）计入 missing，同时清除它的 403 无权限记录（账户 ID 与号池查找一样
//     按原样精确匹配，区分大小写）；
//     与单个删除、账户目录扫描同时删除同一账户时，后到的一方同样按 missing 处理
//
// 逐个删除不放进 changeModels：changeModels 本身只是在变化锁内递增模型代际（合并全部账户目录的是之后的 SyncModels），
// 但等待账户空闲期间占着变化锁会挡住启动收尾与模型同步，各账户的删除也会排成一队。删完后用 markModelsChanged 登记一次，
// 随后的 SyncModels 合并刷新（与批量启用相同）
const (
	// bulkDeleteConcurrency 为同时删除的账户数（关闭 Worker、删除认证目录）
	bulkDeleteConcurrency = 8
)

var (
	// bulkDeleteMaxPerCall 为单次请求最多删除的账户数（只计号池里还有的账户），其余计入 remaining，管理页分批提交
	bulkDeleteMaxPerCall = 500
	// bulkDeleteBusyWait 为整批等待正在使用的账户空闲的共同上限，从登记独占等待开始计时
	bulkDeleteBusyWait = 10 * time.Second
)

// bulkAccountDeleter 为管理接口 POST /api/accounts/bulk-delete 需要的能力；管理器把请求交给当前生成服务的管理端
type bulkAccountDeleter interface {
	DeleteAccounts(context.Context, api.BulkDeleteInput) (api.BulkDeleteResult, error)
}

var (
	_ bulkAccountDeleter = (*runtimeManager)(nil)
	_ bulkAccountDeleter = (*runtimeAdmin)(nil)
)

// bulkDeleteJob 为一个已登记独占等待、等待删除的账户
type bulkDeleteJob struct {
	accountID   string
	reservation *aistudio.AccountReservation
}

// DeleteAccounts 批量删除账户及其认证目录：去掉首尾空白、去重后，号池里还有的账户单次最多处理 bulkDeleteMaxPerCall 个（其余计入 remaining），
// 先为整批登记独占等待，再按 bulkDeleteConcurrency 并行删除，正在使用的账户最多共同等 bulkDeleteBusyWait；
// 全部删完后登记一次模型变化、合并并推送一次快照
func (admin *runtimeAdmin) DeleteAccounts(ctx context.Context, input api.BulkDeleteInput) (api.BulkDeleteResult, error) {
	targets := make([]string, 0, len(input.AccountIDs))
	seen := make(map[string]struct{}, len(input.AccountIDs))
	for _, value := range input.AccountIDs {
		accountID := strings.TrimSpace(value)
		if _, duplicate := seen[accountID]; accountID == "" || duplicate {
			continue
		}
		seen[accountID] = struct{}{}
		targets = append(targets, accountID)
	}
	if len(targets) == 0 {
		return api.BulkDeleteResult{}, invalidAccount(fmt.Errorf("没有指定要删除的账户"))
	}
	result := api.BulkDeleteResult{Failed: []api.BulkDeleteFailure{}}

	// 先为整批登记：正在使用的账户从这时起不再接新请求，排在后面的账户也有完整的等待时间。
	// 单次上限只计号池里还有的账户（与批量启用只计停用账户一样）：已经删掉的不占名额，原样再次提交同一列表即可继续
	waitCtx, cancel := context.WithTimeout(ctx, bulkDeleteBusyWait)
	defer cancel()
	jobs := make([]bulkDeleteJob, 0, min(len(targets), bulkDeleteMaxPerCall))
	missing := make([]string, 0)
	for _, accountID := range targets {
		if len(jobs) >= bulkDeleteMaxPerCall {
			if _, err := admin.pool.Account(accountID); errors.Is(err, aistudio.ErrAccountNotFound) {
				missing = append(missing, accountID)
			} else {
				result.Remaining++
			}
			continue
		}
		reservation, err := admin.pool.ReserveAccount(accountID)
		switch {
		case errors.Is(err, aistudio.ErrAccountNotFound):
			missing = append(missing, accountID)
		case err != nil:
			result.Failed = append(result.Failed, bulkDeleteFailure(accountID, accountOperationError(err)))
		default:
			jobs = append(jobs, bulkDeleteJob{accountID: accountID, reservation: reservation})
		}
	}
	// 兜底撤销：Remove 返回时已经撤销，再撤销是空操作；没交给删除的登记在这里撤销
	defer func() {
		for _, job := range jobs {
			job.reservation.Cancel()
		}
	}()

	var resultMu sync.Mutex
	deleted := make([]string, 0, len(jobs))
	poolChanged := false
	queue := make(chan bulkDeleteJob)
	var workers sync.WaitGroup
	for range min(bulkDeleteConcurrency, len(jobs)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range queue {
				removed, err := admin.removeAccount(func(deleteDirectory func(*aistudio.Account) error) (*aistudio.Account, error) {
					return job.reservation.Remove(waitCtx, deleteDirectory)
				})
				resultMu.Lock()
				if removed != nil {
					poolChanged = true
				}
				switch {
				case err == nil:
					deleted = append(deleted, removed.ID)
				case removed != nil:
					// 已移出号池并删除了认证目录，关闭 Worker 或固定出口失败：与单个删除一样报告这个错误
					result.Failed = append(result.Failed, bulkDeleteFailure(job.accountID, err))
				case errors.Is(err, aistudio.ErrAccountNotFound):
					// 登记后被单个删除或账户目录扫描移出了号池
					missing = append(missing, job.accountID)
				case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
					result.Failed = append(result.Failed, bulkDeleteFailure(job.accountID, errAccountBusy()))
				default:
					result.Failed = append(result.Failed, bulkDeleteFailure(job.accountID, accountOperationError(err)))
				}
				resultMu.Unlock()
			}
		}()
	}
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	workers.Wait()

	// 同一账户重新上传后会再次自动处理；整批只写一次 .onboarding.json
	admin.onboard.forgetMany(deleted)
	if poolChanged {
		// 逐个删除时没有登记模型变化，删完后登记一次，合并刷新并推送一次快照
		admin.service.markModelsChanged()
		admin.syncModelCache()
	}
	if admin.service != nil {
		// 号池里已经没有的账户不会再被清除，403 无权限列表里的条目随之清除。按原样精确匹配，与号池查找一致：
		// 不能用不区分大小写的 permissionDeniedRegistry.clear，否则只差大小写的 ID 会按不存在处理，却清掉号池里那个账户的记录
		for _, accountID := range missing {
			admin.service.permissionDenied.clearAccount(accountID)
		}
	}
	result.Deleted = len(deleted)
	result.Missing = len(missing)
	sort.Slice(result.Failed, func(left, right int) bool { return result.Failed[left].ID < result.Failed[right].ID })
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"批量删除账户 | 删除=%d | 不存在=%d | 失败=%d | 剩余=%d",
		result.Deleted, result.Missing, len(result.Failed), result.Remaining,
	))
	return result, nil
}

// bulkDeleteFailure 把一个账户的错误转换为批量删除结果中的失败项
func bulkDeleteFailure(accountID string, err error) api.BulkDeleteFailure {
	failure := api.BulkDeleteFailure{ID: accountID, Message: strings.TrimSpace(err.Error())}
	var operation *adminOperationError
	if errors.As(err, &operation) {
		failure.Code = operation.code
	}
	return failure
}
