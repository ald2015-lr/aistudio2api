package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// defaultEnableConcurrency 为启动与导入时并行写回 account.json 的账户数
const defaultEnableConcurrency = 16

// enableUnmarkedAccounts 实现“账户默认启用”：account.json 为停用、但没有手动停用记录（.disable-reasons.json 中
// Auto 为假的记录，只由管理页或管理接口停用时写入）的账户改为启用并写回 account.json，清除它们的自动停用记录。
// 外部程序上传的 account.json 常写成停用，新账户自动处理验证未通过、停用记录丢失的账户原先重启后也一直停用，
// 上万个账户只能在管理页逐个启用。
//
// 只能在账户加入账户池之前调用（启动时建立账户池之前、目录扫描导入之前）：直接修改账户字段，不构建账户状态、
// 不推送快照；写回按 defaultEnableConcurrency 并行，上万个账户也只需几秒，全部完成后只写一条汇总日志。
// 写回失败的账户内存中同样启用，下次载入时按同样的规则再次启用。返回启用的账户 ID。
//
// 停用记录文件无法读取时（见 disableNotes.load）分不出哪些是手动停用的：没有记录的停用账户一律保持停用，
// 并补记为手动停用（管理页显示原因，需要启用的在管理页启用）
func enableUnmarkedAccounts(accounts []*aistudio.Account, requests *requestRegistry, stage string) []string {
	targets := make([]*aistudio.Account, 0)
	presumed := make([]string, 0)
	for _, account := range accounts {
		if account == nil || account.Config.Enabled {
			continue
		}
		if !accountDisableNotes.keepDisabled(account.ID) {
			targets = append(targets, account)
		} else if !accountDisableNotes.manual(account.ID) {
			presumed = append(presumed, account.ID)
		}
	}
	if len(presumed) > 0 {
		accountDisableNotes.presumeManual(presumed)
		if requests != nil {
			requests.log("auth", "WARN", fmt.Sprintf(
				"%s保留 %d 个没有停用记录的停用账户：停用记录文件无法读取，无法确认是否为手动停用，已按手动停用记录", stage, len(presumed),
			))
		}
	}
	if len(targets) == 0 {
		return nil
	}
	startedAt := time.Now()
	var (
		resultMu sync.Mutex
		busy     int
		failed   int
		firstErr error
	)
	jobs := make(chan *aistudio.Account)
	var workers sync.WaitGroup
	for range min(defaultEnableConcurrency, len(targets)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for account := range jobs {
				err := aistudio.EnableLoadedAccount(account)
				if err == nil {
					continue
				}
				resultMu.Lock()
				if errors.Is(err, aistudio.ErrAccountLeased) {
					busy++
				} else {
					failed++
					if firstErr == nil {
						firstErr = fmt.Errorf("账户 %s: %w", account.ID, err)
					}
				}
				resultMu.Unlock()
			}
		}()
	}
	for _, account := range targets {
		jobs <- account
	}
	close(jobs)
	workers.Wait()
	enabled := make([]string, 0, len(targets))
	for _, account := range targets {
		enabled = append(enabled, account.ID)
	}
	accountDisableNotes.clearMany(enabled)
	level := "INFO"
	message := fmt.Sprintf("%s启用 %d 个停用账户（没有手动停用记录）| 耗时=%s",
		stage, len(enabled), time.Since(startedAt).Round(time.Millisecond))
	if busy > 0 {
		message += fmt.Sprintf(" | 账户正被其他进程占用、未写回 account.json=%d", busy)
	}
	if failed > 0 {
		level = "WARN"
		message += fmt.Sprintf(" | 写回 account.json 失败=%d | 错误=%s", failed, strings.TrimSpace(firstErr.Error()))
	}
	if requests != nil {
		requests.log("auth", level, message)
	}
	return enabled
}
