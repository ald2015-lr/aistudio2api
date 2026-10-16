package app

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestDisableNotesAutomatic 只有记录为自动停用的账户才算自动停用；没有记录时按手动处理
func TestDisableNotesAutomatic(t *testing.T) {
	store := &disableNotes{notes: map[string]disableNote{}}
	store.set("auto@example.com", "验证未通过", true)
	store.set("manual@example.com", "管理页停用", false)
	if !store.automatic("auto@example.com") || store.manual("auto@example.com") {
		t.Fatal("自动停用的账户判断错误")
	}
	if store.automatic("manual@example.com") || !store.manual("manual@example.com") {
		t.Fatal("手动停用的账户判断错误")
	}
	if store.automatic("unknown@example.com") || store.manual("unknown@example.com") {
		t.Fatal("没有记录的账户不应算作自动停用")
	}
}

// TestDisableNotesRepeatManualDebounced 已有手动停用记录的账户再次停用只更新时间与原因、合并几秒写入：原先每次都整份重写并同步文件，
// 记录上万条时批量停用几千个账户要多花几分钟。新的手动停用记录（包括自动停用改为手动停用）仍在返回前写入
func TestDisableNotesRepeatManualDebounced(t *testing.T) {
	directory := t.TempDir()
	store := &disableNotes{notes: make(map[string]disableNote)}
	store.load(directory, nil)
	// 每次写入文件都先编码一次：统计编码次数即写入次数
	var writes atomic.Int32
	store.encode = func(notes map[string]disableNote) ([]byte, error) {
		writes.Add(1)
		return json.MarshalIndent(notes, "", "  ")
	}
	// 等待中的定时保存到时已没有变化，不会在临时目录删除后再写
	t.Cleanup(store.flush)

	store.set("manual@example.com", "通过管理页或管理接口停用", false)
	if note, exists := readDisableNotesFile(t, directory)["manual@example.com"]; !exists || note.Auto || writes.Load() != 1 {
		t.Fatalf("新的手动停用记录应在返回前写入文件：写入 %d 次", writes.Load())
	}
	for index := range 20 {
		store.set("manual@example.com", fmt.Sprintf("再次停用 %d", index), false)
	}
	if count := writes.Load(); count != 1 {
		t.Fatalf("已有手动停用记录的再次停用不应立即重写文件：写入 %d 次", count)
	}
	store.mu.Lock()
	scheduled := store.saveScheduled
	store.mu.Unlock()
	if !scheduled {
		t.Fatal("再次停用应安排一次合并保存")
	}
	store.flush()
	if note := readDisableNotesFile(t, directory)["manual@example.com"]; note.Reason != "再次停用 19" || note.Auto || writes.Load() != 2 {
		t.Fatalf("合并保存应一次写入最后一次的原因：%+v，写入 %d 次", note, writes.Load())
	}

	store.set("auto@example.com", "新账户自动处理验证未通过", true)
	store.set("auto@example.com", "通过管理页或管理接口停用", false)
	if note, exists := readDisableNotesFile(t, directory)["auto@example.com"]; !exists || note.Auto {
		t.Fatal("自动停用改为手动停用应在返回前写入文件")
	}
}

// TestDisableNotesFlushEncodesOutsideLock 写入时只在 mu 内复制记录，编码与写入在 mu 之外进行（仍持有 writeMu，较旧的快照不会最后落盘）：
// 原先全程持有 mu，记录上万条时管理页构建账户列表、停用与启用都要等编码完成
func TestDisableNotesFlushEncodesOutsideLock(t *testing.T) {
	directory := t.TempDir()
	store := &disableNotes{notes: make(map[string]disableNote)}
	store.load(directory, nil)
	encoded, muHeld, writeMuHeld := 0, false, false
	store.encode = func(notes map[string]disableNote) ([]byte, error) {
		encoded++
		if store.mu.TryLock() {
			store.mu.Unlock()
		} else {
			muHeld = true
		}
		if store.writeMu.TryLock() {
			store.writeMu.Unlock()
		} else {
			writeMuHeld = true
		}
		return json.MarshalIndent(notes, "", "  ")
	}

	store.set("a@example.com", "通过管理页或管理接口停用", false)
	if encoded != 1 {
		t.Fatalf("编码 %d 次，期望 1 次", encoded)
	}
	if muHeld {
		t.Fatal("编码时不应持有 mu")
	}
	if !writeMuHeld {
		t.Fatal("编码与写入期间应持有 writeMu")
	}
	if note, exists := readDisableNotesFile(t, directory)["a@example.com"]; !exists || note.Auto {
		t.Fatal("手动停用记录应写入文件")
	}
}

// TestEnableUnmarkedAccountsClearsAutomaticForEnabled 启动与导入时清除已是启用的账户的自动停用记录：它们不再显示也不挡住启用，
// 原先一直留在文件里，文件越来越大，每次写入都要整份重写。手动停用记录保留
func TestEnableUnmarkedAccountsClearsAutomaticForEnabled(t *testing.T) {
	notes := isolateDisableNotes(t)
	root := t.TempDir()
	writeEnableTestAccount(t, root, "auto-on@example.com", true)
	writeEnableTestAccount(t, root, "manual-on@example.com", true)
	notes.set("auto-on@example.com", "新账户自动处理验证未通过", true)
	notes.set("manual-on@example.com", "通过管理页或管理接口停用", false)
	accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}

	if enabled := enableUnmarkedAccounts(accounts, nil, "启动时"); len(enabled) != 0 {
		t.Fatalf("没有停用账户，不应启用：%v", enabled)
	}
	if notes.automatic("auto-on@example.com") {
		t.Fatal("启用账户的自动停用记录应清除")
	}
	if !notes.manual("manual-on@example.com") {
		t.Fatal("手动停用记录应保留")
	}
}
