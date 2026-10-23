package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 模型目录写盘缓存的恢复（app 层的 catalog_cache.go 负责读写磁盘）
//
// 以前每次启动都对全部就绪账户同步目录（首页 GET + GetAiStudioBenefitTier + ListModels，32 并发），1.24 万账户约 3.5 万个
// 请求、6–13 分钟，全部经同一个出口（诊断 diag_busy_2141）。缓存后启动时一次加锁恢复账户池与 Client 的目录与权益，
// 不发请求即可预热；后台只低速刷新缺失与过期的账户，且只取空闲账户（TryAcquireIdleAccount）

const (
	// emptyCatalogAcceptCount 与 emptyCatalogAcceptSpan：账户已有非空目录时，ListModels 连续返回空列表至少这么多次、
	// 且第一次为空距今至少这么久，才接受空目录（真正失去权限的账户最终变成不可调度）；在此之前保留原目录
	emptyCatalogAcceptCount = 3
	emptyCatalogAcceptSpan  = time.Hour
)

// ModelCatalogData 为一份已解析的 Playground 模型目录（只读）：模型列表、各模型的生成默认值、ListModels 原文与内容键。
// 写盘缓存保存原文，恢复时用 ParseModelCatalogRaw 重新解析，一次得到模型与默认值
type ModelCatalogData struct {
	catalog modelCatalog
}

// ParseModelCatalogRaw 解析写盘缓存保存的 ListModels 原文（经稀疏数组展开后的 JSON），供恢复使用
func ParseModelCatalogRaw(raw []byte) (ModelCatalogData, error) {
	catalog, err := parseModelCatalog(bytes.NewReader(raw))
	if err != nil {
		return ModelCatalogData{}, err
	}
	catalog.contentKey = catalogContentKey(catalog)
	return ModelCatalogData{catalog: catalog}, nil
}

// Models 返回目录模型的拷贝
func (data ModelCatalogData) Models() []Model {
	return cloneModels(data.catalog.models)
}

// Len 返回目录的模型数
func (data ModelCatalogData) Len() int {
	return len(data.catalog.models)
}

// ContentKey 返回目录的内容键：对解析结果（按模型 ID 排序的模型与生成默认值）的规范化 JSON 做 sha256，取前 32 位 hex。
// 不按原始字节哈希：上游给不同账户的字节有细微差别时，内容相同的目录仍合并为一份
func (data ModelCatalogData) ContentKey() string {
	if data.catalog.contentKey != "" {
		return data.catalog.contentKey
	}
	return catalogContentKey(data.catalog)
}

// Raw 返回 ListModels 原文（只读，不得修改）
func (data ModelCatalogData) Raw() []byte {
	return data.catalog.raw
}

// catalogContentKey 计算 Playground 目录的内容键；编码失败时为空（不参与共用与去重）
func catalogContentKey(catalog modelCatalog) string {
	type keyedEntry struct {
		Model    Model              `json:"model"`
		Defaults GenerationDefaults `json:"defaults"`
	}
	models := append([]Model(nil), catalog.models...)
	sort.SliceStable(models, func(left int, right int) bool { return models[left].ID < models[right].ID })
	entries := make([]keyedEntry, 0, len(models))
	for _, model := range models {
		entries = append(entries, keyedEntry{Model: model, Defaults: catalog.entries[model.ID].defaults})
	}
	return contentKeyOf("playground", entries)
}

// BuildCatalogKey 返回 Build 目录的内容键（按模型 ID 排序后的规范化 JSON 的 sha256 前 32 位 hex），写盘缓存按它去重
func BuildCatalogKey(models []Model) string {
	sorted := cloneAccountModels(models)
	sort.SliceStable(sorted, func(left int, right int) bool { return sorted[left].ID < sorted[right].ID })
	return contentKeyOf("build", sorted)
}

