package aistudio

import (
	"fmt"
	"net/http"
)

// DowngradePublicMessage 为因降级拒绝时返回给客户端的说明，与 Google 输入被内容策略拦截（PROHIBITED_CONTENT）时的官方措辞一致。
// 官方 Interactions API 对被拦截的输入返回 HTTP 400，message 为 "Input blocked: The model could not generate output because
// the input image violates Google's [Generative AI Prohibited Use policy](...). If you think this was an error, [send feedback](...)."、
// code 为 prohibited_content；这里的输入是文字，所以去掉了 "image"。末尾按本服务对上游拦截的写法附上 blockReason，
// 客户端按 PROHIBITED_CONTENT 识别为内容被拦截
const DowngradePublicMessage = "Input blocked: The model could not generate output because the input violates Google's " +
	"[Generative AI Prohibited Use policy](https://policies.google.com/terms/generative-ai/use-policy). " +
	"If you think this was an error, [send feedback](https://ai.google.dev/gemini-api/docs/troubleshooting). " +
	"(blockReason: PROHIBITED_CONTENT)"

// DowngradeUnavailableMessage 为按 503 返回因降级拒绝时的说明：如实说明上游换用了其他模型、回复已丢弃，可以重试
const DowngradeUnavailableMessage = "The upstream served this request with a different model than the one requested, " +
	"so the response was discarded. Please try again later."

// DowngradeDecision 为一次降级判定的依据：写进管理日志（downgrade 字段）、排查记录与诊断统计
type DowngradeDecision struct {
	// Verdict：rejected 因降级拒绝；passed 判定正常；unjudged 正文太少、无法按速度判定，放行
	Verdict string `json:"verdict"`
	// Reason：speed 正文出字速度；model 上游标明的模型；memory 近期被判定为降级的同一段对话；
	// final_speed、final_model 为上游结束后的复核；timeout 为正文缓存到最长延后时限仍未判定（放行）
	Reason string `json:"reason,omitempty"`
	// Mode：strict 流式严格模式；fast 流式快速模式；unary 非流式
	Mode string `json:"mode,omitempty"`
	// Channel 为判定时所用的上游通道
	Channel string `json:"channel,omitempty"`
	// ServedModel 为上游标明的实际模型（按模型判定时）
	ServedModel string `json:"served_model,omitempty"`
	// Speed 为判定所用的正文速度（tok/s）；EstimatedSpeed 为按字数估算的速度（Build 通道或没有累计数时）
	Speed          float64 `json:"speed,omitempty"`
	EstimatedSpeed float64 `json:"estimated_speed,omitempty"`
	// Tokens、WindowMS 为判定窗口内的正文 token 与窗口时长（从第一块正文之后算起）
	Tokens   int64   `json:"tokens,omitempty"`
	WindowMS float64 `json:"window_ms,omitempty"`
	// Estimated 为 token 数是按字数估算的
	Estimated bool `json:"estimated,omitempty"`
	// CountTokens 为是否调用了 CountTokens 精确计算，及其耗时与失败原因
	CountTokens      bool    `json:"count_tokens,omitempty"`
	CountTokensMS    float64 `json:"count_tokens_ms,omitempty"`
	CountTokensError string  `json:"count_tokens_error,omitempty"`
	// Memory 为按历史记录直接拒绝（没有发往上游）
	Memory bool `json:"memory,omitempty"`
	// HeldMS 为判定期间缓存事件的时长（放行的流式请求首个内容被延后的时间，严格模式含思考）；
	// TextHeldMS 为正文首字被延后的时间
	HeldMS     float64 `json:"held_ms,omitempty"`
	TextHeldMS float64 `json:"text_held_ms,omitempty"`
	// Capped 为严格模式到了最长延后时限还没判定，已先返回 200 并发出思考（此后判定为降级只能在流中报错）
	Capped bool `json:"capped,omitempty"`
	// Basis 为中文的判定依据
	Basis string `json:"basis"`
}

// ModelDowngradedError 表示上游把请求改由其他模型生成（例如 3.1 Pro 被换成 3.1 Flash-Lite），本服务拒绝把这类回复交给客户端。
// 返回给客户端的是各协议官方格式的 400 与 DowngradePublicMessage；这里的中文说明只写管理日志
type ModelDowngradedError struct {
	Model    string
	Decision DowngradeDecision
	// Status 为返回给客户端的 HTTP 状态码：400（默认，按内容策略拦截的格式）或 503（服务暂时不可用，可重试）
	Status int
}

func (e *ModelDowngradedError) Error() string {
	return fmt.Sprintf("因降级拒绝：上游把 %s 的请求改由其他模型生成（%s）", e.Model, e.Decision.Basis)
}

// HTTPStatus 因降级拒绝按请求错误返回 400
func (e *ModelDowngradedError) HTTPStatus() int {
	if e.Status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}
