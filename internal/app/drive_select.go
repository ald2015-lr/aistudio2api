package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Interaction 模型（gemini-omni）带需要 Drive 的内联附件（视频、其他文件、无法转为 PNG 的图片，见 aistudio.InteractionDriveMedia）的生成
// 选号（见 generateWithRetry）：这些附件要先上传到账号的 Drive 再以文件引用发送（官网发送视频同样先传 Drive），没有 Drive 授权的账号
// 发不出去。PNG、JPEG 图片（以及选号前转成 PNG 的 GIF）照官网内联发送，不需要 Drive：只有图片的请求不经这里，与纯文本请求一样选号。
// 不固定账号的选号分两段：
//   - 号池里有记着 Drive 可用、本请求还没试过的账号时，选号只在这些账号里进行（限定列表，或排除其余账号，取较短的一种，列表不随号池
//     大小增长；照常排队、受主用模型排队预算约束、可以替换空闲 Worker 或冷启动它们，忙或冷却时按现有排队与 429 语义）；在此之前先不等待地
//     取一次，取不到时最多试 driveUnknownProbeLimit 个状态未知、Worker 已就绪的账号（不冷启动、不替换 Worker：线上状态未知的账号绝大多数
//     没有 Drive，为确认一个账号没有 Drive 冷启动一个浏览器不值得）。原先可用账号一忙就按排除列表选到状态未知的账号，连续试满每请求
//     换号硬上限后按 503 结束
//   - 没有这样的账号时只在状态未知的账号里照常选号（限定列表或排除记着不可用的账号，取较短的一种）；候选里没有本请求还能试的可用或
//     未知账号时不选号：只剩记着 Drive 不可用的账号时按 400 FAILED_PRECONDITION 结束，有可用或未知账号但本请求都试过时按号池繁忙 429
//     结束（之前先回到没等到关闭的原账号，等 Worker 恢复后重放）；候选集为空（账号都需要登录、不可调度，或号池里没有支持该模型的账号）
//     时不按 Drive 状态限定，交给普通选号给出原有的 503、404 或全部冷却错误，不误报“号池里没有 Drive 授权的账号”
//
// 需要 Drive 只是因为从本服务媒体链接恢复出来的历史媒体时（aistudio.Part.RestoredLink，客户端没有自己带需要 Drive 的附件、也没有指定
// 视频任务），号池里没有可用的 Drive 授权账号时把这些恢复媒体换回链接文本、按普通请求发送（见 restoredMediaAsLinks），与恢复之前相同，
// 不让原先能成功的多轮对话返回 400 或 503。恢复出来的 PNG、JPEG 图片直接内联（GIF 转成 PNG），不算需要 Drive。按需要 Drive 的
// 恢复媒体分两种：
//   - 有视频（上一轮生成的视频，“把它改成夜景”这类编辑或延长要让模型看到原视频）：与客户端附件一样选号（照常排队、冷启动与替换 Worker，
//     受主用模型排队预算约束，排队超过预算时按 429 返回），只在候选里没有本请求还能试的可用或未知账号、记着可用的账号都在长时间冷却
//     （排队等不到，见 generateWithRetry）、换满每请求换号硬上限、指定账号没有授权时换回链接文本。原先与只有图片时一样只等 10 秒、
//     不冷启动，有 Drive 的账号在跑一两分钟的生成时多半等不到，模型看不到原视频
//   - 只有无法转为 PNG 的图片（如 WebP，多半只是上文，例如出图后追问“谢谢，再来一个”）：不值得让请求久等，只用已启动的 Worker
//     （不冷启动、不替换；记着可用的账号都没有热 Worker 时为其中一个发起后台扩容、不等它），在记着可用的账号上最多排队
//     restoredDriveQueueWait，状态未知的账号只试一个 Worker 已就绪的；取不到同样换回链接文本，也不挤在少数有 Drive 的账号上排队
//
// 纯文本请求、只有图片的 Omni 请求、其他模型、固定账号与限定资源的选号不经这里

