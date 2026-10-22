package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestQuotaCooldownLogFields 冷却日志补记上游给出的限额值与指标（核实各档位的真实上限）：用户样本的分钟限额写明上限=12 与
// 去掉服务名前缀的指标；上游没给这些字段时不写
func TestQuotaCooldownLogFields(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	cooldown, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalMinuteQuotaError(time.Now().Add(-10*time.Second)), "", aistudio.PoolScopeNormal, true, true)
	if !ok || stateErr != nil || cooldown.Kind != aistudio.MinuteQuotaKind || !cooldown.Global {
		t.Fatalf("用户样本应按全局分钟限额冷却: %+v ok=%v err=%v", cooldown, ok, stateErr)
	}
	if !hasLogContaining(requests, "账号冷却 | 类型=分钟限额 | 范围=全局 | 恢复=", "| 上限=12 | 指标=generate_requests_global") {
		t.Fatalf("冷却日志应写明上限与指标，日志 = %v", serviceLogMessages(requests))
	}
	if !pool.AccountCoolingDown(partitionNormalA, "") {
		t.Fatal("分钟限额应冷却整个账号")
	}

	other, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalB, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Release() }()
	if _, ok, stateErr := service.applyQuotaCooldown(other, partitionNormalB, testRuntimeModel, minuteQuotaError(), "", aistudio.PoolScopeNormal, true, true); !ok || stateErr != nil {
		t.Fatalf("分钟限额应被识别为额度错误: ok=%v err=%v", ok, stateErr)
	}
	found := false
	for _, message := range serviceLogMessages(requests) {
		if !strings.Contains(message, "账号冷却 | 类型=分钟限额 | 范围="+testRuntimeModel) {
			continue
		}
		found = true
		if strings.Contains(message, "上限=") || strings.Contains(message, "指标=") {
			t.Fatalf("上游没给限额值与指标时不应写这两个字段：%q", message)
		}
	}
	if !found {
		t.Fatalf("应记录按模型的分钟限额冷却，日志 = %v", serviceLogMessages(requests))
	}
}

// TestQuotaCooldownLogSkipped 同账号取得租约更晚的请求已写回结果时，较早租约的额度 429 不写入冷却：如实记一行 INFO 写明原因，
// 不写“账号冷却”的 WARN，也不登记淘汰
func TestQuotaCooldownLogSkipped(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	// 取得租约更晚的请求先成功并写回（检查时间晚于这个租约）
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(partitionNormalA, testRuntimeModel,
		pool.ModelAccessGeneration(partitionNormalA), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cooldown, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalDailyQuotaError(), "", aistudio.PoolScopeNormal, true, true)
	if !ok || stateErr != nil || cooldown.Kind != aistudio.DailyQuotaKind {
		t.Fatalf("每日限额应被识别为额度错误: %+v ok=%v err=%v", cooldown, ok, stateErr)
	}
	if !hasLogContaining(requests, "账号冷却未写入", "类型=每日限额", "原因=同账号取得租约更晚的请求已写回结果") {
		t.Fatalf("未写入的冷却应如实记录原因，日志 = %v", serviceLogMessages(requests))
	}
	if hasLogContaining(requests, "账号冷却 | 类型=每日限额") {
		t.Fatal("冷却没有写入时不应记“账号冷却”")
	}
	if pool.AccountCoolingDown(partitionNormalA, "") {
		t.Fatal("冷却不应写入")
	}
	if got := manager.retirementCount(); got != 0 {
		t.Fatalf("冷却没有写入时不应登记淘汰，登记数 = %d", got)
	}
}
