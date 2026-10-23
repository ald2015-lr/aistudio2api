package proxyrelay

import (
	"errors"
	"testing"
	"time"
)

// R1：有失败说明时 WrapFailure 的结果命中 ErrUpstream，文本与改动前逐字相同；没有失败说明时原样返回、不命中
func TestWrapFailureMarksUpstream(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	auth := startUpstream(t, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	lease := acquireLease(t, registry, httpUpstreamURL(auth.address()))
	cause := errors.New("加载 AI Studio 页面: NS_ERROR_PROXY_CONNECTION_REFUSED")
	since := time.Now()
	sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443"))
	wrapped := lease.WrapFailure(cause, since, GoogleTarget)
	expected := "加载 AI Studio 页面: NS_ERROR_PROXY_CONNECTION_REFUSED；本机代理中转：" + text407
	if wrapped.Error() != expected {
		t.Fatalf("文本 = %q，期望 %q", wrapped.Error(), expected)
	}
	if !errors.Is(wrapped, ErrUpstream) || !errors.Is(wrapped, cause) {
		t.Fatalf("errors.Is：ErrUpstream=%v 原错误=%v", errors.Is(wrapped, ErrUpstream), errors.Is(wrapped, cause))
	}

	clean := acquireLease(t, registry, httpUpstreamURL(startUpstream(t, serveConnectEcho(t)).address()))
	if plain := clean.WrapFailure(cause, since, GoogleTarget); plain != cause || errors.Is(plain, ErrUpstream) {
		t.Fatalf("没有失败说明时 = %v", plain)
	}
	var empty *Lease
	if plain := empty.WrapFailure(cause, since, GoogleTarget); plain != cause || errors.Is(plain, ErrUpstream) {
		t.Fatalf("空租约 = %v", plain)
	}
	if errors.Is(cause, ErrUpstream) {
		t.Fatal("普通错误命中 ErrUpstream")
	}
}

func TestTotalConnectionLimit(t *testing.T) {
	if limit := TotalConnectionLimit(); limit < minTotalConnections || limit > maxTotalConnections {
		t.Fatalf("进程连接上限 = %d", limit)
	}
}
