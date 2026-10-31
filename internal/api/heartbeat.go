package api

import (
	"net/http"
	"strings"
)

// streamDataHeartbeat 返回流式请求是否使用数据型心跳（STREAM_DATA_HEARTBEAT）；没有配置读取函数时按默认开启。
// 每个流式请求开始写出时读一次，同一条流内不变。
//
// new-api 等中转逐行读取上游 SSE，只转发 data 行（空行、SSE 注释都被丢弃），而且在收到第一条 data 之前不给它自己的客户端写任何字节
// （连响应头都不写）。原先的心跳只写换行（Gemini SSE）或 ": ping"（OpenAI Chat），只能保住本服务到中转这一段；首个可转发的数据来得慢时，
// 中转后面的客户端那一段连接长时间零字节，容易被手机系统、VPN、运营商或前置代理掐断。数据型心跳写的是中转会按 data 转发、
// 不带内容的空数据块：Gemini SSE 为空候选块（geminiChunkWriter.heartbeatChunk），OpenAI Chat 为空 delta 块（chatHeartbeat）
func (config Config) streamDataHeartbeat() bool {
	if config.StreamDataHeartbeat == nil {
		return true
	}
	return config.StreamDataHeartbeat()
}

// officialSDKClientMarkers 为官方 SDK 在 x-goog-api-client 里写的库标识：google-genai（Python、JS、Go 等写 google-genai-sdk/，Python 另写
// gl-python/）、旧版 google-generativeai（genai-py/）与旧版 JS SDK（genai-js/）
var officialSDKClientMarkers = []string{"google-genai-sdk/", "genai-py/", "genai-js/", "gl-python/"}

// officialGeminiSDK 判断请求是不是官方 Gemini SDK 直连（x-goog-api-client 带官方库标识）。这类客户端不经过只转发 data 行的中转，
// 换行心跳就能保活，Gemini SSE 的心跳退回只写换行，不再多写一种无效块：google-genai Python 的 chats 对流里每一块做
// _validate_response，没有 content 的块都判为无效，1.55 及以前还把 text 为 "" 的 part（不论是不是思考）判为无效（2.x 不再这样判），
// send_message_stream 里任一块无效，这一轮就不写进精选对话历史。本服务 Gemini SSE 的结束块只带 finishReason 与 usageMetadata、没有 content，这一轮本来就会被判为无效，
// 换行回退并不能保住对话历史（根因在结束块的形状，与心跳无关）。经 new-api 的 Gemini 原生格式中转时，中转原样转发 data、默认也不透传
// x-goog-api-client，中转后面的官方 SDK 同样会收到空心跳块
func officialGeminiSDK(r *http.Request) bool {
	header := strings.ToLower(r.Header.Get("X-Goog-Api-Client"))
	for _, marker := range officialSDKClientMarkers {
		if strings.Contains(header, marker) {
			return true
		}
	}
	return false
}

// chatHeartbeat 返回 OpenAI Chat 流的心跳。数据型心跳为与本流 id、model、created 一致的空 delta 块（choices[0].delta 为空对象，
// finish_reason 为 null，与本流其他块一样在 include_usage 时带 usage:null）：官方 SDK 当作没有内容的增量，new-api 按 data 原样转发；
// 关闭时为原先的 SSE 注释 ": ping"
func chatHeartbeat(w http.ResponseWriter, data bool, id string, created int64, model string, includeUsage bool) func() error {
	if !data {
		return func() error { return writeSSEHeartbeat(w) }
	}
	return func() error {
		return writeChatChunk(w, id, created, model, map[string]any{}, nil, includeUsage)
	}
}
