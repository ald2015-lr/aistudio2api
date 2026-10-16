package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 停用原因：账户被停用时记下原因与时间，管理页账户列表在停用账户的邮箱下方显示。
// 保存在账户目录下的 .disable-reasons.json，不写入各账户的 account.json：
// 旧版本程序与外部工具读取 account.json 都不受影响，回滚也不会出问题。
// 自动停用记录合并几秒后写入；新的手动停用记录立即写入（见 set）：没有手动停用记录的停用账户重启后按默认启用，
// 这条记录丢了账户就会被悄悄启用。多个进程共用一个账户目录时各自整份写出自己载入的记录，不做合并
const (
	disableNotesFileName  = ".disable-reasons.json"
	disableNotesSaveDelay = 3 * time.Second
	// unreadableNotesReason 为停用记录文件无法读取时，给没有记录的停用账户补上的手动停用原因
	unreadableNotesReason = "停用记录文件（" + disableNotesFileName + "）无法读取，无法确认是否为手动停用，按手动停用保留"
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
	writeMu sync.Mutex
	path    string
	notes   map[string]disableNote
	// dirty 为真时有尚未写入文件的变化（写入失败的也保留，下一次保存再写）；saveScheduled 为真时已有一次定时保存在等待
	dirty         bool
	saveScheduled bool
	// unreadable 为真时文件存在却读不出来：已有的手动停用记录不可知，没有记录的停用账户也按手动停用处理（见 keepDisabled）。
	// backupPending 为真时改写文件前先把读不出来的原文件另存一份，供人工恢复
	unreadable    bool
	backupPending bool
	// explicitSince 为管理员最近一次“处理现有停用账户”的时间（见 noteExplicitRequest）
	explicitSince time.Time
	// requests 用于记录载入与写入失败；测试中可能为空
	requests *requestRegistry
	// encode 把记录编码为文件内容；为空时用 json.MarshalIndent（测试替换它确认编码时不持有 mu）
	encode func(map[string]disableNote) ([]byte, error)
}

var accountDisableNotes = &disableNotes{notes: make(map[string]disableNote)}

// load 读取账户目录下保存的停用原因。重建生成服务时上一代实例可能还有几秒内未保存的变化（例如刚手动停用），
// 先写出再读取，否则会被文件中的旧内容覆盖，账户随后按“没有停用记录”处理。
// 文件存在却读不出来（权限、损坏）时原先静默当作没有记录，手动停用的账户在随后的启动启用中全部被启用、只留下一条计数日志；
// 现在记一条 WARN，本次运行把没有记录的停用账户按手动停用处理
func (store *disableNotes) load(directory string, requests *requestRegistry) {
	store.flush()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.requests = requests
	store.path = filepath.Join(directory, disableNotesFileName)
	store.unreadable = false
	store.backupPending = false
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		var notes map[string]disableNote
		if err = json.Unmarshal(data, &notes); err == nil {
			if notes != nil {
				store.notes = notes
			}
			return
		}
	}
	store.unreadable = true
	store.backupPending = true
	store.logLocked("WARN", fmt.Sprintf(
		"停用记录文件无法读取，本次运行不自动启用没有停用记录的停用账户（全部按手动停用处理，需要启用的在管理页启用）；"+
			"改写前原文件另存为 %s.unreadable-时间 | 文件=%s | 错误=%s",
		disableNotesFileName, store.path, strings.TrimSpace(err.Error()),
	))
}

