package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// proxyPoolRecord 为 runtime/proxy-pool.json 的内容（版本 1，权限 0600）：只有账户 ID、端口号、会话号与时间，
// 不含任何地址、主机名、账号密码。两个模式的记录都保留，切换模式不丢
type proxyPoolRecord struct {
	Version int                    `json:"version"`
	SavedAt time.Time              `json:"saved_at"`
	Static  proxyPoolStaticRecord  `json:"static"`
	Dynamic proxyPoolDynamicRecord `json:"dynamic"`
}

// proxyPoolStaticRecord 为静态 IP 的记录：EndpointKey 与当前设置不同时丢掉 Ports（冷却、归属），保留 Accounts（只是偏好）
type proxyPoolStaticRecord struct {
	EndpointKey string                             `json:"endpoint_key,omitempty"`
	Accounts    map[string]proxyPoolAffinityRecord `json:"accounts,omitempty"`
	Ports       map[string]proxyPoolPortRecord     `json:"ports,omitempty"`
}

type proxyPoolAffinityRecord struct {
	Port int       `json:"port"`
	At   time.Time `json:"at"`
}

type proxyPoolPortRecord struct {
	LastAccount  string    `json:"last_account,omitempty"`
	LastUsed     time.Time `json:"last_used,omitzero"`
	CoolingUntil time.Time `json:"cooling_until,omitzero"`
	CoolingLevel int       `json:"cooling_level,omitempty"`
	CooledAt     time.Time `json:"cooled_at,omitzero"`
}

type proxyPoolDynamicRecord struct {
	Accounts map[string]proxyPoolSessionRecord `json:"accounts,omitempty"`
}

// proxyPoolSessionRecord 为动态 IP 的账户会话：Base 为登录时采用的临时会话号（空为按账户 ID 哈希），UsedAt 用于修剪
type proxyPoolSessionRecord struct {
	Base       string    `json:"base,omitempty"`
	Generation int       `json:"generation,omitempty"`
	RotatedAt  time.Time `json:"rotated_at,omitzero"`
	UsedAt     time.Time `json:"used_at,omitzero"`
}

// load 载入记录文件：不存在时从空开始；JSON 坏或版本不认识时 WARN 后从空开始。只在 newProxyPool 里调用（还没有其他协程）
func (pool *proxyPool) load() []proxyPoolLog {
	if pool.path == "" {
		return nil
	}
	data, err := os.ReadFile(pool.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	failed := func(err error) []proxyPoolLog {
		return []proxyPoolLog{{source: "service", level: "WARN", message: "IP 池记录读取失败，重新开始记录 | 错误=" + err.Error()}}
	}
	if err != nil {
		return failed(err)
	}
	var record proxyPoolRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return failed(err)
	}
	if record.Version != proxyPoolFileVersion {
		return failed(fmt.Errorf("版本 %d 不认识", record.Version))
	}
	pool.portsKey = record.Static.EndpointKey
	for accountID, affinity := range record.Static.Accounts {
		if accountID != "" && affinity.Port >= 1 && affinity.Port <= 65535 {
			pool.affinity[accountID] = proxyPoolAffinity{port: affinity.Port, at: affinity.At}
		}
	}
	for key, port := range record.Static.Ports {
		number, err := strconv.Atoi(key)
		if err != nil || number < 1 || number > 65535 {
			continue
		}
		state := pool.portLocked(number)
		state.lastAccount, state.lastUsedAt = port.LastAccount, port.LastUsed
		state.coolUntil, state.coolLevel, state.cooledAt = port.CoolingUntil, max(port.CoolingLevel, 0), port.CooledAt
	}
	for accountID, session := range record.Dynamic.Accounts {
		if accountID == "" || session.Base != "" && !validProxyPoolBase(session.Base) ||
			session.Generation < 0 || session.Generation > proxyPoolMaxGeneration {
			continue
		}
		pool.sessions[accountID] = &proxyPoolSession{
			base: session.Base, generation: session.Generation, rotatedAt: session.RotatedAt, usedAt: session.UsedAt,
		}
	}
	return nil
}

// markDirtyLocked 记下记录有变化：proxyPoolSaveDelay 后合并写盘一次
func (pool *proxyPool) markDirtyLocked() {
	pool.dirty = true
	if pool.path == "" || pool.saveTimer != nil {
		return
	}
	pool.saveTimer = time.AfterFunc(proxyPoolSaveDelay, func() {
		pool.mu.Lock()
		pool.saveTimer = nil
		pool.mu.Unlock()
		pool.save()
	})
}

