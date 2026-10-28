package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/gofrs/flock"
)

// 模型目录写盘缓存：runtime/model-catalogs/ 下一个 index.json（账户 → 目录 blob 键与同步状态）与按内容寻址的 blobs/<键>.json。
// 以前目录只在内存，每次启动都对全部就绪账户同步一遍（1.24 万账户约 3.5 万个请求、6–13 分钟，全部经同一个出口，
// 诊断 diag_busy_2141）；缓存后启动时直接恢复，后台只低速刷新缺失与过期的账户（见 catalog_sync.go）。
// 写盘最多每 catalogSaveInterval 一次：flock 跨进程加锁，读回磁盘 index 按账户 at 新者胜合并，先写 blob 再写 index，
// 都用同目录临时文件加改名（writeFileAtomic）。上万个账户通常只有几种目录，blob 按内容键去重

const (
	// catalogCacheVersion 为 index 与 blob 的格式版本，不符时忽略（不删，便于回退旧版本）
	catalogCacheVersion = 1
	// catalogSaveInterval 为有变化时两次写盘的最短间隔
	catalogSaveInterval = 30 * time.Second
	// catalogBlobGCAge 为 index 不再引用的 blob 保留的时间：另一个进程可能刚写了 blob、还没写 index，不能立即删
	catalogBlobGCAge = 24 * time.Hour
	// catalogEntryGCAge 为号池里已经没有的账户条目保留的时间（惰性清理，不改删除账户的路径）
	catalogEntryGCAge = 7 * 24 * time.Hour
	// catalogGCInterval 为两次清理的最短间隔（在写盘时、持锁进行）
	catalogGCInterval = time.Hour
	// catalogBlobTouchAge 为复用磁盘上已有的 blob 时刷新修改时间的门槛：清理只删 catalogBlobGCAge 未改过的，
	// 复用时刷新一下，index 写出前不会被本进程或另一进程的清理删掉
	catalogBlobTouchAge = catalogBlobGCAge / 2
	// catalogLockTimeout 为等待跨进程写锁的上限
	catalogLockTimeout = 10 * time.Second
	// catalogBlobPlayground 与 catalogBlobBuild 为 blob 的种类
	catalogBlobPlayground = "playground"
	catalogBlobBuild      = "build"
)

// catalogCacheDir 为模型目录缓存的目录（相对工作目录，与 runtime/requests.db 同级）；只给包内测试改到临时目录
var catalogCacheDir = filepath.Join("runtime", "model-catalogs")

// catalogBlobName 匹配 blob 文件名（内容键为 32 位 hex），清理时只看这类文件，不碰临时文件
var catalogBlobName = regexp.MustCompile(`^[0-9a-f]{32}\.json$`)

// catalogEntry 为一个账户在 index 里的记录（字段名取短名，1.24 万账户的 index 约 1–1.4 MB）
type catalogEntry struct {
	// Tier 为同步时的权益（BenefitTier 数值）
	Tier int64 `json:"t"`
	// Playground 与 Build 为两个目录的 blob 键；Playground 为空表示没有可恢复的目录
	Playground string `json:"p,omitempty"`
	Build      string `json:"b,omitempty"`
	// BuildSynced 为是否同步过 Build 目录（Build 未启用时为假）
	BuildSynced bool `json:"bs,omitempty"`
	// At 为最近一次成功同步（unix 秒）
	At int64 `json:"at,omitempty"`
	// Failures 与 NextRetry 为连续失败次数与下次重试（unix 秒）：重启后不立刻重打失败账户
	Failures  int   `json:"f,omitempty"`
	NextRetry int64 `json:"nr,omitempty"`
	// AuthRequiredAt 与 AuthModTime 为最近一次被标需要登录的时间（unix 秒）与当时认证文件的修改时间（unix 纳秒），
	// 成功同步或验证通过时清零；用于疑似失效账号的预热排序与探测顺序
	AuthRequiredAt int64 `json:"ar,omitempty"`
	AuthModTime    int64 `json:"am,omitempty"`
	// Empties 与 EmptySince 为 ListModels 连续返回空列表的次数与第一次为空的时间（unix 秒）
	Empties    int   `json:"en,omitempty"`
	EmptySince int64 `json:"ez,omitempty"`
}

