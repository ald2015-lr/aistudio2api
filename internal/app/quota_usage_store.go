package app

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 今日发送数的写盘记录（平均调用用，见 balanced.go 与 aistudio/quota_usage.go）：账户池里的计数只在内存，重启后从 0 开始的话，
// 预热会按“近期成功”把重启前用得最多的号先拉起来。这里仿照坏号记忆（bad_accounts.go）原子写一个带额度日的小文件：
//   - 只在平均调用开启、且计数版本有变化时写，每 balancedCheckInterval（30 秒）最多一次（挂在 keepWarm 上）；停止生成服务时
//     （finishStop，见 saveQuotaUsage）与关闭运行时各再写一次（版本没变时不写）。默认部署（没有开启平均调用）不新增任何磁盘写入
//   - 启动时读一次：文件的额度日就是今天（太平洋时间）才并回账户池（SeedQuotaDayUsage，按 max 合并），不是今天、损坏或版本不符时忽略
//     （损坏时写一条警告）。不论开关是否开启都并回：计数本来就总是开着
//   - 文件放在模型目录缓存的目录里（runtime/model-catalogs/quota-usage.json），键是账户 ID，属于运行期数据，不进仓库；
//     多个进程共用运行目录时以最后写入的为准（计数只统计本进程）

const (
	// quotaUsageFileName 为记录文件名（放在模型目录缓存的目录里）
	quotaUsageFileName = "quota-usage.json"
	// quotaUsageFileVersion 为记录文件的格式版本，不符时忽略
	quotaUsageFileVersion = 1
)

// quotaUsageFile 为 quota-usage.json：day_key 为额度日编号（太平洋时间次日零点的 Unix 分钟，见 aistudio.QuotaDayKey），day 为太平洋日期
type quotaUsageFile struct {
	Version  int               `json:"version"`
	DayKey   int32             `json:"day_key"`
	Day      string            `json:"day"`
	Accounts map[string]uint32 `json:"accounts"`
}

// quotaUsageStore 为今日发送数的写盘记录；方法全部 nil 安全（测试直接构造的管理器没有记录，不写盘）
type quotaUsageStore struct {
	mu       sync.Mutex
	path     string
	requests *requestRegistry
	// saved 为已写入（或启动时并回后）的计数版本，版本没变时不写
	saved uint64
	// now 只给包内测试替换
	now func() time.Time
}

// openQuotaUsageStore 打开写盘记录并把今天的计数并回 pool：path 为空时不读不写；文件不存在、不是今天、损坏或版本不符时从账户池现有计数开始
func openQuotaUsageStore(path string, pool *aistudio.AccountPool, requests *requestRegistry) *quotaUsageStore {
	return openQuotaUsageStoreAt(path, pool, requests, nil)
}

// openQuotaUsageStoreAt 与 openQuotaUsageStore 相同，now 不为 nil 时用它判断今天（只给包内测试）
func openQuotaUsageStoreAt(path string, pool *aistudio.AccountPool, requests *requestRegistry, now func() time.Time) *quotaUsageStore {
	store := &quotaUsageStore{path: path, requests: requests, now: now}
	if path == "" {
		return store
	}
	defer func() {
		// 并回之后的计数与文件一致，没有新的发送之前不必再写
		store.saved = pool.QuotaDayUsage().Version
	}()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return store
	}
	var file quotaUsageFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err != nil {
		store.log("WARN", "今日发送数记录读取失败，从 0 开始 | 文件="+path+" | 错误="+err.Error())
		return store
	}
	if file.Version != quotaUsageFileVersion || file.DayKey != aistudio.QuotaDayKey(store.clock()) {
		return store
	}
	pool.SeedQuotaDayUsage(file.DayKey, file.Accounts)
	return store
}

func (store *quotaUsageStore) clock() time.Time {
	if store.now != nil {
		return store.now()
	}
	return time.Now()
}

func (store *quotaUsageStore) log(level string, message string) {
	if store != nil && store.requests != nil {
		store.requests.log("service", level, message)
	}
}

// save 计数版本有变化时原子写入 snapshot；版本没变、没有路径或 store 为 nil 时不写。是否开启平均调用由调用方判断
func (store *quotaUsageStore) save(snapshot aistudio.QuotaUsageSnapshot) error {
	if store == nil || store.path == "" || snapshot.DayKey == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if snapshot.Version == store.saved {
		return nil
	}
	data, err := json.Marshal(quotaUsageFile{
		Version: quotaUsageFileVersion, DayKey: snapshot.DayKey, Day: snapshot.Day, Accounts: snapshot.Counts,
	})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(store.path, data, 0o600); err != nil {
		return err
	}
	store.saved = snapshot.Version
	return nil
}

// close 停止生成服务与关闭运行时调用：平均调用开启时把最后的计数写出（版本没变时不写）
func (store *quotaUsageStore) close(pool *aistudio.AccountPool, balanced bool) error {
	if store == nil || !balanced {
		return nil
	}
	return store.save(pool.QuotaDayUsage())
}

// saveQuotaUsage 在停止生成服务时把今日发送数写出（平均调用开启、版本有变化时；测试构造的管理器没有记录，不写）。
// 重建运行时（管理页停止再启动、应用配置后的重启）先停旧服务、再建新一代（新一代此时读文件并回）、最后才关闭旧一代：只在关闭运行时写的话，
// 新一代并回的是旧一代最后一次定时保存（最多 30 秒前），旧一代之后的发送随后又被新一代的写盘整份覆盖
func (manager *accountWorkerManager) saveQuotaUsage() error {
	return manager.balancedState.store.Load().close(manager.pool, manager.balancedOn())
}

// quotaUsagePath 返回今日发送数记录文件的位置（模型目录缓存的目录里）
func quotaUsagePath() string {
	return filepath.Join(catalogCacheDir, quotaUsageFileName)
}
