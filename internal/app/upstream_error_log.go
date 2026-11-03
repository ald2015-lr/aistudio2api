package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 上游 4xx 证据：上游返回的 4xx（400 参数错误、401、403、404、429 等，不只额度 429）原先只有错误串，400 时管理日志里只剩一句
// “Request contains an invalid argument.”，状态 details 里可能有的 BadRequest 字段违规等全部丢掉，Omni 图片 400 一直没有现场证据。
// 每次尝试（发送前的文件复制、附件上传与生成本身）以上游 4xx 失败、或已输出内容后流内收到上游 4xx 时，在该请求的管理日志记一行 WARN：
// RPC 方法、HTTP 状态、协议错误码、上游文案（去掉内部标记、压成一行，见 aistudio.CleanUpstreamMessage）、details 的全部类型与原文
// （压缩后最多 1200 字节，与“额度 429 详情未识别”相同），错误响应体不是状态数组时另附响应体开头。只写上游返回的内容，不写请求内容；
// 删除临时 Drive 副本的错误不算（不是这次尝试本身的错误）

// upstreamErrorMessageLimit 为证据行里上游文案的最大字数
const upstreamErrorMessageLimit = 500

// upstreamClientErrorLine 返回错误链里上游 4xx（aistudio.RPCError，状态 400–499）的证据行；错误链里没有上游 4xx 时 ok 为 false
func upstreamClientErrorLine(err error) (string, bool) {
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode < 400 || rpcError.StatusCode >= 500 {
		return "", false
	}
	fields := []string{"上游返回 4xx", "方法=" + rpcError.Method, fmt.Sprintf("HTTP=%d", rpcError.StatusCode)}
	if rpcError.Code != 0 {
		fields = append(fields, fmt.Sprintf("协议错误码=%d", rpcError.Code))
	}
	message := strings.Join(strings.Fields(aistudio.CleanUpstreamMessage(rpcError.Message)), " ")
	if runes := []rune(message); len(runes) > upstreamErrorMessageLimit {
		message = string(runes[:upstreamErrorMessageLimit]) + "…"
	}
	fields = append(fields, "文案="+message)
	if len(rpcError.DetailTypes) > 0 {
		fields = append(fields, "详情类型="+strings.Join(rpcError.DetailTypes, ","))
	}
	if rpcError.Details != "" {
		fields = append(fields, "详情="+rpcError.Details)
	}
	if rpcError.Body != "" {
		fields = append(fields, "原文="+rpcError.Body)
	}
	return strings.Join(fields, " | "), true
}

// logUpstreamClientError 在错误链里有上游 4xx 时，在该请求的管理日志（来源为执行账号）记一行证据（见文件开头）；suffix 不为空时附在最后
func (service *trackedService) logUpstreamClientError(requestID string, accountLabel string, err error, suffix string) {
	line, ok := upstreamClientErrorLine(err)
	if !ok {
		return
	}
	if suffix != "" {
		line += " | " + suffix
	}
	service.requests.logRequestProgress(requestID, accountLabel, "WARN", line)
}