// driveMediaLabel 为请求日志里需要 Drive 的 Omni 附件的称呼
const driveMediaLabel = "Omni 的视频（及无法转为 PNG 的图片）"

const (
	// driveUnknownProbeLimit 为一个请求最多试的状态未知账号数：记着 Drive 可用的账号都取不到时试；只因恢复出来的无法转为 PNG 的
	// 历史图片需要 Drive 时没有可用账号也只试这么多个，之后换回链接文本
	driveUnknownProbeLimit = 1
	// driveProbeCandidates 为试状态未知账号时限定列表的上限（只取 Worker 已就绪的）
	driveProbeCandidates = 64
	// driveBusyRetry 为有 Drive 授权的账号都在忙、本请求不能再试时 429 的建议重试间隔
	driveBusyRetry = 10 * time.Second
	// driveBusyReason 为这种 429 的原因（写进管理日志）
	driveBusyReason = "有 Drive 授权的账号都在忙或本请求已试过"
	// restoredDriveQueueMin 为只因恢复出来的历史图片需要 Drive 时排队至少要有的时间：主用模型的排队预算剩余太少时只取立即可用的账号
	restoredDriveQueueMin = 500 * time.Millisecond
)

// restoredDriveQueueWait 为只因恢复出来的历史图片需要 Drive 时，在记着 Drive 可用的账号上最多排队的时间（主用模型另不超过排队预算
// 剩余的一半，见 restoredQueueWait），等不到就换回链接文本按普通请求选号。只给包内测试改短
var restoredDriveQueueWait = 10 * time.Second

// restoredQueueWait 返回只因恢复出来的历史图片需要 Drive 时这次最多排队的时间；不到 restoredDriveQueueMin 时为 0（只取立即可用的账号）
func restoredQueueWait(budget *leaseWaitBudget) time.Duration {
	wait := restoredDriveQueueWait
	if budget != nil {
		wait = min(wait, budget.remaining()/2)
	}
	if wait < restoredDriveQueueMin {
		return 0
	}
	return wait
}

// acquireRestoredDriveLease 为只因恢复出来的历史图片需要 Drive 时在记着 Drive 可用的账号里选号（selection 已限定到这些账号）：
// 只用已启动的 Worker（withoutColdStart：不冷启动、不替换，没有热 Worker 时为其中一个发起后台扩容后立即返回），最多排队
// restoredQueueWait，为 0 或 immediate 时不等待。限时内取不到时返回 *noImmediateLeaseError，调用方换回链接文本
func (service *trackedService) acquireRestoredDriveLease(
	ctx context.Context, selection aistudio.AccountSelection, budget *leaseWaitBudget, immediate bool, failures *workerStartupFailures,
) (*aistudio.AccountLease, error) {
	ctx = withoutColdStart(ctx)
	wait := restoredQueueWait(budget)
	if immediate || wait == 0 {
		return service.acquireWarmLeaseNow(ctx, selection, failures)
	}
	queueCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	lease, err := service.acquireLease(queueCtx, selection, false, failures)
	if err != nil && ctx.Err() == nil && queueCtx.Err() != nil {
		err = &noImmediateLeaseError{reason: fmt.Sprintf("记着 Drive 可用的账号 %s 内没有空出", wait)}
	}
	return lease, err
}

