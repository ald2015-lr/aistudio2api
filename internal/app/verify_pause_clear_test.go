package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

const (
	verifyClearFirst  = "v1@example.com"
	verifyClearSecond = "v2@example.com"
	verifyClearMulti  = "multi@example.com"
	verifyClearOrphan = "orphan@example.com"
	verifyClearReason = "AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: Your account must be verified before it can continue using the Gemini API. Verify your account at https://aistudio.google.com/verify-account."
)

// markTestCooldown 在账户的冷却作用域上写入冷却（空作用域为整个账户），模拟运行时写入的 403 冷却与暂停
func markTestCooldown(t *testing.T, pool *aistudio.AccountPool, accountID string, scope string, until time.Time, reason string) {
	t.Helper()
	if err := pool.MarkCooldownIfGeneration(accountID, scope, pool.ModelAccessGeneration(accountID), time.Now(), until, reason); err != nil {
		t.Fatal(err)
	}
}

// accountPauseReason 返回账户整户冷却（全局作用域）的原因；没有生效的整户冷却时 ok 为 false
func accountPauseReason(t *testing.T, pool *aistudio.AccountPool, accountID string) (string, bool) {
	t.Helper()
	_, reason, ok := modelCooldownUntil(t, pool, accountID, "*")
	return reason, ok
}

// verifyClearService 搭一个号池：两个需要验证账号而整户暂停的账户（第一个另有单模型 403 冷却）、一个多模型连续 403 自动暂停的账户、
// 一个登记已清空但整户暂停还在的账户（7a96176 上点过“清除记录”留下的情形）
func verifyClearService(t *testing.T) (*trackedService, *aistudio.AccountPool, *permissionDeniedRegistry, *runtimeAdmin) {
	t.Helper()
	service, pool := primaryBusyService(t, nil, verifyClearFirst, verifyClearSecond, verifyClearMulti, verifyClearOrphan)
	registry := withPermissionDenied(t, service)
	now := time.Now()
	for _, accountID := range []string{verifyClearFirst, verifyClearSecond, verifyClearOrphan} {
		markTestCooldown(t, pool, accountID, "", now.Add(verifyRequiredPause), verifyRequiredPauseReason)
	}
	markTestCooldown(t, pool, verifyClearFirst, testRuntimeModel, now.Add(6*time.Hour), forbiddenModelReasonPrefix+"The caller does not have permission")
	multiReason := fmt.Sprintf("%d 分钟内 %d 个模型连续 %d "+forbiddenPauseReasonSuffix, int(forbiddenWindow/time.Minute), 2, 3)
	markTestCooldown(t, pool, verifyClearMulti, "", now.Add(forbiddenPause), multiReason)
	registry.record(verifyClearFirst, testRuntimeModel, aistudio.ChannelPlayground, verifyClearReason, "", now)
	registry.record(verifyClearSecond, testRuntimeModel, aistudio.ChannelPlayground, verifyClearReason, "", now)
	registry.record(verifyClearMulti, "model-one", aistudio.ChannelPlayground, "The caller does not have permission", "", now)
	registry.record(verifyClearMulti, "model-two", aistudio.ChannelPlayground, "The caller does not have permission", "", now)
	admin := &runtimeAdmin{pool: pool, service: service, requests: service.requests}
	return service, pool, registry, admin
}