// catalogIndex 为 index.json
type catalogIndex struct {
	Version  int                     `json:"version"`
	SavedAt  time.Time               `json:"saved_at"`
	Accounts map[string]catalogEntry `json:"accounts"`
}

// catalogBlob 为 blobs/<键>.json：Playground 存 ListModels 原文（恢复时用同一个解析器重新解析，一次得到模型与默认值，
// 解析器升级后自动按新逻辑解析），Build 存模型列表（默认值由 buildModelDefaults 推导，不另存）
type catalogBlob struct {
	Version int              `json:"version"`
	Kind    string           `json:"kind"`
	Raw     json.RawMessage  `json:"raw,omitempty"`
	Models  []aistudio.Model `json:"models,omitempty"`
}

// catalogPrimer 为恢复时预置 Client 侧目录与权益的接口（*aistudio.Client 实现）
type catalogPrimer interface {
	PrimeAccountCatalog(accountID string, tier aistudio.BenefitTier, catalog aistudio.ModelCatalogData)
}

// catalogCache 为模型目录的写盘缓存；方法全部 nil 安全（cache 为 nil 时只在内存，测试用）
type catalogCache struct {
	dir      string
	requests *requestRegistry
	mu       sync.Mutex
	entries  map[string]catalogEntry
	// pending 为还没有写到磁盘的 blob（键 → 文件内容），onDisk 为本进程确认过磁盘上有效的 blob 键
	pending map[string][]byte
	onDisk  map[string]struct{}
	dirty   bool
	timer   *time.Timer
	// known 判断账户是否还在号池（清理条目用），restore 时设置；为空时不清理条目
	known  func(accountID string) bool
	lastGC time.Time
	// saveMu 让写盘串行：计时器、调度器退出与关闭运行时都会触发
	saveMu sync.Mutex
	// saveInterval 为写盘间隔，0 为 catalogSaveInterval（只给包内测试改小）
	saveInterval time.Duration
	// foreignWarned 为已提示过磁盘上是另一个版本的 index（只提示一次）
	foreignWarned bool
}

// catalogRestoreSummary 为启动恢复的结果，交给调度器排任务
type catalogRestoreSummary struct {
	// FromDisk 为从磁盘恢复的账户数，Blobs 为载入的目录 blob 数
	FromDisk int
	Blobs    int
	// Stale 为恢复了、但指纹与运行态不一致（或 Build 目录没有恢复）的账户：排在缺失之后尽快刷新
	Stale map[string]struct{}
	// Suspects 为疑似登录失效的账户：缓存记过需要登录，认证文件之后没变过
	Suspects map[string]struct{}
	// TierMismatch、Missing、Version、Removed 为跳过的账户数：权益不符、blob 缺失或损坏、版本不符、已删除账户
	TierMismatch, Missing, Version, Removed int
	Duration                                time.Duration
}

// openCatalogCache 打开模型目录缓存：在跨进程锁内读 index；index 损坏时改名为 index.json.corrupt-<unix> 保留排查，
// 按空缓存继续（读与改名都在锁内，不会把另一进程刚写好的有效 index 改名丢掉）；版本不符时忽略。从不返回 nil
func openCatalogCache(dir string, requests *requestRegistry) *catalogCache {
	cache := &catalogCache{
		dir: dir, requests: requests, entries: make(map[string]catalogEntry),
		pending: make(map[string][]byte), onDisk: make(map[string]struct{}),
	}
	// 还没有缓存目录时（首次升级）不建目录，第一次写盘时再建
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return cache
	}
	err := cache.withLock(func() error {
		index, err := cache.readIndexLocked()
		if err != nil {
			return err
		}
		if index.Version == catalogCacheVersion && index.Accounts != nil {
			cache.entries = index.Accounts
		}
		return nil
	})
	if err != nil {
		cache.log("WARN", "模型目录缓存读取失败 | 目录="+dir+" | 错误="+err.Error())
	}
	return cache
}