// driveSelection 为一个生成请求的 Drive 选号状态
type driveSelection struct {
	source driveCandidateSource
	// restoredOnly：请求里需要 Drive 的内联附件全部是从本服务媒体链接恢复出来的（aistudio.Part.RestoredLink），也没有指定视频任务：
	// 号池里没有可用的 Drive 授权账号时可以换回链接文本（见 fallBackToLinks）。指定了视频任务时恢复的媒体就是任务的输入
	// （edit、extend 的视频，image_to_video 的图片），换回链接文本后任务缺少输入或按多轮返回 400，与客户端附件一样处理
	restoredOnly bool
	// lenient：restoredOnly 且需要 Drive 的恢复媒体里没有视频（只有无法转为 PNG 的图片），选号只用已启动的 Worker、限时排队（见文件开头）
	lenient bool
	// imageHint 为请求里不能内联的图片的英文说明（aistudio.InteractionImageReport.Hint），附在需要 Drive 授权的错误后面
	imageHint string
	// probes 为本请求试过的状态未知账号数（见 driveUnknownProbeLimit）
	probes int
	// skipAvailable：只在记着 Drive 可用的账号里选号时没有候选（账号刚被停用、需要登录或启动失败），之后只在状态未知的账号里选
	skipAvailable bool
	// last 为最近一次查到的候选 Drive 状态
	last aistudio.DriveCandidates
}

// drivePlan 为一次选号的 Drive 安排
type drivePlan struct {
	// exhausted：候选里没有本请求还能试的可用或未知账号
	exhausted bool
	// available：选择条件已限定到记着 Drive 可用、本请求没试过的账号
	available bool
	// fallback：只因恢复出来的历史图片需要 Drive（lenient），又没有记着可用的账号、也没有能试的状态未知账号：换回链接文本按普通请求选号
	fallback bool
	// selection 为这次选号的条件
	selection aistudio.AccountSelection
	// probe 为先不等待地试的状态未知、Worker 已就绪的账号（可为空），probeSelection 为对应的选择条件
	probe          []string
	probeSelection aistudio.AccountSelection
}

// newDriveSelection 返回请求的 Drive 选号状态；source 为 nil 时（生成服务不报告 Drive 状态）不按 Drive 状态选号，
// 只在发送前发现附件仍是 inline data 时换号。contents 为图片整理之后的内容，只看需要 Drive 的内联附件（aistudio.InteractionDriveMedia）；
// video 为请求的视频设置（带任务时恢复的媒体不换回链接文本）
func newDriveSelection(service aistudio.Service, contents []aistudio.Content, video *aistudio.VideoConfig) *driveSelection {
	source, _ := service.(driveCandidateSource)
	restored, total, restoredVideo := 0, 0, false
	for _, content := range contents {
		for _, part := range content.Parts {
			if !aistudio.InteractionDriveMedia(part) {
				continue
			}
			total++
			if part.RestoredLink {
				restored++
				restoredVideo = restoredVideo || strings.HasPrefix(strings.ToLower(part.InlineData.MIME), "video/")
			}
		}
	}
	restoredOnly := total > 0 && restored == total && (video == nil || aistudio.VideoTaskName(video.Task) == "")
	return &driveSelection{source: source, restoredOnly: restoredOnly, lenient: restoredOnly && !restoredVideo}
}

