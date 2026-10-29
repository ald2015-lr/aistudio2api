package aistudio

import (
	"context"
	"encoding/json"
	"time"
)

// Role 表示规范消息角色
type Role string

const (
	// RoleUser 表示用户消息
	RoleUser Role = "user"
	// RoleAssistant 表示模型消息
	RoleAssistant Role = "assistant"
	// RoleTool 表示工具结果消息
	RoleTool Role = "tool"
)

// Blob 表示内联二进制内容
type Blob struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
	// SourceGIF 为协议层把客户端内联的 GIF 首帧转成 PNG 时（Data 为转出的 PNG，见 api 层 normalizeImagePayload）留下的原 GIF，不序列化：
	// Interaction 模型把这张 PNG 与本服务层转换的 GIF 一同计入单次请求的转换额度，超出时换回原 GIF、按需要 Drive 处理
	// （见 PrepareInteractionImages）；其他模型只用 MIME 与 Data，与原先相同。只用于本次请求：存入 Responses 与 Interactions 的
	// 续接状态时去掉（见 api 层 dropSourceGIF），续接历史里的这张 PNG 按普通 PNG 处理
	SourceGIF []byte `json:"-"`
}

// ExternalMedia 表示可由 AI Studio 直接读取的外部媒体
type ExternalMedia struct {
	MIME string `json:"mime"`
	URL  string `json:"url"`
}

// FileRef 表示已上传文件引用
type FileRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	MIME string `json:"mime,omitempty"`
}

// FunctionCall 表示模型发起的函数调用
type FunctionCall struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
}

// FunctionResult 表示客户端返回的函数结果
type FunctionResult struct {
	ID      string          `json:"id"`
	Name    string          `json:"name,omitempty"`
	Content json.RawMessage `json:"content"`
}

// ExecutableCode 表示 AI Studio 内置代码执行器生成的代码
type ExecutableCode struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

// CodeExecutionResult 表示 AI Studio 内置代码执行器返回的结果
type CodeExecutionResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SearchEntryPoint 表示 Google Search 返回的搜索入口
type SearchEntryPoint struct {
	RenderedContent string `json:"rendered_content,omitempty"`
	SDKBlob         string `json:"sdk_blob,omitempty"`
}

// GroundingChunk 表示 Google 工具检索到的一个来源
type GroundingChunk struct {
	Source  string `json:"source"`
	URI     string `json:"uri,omitempty"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text,omitempty"`
	PlaceID string `json:"place_id,omitempty"`
}