// TestClearPermissionDeniedLiftsVerifyPause 管理页按账户清除 403 记录时解除该账户需要验证账号的整户暂停（原先只清列表，账户从列表与
// “出问题”消失，却最长 30 天不参与调度）；账户 ID 大小写不同也能匹配；单模型 403 冷却、多模型连续 403 自动暂停与其他账户的暂停不变
func TestClearPermissionDeniedLiftsVerifyPause(t *testing.T) {
	service, pool, registry, admin := verifyClearService(t)
	ctx := context.Background()
	result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{" V1@Example.COM "}})
	if err != nil || result.Cleared != 1 || result.Lifted != 1 {
		t.Fatalf("按账户清除：result=%+v err=%v，期望清除 1、解除 1", result, err)
	}
	if _, ok := permissionDeniedEntryOf(registry, verifyClearFirst); ok {
		t.Fatal("指定账户的登记应被清除")
	}
	if reason, paused := accountPauseReason(t, pool, verifyClearFirst); paused {
		t.Fatalf("清除记录后应解除需要验证账号的整户暂停，仍有整户冷却：%s", reason)
	}
	if _, reason, cooling := modelCooldownUntil(t, pool, verifyClearFirst, testRuntimeModel); !cooling || reason == "" {
		t.Fatal("单模型 403 冷却不应解除")
	}
	if reason, paused := accountPauseReason(t, pool, verifyClearSecond); !paused || reason != verifyRequiredPauseReason {
		t.Fatalf("其他账户需要验证账号的暂停不应受影响：paused=%v reason=%q", paused, reason)
	}
	if !hasLogContaining(service.requests, "解除需要验证账号的整户暂停") {
		t.Fatalf("解除暂停时应写一行日志，日志 = %v", serviceLogMessages(service.requests))
	}

	// 多模型连续 403 的自动暂停与界面“不解除冷却”的说明一致，保持不变
	if result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{verifyClearMulti}}); err != nil ||
		result.Cleared != 1 || result.Lifted != 0 {
		t.Fatalf("清除多模型 403 的账户：result=%+v err=%v", result, err)
	}
	if reason, paused := accountPauseReason(t, pool, verifyClearMulti); !paused || reason == verifyRequiredPauseReason {
		t.Fatalf("多模型连续 403 的自动暂停不应解除：paused=%v reason=%q", paused, reason)
	}

	// 清除全部：登记里剩下的账户与登记已清空的账户，需要验证账号的暂停都解除
	if result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{All: true}); err != nil ||
		result.Cleared != 1 || result.Lifted != 2 {
		t.Fatalf("清除全部：result=%+v err=%v，期望清除 1、解除 2", result, err)
	}
	for _, accountID := range []string{verifyClearSecond, verifyClearOrphan} {
		if reason, paused := accountPauseReason(t, pool, accountID); paused {
			t.Fatalf("清除全部后 %s 需要验证账号的暂停应解除，仍有整户冷却：%s", accountID, reason)
		}
	}
	if _, paused := accountPauseReason(t, pool, verifyClearMulti); !paused {
		t.Fatal("清除全部也不解除多模型连续 403 的自动暂停")
	}
	if _, _, cooling := modelCooldownUntil(t, pool, verifyClearFirst, testRuntimeModel); !cooling {
		t.Fatal("清除全部也不解除单模型 403 冷却")
	}
}

// TestClearAllPermissionDeniedLiftsOrphanVerifyPause 登记已经清空（7a96176 上点过“清除记录”）、需要验证账号的整户暂停还在的账户：
// 再点一次“清除记录”（all）时解除；按账户清除不碰其他账户的暂停
func TestClearAllPermissionDeniedLiftsOrphanVerifyPause(t *testing.T) {
	_, pool, registry, admin := verifyClearService(t)
	ctx := context.Background()
	if result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{verifyClearSecond}}); err != nil ||
		result.Cleared != 1 || result.Lifted != 1 {
		t.Fatalf("按账户清除：result=%+v err=%v", result, err)
	}
	if _, paused := accountPauseReason(t, pool, verifyClearOrphan); !paused {
		t.Fatal("按账户清除不应解除登记表里没有的其他账户的暂停")
	}
	if _, paused := accountPauseReason(t, pool, verifyClearFirst); !paused {
		t.Fatal("按账户清除不应解除其他账户的暂停")
	}
	registry.clear(nil, true)
	if len(registry.list()) != 0 {
		t.Fatal("登记应已清空")
	}
	result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{All: true})
	if err != nil || result.Cleared != 0 || result.Lifted != 2 {
		t.Fatalf("登记已空时清除全部：result=%+v err=%v，期望清除 0、解除 2（接口据此提示解除了暂停，而不是“已清除 0 个账户”）", result, err)
	}
	for _, accountID := range []string{verifyClearFirst, verifyClearOrphan} {
		if reason, paused := accountPauseReason(t, pool, accountID); paused {
			t.Fatalf("登记已空时清除全部也应解除 %s 需要验证账号的暂停，仍有整户冷却：%s", accountID, reason)
		}
	}
	if _, paused := accountPauseReason(t, pool, verifyClearMulti); !paused {
		t.Fatal("多模型连续 403 的自动暂停不应解除")
	}
}