func (cache *catalogCache) indexPath() string {
	return filepath.Join(cache.dir, "index.json")
}

func (cache *catalogCache) blobPath(key string) string {
	return filepath.Join(cache.dir, "blobs", key+".json")
}

func (cache *catalogCache) log(level string, message string) {
	if cache != nil && cache.requests != nil {
		cache.requests.log("service", level, message)
	}
}

// withLock 在跨进程写锁 .lock 内执行 action
func (cache *catalogCache) withLock(action func() error) error {
	if err := os.MkdirAll(filepath.Join(cache.dir, "blobs"), 0o700); err != nil {
		return fmt.Errorf("创建模型目录缓存目录: %w", err)
	}
	lock := flock.New(filepath.Join(cache.dir, ".lock"))
	ctx, cancel := context.WithTimeout(context.Background(), catalogLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return fmt.Errorf("锁定模型目录缓存: %w", err)
	}
	if !locked {
		return fmt.Errorf("锁定模型目录缓存超时")
	}
	actionErr := action()
	return errors.Join(actionErr, lock.Unlock())
}

// readIndexLocked 读磁盘 index；不存在时为空。解析失败时改名保留并写“模型目录缓存损坏”（持久），按空 index 返回。
// 版本不符原样返回，由调用方忽略。调用方持有跨进程锁
func (cache *catalogCache) readIndexLocked() (catalogIndex, error) {
	path := cache.indexPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return catalogIndex{Version: catalogCacheVersion}, nil
	}
	if err != nil {
		return catalogIndex{}, fmt.Errorf("读取 %s: %w", path, err)
	}
	var index catalogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		renamed := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, renamed); renameErr != nil {
			return catalogIndex{}, errors.Join(fmt.Errorf("解析 %s: %w", path, err), renameErr)
		}
		if cache.requests != nil {
			cache.requests.logDurable("service", "WARN", fmt.Sprintf(
				"模型目录缓存损坏 | 文件=%s | 已改名=%s | 错误=%s", path, renamed, err.Error(),
			))
		}
		return catalogIndex{Version: catalogCacheVersion}, nil
	}
	return index, nil
}

// readBlob 读并校验一个 blob：返回 Playground 目录或 Build 模型。versionMismatch 为版本不符（不删）；
// 其余错误为缺失或损坏
func (cache *catalogCache) readBlob(key string, kind string) (playground aistudio.ModelCatalogData, build []aistudio.Model, versionMismatch bool, err error) {
	data, err := os.ReadFile(cache.blobPath(key))
	if err != nil {
		return aistudio.ModelCatalogData{}, nil, false, err
	}
	return decodeCatalogBlob(data, kind)
}

func decodeCatalogBlob(data []byte, kind string) (playground aistudio.ModelCatalogData, build []aistudio.Model, versionMismatch bool, err error) {
	var blob catalogBlob
	if err := json.Unmarshal(data, &blob); err != nil {
		return aistudio.ModelCatalogData{}, nil, false, fmt.Errorf("解析目录 blob: %w", err)
	}
	if blob.Version != catalogCacheVersion {
		return aistudio.ModelCatalogData{}, nil, true, fmt.Errorf("目录 blob 版本 %d 不符", blob.Version)
	}
	if blob.Kind != kind {
		return aistudio.ModelCatalogData{}, nil, false, fmt.Errorf("目录 blob 种类 %q 不符，期望 %q", blob.Kind, kind)
	}
	if kind == catalogBlobBuild {
		return aistudio.ModelCatalogData{}, blob.Models, false, nil
	}
	playground, err = aistudio.ParseModelCatalogRaw(blob.Raw)
	if err != nil {
		return aistudio.ModelCatalogData{}, nil, false, fmt.Errorf("解析目录 blob 原文: %w", err)
	}
	return playground, nil, false, nil
}

