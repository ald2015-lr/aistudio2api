package app

import (
	"context"
	"errors"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 流式优先 Playground（STREAM_PLAYGROUND_MODELS）。
//
// 需要订阅权益的模型在 Build 通道只能经 ProxyUnaryCall 调用，上游生成完整段回复才一次返回，流式请求表现为最后一次性出现；
// Playground 通道（GenerateContent）逐块返回并携带权益头。列表中的模型的流式请求每次尝试先不等待地取 Playground 账号
// （acquireWarmLeaseNow：空闲的热 Worker 账号，或分区有空槽时现场冷启动的备用账号），Playground 账号全部忙碌、达到并发或
// Worker 容量上限、冷却、不支持该模型时立即按默认条件在全部启用通道中选号（照常排队，同一账号的通道里仍先 Playground），
// 不等 Playground。
// 列表为空（默认）时 Playground 与 Build 照常轮询；非流式请求、指定账号或文件的请求、只能走某个通道的请求不受影响；
// 降级判定拦截的模型沿用降级判定自己的 Playground 优先（会等待 Playground，判定最准）。

// setStreamPlaygroundModels 应用流式优先 Playground 的模型列表（启动时与服务配置页保存后，立即生效）
func (service *trackedService) setStreamPlaygroundModels(models []string) {
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		if name := normalizeGuardModel(model); name != "" {
			set[name] = struct{}{}
		}
	}
	service.streamPlaygroundModels.Store(&set)
}

// streamPlaygroundListed 判断模型（别名已解析为规范 ID）是否在流式优先 Playground 的列表里
func (service *trackedService) streamPlaygroundListed(model string) bool {
	models := service.streamPlaygroundModels.Load()
	if models == nil {
		return false
	}
	_, listed := (*models)[normalizeGuardModel(model)]
	return listed
}

// streamPrefersPlayground 判断这次尝试是否先不等待地取 Playground 账号：流式请求、模型在列表里、Playground 与 Build 都已启用
// （只启用一个通道时没有可选的通道），且选择条件没有限定通道（只走 Build、只走 Playground、降级判定的 Playground 优先）、
// 没有固定账号或文件
func (service *trackedService) streamPrefersPlayground(request aistudio.GenerateRequest, selection aistudio.AccountSelection) bool {
	if !request.Stream || selection.BuildOnly || selection.PlaygroundOnly || selection.PlaygroundFirst ||
		strings.TrimSpace(selection.AccountID) != "" || strings.TrimSpace(selection.ResourceID) != "" {
		return false
	}
	return service.streamPlaygroundListed(selection.ModelID) && service.pool.PlaygroundEnabled() && service.pool.BuildEnabled()
}

// acquireStreamPlaygroundLease 为流式优先 Playground 的选号：先不等待地取 Playground 账号；没有立即可用的 Playground 账号时写入请求进度
// 与 trace，立即按原选择条件在全部启用通道中选号（照常排队）。改为不限通道后每个账号仍先 Playground（PreferPlayground，只调整顺序）：
// 两个通道共用账号并发，排队等到的账号空闲时它的 Playground 同样空闲，不应因通道轮询轮到它的 Build；
// 不等待阶段现场启动 Worker 失败的账号不再启动，候选用尽时按启动失败原因返回
func (service *trackedService) acquireStreamPlaygroundLease(
	ctx context.Context, requestID string, selection aistudio.AccountSelection,
) (*aistudio.AccountLease, error) {
	failures := newWorkerStartupFailures()
	playground := selection
	playground.PlaygroundFirst = true
	lease, err := service.acquireWarmLeaseNow(ctx, playground, failures)
	if err == nil {
		return lease, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	message := "Playground 没有可用账号，改用其他通道"
	var busy *noImmediateLeaseError
	if errors.As(err, &busy) {
		message = "Playground 繁忙，改用其他通道"
	}
	aistudio.TraceFromContext(ctx).Note("流式优先 Playground：" + err.Error() + "，改为不限通道选号")
	service.requests.logRequestProgress(requestID, "service", "INFO", message)
	fallback := selection
	fallback.PreferPlayground = true
	return service.acquireLease(ctx, fallback, false, failures)
}

// streamPlaygroundSummary 为热更新日志里的一句话概况
func streamPlaygroundSummary(models []string) string {
	if len(models) == 0 {
		return "关闭"
	}
	return strings.Join(models, ",")
}