// set 记下停用原因。新的手动停用记录（原先没有记录或是自动停用）立即写入文件再返回：原先合并 3 秒后才写，
// 这期间进程退出（崩溃、被杀、断电）记录就丢了，account.json 已是停用，重启后按“没有手动停用记录”启用。
// 已有手动停用记录的再次停用（批量停用选中了已停用的账户）只更新时间与原因，合并几秒后写入：文件里已有手动停用记录，
// 丢了这几秒账户也保持停用；原先每次都整份重写并同步文件，记录上万条时批量停用几千个账户要多花几分钟、写几 GB。
// 例外是记录早于最近一次“处理现有停用账户”：更新时间决定这个账户是否仍保持停用（见 blocksAutoEnable），同样立即写入。
// 自动停用的记录不覆盖已有的手动停用记录：排队期间被手动停用、随后验证未通过的账户原先被改记为自动停用，重启后又被启用
func (store *disableNotes) set(accountID string, reason string, auto bool) {
	store.mu.Lock()
	existing, exists := store.notes[accountID]
	if auto && exists && !existing.Auto {
		store.mu.Unlock()
		return
	}
	store.notes[accountID] = disableNote{Reason: reason, At: time.Now(), Auto: auto}
	if auto || exists && !existing.Auto && existing.At.After(store.explicitSince) {
		store.scheduleSaveLocked()
		store.mu.Unlock()
		return
	}
	store.markDirtyLocked()
	store.mu.Unlock()
	store.flush()
}

// noteExplicitRequest 记下管理员“处理现有停用账户”的时间（加入队列时，以及重启后从处理记录恢复时）：
// 早于这个时间的手动停用记录再次停用时立即写入（见 set），否则这几秒内进程退出，重启后按文件中较早的时间判断，
// 加入之后的再次停用被当成加入之前的，账户验证通过后被启用
func (store *disableNotes) noteExplicitRequest(at time.Time) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if at.After(store.explicitSince) {
		store.explicitSince = at
	}
}