// blobKeyOf 返回 blob 内容对应的内容键（校验磁盘上同名 blob 用）；无法解析时为空
func blobKeyOf(data []byte, kind string) string {
	playground, build, _, err := decodeCatalogBlob(data, kind)
	if err != nil {
		return ""
	}
	if kind == catalogBlobBuild {
		return aistudio.BuildCatalogKey(build)
	}
	return playground.ContentKey()
}

// removeBadBlob 在锁内删除损坏的 blob（下次同步时重写）
func (cache *catalogCache) removeBadBlob(key string, kind string) {
	_ = cache.withLock(func() error {
		data, err := os.ReadFile(cache.blobPath(key))
		if err != nil {
			return nil
		}
		// 锁内再读一次：另一进程可能刚把它重写好
		if _, _, versionMismatch, decodeErr := decodeCatalogBlob(data, kind); decodeErr == nil || versionMismatch {
			return nil
		}
		return os.Remove(cache.blobPath(key))
	})
}

// restore 把缓存恢复进账户池与 Client：一次加锁恢复账户池（aistudio.RestoreCatalogs），每份 blob 只解析一次、
// 多个账户共用。记过需要登录的条目（ar > 0）stat 一次认证文件：修改时间等于 am 的列为疑似失效，变过的（重新登录过）清掉 ar
func (cache *catalogCache) restore(pool *aistudio.AccountPool, client catalogPrimer, buildEnabled bool) catalogRestoreSummary {
	summary := catalogRestoreSummary{Stale: make(map[string]struct{}), Suspects: make(map[string]struct{})}
	if cache == nil || pool == nil {
		return summary
	}
	startedAt := time.Now()
	cache.mu.Lock()
	cache.known = func(accountID string) bool {
		_, err := pool.Account(accountID)
		return err == nil
	}
	entries := maps.Clone(cache.entries)
	cache.mu.Unlock()
	if len(entries) == 0 {
		return summary
	}
	type loadedPlayground struct {
		data   aistudio.ModelCatalogData
		models []aistudio.Model
		err    error
		older  bool
	}
	type loadedBuild struct {
		models []aistudio.Model
		err    error
		older  bool
	}
	playgrounds := make(map[string]*loadedPlayground)
	builds := make(map[string]*loadedBuild)
	items := make([]aistudio.CatalogRestore, 0, len(entries))
	datas := make(map[string]aistudio.ModelCatalogData, len(entries))
	buildMissing := make(map[string]struct{})
	for accountID, entry := range entries {
		if entry.Playground == "" {
			continue
		}
		loaded := playgrounds[entry.Playground]
		if loaded == nil {
			data, _, older, err := cache.readBlob(entry.Playground, catalogBlobPlayground)
			loaded = &loadedPlayground{data: data, err: err, older: older}
			if err == nil {
				loaded.models = data.Models()
			} else if !older {
				cache.removeBadBlob(entry.Playground, catalogBlobPlayground)
			}
			playgrounds[entry.Playground] = loaded
		}
		if loaded.err != nil {
			if loaded.older {
				summary.Version++
			} else {
				summary.Missing++
			}
			continue
		}
		item := aistudio.CatalogRestore{
			AccountID: accountID, Tier: aistudio.BenefitTier(entry.Tier), Playground: loaded.models,
			EmptyCount: entry.Empties, EmptySince: unixTime(entry.EmptySince),
		}
		// Build 目录不论 Build 通道是否启用都恢复：没有启用时账户池不使用它，运行中经 UPSTREAM_CHANNELS 热更新启用 Build 后
		// 立即可用（见 applyUpstreamChannels），同步时也照旧写回缓存，不丢掉。只有启用了 Build 时没有恢复的账户才排进后台同步
		if entry.BuildSynced && entry.Build != "" {
			build := builds[entry.Build]
			if build == nil {
				_, models, older, err := cache.readBlob(entry.Build, catalogBlobBuild)
				build = &loadedBuild{models: models, err: err, older: older}
				if err != nil && !older {
					cache.removeBadBlob(entry.Build, catalogBlobBuild)
				}
				builds[entry.Build] = build
			}
			if build.err == nil {
				item.Build, item.BuildSynced = build.models, true
			}
		} else if entry.BuildSynced {
			// Build 目录同步过且为空
			item.Build, item.BuildSynced = []aistudio.Model{}, true
		}
		if buildEnabled && !item.BuildSynced {
			buildMissing[accountID] = struct{}{}
		}
		items = append(items, item)
		datas[accountID] = loaded.data
	}
	result := pool.RestoreCatalogs(items)
	for _, accountID := range result.Restored {
		entry := entries[accountID]
		if client != nil {
			client.PrimeAccountCatalog(accountID, aistudio.BenefitTier(entry.Tier), datas[accountID])
		}
		if _, missing := buildMissing[accountID]; missing {
			summary.Stale[accountID] = struct{}{}
		}
	}
	for _, accountID := range result.Stale {
		summary.Stale[accountID] = struct{}{}
	}
	summary.FromDisk = len(result.Restored)
	summary.TierMismatch = len(result.TierMismatch)
	summary.Removed = len(result.Missing)
	cache.mu.Lock()
	for key, loaded := range playgrounds {
		if loaded.err == nil {
			summary.Blobs++
			cache.onDisk[key] = struct{}{}
		}
	}
	for key, loaded := range builds {
		if loaded.err == nil {
			cache.onDisk[key] = struct{}{}
		}
	}
	cache.mu.Unlock()
	// 疑似失效：缓存记过需要登录，认证文件的修改时间与当时相同（用户没有重新登录过）
	cleared := 0
	for accountID, entry := range entries {
		if entry.AuthRequiredAt == 0 {
			continue
		}
		account, err := pool.Account(accountID)
		if err != nil {
			continue
		}
		if modTime, ok := fileModTime(account.StoragePath); ok && modTime.UnixNano() == entry.AuthModTime {
			summary.Suspects[accountID] = struct{}{}
			continue
		}
		cache.clearAuthRequired(accountID)
		cleared++
	}
	summary.Duration = time.Since(startedAt)
	cache.log("INFO", fmt.Sprintf(
		"模型目录缓存载入 | 账户=%d | 目录=%d | 耗时=%s | 跳过=权益不符 %d、文件缺失 %d、版本不符 %d、已删除账户 %d",
		summary.FromDisk, summary.Blobs, summary.Duration.Round(time.Millisecond),
		summary.TierMismatch, summary.Missing, summary.Version, summary.Removed,
	))
	return summary
}