// plan 按候选的 Drive 状态安排这次选号（规则见文件开头）。attempted 为本请求已尝试的账号，warmReady 为 Worker 已就绪的账号
func (drive *driveSelection) plan(
	ctx context.Context, selection aistudio.AccountSelection, attempted map[string]struct{}, warmReady func() []string,
) drivePlan {
	candidates := drive.source.DriveCandidates(ctx, selection)
	drive.last = candidates
	var allowed map[string]struct{}
	if selection.AllowedAccountIDs != nil {
		allowed = make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, accountID := range selection.AllowedAccountIDs {
			allowed[accountID] = struct{}{}
		}
	}
	usable := func(accountIDs []string) []string {
		result := make([]string, 0, min(len(accountIDs), 64))
		for _, accountID := range accountIDs {
			if _, done := attempted[accountID]; done {
				continue
			}
			if _, ok := allowed[accountID]; allowed != nil && !ok {
				continue
			}
			result = append(result, accountID)
		}
		return result
	}
	plan := drivePlan{selection: selection}
	if len(candidates.Available)+len(candidates.Unavailable)+len(candidates.Unknown) == 0 {
		// 候选集为空（账号都需要登录、不可调度或停用，或号池里没有支持该模型的账号）：不按 Drive 状态限定，普通选号给出原有的 503、404
		// 或全部冷却错误，与纯文本请求相同（原先判为号池里没有 Drive 授权的账号、返回 400，中转按客户端错误处理、号池恢复后也不重试）
		return plan
	}
	available := usable(candidates.Available)
	if drive.skipAvailable {
		available = nil
	}
	unknown := usable(candidates.Unknown)
	if len(available) == 0 && len(unknown) == 0 {
		return drivePlan{exhausted: true}
	}
	// 先试的状态未知账号：只取 Worker 已就绪的（候选按账号 ID 排序，见 aistudio.DriveCandidates）。有记着可用的账号时作为它们都取不到时的
	// 补充；只因恢复出来的无法转为 PNG 的历史图片需要 Drive 时没有可用账号也只试这些
	if drive.probes < driveUnknownProbeLimit && len(unknown) > 0 && (len(available) > 0 || drive.lenient) {
		for _, accountID := range warmReady() {
			if _, found := slices.BinarySearch(unknown, accountID); found {
				plan.probe = append(plan.probe, accountID)
				if len(plan.probe) >= driveProbeCandidates {
					break
				}
			}
		}
		if len(plan.probe) > 0 {
			plan.probeSelection = selection
			plan.probeSelection.AllowedAccountIDs = plan.probe
		}
	}
	if len(available) > 0 {
		plan.available = true
		// 限定列表与排除列表取较短的一种：排除列表为本请求已排除的账号加上记着不可用与状态未知的账号（不进等待队列的键，与同模型的请求
		// 同队排队）；可用账号多于其余账号时用它，免得几千个账号的限定列表在每次复查候选时建表
		if allowed != nil || len(available) <= len(candidates.Unavailable)+len(candidates.Unknown) {
			plan.selection.AllowedAccountIDs = available
		} else {
			plan.selection.ExcludedAccountIDs = slices.Concat(selection.ExcludedAccountIDs, candidates.Unavailable, candidates.Unknown)
		}
		return plan
	}
	if drive.lenient {
		plan.fallback = len(plan.probe) == 0
		return plan
	}
	// 只剩状态未知的账号：限定列表与排除列表取较短的一种（排除列表要带上记着不可用的账号，以及不再试的可用账号）
	excluded := len(candidates.Unavailable)
	if drive.skipAvailable {
		excluded += len(candidates.Available)
	}
	if allowed != nil || len(unknown) <= excluded {
		plan.selection.AllowedAccountIDs = unknown
		return plan
	}
	plan.selection.ExcludedAccountIDs = slices.Concat(selection.ExcludedAccountIDs, candidates.Unavailable)
	if drive.skipAvailable {
		plan.selection.ExcludedAccountIDs = append(plan.selection.ExcludedAccountIDs, candidates.Available...)
	}
	return plan
}

// exhaustedError 返回候选里没有本请求还能试的账号时的错误：还有记着 Drive 可用或状态未知的账号（本请求都试过或这次选不到）时
// 按号池繁忙返回 429；只剩记着 Drive 不可用的账号时按号池里没有 Drive 授权的账号返回 400 FAILED_PRECONDITION
func (drive *driveSelection) exhaustedError(modelID string, scope aistudio.PoolScope) error {
	if drive.noDriveAccount() {
		return &aistudio.DriveRequiredError{
			Reason: aistudio.DriveRequiredNoAccount, Model: modelID, Unavailable: len(drive.last.Unavailable), ImageHint: drive.imageHint,
		}
	}
	return drive.busyError(modelID, scope)
}