// presumeManual 给没有停用记录的账户补记手动停用（停用记录文件无法读取时，见 enableUnmarkedAccounts）并立即写入：
// 只在内存中按手动停用处理的话，本次运行任何一次保存都会用不含这些账户的记录改写文件，下次启动又会把它们全部启用
func (store *disableNotes) presumeManual(accountIDs []string) {
	store.mu.Lock()
	now := time.Now()
	changed := false
	for _, accountID := range accountIDs {
		if _, exists := store.notes[accountID]; !exists {
			store.notes[accountID] = disableNote{Reason: unreadableNotesReason, At: now}
			changed = true
		}
	}
	if !changed {
		store.mu.Unlock()
		return
	}
	store.markDirtyLocked()
	store.mu.Unlock()
	store.flush()
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

// clearAutomatic 只清除账户的自动停用记录，手动停用记录保留（目录消失后移出账户池时）
func (store *disableNotes) clearAutomatic(accountID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if note, exists := store.notes[accountID]; !exists || !note.Auto {
		return
	}
	delete(store.notes, accountID)
	store.scheduleSaveLocked()
}

// clearAutomaticFor 清除启用账户的自动停用记录，手动停用记录保留（启动与导入时，见 enableUnmarkedAccounts）：
// 自动停用记录只在账户停用时显示原因，也不挡住启用，启用账户留着它只会让文件越来越大，每次写入都要整份重写
func (store *disableNotes) clearAutomaticFor(accountIDs []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	changed := false
	for _, accountID := range accountIDs {
		if note, exists := store.notes[accountID]; exists && note.Auto {
			delete(store.notes, accountID)
			changed = true
		}
	}
	if changed {
		store.scheduleSaveLocked()
	}
}

// manual 判断账户是否有手动停用记录（管理页或管理接口）
func (store *disableNotes) manual(accountID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	note, exists := store.notes[accountID]
	return exists && !note.Auto
}

// keepDisabled 判断停用账户是否不能被自动启用（启动与导入时、认证文件更新后、新账户自动处理）：
// 有手动停用记录，或者停用记录文件无法读取、账户没有记录
func (store *disableNotes) keepDisabled(accountID string) bool {
	return store.blocksAutoEnable(accountID, time.Time{})
}

// blocksAutoEnable 同 keepDisabled；explicitAt 非零表示管理员在这一时间明确要求处理全部停用账户（管理页“处理现有停用账户”，
// 确认框写明手动停用的也会启用）：在这之前的手动停用不再阻止启用，之后才手动停用的仍保持停用
func (store *disableNotes) blocksAutoEnable(accountID string, explicitAt time.Time) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	note, exists := store.notes[accountID]
	if !exists {
		return store.unreadable && explicitAt.IsZero()
	}
	return !note.Auto && (explicitAt.IsZero() || note.At.After(explicitAt))
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

// markDirtyLocked 登记有尚未写入的变化；还没有文件路径（测试、载入之前）时不写文件
func (store *disableNotes) markDirtyLocked() {
	if store.path != "" {
		store.dirty = true
	}
}

// scheduleSaveLocked 登记变化并在 disableNotesSaveDelay 后保存一次，期间的多次变化合并写入
func (store *disableNotes) scheduleSaveLocked() {
	store.markDirtyLocked()
	if !store.dirty || store.saveScheduled {
		return
	}
	store.saveScheduled = true
	time.AfterFunc(disableNotesSaveDelay, func() {
		store.mu.Lock()
		store.saveScheduled = false
		store.mu.Unlock()
		store.flush()
	})
}

// flush 写出尚未保存的变化：定时保存合并几秒内的多次变化，关闭生成服务时立即写出，原先关闭前 3 秒内的变化会丢失；
// 没有待保存的变化时什么都不做。同时写入的调用排队进行，返回时调用前的变化都已写入（或写入失败并记了日志）。
// 写入失败原先被忽略、待保存标记也被清掉；现在记一条 WARN 并保留标记，下一次保存或关闭时再写。
// 持有 mu 时只复制一份记录，编码与写入在 mu 之外进行：记录上万条时编码要几十毫秒，原先全程持有 mu，
// 管理页构建账户列表（每个停用账户查一次原因）与停用、启用都要等它；writeMu 一直持有到写完，较旧的快照不会最后落盘
func (store *disableNotes) flush() {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.mu.Lock()
	if !store.dirty || store.path == "" {
		store.mu.Unlock()
		return
	}
	store.dirty = false
	path, backup := store.path, store.backupPending
	snapshot := maps.Clone(store.notes)
	encode := store.encode
	store.mu.Unlock()
	if encode == nil {
		encode = func(notes map[string]disableNote) ([]byte, error) { return json.MarshalIndent(notes, "", "  ") }
	}
	data, err := encode(snapshot)
	if err == nil && backup {
		store.backupUnreadable(path)
	}
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.dirty = true
	store.logLocked("WARN", fmt.Sprintf(
		"停用记录保存失败，手动停用的账户重启后可能被按默认启用；下一次保存或关闭时重试 | 文件=%s | 错误=%s",
		path, strings.TrimSpace(err.Error()),
	))
}

// backupUnreadable 改写读不出来的停用记录文件之前，用硬链接另存原文件（读不出来的文件也能保留），供人工恢复其中的记录；
// 另存失败只记日志，没有记录的停用账户已经补记为手动停用（见 presumeManual），改写不会让它们被启用
func (store *disableNotes) backupUnreadable(path string) {
	backupPath := path + ".unreadable-" + time.Now().Format("20060102-150405")
	err := os.Link(path, backupPath)
	store.mu.Lock()
	defer store.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		store.logLocked("WARN", fmt.Sprintf(
			"无法另存读不出来的停用记录文件，原文件将被改写 | 文件=%s | 错误=%s", path, strings.TrimSpace(err.Error()),
		))
	} else if err == nil {
		store.logLocked("INFO", "读不出来的停用记录文件已另存为 "+backupPath)
	}
	store.backupPending = false
}

// logLocked 记录一条日志；调用方持有 mu
func (store *disableNotes) logLocked(level string, message string) {
	if store.requests != nil {
		store.requests.log("auth", level, message)
	}
}
