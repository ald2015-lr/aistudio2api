package app

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 新模型上线时立即读目录（限频版）：目录 24 小时（±10%）刷新一轮，官网上线新模型后最长约 26 小时才出现在全部账户，
// 这期间请求返回“目录中没有请求模型”。请求的模型不在号池任何账户的目录里时，挑 1–3 个权益最高（Ultra 优先）的空闲账户
// 在后台立即读一次目录（复用调度器的单账户同步：只在账户空闲时读，结果照常写缓存、排下次刷新、合并进目录）。
// 当前请求不等待、照旧按目录中没有这个模型返回：读一次目录要一两秒，拼错的模型名也会走到这里，不能让每个这样的请求都多等；
// 读到之后立即合并应用，客户端重试即可用。上游 v0.3.5 是唤醒一整轮巡检并让请求等第一个账户读完，在 1.24 万账户下不可行
const (
	// missingProbeModelInterval 为同一个模型名（不分大小写）两次触发的最短间隔
	missingProbeModelInterval = 10 * time.Minute
	// missingProbeGlobalInterval 为任意模型两次触发的最短间隔（拼错的模型名也会触发，全局限频）；同一时刻最多一轮在进行
	missingProbeGlobalInterval = time.Minute
	// missingProbeAccounts 为一轮最多读目录的账户数，missingProbeCandidates 为一轮挑出的候选数（忙碌、出口限流、疑似登录失效的跳过）
	missingProbeAccounts   = 3
	missingProbeCandidates = 8
)

// missingModelProbe 为缺少模型时读目录的限频状态，受 mu 保护；wg 为进行中的一轮（调度器退出前等它结束）
type missingModelProbe struct {
	mu        sync.Mutex
	recent    map[string]time.Time
	lastRound time.Time
	running   bool
	wg        sync.WaitGroup
}

