package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type nestedURLErrorTransport struct{}

// RoundTrip 模拟 fhttp：传输层自己返回带完整 URL 的 *url.Error，net/http.Client 再包一层
func (nestedURLErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: request.URL.String(), Err: errors.New("connection reset by peer")}
}

func TestSendHTTPRedactsEveryURLErrorLayer(t *testing.T) {
	client := &http.Client{Transport: nestedURLErrorTransport{}}
	request, err := http.NewRequest(http.MethodPost, "https://example.com/bind?SID=session-secret&gsessionid=gs-secret&RID=7#frag", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sendHTTP(client, request)
	if err == nil {
		t.Fatal("期望网络错误")
	}
	wrapped := fmt.Errorf("WebChannel 请求失败: %w", err).Error()
	for _, secret := range []string{"session-secret", "gs-secret", "RID=7", "frag"} {
		if strings.Contains(wrapped, secret) {
			t.Fatalf("错误文本带出查询参数 %q: %s", secret, wrapped)
		}
	}
	if !strings.Contains(wrapped, "https://example.com/bind?<redacted>") || !strings.Contains(wrapped, "connection reset by peer") {
		t.Fatalf("错误文本应保留去掉查询串的地址与原因: %s", wrapped)
	}
	// 发出的请求本身不变
	if request.URL.RawQuery != "SID=session-secret&gsessionid=gs-secret&RID=7" {
		t.Fatalf("请求的查询串被改动: %q", request.URL.RawQuery)
	}
	var urlError *url.Error
	if !errors.As(err, &urlError) {
		t.Fatal("错误类型应仍是 *url.Error（上层按它判断超时与网络错误）")
	}
}

// 实际的浏览器形状传输（fhttp）连接失败时同样不带查询参数
func TestBrowserTransportNetworkErrorOmitsQuery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	client, err := NewProxyHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+address+"/upload?upload_id=upload-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := sendHTTP(client, request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("期望连接失败")
	}
	if strings.Contains(err.Error(), "upload-secret") {
		t.Fatalf("错误文本带出查询参数: %s", err)
	}
}

func TestRedactURLErrorsNil(t *testing.T) {
	redactURLErrors(nil)
	redactURLErrors(errors.New("plain"))
}
