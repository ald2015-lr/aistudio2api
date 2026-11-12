package aistudio

import (
	"errors"
	"net/http"
	"net/url"
)

// sendHTTP 发送 HTTP 请求；请求失败时把错误链里每个 *url.Error 的地址去掉查询串（见 redactURLErrors），只改错误文本，
// 发出的请求不变
func sendHTTP(client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(request)
	redactURLErrors(err)
	return response, err
}

// redactURLErrors 原地去掉错误链里每个 *url.Error 地址的查询串与片段：网络错误的文本带完整 URL，WebChannel 的 SID、
// gsessionid、RID、Drive 断点续传的 upload_id 等会话参数会随它进入运行日志、冷却原因（runtime-state.json）与
// trace_pack 诊断包。浏览器形状传输（browserRoundTripper）经 fhttp 返回的 *url.Error 又被 net/http.Client 包一层，
// 两层都带完整 URL，所以沿 Unwrap 链逐层处理，不能只改最外层
func redactURLErrors(err error) {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if urlError, ok := current.(*url.Error); ok && urlError != nil {
			urlError.URL = redactURLQuery(urlError.URL)
		}
	}
}
