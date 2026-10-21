package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// pageBlockedFillError 返回 Camoufox 填写 bootstrap 提示词失败的原样错误（camoufoxnative/worker.go 的 value=%q err=%v），
// filled 为页面输入框里读回的内容
func pageBlockedFillError(filled string) error {
	var err error
	return fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v", filled, err)
}

// pageBlockedRunError 返回 Camoufox 等待官网 Run 按钮启用超时的原样错误
func pageBlockedRunError() error {
	return fmt.Errorf("等待官网 Run 按钮启用: %w", context.DeadlineExceeded)
}

// bidiClosedError 返回 Camoufox BiDi 连接断开的原样错误（camoufoxnative/bidi.go）
func bidiClosedError() error {
	return fmt.Errorf("BiDi 连接已断开: %w", errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"))
}

// pageBlockedFailure 返回在第 7 阶段页面受阻的启动错误（与 startReservedWorker 返回的一样）
func pageBlockedFailure() error {
	return &accountWorkerInitError{err: pageBlockedFillError(""), stage: 7}
}

// failAtBootstrap 在执行 WAA Bootstrap（第 7 阶段）后以填写 bootstrap 提示词失败结束
func failAtBootstrap(options camoufoxnative.Options) error {
	reportStagesUpTo(options, camoufoxnative.StartupBootstrappingWAA)
	return pageBlockedFillError("")
}

// noteOtherSuccess 记下另一个账户刚刚启动成功（号池健康）
func noteOtherSuccess(manager *accountWorkerManager, at time.Time) {
	manager.lastStartupSuccess.Store(at.UnixNano())
	manager.startupHealth.noteSuccess("healthy-other@example.com", at)
}

// startupEvidenceOf 返回账户计入页面受阻失效确认的连续次数
func startupEvidenceOf(t *testing.T, manager *accountWorkerManager, accountID string) int {
	t.Helper()
	account := workerOf(t, manager, accountID)
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.startFailEvidence
}

// failuresByID 返回失效账户列表（按账户 ID）
func failuresByID(t *testing.T, service *trackedService) map[string]api.AdminStartupFailure {
	t.Helper()
	failures, err := (&runtimeAdmin{pool: service.pool, workers: service.workers, requests: service.requests}).StartupFailures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]api.AdminStartupFailure, len(failures))
	for _, failure := range failures {
		if _, duplicate := byID[failure.ID]; duplicate {
			t.Fatalf("同一账户列了两次：%+v", failures)
		}
		byID[failure.ID] = failure
	}
	return byID
}

// TestClassifyPageBlocked 页面受阻按阶段与原样错误文本识别：阶段 7 的 Camoufox 填写提示词失败（value=%q 含转义）、Run 按钮不可用，
// 以及纯 Go 后端的填写提示词失败；阶段 <7、错误链里有 BiDi 断开等迹象、整个启动期限已到时不归为页面受阻
func TestClassifyPageBlocked(t *testing.T) {
	escaped := pageBlockedFillError("你好\n\"partial\"\t")
	if !strings.Contains(escaped.Error(), `value="你好\n\"partial\"\t" err=<nil>`) {
		t.Fatalf("原样格式 = %s", escaped)
	}
	goBackend := fmt.Errorf("填写 bootstrap 提示词失败: %w", errors.New("页面提示词状态未同步"))
	for _, test := range []struct {
		name  string
		err   error
		stage int
		want  startupFailureKind
	}{
		{"填写提示词失败（空值）", &accountWorkerInitError{err: pageBlockedFillError(""), stage: 7}, 7, startupFailurePageBlocked},
		{"填写提示词失败（转义值）", &accountWorkerInitError{err: escaped, stage: 7}, 7, startupFailurePageBlocked},
		{"填写提示词失败（页面表达式异常）", &accountWorkerInitError{err: fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v", "",
			errors.New(`页面表达式异常: {"type":"exception"}`)), stage: 7}, 7, startupFailurePageBlocked},
		{"Run 按钮不可用", &accountWorkerInitError{err: pageBlockedRunError(), stage: 7}, 7, startupFailurePageBlocked},
		{"纯 Go 后端填写提示词失败", &accountWorkerInitError{err: goBackend, stage: 7}, 7, startupFailurePageBlocked},
		{"阶段 6", &accountWorkerInitError{err: pageBlockedFillError(""), stage: 6}, 6, startupFailureAmbiguous},
		{"阶段 5 的 Run 按钮文字", &accountWorkerInitError{err: pageBlockedRunError(), stage: 5}, 5, startupFailureAmbiguous},
		{"阶段 4", &accountWorkerInitError{err: pageBlockedFillError(""), stage: 4}, 4, startupFailureEnvironment},
		{"填写时 BiDi 断开", &accountWorkerInitError{err: fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v", "", bidiClosedError()),
			stage: 7}, 7, startupFailureAmbiguous},
		{"等 Run 按钮时 BiDi 断开", &accountWorkerInitError{err: fmt.Errorf("等待官网 Run 按钮启用: %w", bidiClosedError()), stage: 7},
			7, startupFailureAmbiguous},
		{"纯 Go 后端运行时已关闭", &accountWorkerInitError{err: fmt.Errorf("填写 bootstrap 提示词失败: %w", errors.New("WAA runtime 已关闭")),
			stage: 7}, 7, startupFailureAmbiguous},
		{"整个启动期限已到", &accountWorkerInitError{err: pageBlockedRunError(), stage: 7, initExpired: true}, 7, startupFailureAmbiguous},
		{"输入框未就绪", pageNotReadyFailure(), 5, startupFailureAmbiguous},
		{"输入框未就绪（阶段 7 之前读不到）", &accountWorkerInitError{err: pageNotReadyError(), stage: 7}, 7, startupFailureAmbiguous},
	} {
		if got := classifyStartupFailure(test.err, test.stage); got != test.want {
			t.Errorf("%s：分类 = %q，期望 %q", test.name, got, test.want)
		}
	}
}

