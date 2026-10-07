package app

import "testing"

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