// flush 立即写出未保存的记录（生成服务关闭与 runtimeManager.Close 时调用）
func (pool *proxyPool) flush() {
	if pool == nil {
		return
	}
	pool.mu.Lock()
	if pool.saveTimer != nil {
		pool.saveTimer.Stop()
		pool.saveTimer = nil
	}
	pool.mu.Unlock()
	pool.save()
}

// save 锁内复制快照、锁外编码与原子写入；saveMu 保证较旧的快照不会最后落盘。写入失败时 WARN，下一次变化再写
func (pool *proxyPool) save() {
	if pool.path == "" {
		return
	}
	pool.saveMu.Lock()
	defer pool.saveMu.Unlock()
	pool.mu.Lock()
	owner, dirty := pool.owner, pool.dirty
	pool.mu.Unlock()
	if !dirty {
		return
	}
	var known map[string]struct{}
	if owner != nil {
		ids := owner.AccountIDs()
		known = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			known[id] = struct{}{}
		}
	}
	pool.mu.Lock()
	record := pool.snapshotLocked(known, pool.now())
	pool.dirty = false
	pool.mu.Unlock()
	data, err := json.MarshalIndent(record, "", "  ")
	if err == nil {
		err = writeFileAtomic(pool.path, append(data, '\n'), 0o600)
	}
	if err != nil {
		pool.mu.Lock()
		pool.dirty = true
		pool.mu.Unlock()
		pool.emit([]proxyPoolLog{{source: "service", level: "WARN", message: "IP 池记录写入失败 | 错误=" + err.Error()}})
	}
}

// snapshotLocked 复制要写盘的记录，并修剪已不在账户池里、且 30 天没有用过的账户记录（known 为空时不按账户池修剪）。
// 端口只写有归属或冷却档位的
func (pool *proxyPool) snapshotLocked(known map[string]struct{}, now time.Time) proxyPoolRecord {
	stale := func(accountID string, at time.Time) bool {
		if known == nil {
			return false
		}
		if _, ok := known[accountID]; ok {
			return false
		}
		return now.Sub(at) >= proxyPoolAffinityTTL
	}
	record := proxyPoolRecord{
		Version: proxyPoolFileVersion, SavedAt: now.UTC(),
		Static: proxyPoolStaticRecord{
			EndpointKey: pool.portsKey,
			Accounts:    make(map[string]proxyPoolAffinityRecord, len(pool.affinity)),
			Ports:       make(map[string]proxyPoolPortRecord),
		},
		Dynamic: proxyPoolDynamicRecord{Accounts: make(map[string]proxyPoolSessionRecord, len(pool.sessions))},
	}
	for accountID, affinity := range pool.affinity {
		if stale(accountID, affinity.at) {
			delete(pool.affinity, accountID)
			continue
		}
		record.Static.Accounts[accountID] = proxyPoolAffinityRecord{Port: affinity.port, At: affinity.at.UTC()}
	}
	for port, state := range pool.ports {
		if state.lastAccount == "" && state.coolLevel == 0 {
			continue
		}
		record.Static.Ports[strconv.Itoa(port)] = proxyPoolPortRecord{
			LastAccount: state.lastAccount, LastUsed: proxyPoolUTC(state.lastUsedAt),
			CoolingUntil: proxyPoolUTC(state.coolUntil), CoolingLevel: state.coolLevel, CooledAt: proxyPoolUTC(state.cooledAt),
		}
	}
	for accountID, session := range pool.sessions {
		if session.base == "" && session.generation == 0 {
			// 只有连通失败计数的会话不写盘
			continue
		}
		at := session.usedAt
		if session.rotatedAt.After(at) {
			at = session.rotatedAt
		}
		if stale(accountID, at) {
			delete(pool.sessions, accountID)
			continue
		}
		record.Dynamic.Accounts[accountID] = proxyPoolSessionRecord{
			Base: session.base, Generation: session.generation,
			RotatedAt: proxyPoolUTC(session.rotatedAt), UsedAt: proxyPoolUTC(session.usedAt),
		}
	}
	return record
}

// validProxyPoolBase 判断记录里的基础会话号：1 到 12 个小写字母与数字
func validProxyPoolBase(base string) bool {
	if len(base) == 0 || len(base) > proxyPoolSessionHashLen {
		return false
	}
	for index := 0; index < len(base); index++ {
		character := base[index]
		if !('a' <= character && character <= 'z' || '0' <= character && character <= '9') {
			return false
		}
	}
	return true
}

func proxyPoolUTC(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}
