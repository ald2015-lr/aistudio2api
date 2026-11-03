package aistudio

import (
	"encoding/json"
)

// decodeInlineMedia 解码 Part field 3 的 inline data [mime, base64]。媒体就是图片等模型的正文：不是数组、缺少数据或数据不是 Base64 时
// 仍按协议证据错误返回。Base64 接受标准与 URL 安全两种字母表、带或不带填充（原先只接受标准带填充的写法）；MIME 不是字符串时忽略，
// 为空时 sniff 为真则按文件头判断（Live 会话传假：保留空 MIME，客户端按缺省的 PCM 处理，与原先相同）
func decodeInlineMedia(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance, sniff bool) (Media, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return Media{}, withMethod(err, gcMethod)
	}
	if len(values) < 2 {
		return Media{}, &ProtocolEvidenceError{Method: gcMethod, Path: path, Detail: "inline media 字段不足", Raw: raw}
	}
	var mime string
	if !isJSONNull(values[0]) && json.Unmarshal(values[0], &mime) != nil {
		mime = ""
		tolerance.note(gcMethod, path+"[0]", "inline media 的 MIME 不是字符串，已忽略", values[0])
	}
	encoded, err := rawString(values[1], path+"[1]", raw)
	if err != nil {
		return Media{}, withMethod(err, gcMethod)
	}
	data, ok := decodeLenientBase64(encoded)
	if !ok {
		return Media{}, &ProtocolEvidenceError{Method: gcMethod, Path: path + "[1]", Detail: "inline media 不是有效 Base64", Raw: raw}
	}
	if mime == "" && sniff && len(data) > 0 {
		mime = sniffMediaMIME(data)
		tolerance.note(gcMethod, path+"[0]", "inline media 没有 MIME，按文件头判断为 "+mime, values[0])
	}
	return Media{MIME: mime, Data: data}, nil
}