// GroundingSegment 表示正文中由来源支撑的片段
type GroundingSegment struct {
	PartIndex  int    `json:"part_index,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
	Text       string `json:"text,omitempty"`
}

// GroundingSupport 表示正文片段与来源下标的关联
type GroundingSupport struct {
	Segment          GroundingSegment `json:"segment"`
	ChunkIndices     []int            `json:"chunk_indices,omitempty"`
	ConfidenceScores []float64        `json:"confidence_scores,omitempty"`
}

// GroundingMetadata 表示 Google Search、URL Context 和 Maps 的来源信息
type GroundingMetadata struct {
	SearchEntryPoint       *SearchEntryPoint  `json:"search_entry_point,omitempty"`
	Chunks                 []GroundingChunk   `json:"chunks,omitempty"`
	Supports               []GroundingSupport `json:"supports,omitempty"`
	DynamicRetrievalScore  *float64           `json:"dynamic_retrieval_score,omitempty"`
	WebSearchQueries       []string           `json:"web_search_queries,omitempty"`
	MapsWidgetContextToken string             `json:"maps_widget_context_token,omitempty"`
}

// Part 表示规范消息中的一个内容块
type Part struct {
	Text                string               `json:"text,omitempty"`
	InlineData          *Blob                `json:"inline_data,omitempty"`
	ExternalMedia       *ExternalMedia       `json:"external_media,omitempty"`
	File                *FileRef             `json:"file,omitempty"`
	FunctionCall        *FunctionCall        `json:"function_call,omitempty"`
	FunctionResult      *FunctionResult      `json:"function_result,omitempty"`
	ExecutableCode      *ExecutableCode      `json:"executable_code,omitempty"`
	CodeExecutionResult *CodeExecutionResult `json:"code_execution_result,omitempty"`
	Thought             bool                 `json:"thought,omitempty"`
	ThoughtSignature    string               `json:"thought_signature,omitempty"`
	SpeechMetadata      *SpeechMetadata      `json:"speech_metadata,omitempty"`
	// FromToolResult 标记从工具结果中取出的媒体，整理对话时会移到工具结果之后的用户消息里
	FromToolResult bool `json:"-"`
	// RestoredLink 标记从回传历史里本服务的媒体链接（含指向它的 fileData 与外部地址）恢复出来的媒体（见 api 层 restoreMediaLinks；
	// 最后一条用户消息里的不算，与客户端直接带的附件相同），RestoredText 为恢复前的链接文本（同一条消息里已有这处媒体的文本链接时为空）。
	// Interaction 模型（gemini-omni）的视频与无法转为 PNG 的图片要上传到账号的 Drive（PNG、JPEG 直接内联），需要 Drive 只是因为这类
	// 恢复媒体、号池又没有可用的 Drive 授权账号时，换回链接文本发送（与恢复之前相同），不让请求失败
	RestoredLink bool   `json:"-"`
	RestoredText string `json:"-"`
}

// SpeechMetadata 表示语音文本分段的说话人与风格
type SpeechMetadata struct {
	Speaker string `json:"speaker,omitempty"`
	Style   string `json:"style,omitempty"`
}

// Content 表示一条规范消息
type Content struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// FunctionDeclaration 表示客户端声明的函数
type FunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Strict 要求上游返回的参数严格符合 Parameters
	Strict bool `json:"strict,omitempty"`
}

// ToolConfig 表示工具启用策略
type ToolConfig struct {
	// Mode 为 auto、none、required（必须调用工具）或 validated（Gemini VALIDATED：参数按 Schema 校验）
	Mode string `json:"mode,omitempty"`
	// AllowedFunctionNames 限定可调用的函数；required 时为指定函数
	AllowedFunctionNames []string `json:"allowed_function_names,omitempty"`
	// ParallelCalls 为 false 时一次回复最多调用一个函数；nil 表示不限制
	ParallelCalls *bool `json:"parallel_calls,omitempty"`
}

// GoogleSearchTimeRange 表示 Google Search 的检索时间范围
type GoogleSearchTimeRange struct {
	StartTime time.Time `json:"start_time,omitzero"`
	EndTime   time.Time `json:"end_time,omitzero"`
}

// GoogleSearchOptions 表示 Google Search 的检索类型与时间范围
type GoogleSearchOptions struct {
	WebSearch   bool                   `json:"web_search,omitempty"`
	ImageSearch bool                   `json:"image_search,omitempty"`
	TimeRange   *GoogleSearchTimeRange `json:"time_range,omitempty"`
	// ContextSize、UserLocation、AllowedDomains、BlockedDomains 为客户端的搜索偏好，上游没有对应字段，以系统指令提示的方式传入
	ContextSize    string          `json:"context_size,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
	AllowedDomains []string        `json:"allowed_domains,omitempty"`
	BlockedDomains []string        `json:"blocked_domains,omitempty"`
}

// Tools 表示一次请求启用的工具
type Tools struct {
	Functions    []FunctionDeclaration `json:"functions,omitempty"`
	Google       []string              `json:"google,omitempty"`
	GoogleSearch *GoogleSearchOptions  `json:"google_search,omitempty"`
	ToolConfig   ToolConfig            `json:"tool_config,omitempty"`
}

// ResponseModality 表示模型输出模态
type ResponseModality string

const (
	// ResponseModalityText 表示文本输出
	ResponseModalityText ResponseModality = "TEXT"
	// ResponseModalityImage 表示图片输出
	ResponseModalityImage ResponseModality = "IMAGE"
	// ResponseModalityAudio 表示音频输出
	ResponseModalityAudio ResponseModality = "AUDIO"
)

// ImageConfig 表示图片生成参数
type ImageConfig struct {
	AspectRatio string `json:"aspect_ratio,omitempty"`
	ImageSize   string `json:"image_size,omitempty"`
}