// noDriveAccount 判断最近一次查到的候选里只有记着 Drive 不可用的账号（没有记着可用、也没有状态未知的）
func (drive *driveSelection) noDriveAccount() bool {
	return len(drive.last.Available) == 0 && len(drive.last.Unknown) == 0 && len(drive.last.Unavailable) > 0
}

// busyError 为有 Drive 授权的账号都在忙或本请求已试过、不能再换号时的 429（Retry-After 为 driveBusyRetry）
func (drive *driveSelection) busyError(modelID string, scope aistudio.PoolScope) error {
	return &aistudio.CapacityBusyError{ModelID: modelID, Pool: scope, Reason: driveBusyReason, Until: time.Now().Add(driveBusyRetry)}
}

// availableUnusable 判断只在记着 Drive 可用的账号里选号失败是因为这些账号现在用不了（没有符合条件的账号：刚被停用、需要登录、
// 目录变了；现场启动失败），而不是忙碌、冷却或排队超限：之后改在状态未知的账号里选
func availableUnusable(err error, failures *workerStartupFailures) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, aistudio.ErrNoEligibleAccount) {
		return true
	}
	return failures != nil && failures.last != nil && errors.Is(err, failures.last)
}

// restoredMediaAsLinks 把从本服务媒体链接恢复出来、需要 Drive 的内联媒体（aistudio.Part.RestoredLink 且 aistudio.InteractionDriveMedia：
// 视频与无法转为 PNG 的图片；能内联的图片不动）换回恢复前的链接文本，与恢复时拆开的前后文本合并回一段（签名留在第一段上，同恢复时）；
// 没有链接文本（同一条消息里已有这处媒体的文本链接）时只留签名。返回新的内容与换回的个数，不改动 contents
func restoredMediaAsLinks(contents []aistudio.Content) ([]aistudio.Content, int) {
	result := make([]aistudio.Content, len(contents))
	replaced := 0
	plainText := func(part aistudio.Part) bool {
		return part.Text != "" && !part.Thought && part.SpeechMetadata == nil && part.InlineData == nil && part.File == nil &&
			part.ExternalMedia == nil && part.FunctionCall == nil && part.FunctionResult == nil && part.ExecutableCode == nil &&
			part.CodeExecutionResult == nil
	}
	for index, content := range contents {
		result[index] = content
		replaceable := func(part aistudio.Part) bool { return part.RestoredLink && aistudio.InteractionDriveMedia(part) }
		if !slices.ContainsFunc(content.Parts, replaceable) {
			continue
		}
		parts := make([]aistudio.Part, 0, len(content.Parts))
		// previousPlain 为上一个 part 是普通文本，previousLink 为它以换回的链接文本结尾（随后不带签名的普通文本接上去）
		previousPlain, previousLink := false, false
		for _, part := range content.Parts {
			switch {
			case replaceable(part):
				replaced++
				switch {
				case part.RestoredText == "" && part.ThoughtSignature != "":
					parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
					previousPlain, previousLink = false, false
				case part.RestoredText == "":
				case previousPlain && part.ThoughtSignature == "":
					parts[len(parts)-1].Text += part.RestoredText
					previousLink = true
				default:
					parts = append(parts, aistudio.Part{Text: part.RestoredText, ThoughtSignature: part.ThoughtSignature})
					previousPlain, previousLink = true, true
				}
			case previousLink && plainText(part) && part.ThoughtSignature == "":
				parts[len(parts)-1].Text += part.Text
				previousLink = false
			default:
				parts = append(parts, part)
				previousPlain, previousLink = plainText(part), false
			}
		}
		result[index].Parts = parts
	}
	return result, replaced
}

// restoredFallbackNote 为换回链接文本时写进请求进度的说明
func restoredFallbackNote(replaced int, why string) string {
	return fmt.Sprintf("%s，回传历史里恢复的 %d 个媒体按链接文本发送（Omni 的视频及无法转为 PNG 的图片需要 Drive 授权）", why, replaced)
}
