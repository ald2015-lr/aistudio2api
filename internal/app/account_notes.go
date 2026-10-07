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

// disableNote 为一条停用记录。Auto 为真表示自动停用（导入时 account.json 为停用、验证未通过等），
// 认证文件更新后会重新验证；手动停用的保持停用
type disableNote struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	Auto   bool      `json:"auto,omitempty"`
}

type disableNotes struct {
	mu          sync.Mutex
	path        string
	notes       map[string]disableNote
	savePending bool
}

var accountDisableNotes = &disableNotes{notes: make(map[string]disableNote)}

// load 读取账户目录下保存的停用原因
func (store *disableNotes) load(directory string) {
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

func (store *disableNotes) setIfAbsent(accountID string, reason string, auto bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.notes[accountID]; exists {
		return
	}
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

// manual 判断账户是否为手动停用（管理页或管理接口）
func (store *disableNotes) manual(accountID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	note, exists := store.notes[accountID]
	return exists && !note.Auto
}

// message 返回停用账户在管理页显示的原因
func (store *disableNotes) message(accountID string) string {
	store.mu.Lock()
	note, exists := store.notes[accountID]
	store.mu.Unlock()
	if !exists {
		return "停用原因未记录：account.json 中为停用（在加入停用原因记录之前就已停用，或由外部程序写入）"
	}
	return fmt.Sprintf("停用原因：%s（%s）", note.Reason, note.At.Local().Format("01-02 15:04"))
}

func (store *disableNotes) scheduleSaveLocked() {
	if store.savePending || store.path == "" {
		return
	}
	store.savePending = true
	time.AfterFunc(disableNotesSaveDelay, store.save)
}

// save 合并几秒内的多次变化后写一次文件
func (store *disableNotes) save() {
	store.mu.Lock()
	store.savePending = false
	path := store.path
	data, err := json.MarshalIndent(store.notes, "", "  ")
	store.mu.Unlock()
	if err != nil || path == "" {
		return
	}
	_ = writeFileAtomic(path, append(data, '\n'), 0o600)
}