// TestPageBlockedConfirmedWhenPoolHealthy 阶段 7 页面受阻（Camoufox 原样文本），其他账户刚启动成功（号池健康）：连续 3 次后确认为失效。
// 暂停 2m、10m、30m，之后每次 30m；账户状态不变；写入 .startup-failures.json（kind=page_blocked、identity、count、message、stage、
// first_at、last_at），列为确定失效（带暂停到期时间），计入状态接口的确定失效数；确认的 WARN 只写一次
func TestPageBlockedConfirmedWhenPoolHealthy(t *testing.T) {
	const other, b = "pb-other@example.com", "pb-b@example.com"
	launch := newStartupTestLaunch()
	launch.setFailure(b, failAtBootstrap)
	service := startupTestService(t, launch, 3, other, b)
	manager := service.workers
	directory := t.TempDir()
	manager.startupFailures = newStartupFailureRegistry(directory, service.requests)
	ctx := service.lifecycle
	if _, err := manager.Worker(ctx, other, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	for index, delay := range []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute} {
		if _, err := manager.Worker(ctx, b, testRuntimeModel); err == nil {
			t.Fatal("b 应启动失败")
		}
		expectPause(t, manager, b, delay, index+1)
		if got := startupEvidenceOf(t, manager, b); got != index+1 {
			t.Fatalf("第 %d 次失败后失效确认 = %d", index+1, got)
		}
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败", "阶段=7/7", "类别=页面受阻") {
		t.Fatalf("启动失败日志的类别应为页面受阻，日志 = %v", serviceLogMessages(service.requests))
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败计入账户", "类别=页面受阻", "阶段=7/7", "连续失败=1", "暂停调度=2m0s", "失效确认=1/3") {
		t.Fatalf("应写计入账户的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if countLogs(service.requests, "WAA Worker 页面受阻已确认为失效账户", "连续失败=3", "最近错误=填写 bootstrap 提示词失败",
		"可在账户页删除，或到官网登录检查是否有需要处理的提示") != 1 {
		t.Fatalf("应写一次确认日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if status := testStatus(t, service.pool, b); status.State != aistudio.AccountReady {
		t.Fatalf("页面受阻不应改变账户状态，实际 %s", status.State)
	}
	identity, _ := service.pool.LoginIdentity(b)
	entries := manager.startupFailures.list()
	if len(entries) != 1 || entries[0].accountID != b {
		t.Fatalf("失效账户记录 = %+v", entries)
	}
	record := entries[0].record
	if record.Kind != "page_blocked" || record.Count != 3 || record.Stage != 7 || record.Identity != identity || identity == "" ||
		!strings.Contains(record.Message, `填写 bootstrap 提示词失败 value=""`) || record.FirstAt.IsZero() || record.LastAt.Before(record.FirstAt) {
		t.Fatalf("记录 = %+v", record)
	}
	manager.startupFailures.flush()
	reloaded := newStartupFailureRegistry(directory, nil).list()
	if len(reloaded) != 1 || reloaded[0].record.Kind != "page_blocked" || reloaded[0].record.Count != 3 || reloaded[0].record.Identity != identity {
		t.Fatalf("重新读取的记录 = %+v", reloaded)
	}
	entry := failuresByID(t, service)[b]
	if !entry.Confirmed || entry.Kind != "page_blocked" || entry.Count != 3 || entry.Stage != 7 || entry.Until == "" ||
		!strings.Contains(entry.Message, "填写 bootstrap 提示词失败") || entry.FirstAt == "" || entry.LastAt == "" {
		t.Fatalf("失效账户列表 = %+v", entry)
	}
	if status := manager.startupStatus(0); status.Dead != 1 || status.PageBlockedBurst {
		t.Fatalf("状态 startup = %+v", status)
	}

	// 确认之后再失败：暂停 30m，记录次数加一，不再写确认日志
	if _, err := manager.Worker(ctx, b, testRuntimeModel); err == nil {
		t.Fatal("b 应启动失败")
	}
	expectPause(t, manager, b, 30*time.Minute, 4)
	if entries := manager.startupFailures.list(); len(entries) != 1 || entries[0].record.Count != 4 {
		t.Fatalf("确认之后的失败应更新记录，记录 = %+v", entries)
	}
	if countLogs(service.requests, "WAA Worker 页面受阻已确认为失效账户") != 1 {
		t.Fatal("确认日志只写一次")
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "连续失败=4", "暂停调度=30m0s", "已确认为失效账户") {
		t.Fatalf("确认之后的计入日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestPageBlockedNotConfirmedWithoutOtherSuccess 失败前 10 分钟内没有其他账户启动成功（只有它自己成功过，或其他账户的成功已超过 10 分钟）：
// 号池不一定健康，失败照常暂停（2m、10m、30m），失效确认的连续次数不增加，不写持久表、不列为确定失效、不计入确定失效数
func TestPageBlockedNotConfirmedWithoutOtherSuccess(t *testing.T) {
	const b = "pb-alone@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, b)
	manager := service.workers
	now := time.Now()
	manager.lastStartupSuccess.Store(now.UnixNano())
	manager.startupHealth.noteSuccess(b, now)
	manager.startupHealth.noteSuccess("stale-other@example.com", now.Add(-11*time.Minute))
	manager.startupHealth.noteSuccess(b, now)
	for index, delay := range []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
		expectPause(t, manager, b, delay, index+1)
	}
	if got := startupEvidenceOf(t, manager, b); got != 0 {
		t.Fatalf("号池不健康时失效确认不应增加，实际 %d", got)
	}
	if entries := manager.startupFailures.list(); len(entries) != 0 {
		t.Fatalf("不应写入持久表，记录 = %+v", entries)
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "近 10 分钟没有其他账户启动成功，不计入失效确认", "失效确认=0/3") {
		t.Fatalf("应写明不计入失效确认，日志 = %v", serviceLogMessages(service.requests))
	}
	if hasLogContaining(service.requests, "WAA Worker 页面受阻已确认为失效账户") {
		t.Fatal("不应确认为失效")
	}
	entry := failuresByID(t, service)[b]
	if entry.Confirmed || entry.Kind != "page_blocked" || entry.Until == "" || entry.Count != 4 || !strings.Contains(entry.Reason, "0/3") {
		t.Fatalf("尚未确认的页面受阻应列为未确认，实际 %+v", entry)
	}
	if status := manager.startupStatus(0); status.Dead != 0 || status.Paused != 1 {
		t.Fatalf("状态 startup = %+v", status)
	}

	// 号池不健康的失败不清零：之后号池恢复健康，再连续 3 次健康的失败就确认
	noteOtherSuccess(manager, time.Now())
	for range startupPageBlockedConfirmCount {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
	}
	if got := startupEvidenceOf(t, manager, b); got != startupPageBlockedConfirmCount || !recorded(manager.startupFailures, b) {
		t.Fatalf("号池恢复健康后应确认，失效确认 = %d", got)
	}
}

// TestPageBlockedSystemic 近 5 分钟没有任何 Worker 启动成功（含服务刚启动）时与原因不确定一样按系统性问题只暂停 1 分钟、不计次数
func TestPageBlockedSystemic(t *testing.T) {
	const b = "pb-sys@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, b)
	manager := service.workers
	manager.startupHealth.noteSuccess("healthy-other@example.com", time.Now().Add(-6*time.Minute))
	manager.lastStartupSuccess.Store(time.Now().Add(-6 * time.Minute).UnixNano())
	for range 4 {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
		expectPause(t, manager, b, time.Minute, 0)
	}
	if got := startupEvidenceOf(t, manager, b); got != 0 || recorded(manager.startupFailures, b) {
		t.Fatalf("系统性问题不计入失效确认，实际 %d", got)
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "近 5 分钟没有任何 Worker 启动成功", "暂停调度=1m0s") {
		t.Fatalf("应写明按系统性问题处理，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestPageBlockedBurstBlocksConfirmation 近 30 分钟页面受阻的不同账户数超过 max(10, 尝试启动的账户数 × 20%)：可能是官网改版，
// 不确认任何新的页面受阻失效（已有记录不动），进入爆发时写一次 WARN，状态接口 page_blocked_burst 为真；爆发结束后照常确认
func TestPageBlockedBurstBlocksConfirmation(t *testing.T) {
	const b, c, kept = "pb-burst@example.com", "pb-burst-c@example.com", "pb-kept@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 3, b, c, kept)
	manager := service.workers
	noteOtherSuccess(manager, time.Now())
	keptIdentity, _ := service.pool.LoginIdentity(kept)
	manager.startupFailures.recordPageBlocked(kept, startupPageBlockedConfirmCount, "", "填写 bootstrap 提示词失败", 7, keptIdentity,
		time.Now(), time.Now())
	for range 2 {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
	}
	if got := startupEvidenceOf(t, manager, b); got != 2 {
		t.Fatalf("号池健康时失效确认 = %d，期望 2", got)
	}
	// 近 30 分钟尝试启动的账户 40 个（20% 为 8），另有 9 个账户页面受阻：加上 b 共 10 个，不超过 10
	now := time.Now()
	for index := range 40 {
		manager.startupHealth.noteAttempt(fmt.Sprintf("attempt-%02d@example.com", index), now)
	}
	for index := range 9 {
		if verdict := manager.startupHealth.notePageBlocked(fmt.Sprintf("attempt-%02d@example.com", index), true, now); verdict.burst {
			t.Fatalf("第 %d 个页面受阻的账户还不到爆发门槛", index+2)
		}
	}
	if manager.startupStatus(0).PageBlockedBurst {
		t.Fatal("10 个账户页面受阻不超过门槛")
	}
	// c 页面受阻：共 11 个，进入爆发，写一次 WARN；c 自己也不计入失效确认
	manager.noteStartupFailure(service.lifecycle, c, service.pool.AuthGeneration(c), time.Now(), pageBlockedFailure())
	if got := startupEvidenceOf(t, manager, c); got != 0 {
		t.Fatalf("爆发期间 c 不计入失效确认，实际 %d", got)
	}
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
	if got := startupEvidenceOf(t, manager, b); got != 2 || recorded(manager.startupFailures, b) {
		t.Fatalf("爆发期间不应确认新的页面受阻失效，失效确认 = %d", got)
	}
	expectPause(t, manager, b, 30*time.Minute, 3)
	if countLogs(service.requests, "大量账户页面受阻，可能是官网页面改版，暂不确认为失效", "账户数=11", "启动尝试账户数=40") != 1 {
		t.Fatalf("进入爆发时应写一次 WARN，日志 = %v", serviceLogMessages(service.requests))
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "大量账户页面受阻（可能是官网页面改版），不计入失效确认", "失效确认=2/3") {
		t.Fatalf("计入日志应写明爆发，日志 = %v", serviceLogMessages(service.requests))
	}
	status := manager.startupStatus(0)
	if !status.PageBlockedBurst || status.Dead != 1 {
		t.Fatalf("状态 startup = %+v，期望爆发为真、已有记录仍计入确定失效", status)
	}
	if !recorded(manager.startupFailures, kept) || !failuresByID(t, service)[kept].Confirmed {
		t.Fatal("爆发期间已有记录不动")
	}
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
	if countLogs(service.requests, "大量账户页面受阻，可能是官网页面改版") != 1 {
		t.Fatal("同一轮爆发只写一次 WARN")
	}

	// 尝试启动的账户增加到 140 个（20% 为 28）：爆发结束，b 下一次号池健康的失败计入并确认
	for index := 40; index < 140; index++ {
		manager.startupHealth.noteAttempt(fmt.Sprintf("attempt-%03d@example.com", index), time.Now())
	}
	if manager.startupStatus(0).PageBlockedBurst {
		t.Fatal("尝试启动的账户增加后不再是爆发")
	}
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
	if got := startupEvidenceOf(t, manager, b); got != startupPageBlockedConfirmCount || !recorded(manager.startupFailures, b) {
		t.Fatalf("爆发结束后应确认，失效确认 = %d", got)
	}
	if status := manager.startupStatus(0); status.Dead != 2 || status.PageBlockedBurst {
		t.Fatalf("状态 startup = %+v", status)
	}
}

// attemptedAccounts 返回近 startupBurstWindow 尝试启动的不同账户数
func attemptedAccounts(manager *accountWorkerManager) int {
	manager.startupHealth.mu.Lock()
	defer manager.startupHealth.mu.Unlock()
	return manager.startupHealth.attempts.count(time.Now(), startupBurstWindow)
}

// TestPageBlockedConfirmedAccountsDoNotCauseBurst 已确认为页面受阻失效、留在号池里等用户到浏览器里检查的账户每 30 分钟重试、
// 每次都页面受阻，不计入爆发统计（页面受阻与尝试启动的账户数都不计）：12 个这样的账户重试（另有 30 个账户近期尝试启动、其他账户刚启动成功），
// 状态接口 page_blocked_burst 仍为假、不写爆发 WARN，新的账户连续 3 次健康的页面受阻照常确认为失效
func TestPageBlockedConfirmedAccountsDoNotCauseBurst(t *testing.T) {
	const fresh = "pb-fresh@example.com"
	confirmedIDs := make([]string, 0, 12)
	for index := range 12 {
		confirmedIDs = append(confirmedIDs, fmt.Sprintf("pb-confirmed-%02d@example.com", index))
	}
	launch := newStartupTestLaunch()
	launch.setFailure(fresh, failAtBootstrap)
	launch.setFailure(confirmedIDs[0], failAtBootstrap)
	service := startupTestService(t, launch, 3, append([]string{fresh}, confirmedIDs...)...)
	manager := service.workers
	ctx := service.lifecycle
	noteOtherSuccess(manager, time.Now())
	now := time.Now()
	for index := range 30 {
		manager.startupHealth.noteAttempt(fmt.Sprintf("attempt-%02d@example.com", index), now)
	}
	for _, accountID := range confirmedIDs {
		identity, _ := service.pool.LoginIdentity(accountID)
		manager.startupFailures.recordPageBlocked(accountID, startupPageBlockedConfirmCount, "", "填写 bootstrap 提示词失败 value=\"\" err=<nil>", 7,
			identity, now.Add(-2*time.Hour), now.Add(-31*time.Minute))
	}

	// 第一个经真实的启动路径重试（不计入尝试启动的账户数），其余直接计入失败
	if _, err := manager.Worker(ctx, confirmedIDs[0], testRuntimeModel); err == nil {
		t.Fatal("已确认的账户应启动失败")
	}
	for _, accountID := range confirmedIDs[1:] {
		manager.noteStartupFailure(ctx, accountID, service.pool.AuthGeneration(accountID), time.Now(), pageBlockedFailure())
	}
	for _, accountID := range confirmedIDs {
		expectPause(t, manager, accountID, 30*time.Minute, 1)
	}
	if got := attemptedAccounts(manager); got != 30 {
		t.Fatalf("已确认的账户不计入尝试启动的账户数，实际 %d", got)
	}
	if status := manager.startupStatus(0); status.PageBlockedBurst || status.Dead != len(confirmedIDs) {
		t.Fatalf("状态 startup = %+v，期望不是爆发、确定失效 %d", status, len(confirmedIDs))
	}
	if hasLogContaining(service.requests, "大量账户页面受阻") {
		t.Fatalf("已确认的账户重试不应触发爆发，日志 = %v", serviceLogMessages(service.requests))
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "暂停调度=30m0s", "已确认为失效账户（账户状态不变，每 30 分钟重试一次）") {
		t.Fatalf("已确认的账户重试的计入日志 = %v", serviceLogMessages(service.requests))
	}

	// 新的账户照常计入失效确认（经真实的启动路径，计入尝试启动的账户数）
	for index := range startupPageBlockedConfirmCount {
		if _, err := manager.Worker(ctx, fresh, testRuntimeModel); err == nil {
			t.Fatal("fresh 应启动失败")
		}
		if got := startupEvidenceOf(t, manager, fresh); got != index+1 {
			t.Fatalf("第 %d 次失败后失效确认 = %d", index+1, got)
		}
	}
	if !recorded(manager.startupFailures, fresh) || countLogs(service.requests, "WAA Worker 页面受阻已确认为失效账户") != 1 {
		t.Fatalf("新的账户应确认为失效，日志 = %v", serviceLogMessages(service.requests))
	}
	if got := attemptedAccounts(manager); got != 31 {
		t.Fatalf("尝试启动的账户数 = %d，期望 31", got)
	}
	if status := manager.startupStatus(0); status.PageBlockedBurst || status.Dead != len(confirmedIDs)+1 {
		t.Fatalf("状态 startup = %+v", status)
	}
}

// TestPageBlockedConfirmedSystemicKeepsLongPause 已确认的账户在近 5 分钟没有任何 Worker 启动成功时（如服务刚重启、记录的暂停已过期）
// 再失败：同样不计次数、不更新记录，但仍暂停 30 分钟（每 30 分钟重试一次），行尾写已确认，不写系统性问题
func TestPageBlockedConfirmedSystemicKeepsLongPause(t *testing.T) {
	const b, other = "pb-sys-confirmed@example.com", "pb-sys-other@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, b, other)
	manager := service.workers
	identity, _ := service.pool.LoginIdentity(b)
	now := time.Now()
	manager.startupFailures.recordPageBlocked(b, 4, "", "等待官网 Run 按钮启用: context deadline exceeded", 7, identity,
		now.Add(-3*time.Hour), now.Add(-40*time.Minute))
	for range 2 {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
		expectPause(t, manager, b, 30*time.Minute, 0)
	}
	if entries := manager.startupFailures.list(); len(entries) != 1 || entries[0].record.Count != 4 {
		t.Fatalf("按系统性问题处理时不更新记录，记录 = %+v", entries)
	}
	if !hasLogContaining(service.requests, "类别=页面受阻", "连续失败=0", "暂停调度=30m0s", "已确认为失效账户（账户状态不变，每 30 分钟重试一次）") ||
		hasLogContaining(service.requests, "近 5 分钟没有任何 Worker 启动成功") {
		t.Fatalf("已确认的账户的计入日志 = %v", serviceLogMessages(service.requests))
	}
	// 没有确认的账户仍按系统性问题只暂停 1 分钟
	manager.noteStartupFailure(service.lifecycle, other, service.pool.AuthGeneration(other), time.Now(), pageBlockedFailure())
	expectPause(t, manager, other, time.Minute, 0)
}

// TestStartupRecoveryKeepsInitExpired 带租约的按需启动先被跳转到登录页、续签成功后第二次启动用满 INIT_TIMEOUT 才报 Run 按钮不可用：
// 包装第二次失败时保留 initExpired，计入账户时按原因不确定处理、不计入页面受阻的失效确认（与第二次启动自己的失败日志一致）
func TestStartupRecoveryKeepsInitExpired(t *testing.T) {
	pool, account := authTestPool(t, true)
	manager, _ := authTestManager(t, pool, account, nil)
	manager.initTimeout.Store(int64(200 * time.Millisecond))
	var launches atomic.Int32
	manager.launch = func(ctx context.Context, _ string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if launches.Add(1) == 1 {
			return nil, loginRedirectError()
		}
		reportStagesUpTo(options, camoufoxnative.StartupBootstrappingWAA)
		<-ctx.Done()
		return nil, pageBlockedRunError()
	}
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.reset = func(accountID string) error {
		counts.resets.Add(1)
		return manager.Reset(accountID)
	}
	manager.refresher = refresher
	noteOtherSuccess(manager, time.Now())
	_, leaseCtx := authTestLease(t, pool)
	ctx, cancel := context.WithTimeout(leaseCtx, 10*time.Second)
	defer cancel()
	_, err := manager.Worker(ctx, authTestAccount, "gemini-test")
	var initErr *accountWorkerInitError
	if !errors.As(err, &initErr) || !initErr.authHandled || !initErr.initExpired || initErr.stage != 7 {
		t.Fatalf("续签后的启动失败应保留 initExpired，实际 %#v（%v）", initErr, err)
	}
	if launches.Load() != 2 || counts.refreshes.Load() != 1 {
		t.Fatalf("应续签一次后重新启动: 启动=%d 续签=%d", launches.Load(), counts.refreshes.Load())
	}
	if got := classifyStartupFailure(err, 7); got != startupFailureAmbiguous {
		t.Fatalf("用满 INIT_TIMEOUT 的页面受阻文本应为原因不确定，实际 %q", got)
	}
	worker := workerOf(t, manager, authTestAccount)
	worker.mu.Lock()
	kind, evidence := worker.startFailKind, worker.startFailEvidence
	worker.mu.Unlock()
	if kind != startupFailureAmbiguous || evidence != 0 {
		t.Fatalf("计入账户的类别 = %q、失效确认 = %d", kind, evidence)
	}
	if !hasLogContaining(manager.requests, "WAA Worker 启动失败计入账户", "类别=原因不确定", "阶段=7/7") ||
		hasLogContaining(manager.requests, "WAA Worker 启动失败计入账户", "类别=页面受阻") {
		t.Fatalf("计入日志 = %v", serviceLogMessages(manager.requests))
	}
}

// TestPageBlockedCountResets 中间一次启动成功清零；认证代际变化（如重新上传、续签）后从头计；换一类原因的失败也从头计
func TestPageBlockedCountResets(t *testing.T) {
	const b = "pb-reset@example.com"
	launch := newStartupTestLaunch()
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	ctx := service.lifecycle
	noteOtherSuccess(manager, time.Now())
	fail := func(times int) {
		t.Helper()
		for range times {
			manager.noteStartupFailure(ctx, b, service.pool.AuthGeneration(b), time.Now(), pageBlockedFailure())
		}
	}

	fail(2)
	if _, err := manager.Worker(ctx, b, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if got := startupEvidenceOf(t, manager, b); got != 0 {
		t.Fatalf("启动成功后应清零，实际 %d", got)
	}
	noteOtherSuccess(manager, time.Now())
	fail(2)
	if got := startupEvidenceOf(t, manager, b); got != 2 || recorded(manager.startupFailures, b) {
		t.Fatalf("成功之后重新计数，实际 %d", got)
	}

	lease, err := service.pool.AcquireAccount(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(lease.Account().StorageState); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	fail(1)
	if got := startupEvidenceOf(t, manager, b); got != 1 || recorded(manager.startupFailures, b) {
		t.Fatalf("认证代际变化后应从头计，实际 %d", got)
	}
	expectPause(t, manager, b, 2*time.Minute, 1)

	fail(1)
	manager.noteStartupFailure(ctx, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
	fail(1)
	if got := startupEvidenceOf(t, manager, b); got != 1 || recorded(manager.startupFailures, b) {
		t.Fatalf("中间一次原因不确定的失败后应从头计，实际 %d", got)
	}
}

// TestPageBlockedReconcile 重启时按登录签名对账：页面受阻的记录保留并列为确定失效，最近一次失败后 30 分钟还没到的恢复暂停，
// 到了的让账户重试；登录签名不同的删除。恢复暂停后再失败直接暂停 30 分钟、更新记录
func TestPageBlockedReconcile(t *testing.T) {
	const recent, old, replaced = "pb-recent@example.com", "pb-old@example.com", "pb-replaced@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 3, recent, old, replaced)
	manager := service.workers
	directory := t.TempDir()
	before := newStartupFailureRegistry(directory, nil)
	now := time.Now()
	identity := func(accountID string) string {
		value, _ := service.pool.LoginIdentity(accountID)
		return value
	}
	before.recordPageBlocked(recent, 3, "", "等待官网 Run 按钮启用: context deadline exceeded", 7, identity(recent), now.Add(-time.Hour), now.Add(-5*time.Minute))
	before.recordPageBlocked(old, 4, "", "填写 bootstrap 提示词失败 value=\"\" err=<nil>", 7, identity(old), now.Add(-3*time.Hour), now.Add(-40*time.Minute))
	before.recordPageBlocked(replaced, 3, "", "填写 bootstrap 提示词失败", 7, "old-identity", now.Add(-time.Hour), now.Add(-time.Minute))
	before.flush()

	manager.startupFailures = newStartupFailureRegistry(directory, service.requests)
	manager.reconcileStartupFailures()
	if !recorded(manager.startupFailures, recent) || !recorded(manager.startupFailures, old) || recorded(manager.startupFailures, replaced) {
		t.Fatalf("对账后的记录 = %+v", manager.startupFailures.list())
	}
	expectPause(t, manager, recent, 25*time.Minute, len(startupAmbiguousPauses))
	if until, _ := startupPauseOf(t, manager, old); !until.IsZero() {
		t.Fatal("30 分钟已过的不恢复暂停，让账户重试")
	}
	if !hasLogContaining(service.requests, "失效账户记录对账", "恢复页面受阻暂停=1", "清除认证文件已更新=1") {
		t.Fatalf("应记录对账结果，日志 = %v", serviceLogMessages(service.requests))
	}
	byID := failuresByID(t, service)
	if entry := byID[recent]; !entry.Confirmed || entry.Kind != "page_blocked" || entry.Count != 3 || entry.Until == "" {
		t.Fatalf("恢复暂停的账户 = %+v", entry)
	}
	if entry := byID[old]; !entry.Confirmed || entry.Kind != "page_blocked" || entry.Count != 4 || entry.Until != "" {
		t.Fatalf("没有暂停的确认账户 = %+v", entry)
	}
	if _, exists := byID[replaced]; exists || len(byID) != 2 {
		t.Fatalf("失效账户列表 = %+v", byID)
	}
	if status := manager.startupStatus(0); status.Dead != 2 {
		t.Fatalf("确定失效数 = %d，期望 2", status.Dead)
	}
	for _, accountID := range []string{recent, old} {
		if status := testStatus(t, service.pool, accountID); status.State != aistudio.AccountReady {
			t.Fatalf("%s：对账不改变账户状态，实际 %s", accountID, status.State)
		}
	}

	// 已确认的账户重启后第一次失败（号池不健康也一样）就暂停 30 分钟；号池健康时更新记录
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	manager.noteStartupFailure(service.lifecycle, old, service.pool.AuthGeneration(old), time.Now(), pageBlockedFailure())
	expectPause(t, manager, old, 30*time.Minute, 1)
	noteOtherSuccess(manager, time.Now())
	manager.noteStartupFailure(service.lifecycle, old, service.pool.AuthGeneration(old), time.Now(), pageBlockedFailure())
	for _, entry := range manager.startupFailures.list() {
		if entry.accountID == old && entry.record.Count != 5 {
			t.Fatalf("号池健康时应更新已确认的记录，记录 = %+v", entry.record)
		}
	}
	if hasLogContaining(service.requests, "WAA Worker 页面受阻已确认为失效账户") {
		t.Fatal("重启前已确认的不再写确认日志")
	}
}

// TestPageBlockedDeleteFromList 确认的页面受阻账户在列表里列为确定失效，一键删除复用批量删除接口：账户与记录一起删除，确定失效数归零
func TestPageBlockedDeleteFromList(t *testing.T) {
	isolateDisableNotes(t)
	const dead, healthy = "pb-delete@example.com", "pb-healthy@example.com"
	admin, _, _ := deleteTestAdmin(t, dead, healthy)
	registry := newStartupFailureRegistry("", admin.requests)
	admin.workers.startupFailures = registry
	noteOtherSuccess(admin.workers, time.Now())
	for range startupPageBlockedConfirmCount {
		admin.workers.notePausedFailure(workerOf(t, admin.workers, dead), startupFailurePageBlocked, admin.pool.AuthGeneration(dead), 7,
			pageBlockedRunError(), time.Now())
	}
	failures, err := admin.StartupFailures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	confirmed := make([]string, 0)
	for _, failure := range failures {
		if failure.Confirmed {
			if failure.Kind != "page_blocked" || !strings.Contains(failure.Message, "等待官网 Run 按钮启用") {
				t.Fatalf("确定失效条目 = %+v", failure)
			}
			confirmed = append(confirmed, failure.ID)
		}
	}
	if len(confirmed) != 1 || confirmed[0] != dead {
		t.Fatalf("确定失效 = %v，列表 = %+v", confirmed, failures)
	}
	status, err := admin.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Startup.Dead != 1 {
		t.Fatalf("状态接口确定失效数 = %d，期望与列表一致为 1", status.Startup.Dead)
	}
	result, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: confirmed})
	if err != nil || result.Deleted != 1 || len(result.Failed) != 0 {
		t.Fatalf("批量删除结果 = %+v %v", result, err)
	}
	if recorded(registry, dead) {
		t.Fatal("删除账户应清除页面受阻记录")
	}
	if _, err := admin.pool.Account(dead); !errors.Is(err, aistudio.ErrAccountNotFound) {
		t.Fatalf("账户应已删除，err=%v", err)
	}
	if status, err := admin.Status(context.Background()); err != nil || status.Startup.Dead != 0 {
		t.Fatalf("删除后确定失效数 = %+v %v", status.Startup, err)
	}
}

// TestRecentAccountSet 时间窗内的不同账户数：同一账户只计一次，过期的事件从队首去掉，账户最近一次出现也过期时才不再计入
func TestRecentAccountSet(t *testing.T) {
	var set recentAccountSet
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	set.note("a", base, startupBurstWindow)
	set.note("b", base.Add(time.Minute), startupBurstWindow)
	set.note("a", base.Add(20*time.Minute), startupBurstWindow)
	if got := set.count(base.Add(20*time.Minute), startupBurstWindow); got != 2 {
		t.Fatalf("不同账户数 = %d，期望 2", got)
	}
	if got := set.count(base.Add(31*time.Minute+time.Second), startupBurstWindow); got != 1 {
		t.Fatalf("b 过期、a 最近一次未过期，账户数 = %d，期望 1", got)
	}
	if got := set.count(base.Add(51*time.Minute), startupBurstWindow); got != 0 || len(set.events) != 0 {
		t.Fatalf("全部过期后账户数 = %d、事件 %d", got, len(set.events))
	}
}

// TestStartupHealthOtherSuccess 判断其他账户的启动成功：最近一次是自己时看上一个其他账户的成功
func TestStartupHealthOtherSuccess(t *testing.T) {
	var tracker startupHealthTracker
	now := time.Now()
	tracker.noteSuccess("x", now.Add(-5*time.Minute))
	tracker.noteSuccess("b", now.Add(-time.Minute))
	tracker.noteSuccess("b", now)
	if verdict := tracker.notePageBlocked("b", true, now); !verdict.otherSuccess || !verdict.healthy() {
		t.Fatalf("x 在 10 分钟内成功过，verdict = %+v", verdict)
	}
	if verdict := tracker.notePageBlocked("x", true, now); !verdict.otherSuccess {
		t.Fatalf("b 刚成功过，verdict = %+v", verdict)
	}
	if verdict := tracker.notePageBlocked("b", true, now.Add(6*time.Minute)); verdict.otherSuccess {
		t.Fatalf("x 的成功已超过 10 分钟，verdict = %+v", verdict)
	}
}

// TestStartupHealthUncountedPageBlocked counted 为假（已确认为页面受阻失效的账户）时只返回号池的情况，不计入页面受阻的账户数
func TestStartupHealthUncountedPageBlocked(t *testing.T) {
	var tracker startupHealthTracker
	now := time.Now()
	tracker.noteSuccess("healthy", now)
	for index := range 20 {
		if verdict := tracker.notePageBlocked(fmt.Sprintf("confirmed-%02d", index), false, now); !verdict.healthy() || verdict.blockedAccounts != 0 {
			t.Fatalf("已确认的账户不计入页面受阻的账户数，verdict = %+v", verdict)
		}
	}
	if tracker.burst(now) {
		t.Fatal("只有已确认的账户页面受阻时不是爆发")
	}
	for index := range 11 {
		tracker.notePageBlocked(fmt.Sprintf("fresh-%02d", index), true, now)
	}
	if verdict := tracker.notePageBlocked("confirmed-00", false, now); !verdict.burst || verdict.blockedAccounts != 11 {
		t.Fatalf("其他账户的爆发照常返回，verdict = %+v", verdict)
	}
}