// noteMissingModel 在选号以“目录中没有请求模型”失败后调用：只有号池里确实没有账户列出这个模型时（不是方法或能力不支持）
// 才交给调度器限频地读目录
func (service *trackedService) noteMissingModel(selection aistudio.AccountSelection, err error) {
	if service == nil || service.catalogSync == nil || !errors.Is(err, aistudio.ErrModelNotFound) {
		return
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if modelID == "" || service.pool.ModelListedIn(selection.Pool, modelID) {
		return
	}
	service.catalogSync.probeMissingModel(modelID, selection.Pool)
}

// probeMissingModel 按限频决定是否为 modelID 在后台读一轮目录，返回是否开始：同一模型名 10 分钟一次、全局 1 分钟一次、
// 同时只有一轮；上游 429 全局暂停期间不读（不记下，暂停结束后的请求还能触发）。生成服务没有运行时不读
func (syncer *catalogSyncer) probeMissingModel(modelID string, scope aistudio.PoolScope) bool {
	if syncer == nil || syncer.service == nil {
		return false
	}
	service := syncer.service
	service.lifecycleMu.Lock()
	ctx := service.dataContext
	service.lifecycleMu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	now := syncer.clock()
	syncer.mu.Lock()
	paused := now.Before(syncer.pausedUntil)
	syncer.mu.Unlock()
	if paused {
		return false
	}
	key := strings.ToLower(modelID)
	probe := &syncer.probe
	probe.mu.Lock()
	if probe.running || !probe.lastRound.IsZero() && now.Sub(probe.lastRound) < missingProbeGlobalInterval {
		probe.mu.Unlock()
		return false
	}
	if last, seen := probe.recent[key]; seen && now.Sub(last) < missingProbeModelInterval {
		probe.mu.Unlock()
		return false
	}
	if probe.recent == nil {
		probe.recent = make(map[string]time.Time)
	}
	// 过了间隔的记录不再有用：全局限频下 10 分钟内最多 10 条，表不会随拼错的模型名增长
	for name, at := range probe.recent {
		if now.Sub(at) >= missingProbeModelInterval {
			delete(probe.recent, name)
		}
	}
	probe.recent[key] = now
	probe.lastRound = now
	probe.running = true
	probe.wg.Add(1)
	probe.mu.Unlock()
	go func() {
		defer func() {
			probe.mu.Lock()
			probe.running = false
			probe.mu.Unlock()
			probe.wg.Done()
		}()
		syncer.runMissingProbe(ctx, modelID, scope)
	}()
	return true
}

// runMissingProbe 按权益从高到低读最多 missingProbeAccounts 个空闲账户的目录，读到这个模型即停并立即合并应用目录；
// 遇到上游 429（调度器随之全局暂停）即停。写一行日志：触发原因、读了哪些账户与结果
func (syncer *catalogSyncer) runMissingProbe(ctx context.Context, modelID string, scope aistudio.PoolScope) {
	service := syncer.service
	startedAt := syncer.clock()
	read, skipped := make([]string, 0, missingProbeAccounts), 0
	found := ""
	stop := ""
	var lastErr error
	for _, accountID := range service.pool.CatalogProbeCandidates(scope, missingProbeCandidates) {
		if len(read) >= missingProbeAccounts || ctx.Err() != nil {
			break
		}
		syncer.mu.Lock()
		paused := syncer.clock().Before(syncer.pausedUntil)
		syncer.mu.Unlock()
		if paused {
			stop = "上游 429 暂停"
			break
		}
		if service.authRecheck.suspected(accountID) || syncer.catalogPausedFor(accountID) || !syncer.beginProbeTask(accountID) {
			skipped++
			continue
		}
		_, busy, err := service.refreshAccountModelCatalogWith(ctx, accountID, true)
		if busy {
			skipped++
			continue
		}
		read = append(read, accountID)
		if err != nil {
			lastErr = err
			if aistudio.CatalogRateLimited(err) {
				stop = "上游 429"
				break
			}
			continue
		}
		if service.pool.ModelListedIn(scope, modelID) {
			found = accountID
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	result := "仍没有这个模型"
	if found != "" {
		result = "已读到（" + found + "），已合并进目录"
		// 立即合并应用（调度器的后台结果平时最多每 catalogApplyInterval 合并一次），客户端重试即可用
		service.modelSyncMu.Lock()
		service.applyCachedModelCatalog()
		service.modelSyncMu.Unlock()
		service.publishModelAccess()
	} else if len(read) == 0 {
		result = "没有可读的空闲账户"
	}
	if stop != "" {
		result += "，因" + stop + "停止"
	}
	line := fmt.Sprintf(
		"目录缺少请求模型，立即读取 | 模型=%s | 号池=%s | 已读=%s | 跳过=%d | 结果=%s | 耗时=%s",
		modelID, scopeText(scope), accountListText(read), skipped, result, syncer.clock().Sub(startedAt).Round(time.Millisecond),
	)
	level := "INFO"
	if lastErr != nil && found == "" {
		level = "WARN"
		line += " | 错误=" + catalogErrorSummary(lastErr)
	}
	service.requests.log("service", level, line)
}

// beginProbeTask 把账户的目录任务标为执行中（与调度器取任务相同：移出堆、计入 running），同步结束由 afterRefresh 放回；
// 任务正由调度器执行时返回 false
func (syncer *catalogSyncer) beginProbeTask(accountID string) bool {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	task := syncer.taskLocked(accountID)
	if task.running {
		return false
	}
	if task.index >= 0 {
		heap.Remove(&syncer.heaps[task.priority], task.index)
	}
	task.running = true
	syncer.running++
	return true
}

// waitProbe 等进行中的一轮缺少模型读目录结束（调度器退出前、写盘之前）
func (syncer *catalogSyncer) waitProbe() {
	syncer.probe.wg.Wait()
}

func scopeText(scope aistudio.PoolScope) string {
	if label := scope.Label(); label != "" {
		return label
	}
	return "不限"
}

func accountListText(accountIDs []string) string {
	if len(accountIDs) == 0 {
		return "-"
	}
	return strings.Join(accountIDs, ",")
}