func contentKeyOf(kind string, value any) string {
	data, err := json.Marshal(struct {
		Kind  string `json:"kind"`
		Value any    `json:"value"`
	}{Kind: kind, Value: value})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// EmptyModelCatalogError 表示 ListModels 返回了空模型列表，而账户已有非空目录、尚未满“连续 3 次且跨 1 小时”，
// 账户池与 Client 两侧都保留原目录。errors.Is(err, ErrEmptyModelCatalog) 成立
type EmptyModelCatalogError struct {
	AccountID string
	// Models 为保留的原目录模型数
	Models int
	// Count 与 Since 为连续空结果的次数与第一次为空的时间
	Count int
	Since time.Time
}

func (e *EmptyModelCatalogError) Error() string {
	return fmt.Sprintf("%s，保留原目录（模型=%d，连续空结果=%d）", ErrEmptyModelCatalog.Error(), e.Models, e.Count)
}

// Is 让 errors.Is(err, ErrEmptyModelCatalog) 成立
func (e *EmptyModelCatalogError) Is(target error) bool {
	return target == ErrEmptyModelCatalog
}

// CatalogRateLimited 判断目录同步失败是否应全局暂停：上游 429 或协议错误码 8（RESOURCE_EXHAUSTED），以及连不上代理
// （拨号或代理握手失败）。这些失败与具体账户无关，继续按账户重试只会把出口打得更满
func CatalogRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rpcError *RPCError
	if errors.As(err, &rpcError) && (rpcError.StatusCode == http.StatusTooManyRequests || rpcError.Code == 8) {
		return true
	}
	var opError *net.OpError
	return errors.As(err, &opError) && (opError.Op == "dial" || opError.Op == "proxyconnect")
}

// CatalogRestore 为一个账户从写盘缓存恢复的目录
type CatalogRestore struct {
	AccountID string
	// Tier 为缓存时的权益，与账户运行态记录的权益不一致时不恢复（权益决定号池与权益头）
	Tier BenefitTier
	// Playground 为 Playground 目录；同一份缓存目录的账户应传同一个切片（指纹按切片只算一次），恢复时各自拷贝
	Playground []Model
	// Build 与 BuildSynced 为 Build 目录及其是否同步过（Build 未启用时为假，不恢复 Build 目录）
	Build       []Model
	BuildSynced bool
	// EmptyCount 与 EmptySince 为 ListModels 连续返回空列表的次数与第一次为空的时间
	EmptyCount int
	EmptySince time.Time
}

// CatalogRestoreResult 为 RestoreCatalogs 的结果
type CatalogRestoreResult struct {
	// Restored 为已恢复的账户
	Restored []string
	// Stale 为已恢复、但目录指纹与运行态记录不一致的账户（多半是别的进程同步过更新的目录，或解析器升级）：应尽快刷新。
	// 不恢复它们的话，升级后会出现整池没有目录、只能按每秒一个慢慢补的大面积不可服务
	Stale []string
	// TierMismatch 为缓存权益与运行态不一致、没有恢复的账户（按缺失同步）
	TierMismatch []string
	// Missing 为号池里没有的账户
	Missing []string
}

