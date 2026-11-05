package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Interaction 模型（gemini-omni）带内联图片的请求（见 aistudio/interaction_binding.go）：
//   - OMNI_IMAGE_BINDING（默认开启，保存后立即生效）：开启时 CreateInteractionStream 交给 DoProtected 的 binding 计入请求体里内联发送的图片，
//     关闭时与 a574e64 相同只含文本；每次尝试按当前设置挂在尝试的 context 上（aistudio.ContextWithInteractionImageBinding），
//     请求结构摘要的“校验值含图片=是/否/关闭”与这次尝试的 binding 一致
//   - 带图请求被上游以 HTTP 403 / 协议错误码 7 拒绝（imageDenied，见 generateWithRetry 与 interactionImagePermissionDenied）：纯文字请求
//     在同样的账户上正常，多半是请求本身（校验值与官网不一致）而不是账户没有权限。不进入单模型 403 冷却、不计入多模型 403 暂停、不登记为
//     403 无权限账户（管理页可一键删除这些账户，不能把好账户误登记进去）、不计入号池级 403 与出口被拒判定、不计入连续失败上限、不退避；
//     不固定账号的请求最多试 interactionImageDeniedLimit 个账户，每次写一行请求日志（imageDeniedNote），结束时按
//     aistudio.InteractionImageRejectedError 对外返回 400；第一次 403 后换号的选号失败（Drive 选号用尽、排队超限、全部冷却等）同样
//     按这个 400 结束并补写一行（imageDeniedEndNote），不换成号池繁忙等选号错误。中间换到的账户没有向上游发出（attemptReachedUpstream
//     为假：没有 Drive 授权、发出前已冷却、准备 WAA proof 等发出前的本地步骤失败、文件复制或附件上传失败等）不算换到了其他账户，之后的
//     选号失败、换号次数用完或到换号上限仍按这个 400 结束；换到的账户真正发到上游、以别的错误失败时不再保留这次 403。被拒的账号不在它上面
//     重建 Worker 后重放，也不回到它等 Worker 关闭后重放（它的 Worker 正好失败时同样如此）。这条与 OMNI_IMAGE_BINDING 的取值无关：开关只决定
//     校验值是否计入图片。
//     错误体解析不出协议错误码的 403（协议错误码为 0）、纯文字的 Interaction 请求与其他模型的 403 照原规则

// interactionImageDeniedLimit 为带图的 Interaction 请求被 403 拒绝时最多试的账户数：第一次 403 后换一个账户再试一次，第二次仍 403 就结束
const interactionImageDeniedLimit = 2

// setOmniImageBinding 设置 OMNI_IMAGE_BINDING（启动时与热更新时调用），返回值是否有变化。字段按“关闭”存，零值即默认开启
// （测试直接构造的服务与线上默认一致）
func (service *trackedService) setOmniImageBinding(enabled bool) bool {
	return service.omniImageBindingOff.Swap(!enabled) != !enabled
}

// omniImageBinding 返回当前的 OMNI_IMAGE_BINDING：每次尝试读取一次，管理页保存后下一次尝试立即按新值
func (service *trackedService) omniImageBinding() bool {
	return !service.omniImageBindingOff.Load()
}

// interactionImagePermissionDenied 判断带内联图片的 Interaction 请求的这次失败是否为官网的 HTTP 403 / 协议错误码 7（PERMISSION_DENIED，
// 线上带图请求被拒时就是这样）。只认错误体里解析出协议错误码 7 的：错误体解析不出状态的 403（GFE 或出口被封时的 HTML 错误页、空响应体等，
// 协议错误码为 0）与图片、校验值无关，isPermissionDenied 算它，这里不算，照原规则单模型 403 冷却、登记并计入出口被拒判定，不按带图被拒结束。
// 组织停用（AI Studio app is disabled）与 isPermissionDenied 一样排除
func interactionImagePermissionDenied(err error) bool {
	var rpcError *aistudio.RPCError
	return isPermissionDenied(err) && errors.As(err, &rpcError) && rpcError.Code == 7
}

