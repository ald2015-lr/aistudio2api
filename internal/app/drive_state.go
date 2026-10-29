package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Drive 授权状态写盘：生成服务按账号记住 Drive 可用（换到过 Drive token）与不可用（GenerateAccessToken 返回 401 / Code 16），
// 原先只在内存，重启后全部变成未知，gemini-omni 需要 Drive 的输入（视频与无法转为 PNG 的图片；当时图片也都走 Drive）要靠用户请求
// 重新试号（线上约 1000 个账号里 958 个没有 Drive 授权，重启后每个带图请求最多试 8 个账号，约七成以 503 结束）。这里把记忆写进模型目录缓存同目录的 drive-accounts.json，装配运行时恢复：
// 记录带记下时认证文件的登录签名（aistudio.LoginIdentity，只认登录签名 Cookie），重启后认证文件换成另一个登录的账号不恢复；
// 有效期仍按记下时间计（见 aistudio.driveAvailableTTL、driveUnavailableTTL）

const (
	// driveStateFileName 为记录文件名（放在模型目录缓存的目录里）
	driveStateFileName = "drive-accounts.json"
	// driveStateFileVersion 为记录文件的格式版本，不符时忽略
	driveStateFileVersion = 1
	// driveStateSaveInterval 为有变化时两次写盘的最短间隔
	driveStateSaveInterval = 30 * time.Second
	// driveStateKeep 为记录的保留时长（与记忆的有效期一致，更早的记录恢复时也已过期，载入与写盘时丢掉）
	driveStateKeep = 24 * time.Hour
)

// driveStateEntry 为一个账号的记录（字段取短名）
type driveStateEntry struct {
	// State 为 a（可用）或 u（不可用）
	State string `json:"s"`
	// At 为记下的时间（unix 秒）
	At int64 `json:"at"`
	// Identity 为记下时认证文件的登录签名
	Identity string `json:"id"`
}

// driveStateFile 为 drive-accounts.json
type driveStateFile struct {
	Version  int                        `json:"version"`
	SavedAt  time.Time                  `json:"saved_at"`
	Accounts map[string]driveStateEntry `json:"accounts"`
}

// driveRecordStore 为能恢复与上报 Drive 授权状态记录的生成服务（aistudio.PooledService）
type driveRecordStore interface {
	SetDriveObserver(func(aistudio.DriveRecord))
	RestoreDriveRecords([]aistudio.DriveRecord) int
}

var _ driveRecordStore = (*aistudio.PooledService)(nil)

// driveStateMemory 为写盘的 Drive 授权状态记录；方法全部 nil 安全
type driveStateMemory struct {
	mu       sync.Mutex
	path     string
	requests *requestRegistry
	entries  map[string]driveStateEntry
	dirty    bool
	timer    *time.Timer
	saveMu   sync.Mutex
	closed   bool
	// now 只给包内测试替换
	now func() time.Time
}

// driveStatePath 返回记录文件的路径
func driveStatePath() string {
	return filepath.Join(catalogCacheDir, driveStateFileName)
}

// openDriveStateMemory 打开记录：path 为空时只在内存；文件不存在、损坏或版本不符时从空开始（损坏时写一条警告）
func openDriveStateMemory(path string, requests *requestRegistry) *driveStateMemory {
	memory := &driveStateMemory{path: path, requests: requests, entries: make(map[string]driveStateEntry)}
	if path == "" {
		return memory
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return memory
	}
	var file driveStateFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err != nil {
		memory.log("WARN", "Drive 授权状态记录读取失败，从空开始 | 文件="+path+" | 错误="+err.Error())
		return memory
	}
	if file.Version != driveStateFileVersion {
		return memory
	}
	now := memory.clock()
	for accountID, entry := range file.Accounts {
		if memory.keep(entry, now) {
			memory.entries[accountID] = entry
		}
	}
	return memory
}

func (memory *driveStateMemory) clock() time.Time {
	if memory.now != nil {
		return memory.now()
	}
	return time.Now()
}

func (memory *driveStateMemory) log(level string, message string) {
	if memory != nil && memory.requests != nil {
		memory.requests.log("service", level, message)
	}
}

