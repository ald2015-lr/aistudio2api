package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// Transcribe 跟踪音频转录使用的账户与生成结果
func (service *trackedService) Transcribe(
	ctx context.Context,
	request aistudio.TranscriptionRequest,
) (aistudio.TranscriptionResult, error) {
	api.SetAccessLogTarget(ctx, request.Model, "")
	api.SetAccessLogGenerationConfig(ctx, request.Config)
	api.StartAccessLog(ctx)
	request.Model = service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), request.Model)
	requestCtx, cancel, err := service.dataRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		service.requests.start(ctx, aistudio.GenerateRequest{ID: request.ID, Model: request.Model}, func() {})
		service.requests.finish(request.ID, "failed", err)
		return aistudio.TranscriptionResult{}, err
	}
	// 转写经 AcquireFor 直接取生成租约（不经 acquireLease），出口令牌桶已满时的累计等待另设上限，超过按“出口限速”429 返回
	requestCtx = aistudio.WithEgressWaitLimit(requestCtx, service.egressQueueWaitFor())
	// 转写最多换 dataAttemptCeiling-1 个账号（见 quota_switch.go）
	requestCtx = aistudio.WithAttemptCeiling(requestCtx, dataAttemptCeiling)
	service.requests.start(ctx, aistudio.GenerateRequest{
		ID: request.ID, Model: request.Model, Config: request.Config,
	}, cancel)
	request.CandidateAccountIDs, err = service.transcriptionCandidates(requestCtx, request.Model)
	if err != nil {
		api.SetAccessLogError(requestCtx, err)
		service.requests.finish(request.ID, finalRequestState(err), err)
		cancel()
		return aistudio.TranscriptionResult{}, err
	}
	workerGenerations := make(map[string]uint64)
	recoveredWorkers := make(map[string]struct{})
	// resetSelection 为判断恢复时还能不能换号的选号条件（见 dataResetSwitchWait）；转写不固定账号
	modelID := strings.TrimPrefix(request.Model, "models/")
	resetSelection := aistudio.AccountSelection{
		ModelID: modelID, ModelAccessScope: modelID, Method: "generateContent", Capability: "transcription_output",
		AllowedAccountIDs: request.CandidateAccountIDs,
	}
	request.ObserveAccountFailure = func(accountID string, cause error) {
		label := accountID
		for _, status := range service.pool.Status() {
			if status.ID == accountID {
				label = status.Label
				break
			}
		}
		service.requests.log(label, "WARN", fmt.Sprintf(
			"账号切换 | 模型=%s\n原因: %s",
			strings.TrimPrefix(request.Model, "models/"), strings.TrimSpace(cause.Error()),
		))
	}
	request.RecoverWAARuntime = func(recoveryCtx context.Context, accountID string, cause error) (bool, error) {
		if !aistudio.TranscriptionGenerationFailure(cause) {
			return false, nil
		}
		workerFailed := service.workers.WorkerFailed(accountID)
		workerReplaced := errors.Is(cause, errAccountWorkerReplaced)
		if recoveryCtx.Err() != nil || !needsWAARuntimeRecovery(cause, false, workerFailed, workerReplaced) {
			return false, nil
		}
		waaRuntimeFailed := aistudio.DefinitiveWAARuntimeFailure(cause)
		expectedGeneration := workerGenerations[accountID]
		// Code 5 且还有其他候选（有进行中后台关闭的账号不算，见 dataResetSwitchWait）时只等一小会儿，等不到返回未恢复，aistudio 层换号；
		// 只剩这个账号时照旧等关闭完成（受 REQUEST_TIMEOUT 限制）。
		// Worker 浏览器失败（不是 Code 5）的错误类型不一，aistudio 层只对超时、网络错误、断流换号，其余直接失败：照旧等关闭完成后
		// 在原账号重放。浏览器退出时同账号的流随之断开，关闭很快完成；浏览器卡死时，同账号的流最多在 60 秒无进展（protectedStallLimit）后断开
		wait := workerResetWaitUnbounded
		if waaRuntimeFailed {
			if switchWait, ok := service.dataResetSwitchWait(recoveryCtx, false, resetSelection, workerGenerations, accountID); ok {
				wait = switchWait
			}
		}
		recovered, currentGeneration, _, recoveryErr := service.recoverWorkerOnce(
			recoveryCtx, nil, accountID, expectedGeneration, recoveredWorkers, true, workerFailed || waaRuntimeFailed,
			wait,
		)
		if recoveryErr != nil {
			return false, recoveryErr
		}
		if !recovered {
			return false, nil
		}
		if !currentGeneration {
			return true, nil
		}
		label := accountID
		for _, status := range service.pool.Status() {
			if status.ID == accountID {
				label = status.Label
				break
			}
		}
		if workerFailed || waaRuntimeFailed {
			service.requests.log(label, "WARN", "WAA Worker 重建 | 模型="+modelID)
		}
		service.requests.log(label, "WARN", "WAA Worker 已更新 | 模型="+modelID+" | 重放当前请求")
		return true, nil
	}
	observed := aistudio.ContextWithAccountSelectionObserver(requestCtx, func(account *aistudio.Account) {
		workerGenerations[account.ID] = service.workers.WorkerGeneration(account.ID)
		api.SetAccessLogTarget(requestCtx, request.Model, account.Config.Label)
		service.requests.markRunning(request.ID, account.ID, account.Config.Label)
	})
	transcriptions, ok := service.service.(aistudio.TranscriptionService)
	if !ok {
		err = fmt.Errorf("transcription service 不可用")
	} else {
		var result aistudio.TranscriptionResult
		result, err = transcriptions.Transcribe(observed, request)
		api.SetAccessLogFirstEvent(requestCtx, result.FirstEvent)
		api.SetAccessLogGenerationResult(
			requestCtx, &result.Usage, 0,
		)
		api.SetAccessLogTarget(requestCtx, result.ProviderModel, "")
		api.SetAccessLogFinishReason(requestCtx, result.FinishReason)
		api.SetAccessLogError(requestCtx, err)
		service.requests.finish(request.ID, finalRequestState(err), err)
		cancel()
		return result, err
	}
	api.SetAccessLogError(requestCtx, err)
	service.requests.finish(request.ID, finalRequestState(err), err)
	cancel()
	return aistudio.TranscriptionResult{}, err
}

func (service *trackedService) transcriptionCandidates(ctx context.Context, model string) ([]string, error) {
	modelID := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	selection := aistudio.AccountSelection{
		ModelID: modelID, ModelAccessScope: modelID,
		Method: "generateContent", Capability: "transcription_output",
		Pool: aistudio.PoolScopeFromContext(ctx),
	}
	groups, err := service.pool.ClassifyCandidates(ctx, selection, service.workers.WarmAccountIDs())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	service.withoutLimitedStandby(&groups)
	warmAvailable := append(append([]string(nil), groups.WarmReady...), groups.WarmAvailable...)
	candidates := service.pool.OrderCandidates(warmAvailable, modelID)
	candidates = append(candidates, service.pool.OrderCandidates(groups.StandbyReady, modelID)...)
	candidates = append(candidates, service.pool.OrderCandidates(groups.WarmBusy, modelID)...)
	candidates = append(candidates, service.pool.OrderCandidates(groups.StandbyBusy, modelID)...)
	if len(candidates) == 0 {
		// 区分原因：全部冷却按 429，账户都不可调度按 503，没有账户支持该模型才按请求本身的原因
		return nil, service.pool.NoCandidateError(selection, groups)
	}
	return candidates, nil
}

var _ aistudio.TranscriptionService = (*trackedService)(nil)
