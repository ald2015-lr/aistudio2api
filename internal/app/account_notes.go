package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 停用原因：账户被停用时记下原因与时间，管理页账户列表在停用账户的邮箱下方显示。
// 保存在账户目录下的 .disable-reasons.json，不写入各账户的 account.json：
// 旧版本程序与外部工具读取 account.json 都不受影响，回滚也不会出问题
const (
	disableNotesFileName  = ".disable-reasons.json"
	disableNotesSaveDelay = 3 * time.Second
)

// disableNote 为一条停用记录。Auto 为真表示自动停用（新账户自动处理验证未通过或出错）：账户默认启用，
// 自动停用与没有记录的账户在重启、导入或认证文件更新后按默认启用；只有手动停用的（管理页或管理接口）保持停用
type disableNote struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	Auto   bool      `json:"auto,omitempty"`
}

type disableNotes struct {
	mu sync.Mutex
	// writeMu 让文件按快照的先后写入：定时保存与关闭时的立即保存同时进行时，较旧的快照不会最后落盘
	writeMu     sync.Mutex
	path        string
	notes       map[string]disableNote
	savePending bool
}

var accountDisableNotes = &disableNotes{notes: make(map[string]disableNote)}

// load 读取账户目录下保存的停用原因。重建生成服务时上一代实例可能还有几秒内未保存的变化（例如刚手动停用），
// 先写出再读取，否则会被文件中的旧内容覆盖，账户随后按“没有停用记录”处理
func (store *disableNotes) load(directory string) {
	store.flush()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.path = filepath.Join(directory, disableNotesFileName)
	data, err := os.ReadFile(store.path)
	if err != nil {
		return
	}
	var notes map[string]disableNote
	if json.Unmarshal(data, &notes) == nil && notes != nil {
		store.notes = notes
	}
}

func (store *disableNotes) set(accountID string, reason string, auto bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.notes[accountID] = disableNote{Reason: reason, At: time.Now(), Auto: auto}
	store.scheduleSaveLocked()
}

func (store *disableNotes) clear(accountID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.notes[accountID]; !exists {
		return
	}
	delete(store.notes, accountID)
	store.scheduleSaveLocked()
}

// clearMany 一次清除多个账户的停用原因（启动或导入时批量启用）
func (store *disableNotes) clearMany(accountIDs []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	changed := false
	for _, accountID := range accountIDs {
		if _, exists := store.notes[accountID]; exists {
			delete(store.notes, accountID)
			changed = true
		}
	}
	if changed {
		store.scheduleSaveLocked()
	}
}

// manual 判断账户是否为手动停用（管理页或管理接口）
func (store *disableNotes) manual(accountID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	note, exists := store.notes[accountID]
	return exists && !note.Auto
}

// automatic 判断账户是否有记录且为自动停用（新账户自动处理验证未通过等）；没有记录时返回 false
func (store *disableNotes) automatic(accountID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	note, exists := store.notes[accountID]
	return exists && note.Auto
}

// message 返回停用账户在管理页显示的原因
func (store *disableNotes) message(accountID string) string {
	store.mu.Lock()
	note, exists := store.notes[accountID]
	store.mu.Unlock()
	if !exists {
		return "停用原因未记录：account.json 中为停用（由外部程序写入或停用记录丢失），重启或认证文件更新后按默认启用"
	}
	return fmt.Sprintf("停用原因：%s（%s）", note.Reason, note.At.Local().Format("01-02 15:04"))
}

func (store *disableNotes) scheduleSaveLocked() {
	if store.savePending || store.path == "" {
		return
	}
	store.savePending = true
	time.AfterFunc(disableNotesSaveDelay, store.flush)
}

// flush 写出尚未保存的变化：定时保存合并几秒内的多次变化，关闭生成服务时立即写出，原先关闭前 3 秒内的变化会丢失；
// 没有待保存的变化时什么都不做
func (store *disableNotes) flush() {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.mu.Lock()
	if !store.savePending {
		store.mu.Unlock()
		return
	}
	store.savePending = false
	path := store.path
	data, err := json.MarshalIndent(store.notes, "", "  ")
	store.mu.Unlock()
	if err != nil || path == "" {
		return
	}
	_ = writeFileAtomic(path, append(data, '\n'), 0o600)
}