// VideoConfig 表示 Omni 视频输出设置，对应官网 Run settings 的 Aspect ratio、Video duration、Resolution、Frame rate 与 Video task；
// 空值与 0 表示未设置，使用模型默认值（宽高比不带即官网 Auto，分辨率取模型目录的默认分辨率）
type VideoConfig struct {
	AspectRatio     string `json:"aspect_ratio,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	// FrameRate 为客户端要求的帧率（每秒帧数），只用于校验：官网只有 24fps 且不可选，请求里不发送
	FrameRate float64 `json:"frame_rate,omitempty"`
	// Task 取 Interactions API video_config.task 的取值：text_to_video、image_to_video、reference_to_video、edit、extend
	Task string `json:"task,omitempty"`
}

// SafetySetting 表示一个安全类别的拦截阈值，取 Gemini API 枚举名
type SafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// SpeakerVoiceConfig 表示多说话人的声音选择
type SpeakerVoiceConfig struct {
	Speaker   string `json:"speaker"`
	VoiceName string `json:"voice_name"`
}

// SpeechConfig 表示语音生成参数
type SpeechConfig struct {
	VoiceName string               `json:"voice_name,omitempty"`
	Speakers  []SpeakerVoiceConfig `json:"speakers,omitempty"`
	// Mode 表示多说话人朗读模式 VERBATIM 或 CONVERSATIONAL
	Mode string `json:"mode,omitempty"`
}

// TranscriptionConfig 表示音频转录参数
type TranscriptionConfig struct {
	WordTimestamps     bool     `json:"word_timestamps,omitempty"`
	SpeakerLabels      bool     `json:"speaker_labels,omitempty"`
	CustomVocabulary   []string `json:"custom_vocabulary,omitempty"`
	LanguageCodes      []string `json:"language_codes,omitempty"`
	SmartTranscription bool     `json:"smart_transcription,omitempty"`
}

// GenerationConfig 表示已验证的生成参数
type GenerationConfig struct {
	Temperature         *float64             `json:"temperature,omitempty"`
	TopP                *float64             `json:"top_p,omitempty"`
	TopK                *int                 `json:"top_k,omitempty"`
	MaxOutputTokens     *int64               `json:"max_output_tokens,omitempty"`
	StopSequences       []string             `json:"stop_sequences,omitempty"`
	ResponseMIMEType    string               `json:"response_mime_type,omitempty"`
	ResponseSchema      json.RawMessage      `json:"response_schema,omitempty"`
	ResponseModalities  []ResponseModality   `json:"response_modalities,omitempty"`
	ImageConfig         *ImageConfig         `json:"image_config,omitempty"`
	VideoConfig         *VideoConfig         `json:"video_config,omitempty"`
	SpeechConfig        *SpeechConfig        `json:"speech_config,omitempty"`
	TranscriptionConfig *TranscriptionConfig `json:"transcription_config,omitempty"`
	ReasoningEffort     string               `json:"reasoning_effort,omitempty"`
	ThinkingBudget      *int64               `json:"thinking_budget,omitempty"`
	Seed                *int64               `json:"seed,omitempty"`
	// MediaResolution 为输入媒体分辨率，取 Gemini API 枚举名（MEDIA_RESOLUTION_LOW 等）
	MediaResolution string `json:"media_resolution,omitempty"`
	// HideThinking 不返回思考正文，只保留思考签名（Anthropic thinking.type=disabled）
	HideThinking bool `json:"hide_thinking,omitempty"`
}

// GenerateRequest 表示供应商无关的生成请求
type GenerateRequest struct {
	// MinOutputTokens 为最大输出 token 的下限（服务配置），编码时生效，不超过模型上限；0 表示不调整
	MinOutputTokens int64            `json:"-"`
	ID              string           `json:"id"`
	Model           string           `json:"model"`
	System          string           `json:"system,omitempty"`
	Contents        []Content        `json:"contents"`
	Config          GenerationConfig `json:"config,omitempty"`
	Tools           Tools            `json:"tools,omitempty"`
	AccountID       string           `json:"account_id,omitempty"`
	// SafetySettings 覆盖对应类别的拦截阈值；非图片模型未列出的官网四类保持 OFF
	SafetySettings []SafetySetting `json:"safety_settings,omitempty"`
	// ImageRoute 内部标记：图像生成模型（由模型目录能力推导）
	ImageRoute bool `json:"-"`
	// Stream 为客户端是否要求流式响应：降级判定按此选择缓存方式（流式严格模式在判定前不发出任何内容）
	Stream bool `json:"-"`
	// Truncate 在输入超过模型上下文窗口时按权威计数删除最早的完整对话轮次（Responses truncation=auto）
	Truncate bool `json:"-"`
	// Notes 为协议层整理请求时留下的说明（例如 Omni 模型忽略的 stop sequences、按模型范围调整的视频时长），
	// 生成服务在选号前逐条写进该请求的管理日志
	Notes []string `json:"-"`
}

// TokenCountRequest 表示计数请求
type TokenCountRequest struct {
	Model    string    `json:"model"`
	System   string    `json:"system,omitempty"`
	Contents []Content `json:"contents"`
	Tools    Tools     `json:"tools,omitempty"`
}

// TokenCount 表示上游返回的权威计数
type TokenCount struct {
	InputTokens int64 `json:"input_tokens"`
}

// Model 表示实时模型目录中的模型
type Model struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Description       string              `json:"description,omitempty"`
	Methods           []string            `json:"methods"`
	InputTokenLimit   int64               `json:"input_token_limit,omitempty"`
	OutputTokenLimit  int64               `json:"output_token_limit,omitempty"`
	Capabilities      map[string]bool     `json:"capabilities,omitempty"`
	CapabilityOptions map[string][]string `json:"capability_options,omitempty"`
	AccessModes       []int64             `json:"access_modes,omitempty"`
	Paid              bool                `json:"paid,omitempty"`
	Channels          []string            `json:"channels,omitempty"`
	// BuildUnary 只在管理页目录中设置：模型需要订阅权益，在 Build 通道只能一次性返回整段回复（见 BuildUsesUnary）
	BuildUnary bool `json:"build_unary,omitempty"`
}

// Usage 表示一次生成的 token 用量
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens,omitempty"`
	ToolTokens          int64 `json:"tool_tokens,omitempty"`
	TotalTokens         int64 `json:"total_tokens"`
	OutputTokensMissing bool  `json:"-"`
}

