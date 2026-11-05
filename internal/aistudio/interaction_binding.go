package aistudio

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Interaction 带图请求的 WAA binding 与 403（用户特许的唯一例外，只针对 CreateInteractionStream 交给 DoProtected 的 binding 内容）
//
// 线上同一个 gemini-omni-1.1-flash：纯文字的多轮请求正常出结果，带内联图片的请求在多个账户上都被官网以 HTTP 403、协议错误码 7
// （The caller does not have permission）拒绝。原先 Interaction 的 binding 只含文本（规则来自上游 Mag1cFall/AIStudio2API，但上游的
// Interaction 从不内联图片，没有验证过内联图片时的 binding）；GenerateContent 的 binding 对 inline data 写 base64 原数据，文字模型带图一直正常。
// 推断官网页面对 Interaction 带图请求算 proof 时同样计入了图片，本服务没计入，proof 与内容对不上（未证实）。
// OMNI_IMAGE_BINDING（默认开启，保存后立即生效）开启时请求体里内联发送的图片按 GenerateContent 的写法计入 binding（见 encodeInteractionSteps），
// 关闭时与 a574e64 逐字节相同；Drive 文件引用（文件 ID）不计入。纯文字的 Interaction 请求、其他模型与通道的 binding 不受影响。
// 带图请求仍被 403 拒绝时 app 层不冷却、不登记账户，最多试 2 个账户，按 InteractionImageRejectedError 结束

// interactionImageBindingKey 为 OMNI_IMAGE_BINDING 在 context 里的键
type interactionImageBindingKey struct{}

// ContextWithInteractionImageBinding 挂上这次尝试的 OMNI_IMAGE_BINDING：app 层每次尝试按当前设置挂一次，同一次尝试的 binding 与请求日志
// 用同一个值
func ContextWithInteractionImageBinding(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, interactionImageBindingKey{}, enabled)
}

// InteractionImageBindingFromContext 返回 ctx 上的 OMNI_IMAGE_BINDING；没有挂时按默认开启
func InteractionImageBindingFromContext(ctx context.Context) bool {
	if enabled, ok := ctx.Value(interactionImageBindingKey{}).(bool); ok {
		return enabled
	}
	return true
}

// InteractionInlineImageCount 返回发往 Interaction 模型的内容里会按官网内联发送（Content field 2）的图片数：用户消息与模型消息里的
// 内联图片（模型消息里的移到随后的用户 step，见 encodeInteractionSteps），思考 part 不发送、不计入。GIF 要先经 PrepareInteractionImages
// 转成 PNG 才算；不能内联的图片、视频等要先传 Drive 的附件不算
func InteractionInlineImageCount(contents []Content) int {
	count := 0
	for _, content := range contents {
		if content.Role != RoleUser && content.Role != RoleAssistant {
			continue
		}
		for _, part := range content.Parts {
			if !part.Thought && InteractionInlineImage(part) {
				count++
			}
		}
	}
	return count
}

// InteractionImageRejectedError 表示带内联图片的 Interaction 请求（gemini-omni）被上游以 HTTP 403 / 协议错误码 7 拒绝，本请求不再换号：
// 纯文字请求在同样的账户上正常，多半是请求本身（带图请求的 WAA 校验值与官网不一致，见文件开头）而不是账户没有权限，所以 app 层不冷却、
// 不登记为 403 无权限账户、不计入多模型 403 暂停与出口判定，最多试 2 个账户。
// 对外按 400 FAILED_PRECONDITION 返回英文说明：写明 Omni 拒绝了带图请求、可以去掉图片重试；不用 401、403 与上游的 “does not have
// permission” 原文（new-api 等中转据此自动禁用整个渠道），也不用 5xx（中转会立即换渠道重发同一个带图请求，再耗掉 2 个账户）
type InteractionImageRejectedError struct {
	Model string
	// Tried 为本请求被这样拒绝的账户数
	Tried int
	// Images 为请求体里内联发送的图片数
	Images int
	// ImageBinding 为最后一次尝试时 OMNI_IMAGE_BINDING 的值（写管理日志，便于对照开关前后）
	ImageBinding bool
	// Cause 为最后一次上游错误（只写管理日志，不返回客户端）
	Cause error
}

func (e *InteractionImageRejectedError) Error() string {
	message := fmt.Sprintf("模型 %s 的带图请求（%d 张图片）被官网拒绝（HTTP 403，没有权限），试过 %d 个账户；可能是校验值与官网不一致"+
		"（OMNI_IMAGE_BINDING=%t），账户不冷却、不登记", strings.TrimSpace(e.Model), e.Images, e.Tried, e.ImageBinding)
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap 返回最后一次上游错误
func (e *InteractionImageRejectedError) Unwrap() error {
	return e.Cause
}

// HTTPStatus 返回对外状态码 400（理由见类型说明）
func (e *InteractionImageRejectedError) HTTPStatus() int {
	return http.StatusBadRequest
}

// RPCStatus 返回对外的 Google RPC 状态名
func (e *InteractionImageRejectedError) RPCStatus() string {
	return "FAILED_PRECONDITION"
}

// PublicMessage 返回给客户端的英文说明（不含账户信息与上游原文）
func (e *InteractionImageRejectedError) PublicMessage() string {
	model := strings.TrimSpace(e.Model)
	if model == "" {
		model = "The Omni model"
	} else {
		model = "The Omni model " + model
	}
	accounts := "1 account"
	if e.Tried != 1 {
		accounts = fmt.Sprintf("%d accounts", e.Tried)
	}
	return model + " rejected this request because it contains image input (tried on " + accounts + "). " +
		"Remove the images and send the request again; text-only requests are not affected."
}
