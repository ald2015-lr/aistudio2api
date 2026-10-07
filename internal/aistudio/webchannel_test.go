package aistudio

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingRoundTripper struct {
	mu   sync.Mutex
	urls []string
}

func (transport *recordingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.urls = append(transport.urls, request.URL.String())
	transport.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
}

func (transport *recordingRoundTripper) terminates() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	count := 0
	for _, url := range transport.urls {
		if strings.Contains(url, "TYPE=terminate") {
			count++
		}
	}
	return count
}

func closedTestSession(parent context.Context, transport http.RoundTripper) *BidiSession {
	ctx, cancel := context.WithCancel(parent)
	session := &BidiSession{
		ctx: ctx, cancel: cancel, client: &http.Client{Transport: transport},
		headers: http.Header{}, gsessionID: "gsession", sid: "SID", done: make(chan struct{}),
	}
	close(session.done)
	return session
}

// TestBidiCloseTerminatesAfterClientDisconnect 客户端断开（父 context 已取消）后关闭会话仍向上游发送 terminate
func TestBidiCloseTerminatesAfterClientDisconnect(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	transport := &recordingRoundTripper{}
	session := closedTestSession(parent, transport)
	cancelParent()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if transport.terminates() != 1 {
		t.Fatalf("terminate 请求数 = %d，期望 1", transport.terminates())
	}
}

// TestBidiCloseSkipsTerminateAfterUpstreamEnded 上游已经结束会话时不再发送 terminate
func TestBidiCloseSkipsTerminateAfterUpstreamEnded(t *testing.T) {
	transport := &recordingRoundTripper{}
	session := closedTestSession(context.Background(), transport)
	session.upstreamEnded.Store(true)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if transport.terminates() != 0 {
		t.Fatalf("terminate 请求数 = %d，期望 0", transport.terminates())
	}
}

// TestBidiReconnectDelay 重连间隔从 250ms 起翻倍，最长 5 秒
func TestBidiReconnectDelay(t *testing.T) {
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for index, expected := range want {
		if got := bidiReconnectDelay(index + 1); got != expected {
			t.Fatalf("第 %d 次失败间隔 = %s，期望 %s", index+1, got, expected)
		}
	}
}

// TestOutboxHasRoom 发送队列按条数与字节数设上限
func TestOutboxHasRoom(t *testing.T) {
	if !outboxHasRoom(nil, 1024) {
		t.Fatal("空队列应能放入")
	}
	full := make([]bidiOutgoing, bidiOutboxLimit)
	if outboxHasRoom(full, 1) {
		t.Fatal("条数达到上限时应拒绝")
	}
	large := []bidiOutgoing{{payload: make([]byte, bidiOutboxByteLimit)}}
	if outboxHasRoom(large, 1) {
		t.Fatal("字节数超过上限时应拒绝")
	}
}
