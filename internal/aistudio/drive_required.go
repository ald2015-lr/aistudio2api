package aistudio

import (
	"fmt"
	"net/http"
	"strings"
)

// DriveRequiredReason 为 Interaction 模型的视频输入（及无法转为 PNG 的图片）找不到有 Drive 授权的账号的原因
type DriveRequiredReason int

const (
	// DriveRequiredNoAccount 为号池里没有已知或可能有 Drive 授权的账号：候选全部记着 Drive 不可用
	DriveRequiredNoAccount DriveRequiredReason = iota
	// DriveRequiredAccount 为指定账号的请求（不能换号），这个账号没有 Drive 授权
	DriveRequiredAccount
	// DriveRequiredExhausted 为本请求换过的账号都没有 Drive 授权，换号次数已用完（号池里可能还有状态未知的账号）
	DriveRequiredExhausted
)

// DriveRequiredError 表示 Interaction 模型（gemini-omni）的视频输入（以及其他非图片文件、无法转为 PNG 的图片）需要已授权 Google Drive
// 的账号：这些附件要先上传到账号的 Drive，再以文件引用发送（官网发送视频同样先传 Drive），没有 Drive 授权的账号发不出去。PNG、JPEG 图片
// 与转成 PNG 的 GIF 按官网内联发送，不需要 Drive（见 PrepareInteractionImages）。
// 对外给出英文说明：没有可用账号与指定账号为 400 FAILED_PRECONDITION（换个请求也不会好转，要先让账号获得 Drive 授权，或把图片改成 PNG、JPEG），
// 换号次数用完为 503（号池里可能还有没试过的账号，稍后重试）
type DriveRequiredError struct {
	Reason DriveRequiredReason
	Model  string
	// Tried 为本请求试过、没有 Drive 授权的账号数（只在 DriveRequiredExhausted 时有）
	Tried int
	// Unavailable 为候选里记着 Drive 不可用的账号数（只在 DriveRequiredNoAccount 时有，写管理日志）
	Unavailable int
	// ImageHint 为请求里不能内联的图片的英文说明（见 InteractionImageReport.Hint），附在对外说明后面；只有视频等其他附件时为空
	ImageHint string
}

func (e *DriveRequiredError) Error() string {
	model := strings.TrimSpace(e.Model)
	switch e.Reason {
	case DriveRequiredAccount:
		return "模型 " + model + " 的视频输入（及无法转为 PNG 的图片）需要已授权 Google Drive 的账号，指定的账号没有 Drive 授权"
	case DriveRequiredExhausted:
		return fmt.Sprintf("模型 %s 的视频输入（及无法转为 PNG 的图片）需要已授权 Google Drive 的账号，本请求换过的 %d 个账号都没有 Drive 授权", model, e.Tried)
	default:
		return fmt.Sprintf("模型 %s 的视频输入（及无法转为 PNG 的图片）需要已授权 Google Drive 的账号，号池里没有已知或可能有 Drive 授权的账号"+
			"（%d 个候选都记着 Drive 不可用）", model, e.Unavailable)
	}
}

// HTTPStatus 返回对外状态码：换号次数用完为 503，其余为 400
func (e *DriveRequiredError) HTTPStatus() int {
	if e.Reason == DriveRequiredExhausted {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

// RPCStatus 返回对外的 Google RPC 状态名：换号次数用完为 UNAVAILABLE，其余为 FAILED_PRECONDITION
func (e *DriveRequiredError) RPCStatus() string {
	if e.Reason == DriveRequiredExhausted {
		return "UNAVAILABLE"
	}
	return "FAILED_PRECONDITION"
}

// PublicMessage 返回给客户端的英文说明（不含账号信息）；请求里有不能内联的图片时附上 ImageHint
func (e *DriveRequiredError) PublicMessage() string {
	model := strings.TrimSpace(e.Model)
	if model == "" {
		model = "this model"
	} else {
		model = "model " + model
	}
	prefix := "Video input for " + model + " (and image input that cannot be sent inline as PNG or JPEG) requires an account with Google Drive authorized"
	var message string
	switch e.Reason {
	case DriveRequiredAccount:
		message = prefix + ", and the requested account has no Google Drive authorization."
	case DriveRequiredExhausted:
		message = fmt.Sprintf("%s, and none of the %d accounts tried for this request has Google Drive authorization. Please try again later.", prefix, e.Tried)
	default:
		message = prefix + ", and no account in the pool has Google Drive authorization."
	}
	if hint := strings.TrimSpace(e.ImageHint); hint != "" {
		message += " " + hint + "."
	}
	return message
}

// interactionExternalMediaMessage 为 Interaction 模型收到外部地址的图片或视频时的英文说明（原样返回客户端）
func interactionExternalMediaMessage(index int) string {
	return fmt.Sprintf("contents[%d]: this model only accepts inline or uploaded image and video input; "+
		"external URLs are not supported, send the media as base64 data (inline data or a data: URI) instead", index)
}

// InteractionExternalMediaError 检查发往 Interaction 模型（gemini-omni）的内容里有没有外部地址的图片或视频（http(s) 图片链接、
// YouTube 链接等）：CreateInteractionStream 只接受内联的图片与已上传到账号 Drive 的文件，本服务没有带大小上限与地址安全限制的远程媒体下载，
// 不替客户端下载。有时返回参数错误（英文说明原样返回客户端），选号前按 400 返回；本服务 /media/ 的链接已在协议层恢复为内联媒体
func InteractionExternalMediaError(contents []Content) error {
	for index, content := range contents {
		for _, part := range content.Parts {
			if part.ExternalMedia != nil {
				return fmt.Errorf("%w: %s", ErrInvalidArgument, interactionExternalMediaMessage(index))
			}
		}
	}
	return nil
}

// InteractionModel 判断模型经 Interactions 生成（目录带 Interaction 配置 field 79，如 gemini-omni）：按 Playground 模型索引
// （ID 或别名）查找，不看账户是否可调度。这类模型不走 Build 通道（见 buildExcludedPlaygroundModel）；PNG、JPEG 图片按官网内联发送，
// 视频与无法转为 PNG 的图片要先上传到账号的 Drive（见 InteractionDriveMedia）
func (p *AccountPool) InteractionModel(modelID string) bool {
	if p == nil {
		return false
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	model, exists := p.playgroundModelLocked(modelID)
	return exists && model.Capabilities["interactions_api"]
}