// Citation 表示模型返回的来源
type Citation struct {
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	Start     int    `json:"start,omitempty"`
	End       int    `json:"end,omitempty"`
	Publisher string `json:"publisher,omitempty"`
}

// Media 表示生成的媒体产物
type Media struct {
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Name     string `json:"name,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Duration int64  `json:"duration_ms,omitempty"`
}

// TranscriptDuration 表示转录时间值
type TranscriptDuration struct {
	Seconds int64 `json:"seconds"`
	Nanos   int64 `json:"nanos,omitempty"`
}

// TranscriptTimestamp 表示转录文本的时间范围
type TranscriptTimestamp struct {
	Start TranscriptDuration `json:"start"`
	End   TranscriptDuration `json:"end"`
}

// TranscriptMetadata 表示转录正文附带的说话人和时间信息
type TranscriptMetadata struct {
	Text       string                `json:"text"`
	Speaker    string                `json:"speaker,omitempty"`
	Timestamps []TranscriptTimestamp `json:"timestamps,omitempty"`
}

// EventKind 表示规范流事件类型
type EventKind string

const (
	// EventText 表示正文增量
	EventText EventKind = "text"
	// EventReasoning 表示思考摘要增量
	EventReasoning EventKind = "reasoning"
	// EventToolCall 表示函数调用
	EventToolCall EventKind = "tool_call"
	// EventExecutableCode 表示内置代码执行器生成代码
	EventExecutableCode EventKind = "executable_code"
	// EventCodeExecutionResult 表示内置代码执行结果
	EventCodeExecutionResult EventKind = "code_execution_result"
	// EventGrounding 表示 Google 工具来源信息
	EventGrounding EventKind = "grounding"
	// EventCitation 表示来源信息
	EventCitation EventKind = "citation"
	// EventMedia 表示媒体产物
	EventMedia EventKind = "media"
	// EventThoughtSignature 表示独立思考签名
	EventThoughtSignature EventKind = "thought_signature"
	// EventUsage 表示权威用量
	EventUsage EventKind = "usage"
	// EventFinish 表示正常完成
	EventFinish EventKind = "finish"
	// EventError 表示上游错误
	EventError EventKind = "error"
)

// Event 表示协议核心输出的规范事件
type Event struct {
	Kind                EventKind            `json:"kind"`
	Text                string               `json:"text,omitempty"`
	ToolCall            *FunctionCall        `json:"tool_call,omitempty"`
	ExecutableCode      *ExecutableCode      `json:"executable_code,omitempty"`
	CodeExecutionResult *CodeExecutionResult `json:"code_execution_result,omitempty"`
	Grounding           *GroundingMetadata   `json:"grounding,omitempty"`
	Citation            *Citation            `json:"citation,omitempty"`
	Media               *Media               `json:"media,omitempty"`
	Transcript          *TranscriptMetadata  `json:"transcript,omitempty"`
	Usage               *Usage               `json:"usage,omitempty"`
	FinishReason        string               `json:"finish_reason,omitempty"`
	StopSequence        string               `json:"stop_sequence,omitempty"`
	ProviderModel       string               `json:"provider_model,omitempty"`
	ThoughtSignature    string               `json:"thought_signature,omitempty"`
	// FinishMessage 为上游对结束原因的说明（Gemini API finishMessage，如 MALFORMED_FUNCTION_CALL 的具体问题），没有时为空
	FinishMessage string `json:"finish_message,omitempty"`
	// UpstreamOutputTokens 为上游在这一块报告的累计正文 token（Playground 正文阶段每块都有；0 表示这一块没有），只供降级判定计算出字速度
	UpstreamOutputTokens int64 `json:"-"`
	// UpstreamPromptTokens 为上游在这一块报告的输入 token（0 表示这一块没有），只供降级判定校准"字数 → token"的比例
	UpstreamPromptTokens int64 `json:"-"`
	Err                  error `json:"-"`
}

// Service 定义公开协议适配器依赖的最小能力
type Service interface {
	Models(context.Context) ([]Model, error)
	CountTokens(context.Context, TokenCountRequest) (TokenCount, error)
	Generate(context.Context, GenerateRequest) (<-chan Event, error)
}