// TestClearPermissionDeniedLiftsOrphanVerifyPauseByAccount 登记表里没有、需要验证账号的整户暂停还在的账户（7a96176 上点过“清除记录”留下的）：
// 管理页按账户清除（403 区块与“出问题”里的“解除暂停”）同样解除，账户 ID 大小写不同也能匹配；登记清除数为 0、解除数为 1，
// 其他账户的暂停不变。原先按账户清除只解除登记表里清到的账户，这类账户在界面上只能逐个“验证”
func TestClearPermissionDeniedLiftsOrphanVerifyPauseByAccount(t *testing.T) {
	service, pool, _, admin := verifyClearService(t)
	ctx := context.Background()
	result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{" Orphan@Example.COM "}})
	if err != nil || result.Cleared != 0 || result.Lifted != 1 {
		t.Fatalf("按账户解除登记表里没有的暂停：result=%+v err=%v，期望清除 0、解除 1", result, err)
	}
	if reason, paused := accountPauseReason(t, pool, verifyClearOrphan); paused {
		t.Fatalf("登记表里没有的账户按账户清除后应解除需要验证账号的暂停，仍有整户冷却：%s", reason)
	}
	for _, accountID := range []string{verifyClearFirst, verifyClearSecond} {
		if _, paused := accountPauseReason(t, pool, accountID); !paused {
			t.Fatalf("按账户解除不应碰 %s 的暂停", accountID)
		}
	}
	if !hasLogContaining(service.requests, "解除需要验证账号的整户暂停") {
		t.Fatalf("解除暂停时应写一行日志，日志 = %v", serviceLogMessages(service.requests))
	}
	// 号池里没有的账户：不报错，什么也不解除
	if result, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{"missing@example.com"}}); err != nil ||
		result.Cleared != 0 || result.Lifted != 0 {
		t.Fatalf("号池里没有的账户：result=%+v err=%v", result, err)
	}
}

// TestLiftVerifyRequiredPausesSkipsExpired 已经到期的需要验证账号暂停不算解除：运行态里到期的冷却不会自动删除，原先按原因匹配会把它
// 计为解除、写出“账户重新参与调度”的日志并白推送一次；到期的不计数、不写日志，仍在暂停的照常解除
func TestLiftVerifyRequiredPausesSkipsExpired(t *testing.T) {
	service, pool, _, _ := verifyClearService(t)
	later := time.Now().Add(verifyRequiredPause + time.Hour)
	if lifted := service.liftVerifyRequiredPauses([]string{verifyClearOrphan}, false, later); lifted != 0 {
		t.Fatalf("到期的暂停按账户解除 lifted=%d，期望 0", lifted)
	}
	if lifted := service.liftVerifyRequiredPauses(nil, true, later); lifted != 0 {
		t.Fatalf("到期的暂停清除全部 lifted=%d，期望 0", lifted)
	}
	if hasLogContaining(service.requests, "解除需要验证账号的整户暂停") {
		t.Fatalf("到期的暂停不应写“重新参与调度”的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if _, paused := accountPauseReason(t, pool, verifyClearOrphan); !paused {
		t.Fatal("此刻仍在暂停的账户不应被按“以后的时间”解除")
	}
	if lifted := service.liftVerifyRequiredPauses([]string{verifyClearOrphan}, false, time.Now()); lifted != 1 {
		t.Fatalf("仍在暂停的账户 lifted=%d，期望 1", lifted)
	}
}
