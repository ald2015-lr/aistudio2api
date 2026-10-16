package aistudio

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestClearCooldownsMatching 按条件清除账户的冷却：只清除满足条件的作用域（全局与各模型、各通道），其余冷却保留，
// 结果写回运行态文件；没有符合条件的冷却时不改动；账户不存在时返回 ErrAccountNotFound
func TestClearCooldownsMatching(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	until := time.Now().Add(2 * time.Hour)
	for scope, reason := range map[string]string{
		"":          "10 分钟内 3 个模型连续 3 次返回 403 无权限，自动暂停",
		"pro":       "无权限（模型）: denied",
		"build:pro": "无权限（模型）: denied",
		"flash":     "每日限额",
	} {
		if err := pool.MarkCooldownIfGeneration(id, scope, pool.ModelAccessGeneration(id), time.Now(), until, reason); err != nil {
			t.Fatal(err)
		}
	}
	forbidden := func(_ string, cooldown CooldownState) bool {
		return strings.HasPrefix(cooldown.Reason, "无权限（模型）") || strings.HasSuffix(cooldown.Reason, "403 无权限，自动暂停")
	}
	cleared, err := pool.ClearCooldownsMatching(id, forbidden)
	if err != nil || cleared != 3 {
		t.Fatalf("cleared=%d err=%v，期望清除 3 条", cleared, err)
	}
	status, _ := pool.StatusOf(id)
	keys := make([]string, 0, len(status.Cooldowns))
	for key := range status.Cooldowns {
		keys = append(keys, key)
	}
	if !slices.Equal(keys, []string{"flash"}) {
		t.Fatalf("剩余冷却 = %v，期望只剩每日限额", keys)
	}
	if pool.AccountCoolingDown(id, "") || pool.AccountCoolingDown(id, "pro") || !pool.AccountCoolingDown(id, "flash") {
		t.Fatal("清除后应只剩 flash 冷却")
	}
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(account.RuntimePath)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Cooldowns map[string]CooldownState `json:"cooldowns"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if _, kept := stored.Cooldowns["flash"]; len(stored.Cooldowns) != 1 || !kept {
		t.Fatalf("运行态文件中的冷却 = %v，期望只剩 flash", stored.Cooldowns)
	}
	if cleared, err := pool.ClearCooldownsMatching(id, forbidden); err != nil || cleared != 0 {
		t.Fatalf("没有符合条件的冷却时 cleared=%d err=%v", cleared, err)
	}
	if _, err := pool.ClearCooldownsMatching("missing@example.com", forbidden); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("账户不存在时 err = %v", err)
	}
}