// attemptReachedUpstream 判断一次失败的尝试是否真正把生成请求发到了上游（之后的错误是上游给的结果：5xx、额度 429、登录失效、首事件超时等）。
// 按发到上游的证据判断，不靠排除错误类型：sent 为这次尝试报告过向上游发送（RequestPhaseSendingUpstream）或已拿到上游的事件流，
// 或者错误本身是上游的 RPC 错误（*aistudio.RPCError，兼容不报告发送阶段的通道与测试替身）。没有调用 Generate（文件复制或附件上传失败、
// 账号没有 Drive 授权）、Generate 在发送前就失败（租约校验、准备 WAA proof、读取浏览器 Cookie、签名等，线上 NativeWorker 准备 proof 失败时
// 还会把 Worker 标为失败）都不算；租约账号在发出前已进入冷却（ErrAccountCoolingDown）、Worker 已更新（errAccountWorkerReplaced）或
// Worker 初始化失败（accountWorkerInitError）即使带着上面的证据也不算。带图 403 写了“换一个账户再试”之后的这类尝试不算换到了其他账户
// （见 generateWithRetry 的 imageDeniedSwitching）。err 为这次尝试本身的错误（不含删除临时 Drive 副本的错误）
func attemptReachedUpstream(generated bool, sent bool, err error) bool {
	if !generated {
		return false
	}
	var workerInit *accountWorkerInitError
	if errors.Is(err, aistudio.ErrAccountCoolingDown) || errors.Is(err, errAccountWorkerReplaced) || errors.As(err, &workerInit) {
		return false
	}
	var rpcError *aistudio.RPCError
	return sent || errors.As(err, &rpcError)
}

// imageDeniedNext 为带图 403 之后本请求的去向，写进请求日志（见 imageDeniedNote）
type imageDeniedNext int

const (
	// imageDeniedSwitch：换一个账户再试
	imageDeniedSwitch imageDeniedNext = iota
	// imageDeniedLimitReached：已试满本请求的上限（不固定账号 interactionImageDeniedLimit 个，指定账号 1 个），不再换号
	imageDeniedLimitReached
	// imageDeniedNoOther：还没试满上限，但这次之后本来也不会再换号：号池里没有其他可试的账户（换号次数已用完，例如只有 1 个启用账户）、
	// 已到每请求换号硬上限（generateSwitchCeiling）或这次失败不可重试
	imageDeniedNoOther
)

// imageDeniedNote 为带图的 Interaction 请求被 403 拒绝时写进请求日志的一行：第 denials/limit 个账户、之后的去向、当时的 OMNI_IMAGE_BINDING
// 与图片数（不含图片数据与提示词）
func imageDeniedNote(denials int, limit int, next imageDeniedNext, binding bool, images int) string {
	action := "换一个账户再试"
	switch next {
	case imageDeniedLimitReached:
		action = "不再换号，按 400 返回（可以去掉图片重试）"
	case imageDeniedNoOther:
		action = "没有其他可换的账户，不再换号，按 400 返回（可以去掉图片重试）"
	}
	return fmt.Sprintf("视频模型带图请求被官网拒绝（没有权限），可能是校验值与官网不一致；不冷却、不登记账户，第 %d/%d 个账户，%s | "+
		"OMNI_IMAGE_BINDING=%t | 图片=%d 张", denials, limit, action, binding, images)
}

// imageDeniedEndNote 为上一行写的是“换一个账户再试”、却没有换到其他真正发出请求的账户就以这个 403 结束时在循环后补写的一行：请求日志与
// 对外的 tried on N accounts 一致，denials 为被这样拒绝的账户数。换号的选号失败时（没有候选、Drive 选号用尽、主用模型排队超限、其余候选
// 全部冷却等，这些情况同样按带图被拒的 400 结束，不换成号池繁忙等选号错误，见 generateWithRetry 的 imageDeniedPending）reason 为
// 选号失败的原因，换到的账户没有向上游发出后换号次数用完或到每请求换号上限时为这一情况与那次尝试的错误，附在行尾；释放租约出错等
// 没有这类原因时为空
func imageDeniedEndNote(denials int, binding bool, images int, reason string) string {
	note := fmt.Sprintf("视频模型带图请求被官网拒绝（没有权限），没有换到其他账户，不再换号，按 400 返回（共试 %d 个账户，可以去掉图片重试） | "+
		"OMNI_IMAGE_BINDING=%t | 图片=%d 张", denials, binding, images)
	if reason = strings.TrimSpace(reason); reason != "" {
		note += " | 原因=" + reason
	}
	return note
}

// imageDeniedLimitFor 返回本请求带图 403 最多试的账户数：指定账号的请求不能换号，为 1
func imageDeniedLimitFor(switchable bool) int {
	if switchable {
		return interactionImageDeniedLimit
	}
	return 1
}

// interactionImageRejected 把带图 403 结束的请求错误包装成对外的说明（见 aistudio.InteractionImageRejectedError）
func interactionImageRejected(modelID string, denials int, images int, binding bool, cause error) error {
	return &aistudio.InteractionImageRejectedError{Model: modelID, Tried: denials, Images: images, ImageBinding: binding, Cause: cause}
}