// RestoreCatalogs 用写盘缓存恢复账户的目录与权益：一次加锁处理全部账户，不走 updateRuntime（逐个走是上万次 flock 加读文件），
// 不写 runtime-state。恢复后账户与刚同步过一样：initializedAt 为恢复时刻，之后的同步按“更新”处理（SetCatalog 的 firstCatalog 为假，
// 目录是否变化与恢复的目录比较）。已经同步过目录的账户（initializedAt 非零）不覆盖。停用账户也恢复：启用时就不必再同步。
// 目录指纹与拷贝在锁外按目录切片各做一次，同一份缓存目录的账户共用一份只读拷贝：账户目录从不原地修改（更新时整体替换，
// 见 SetCatalog 与 CachedModels），共用是安全的；逐个账户拷贝 1.2 万次要在锁内花半秒以上
func (p *AccountPool) RestoreCatalogs(items []CatalogRestore) CatalogRestoreResult {
	var result CatalogRestoreResult
	if p == nil || len(items) == 0 {
		return result
	}
	type sliceKey struct {
		first  *Model
		length int
	}
	keyOf := func(models []Model) sliceKey {
		key := sliceKey{length: len(models)}
		if len(models) > 0 {
			key.first = &models[0]
		}
		return key
	}
	type fingerprintKey struct {
		tier   BenefitTier
		models sliceKey
	}
	fingerprintMemo := make(map[fingerprintKey]string)
	copies := make(map[sliceKey][]Model)
	copyOf := func(models []Model) []Model {
		key := keyOf(models)
		copied, exists := copies[key]
		if !exists {
			copied = cloneAccountModels(models)
			copies[key] = copied
		}
		return copied
	}
	fingerprints := make([]string, len(items))
	playgrounds := make([][]Model, len(items))
	builds := make([][]Model, len(items))
	for index, item := range items {
		key := fingerprintKey{tier: item.Tier, models: keyOf(item.Playground)}
		fingerprint, exists := fingerprintMemo[key]
		if !exists {
			fingerprint, _ = accountCatalogFingerprint(item.Tier, item.Playground)
			fingerprintMemo[key] = fingerprint
		}
		fingerprints[index] = fingerprint
		playgrounds[index] = copyOf(item.Playground)
		if item.BuildSynced {
			builds[index] = copyOf(item.Build)
		}
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// 先作废候选索引与状态缓存：逐个账户的 accountChangedLocked 不必同步索引与缓存项，全部处理完后统一重建
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	for index, item := range items {
		accountID := strings.TrimSpace(item.AccountID)
		account := p.byID[accountID]
		if account == nil {
			result.Missing = append(result.Missing, accountID)
			continue
		}
		if !account.initializedAt.IsZero() || fingerprints[index] == "" {
			continue
		}
		if item.Tier != account.runtime.BenefitTier {
			result.TierMismatch = append(result.TierMismatch, accountID)
			continue
		}
		account.Models = playgrounds[index]
		if item.BuildSynced {
			account.buildModels = builds[index]
		}
		account.BenefitTier = item.Tier
		setCatalogKeyLocked(account, fingerprints[index])
		account.initializedAt = now
		account.emptyCatalogs, account.emptyCatalogSince = item.EmptyCount, item.EmptySince
		account.modelAccessGeneration++
		if account.State == AccountUnavailable {
			account.State = AccountReady
			account.stateMessage = ""
		}
		p.accountChangedLocked(account)
		result.Restored = append(result.Restored, accountID)
		if fingerprints[index] != account.runtime.CatalogFingerprint {
			result.Stale = append(result.Stale, accountID)
		}
	}
	if len(result.Restored) > 0 {
		p.catalogVersion++
		p.eligibilityVersion++
	}
	p.notifyLocked()
	return result
}

// BuildCatalogOf 返回账户 Build 目录的拷贝；ok 为假表示账户不存在或还没有同步过 Build 目录
func (p *AccountPool) BuildCatalogOf(accountID string) ([]Model, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || account.buildModels == nil {
		return nil, false
	}
	return cloneAccountModels(account.buildModels), true
}

// TryAcquireIdleAccount 在账户空闲时立即取得独占租约（目录后台刷新用）：没有进行中的租约、没有独占、独占等待与续签时取得，
// 否则返回 nil, false, nil，不登记独占等待。AcquireAccount 的独占等待期间账户不再发出新租约，后台刷新用它会让正在服务长流的
// 热 Worker 暂停接新请求；空闲账户的独占只持续一次目录同步（约 1–2 秒），这期间请求换到别的账户
func (p *AccountPool) TryAcquireIdleAccount(ctx context.Context, accountID string) (*AccountLease, bool, error) {
	if p == nil {
		return nil, false, ErrAccountNotFound
	}
	accountID = strings.TrimSpace(accountID)
	p.mu.Lock()
	account := p.byID[accountID]
	if account == nil {
		p.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if accountBlocked(account) || account.active > 0 {
		p.mu.Unlock()
		return nil, false, nil
	}
	checkedAt := time.Now().UTC()
	leaseLock, leasePath, err := acquireAccountFileLease(account.StoragePath)
	if err != nil {
		p.mu.Unlock()
		if errors.Is(err, errAccountLeaseBusy) {
			return nil, false, nil
		}
		return nil, false, err
	}
	account.exclusive = true
	account.leaseLock = leaseLock
	account.leasePath = leasePath
	p.busyChangedLocked(account)
	// 出口通道与 AcquireAccount 一样在锁内读取：后台刷新的 ListModels 经 Client.do 计入所在通道的非生成发送（OtherSent1m，见 egress_rate.go）
	authGeneration, modelAccessGeneration, egress := account.authGeneration, account.modelAccessGeneration, account.egress
	p.mu.Unlock()
	lease := &AccountLease{
		pool: p, account: account, exclusive: true, authGeneration: authGeneration,
		modelAccessGeneration: modelAccessGeneration, checkedAt: checkedAt, egress: egress,
	}
	if err := p.refreshAccountRuntime(ctx, account); err != nil {
		releaseErr := lease.Release()
		if errors.Is(err, ErrAccountNotFound) {
			p.markStaleAccountUnavailable(account)
		}
		return nil, false, errors.Join(err, releaseErr)
	}
	p.mu.Lock()
	lease.modelAccessGeneration = account.modelAccessGeneration
	p.mu.Unlock()
	return lease, true, nil
}

// noteCatalogListing 记录一次 ListModels 的模型数：非空时清零连续空结果；为空时累计次数与第一次为空的时间。
// keep 为真表示账户已有非空目录、尚未满“连续 3 次且跨 1 小时”，应保留原目录；账户没有目录时照常写空目录（不改首次语义）
func (p *AccountPool) noteCatalogListing(accountID string, count int, now time.Time) (empties int, since time.Time, kept int, keep bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return 0, time.Time{}, 0, false
	}
	if count > 0 {
		account.emptyCatalogs, account.emptyCatalogSince = 0, time.Time{}
		return 0, time.Time{}, 0, false
	}
	account.emptyCatalogs++
	if account.emptyCatalogSince.IsZero() {
		account.emptyCatalogSince = now
	}
	empties, since, kept = account.emptyCatalogs, account.emptyCatalogSince, len(account.Models)
	if kept == 0 {
		return empties, since, 0, false
	}
	keep = empties < emptyCatalogAcceptCount || now.Sub(since) < emptyCatalogAcceptSpan
	return empties, since, kept, keep
}

// CatalogSnapshot 为一个账户当前的目录（目录同步成功后写缓存用）
type CatalogSnapshot struct {
	Tier BenefitTier
	// Playground 为 Client 侧的 Playground 目录（含原文与内容键）
	Playground ModelCatalogData
	// Build 与 BuildSynced 为账户池里的 Build 目录及其是否同步过
	Build       []Model
	BuildSynced bool
	// EmptyCount 与 EmptySince 为 ListModels 连续返回空列表的次数与第一次为空的时间（缓存的 en/ez）
	EmptyCount int
	EmptySince time.Time
}

// catalogStateOf 返回账户池一侧写缓存需要的目录状态；账户不存在时 ok 为假
func (p *AccountPool) catalogStateOf(accountID string) (snapshot CatalogSnapshot, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return CatalogSnapshot{}, false
	}
	snapshot.Tier = account.BenefitTier
	if account.buildModels != nil {
		snapshot.Build, snapshot.BuildSynced = cloneAccountModels(account.buildModels), true
	}
	snapshot.EmptyCount, snapshot.EmptySince = account.emptyCatalogs, account.emptyCatalogSince
	return snapshot, true
}