// fileModTime 返回文件的修改时间；文件不存在或路径为空时 ok 为假
func fileModTime(path string) (time.Time, bool) {
	if strings.TrimSpace(path) == "" {
		return time.Time{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func unixTime(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func unixSeconds(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()
}

// entryOf 返回账户的缓存记录
func (cache *catalogCache) entryOf(accountID string) (catalogEntry, bool) {
	if cache == nil {
		return catalogEntry{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, exists := cache.entries[accountID]
	return entry, exists
}

// put 记录一次成功同步：写入目录 blob（同键已有时不重复）与账户记录，清掉失败次数与需要登录记录。
// Playground 为空（接受了空目录）时不写 blob，重启后按缺失同步
func (cache *catalogCache) put(accountID string, snapshot aistudio.CatalogSnapshot, at time.Time) {
	if cache == nil {
		return
	}
	playgroundKey, playgroundBlob := "", []byte(nil)
	if snapshot.Playground.Len() > 0 && len(snapshot.Playground.Raw()) > 0 {
		playgroundKey = snapshot.Playground.ContentKey()
		if playgroundKey != "" && cache.blobNeeded(playgroundKey) {
			playgroundBlob, _ = json.Marshal(catalogBlob{Version: catalogCacheVersion, Kind: catalogBlobPlayground, Raw: snapshot.Playground.Raw()})
		}
	}
	buildKey, buildBlob := "", []byte(nil)
	if snapshot.BuildSynced && len(snapshot.Build) > 0 {
		buildKey = aistudio.BuildCatalogKey(snapshot.Build)
		if buildKey != "" && cache.blobNeeded(buildKey) {
			buildBlob, _ = json.Marshal(catalogBlob{Version: catalogCacheVersion, Kind: catalogBlobBuild, Models: snapshot.Build})
		}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if playgroundBlob != nil {
		cache.pending[playgroundKey] = playgroundBlob
	}
	if buildBlob != nil {
		cache.pending[buildKey] = buildBlob
	}
	cache.entries[accountID] = catalogEntry{
		Tier: int64(snapshot.Tier), Playground: playgroundKey, Build: buildKey, BuildSynced: snapshot.BuildSynced,
		At: at.Unix(), Empties: snapshot.EmptyCount, EmptySince: unixSeconds(snapshot.EmptySince),
	}
	cache.markDirtyLocked()
}

// blobNeeded 判断 put 要不要为这个键准备 blob：还没写盘（pending）时不用；本进程确认过在盘上时看文件是否还在——
// 清理（本进程或另一进程，只删 index 不再引用、24 小时未改的）可能已删掉它，原先仍当它在盘上、不再写，index 却引用它，
// 下次启动这些账户都按“文件缺失”重新同步。文件还在时修改时间超过 catalogBlobTouchAge 就刷新，免得 index 写出前被清理删掉
func (cache *catalogCache) blobNeeded(key string) bool {
	cache.mu.Lock()
	_, onDisk := cache.onDisk[key]
	_, pending := cache.pending[key]
	cache.mu.Unlock()
	if pending {
		return false
	}
	if !onDisk {
		return true
	}
	path := cache.blobPath(key)
	info, err := os.Stat(path)
	if err != nil {
		cache.mu.Lock()
		delete(cache.onDisk, key)
		cache.mu.Unlock()
		return true
	}
	if now := time.Now(); now.Sub(info.ModTime()) > catalogBlobTouchAge {
		_ = os.Chtimes(path, now, now)
	}
	return false
}

// noteFailure 记录账户的连续失败次数与下次重试时间（重启后不立刻重打失败账户）
func (cache *catalogCache) noteFailure(accountID string, failures int, next time.Time) {
	cache.update(accountID, func(entry *catalogEntry) {
		entry.Failures, entry.NextRetry = failures, unixSeconds(next)
	})
}

// noteEmpty 记录 ListModels 连续返回空列表的次数与第一次为空的时间
func (cache *catalogCache) noteEmpty(accountID string, count int, since time.Time) {
	cache.update(accountID, func(entry *catalogEntry) {
		entry.Empties, entry.EmptySince = count, unixSeconds(since)
	})
}

// noteAuthRequired 记录账户被标需要登录的时间与当时认证文件的修改时间
func (cache *catalogCache) noteAuthRequired(accountID string, at time.Time, authFileModTime time.Time) {
	cache.update(accountID, func(entry *catalogEntry) {
		entry.AuthRequiredAt = unixSeconds(at)
		entry.AuthModTime = 0
		if !authFileModTime.IsZero() {
			entry.AuthModTime = authFileModTime.UnixNano()
		}
	})
}

// clearAuthRequired 清掉账户的需要登录记录（认证文件变过，或验证通过）
func (cache *catalogCache) clearAuthRequired(accountID string) {
	if entry, exists := cache.entryOf(accountID); !exists || entry.AuthRequiredAt == 0 {
		return
	}
	cache.update(accountID, func(entry *catalogEntry) {
		entry.AuthRequiredAt, entry.AuthModTime = 0, 0
	})
}

func (cache *catalogCache) update(accountID string, change func(*catalogEntry)) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[accountID]
	before := entry
	change(&entry)
	if entry == before {
		return
	}
	cache.entries[accountID] = entry
	cache.markDirtyLocked()
}

// markDirtyLocked 标记有变化，并在 saveInterval 后写盘；调用方持有 cache.mu
func (cache *catalogCache) markDirtyLocked() {
	cache.dirty = true
	if cache.timer != nil {
		return
	}
	interval := cache.saveInterval
	if interval <= 0 {
		interval = catalogSaveInterval
	}
	cache.timer = time.AfterFunc(interval, func() {
		if err := cache.flush(); err != nil {
			cache.log("WARN", "模型目录缓存写入失败 | 错误="+err.Error())
		}
	})
}

// flush 立即写盘（没有变化时什么也不做）：锁外编码，flock 内读回磁盘 index、按账户 at 新者胜合并，先写 blob 再写 index。
// 失败时保留变化，下个间隔重试
func (cache *catalogCache) flush() error {
	if cache == nil {
		return nil
	}
	cache.saveMu.Lock()
	defer cache.saveMu.Unlock()
	cache.mu.Lock()
	if cache.timer != nil {
		cache.timer.Stop()
		cache.timer = nil
	}
	if !cache.dirty {
		cache.mu.Unlock()
		return nil
	}
	entries := maps.Clone(cache.entries)
	pending := maps.Clone(cache.pending)
	known := cache.known
	gc := time.Since(cache.lastGC) >= catalogGCInterval
	cache.dirty = false
	cache.mu.Unlock()

	var adopted map[string]catalogEntry
	var collected, dropped []string
	err := cache.withLock(func() error {
		var saveErr error
		adopted, collected, dropped, saveErr = cache.saveLocked(entries, pending, known, gc)
		return saveErr
	})
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if errors.Is(err, errCatalogIndexForeign) {
		// 磁盘上是另一个版本的 index：不覆盖（便于回退），本进程的变化只留在内存，下次有变化时再看；只提示一次
		if !cache.foreignWarned {
			cache.foreignWarned = true
			cache.log("WARN", "模型目录缓存版本不符，不写盘 | 文件="+cache.indexPath())
		}
		return nil
	}
	if err != nil {
		cache.markDirtyLocked()
		return err
	}
	// 清理删掉的 blob 不再当作在盘上：之后有账户的目录又回到这个键时重新写
	for _, key := range collected {
		delete(cache.onDisk, key)
	}
	// 清理删掉的条目（已删除账户超过 7 天）同样从内存里删掉（这期间没有再改过它时）：原先只从这次写盘的合并结果里删，
	// 下一次写盘又从内存拷贝写回 index，载入时一直计“已删除账户”，它引用的 blob 被清理后还会计成“文件缺失”
	for _, accountID := range dropped {
		if current, exists := cache.entries[accountID]; exists && current == entries[accountID] {
			delete(cache.entries, accountID)
		}
	}
	if gc {
		cache.lastGC = time.Now()
	}
	for key, data := range pending {
		if current, exists := cache.pending[key]; exists && string(current) == string(data) {
			delete(cache.pending, key)
		}
		cache.onDisk[key] = struct{}{}
	}
	// 另一个进程同步得更晚的账户：内存里改用磁盘上的记录（本进程这期间没有再改过它时）
	for accountID, entry := range adopted {
		if current, exists := cache.entries[accountID]; !exists || current == entries[accountID] {
			cache.entries[accountID] = entry
		}
	}
	return nil
}

// errCatalogIndexForeign 表示磁盘上的 index 是另一个版本：忽略、不删也不覆盖（便于回退旧版本）
var errCatalogIndexForeign = errors.New("模型目录缓存 index 版本不符")

// saveLocked 为写盘的锁内部分，返回合并时磁盘一侧胜出的账户记录、清理删掉的 blob 键与清理删掉的账户条目；磁盘上是另一个版本的
// index 时什么也不写，返回 errCatalogIndexForeign。调用方持有跨进程锁
func (cache *catalogCache) saveLocked(
	entries map[string]catalogEntry,
	pending map[string][]byte,
	known func(string) bool,
	gc bool,
) (map[string]catalogEntry, []string, []string, error) {
	disk, err := cache.readIndexLocked()
	if err != nil {
		return nil, nil, nil, err
	}
	if disk.Version != catalogCacheVersion {
		return nil, nil, nil, errCatalogIndexForeign
	}
	merged := make(map[string]catalogEntry, len(entries))
	maps.Copy(merged, disk.Accounts)
	adopted := make(map[string]catalogEntry)
	for accountID, entry := range entries {
		if current, exists := merged[accountID]; exists && current.At > entry.At {
			adopted[accountID] = current
			continue
		}
		merged[accountID] = entry
	}
	now := time.Now()
	var dropped []string
	if gc && known != nil {
		for accountID, entry := range merged {
			if now.Sub(unixTime(entry.At)) > catalogEntryGCAge && !known(accountID) {
				delete(merged, accountID)
				delete(adopted, accountID)
				dropped = append(dropped, accountID)
			}
		}
	}
	// 先写 blob 再写 index：index 引用的 blob 一定已在盘上。磁盘上已有同键 blob 时读回校验，能解析且内容键一致才跳过
	for key, data := range pending {
		path := cache.blobPath(key)
		kind := catalogBlobPlayground
		var header struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(data, &header) == nil && header.Kind != "" {
			kind = header.Kind
		}
		if existing, readErr := os.ReadFile(path); readErr == nil && blobKeyOf(existing, kind) == key {
			continue
		}
		if err := writeFileAtomic(path, data, 0o600); err != nil {
			return nil, nil, nil, fmt.Errorf("写入模型目录 blob: %w", err)
		}
	}
	data, err := json.Marshal(catalogIndex{Version: catalogCacheVersion, SavedAt: now.UTC(), Accounts: merged})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("编码模型目录缓存 index: %w", err)
	}
	if err := writeFileAtomic(cache.indexPath(), data, 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("写入模型目录缓存 index: %w", err)
	}
	var collected []string
	if gc {
		collected = cache.collectBlobsLocked(merged, now)
	}
	return adopted, collected, dropped, nil
}

// collectBlobsLocked 删除 index 不再引用、且修改时间超过 catalogBlobGCAge 的 blob，返回删掉的键；调用方持有跨进程锁
func (cache *catalogCache) collectBlobsLocked(entries map[string]catalogEntry, now time.Time) []string {
	referenced := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Playground != "" {
			referenced[entry.Playground] = struct{}{}
		}
		if entry.Build != "" {
			referenced[entry.Build] = struct{}{}
		}
	}
	directory := filepath.Join(cache.dir, "blobs")
	files, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var removed []string
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !catalogBlobName.MatchString(name) {
			continue
		}
		if _, used := referenced[strings.TrimSuffix(name, ".json")]; used {
			continue
		}
		info, err := file.Info()
		if err != nil || now.Sub(info.ModTime()) <= catalogBlobGCAge {
			continue
		}
		if os.Remove(filepath.Join(directory, name)) == nil {
			removed = append(removed, strings.TrimSuffix(name, ".json"))
		}
	}
	return removed
}

// close 停止写盘计时器并立即写出（关闭运行时调用；没有变化时是空操作）
func (cache *catalogCache) close() error {
	return cache.flush()
}