// keep 判断记录是否保留：状态有效、带登录签名、记下时间不在未来且不超过 driveStateKeep
func (memory *driveStateMemory) keep(entry driveStateEntry, now time.Time) bool {
	if entry.State != "a" && entry.State != "u" || entry.Identity == "" {
		return false
	}
	at := time.Unix(entry.At, 0)
	return !at.After(now) && now.Sub(at) < driveStateKeep
}

// attach 把记录恢复进生成服务的记忆，并让之后的变化写盘；装配运行时、开始调度之前调用
func (memory *driveStateMemory) attach(service driveRecordStore) {
	if memory == nil || service == nil {
		return
	}
	memory.mu.Lock()
	records := make([]aistudio.DriveRecord, 0, len(memory.entries))
	for accountID, entry := range memory.entries {
		state := aistudio.DriveStateUnavailable
		if entry.State == "a" {
			state = aistudio.DriveStateAvailable
		}
		records = append(records, aistudio.DriveRecord{
			AccountID: accountID, State: state, CheckedAt: time.Unix(entry.At, 0), Identity: entry.Identity,
		})
	}
	memory.mu.Unlock()
	restored := service.RestoreDriveRecords(records)
	service.SetDriveObserver(memory.observe)
	if len(records) > 0 {
		memory.log("INFO", fmt.Sprintf("Drive 授权状态载入 | 记录=%d | 恢复=%d（账户已删除、重新登录或已过期的不恢复）", len(records), restored))
	}
}

// observe 记下生成服务上报的一条记录（aistudio.PooledService.SetDriveObserver 的回调，不阻塞）：State 为未知时删除
func (memory *driveStateMemory) observe(record aistudio.DriveRecord) {
	if memory == nil || record.AccountID == "" {
		return
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	switch record.State {
	case aistudio.DriveStateAvailable, aistudio.DriveStateUnavailable:
		state := "u"
		if record.State == aistudio.DriveStateAvailable {
			state = "a"
		}
		entry := driveStateEntry{State: state, At: record.CheckedAt.Unix(), Identity: record.Identity}
		if memory.entries[record.AccountID] == entry {
			return
		}
		memory.entries[record.AccountID] = entry
	default:
		if _, exists := memory.entries[record.AccountID]; !exists {
			return
		}
		delete(memory.entries, record.AccountID)
	}
	memory.dirty = true
	memory.scheduleSaveLocked()
}

// scheduleSaveLocked 有变化时安排一次写盘（最多每 driveStateSaveInterval 一次）；调用方持有 mu
func (memory *driveStateMemory) scheduleSaveLocked() {
	if memory.path == "" || !memory.dirty || memory.timer != nil || memory.closed {
		return
	}
	memory.timer = time.AfterFunc(driveStateSaveInterval, func() {
		memory.mu.Lock()
		memory.timer = nil
		memory.mu.Unlock()
		if err := memory.save(); err != nil {
			memory.log("WARN", "Drive 授权状态记录写入失败 | 错误="+err.Error())
		}
	})
}

// save 写盘（原子写），超过 driveStateKeep 的记录不写
func (memory *driveStateMemory) save() error {
	if memory == nil || memory.path == "" {
		return nil
	}
	memory.saveMu.Lock()
	defer memory.saveMu.Unlock()
	now := memory.clock()
	memory.mu.Lock()
	if !memory.dirty {
		memory.mu.Unlock()
		return nil
	}
	file := driveStateFile{Version: driveStateFileVersion, SavedAt: now.UTC(), Accounts: make(map[string]driveStateEntry, len(memory.entries))}
	for accountID, entry := range memory.entries {
		if !memory.keep(entry, now) {
			delete(memory.entries, accountID)
			continue
		}
		file.Accounts[accountID] = entry
	}
	memory.dirty = false
	memory.mu.Unlock()
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(memory.path, data, 0o600); err != nil {
		memory.mu.Lock()
		memory.dirty = true
		memory.mu.Unlock()
		return err
	}
	return nil
}

// close 停止计时器并立即写出没有落盘的变化（关闭运行时调用）
func (memory *driveStateMemory) close() error {
	if memory == nil {
		return nil
	}
	memory.mu.Lock()
	memory.closed = true
	if memory.timer != nil {
		memory.timer.Stop()
		memory.timer = nil
	}
	memory.mu.Unlock()
	return memory.save()
}
