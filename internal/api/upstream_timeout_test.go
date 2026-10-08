package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpstreamTimeoutProtocolShapes 上游超时（含每次尝试的首事件超时，满足 DeadlineExceeded）三种协议都按官方的 504 结构返回
func TestUpstreamTimeoutProtocolShapes(t *testing.T) {
	err := fmt.Errorf("上游首事件超时: %w", context.DeadlineExceeded)
	for _, test := range []struct {
		name  string
		write func(http.ResponseWriter, error)
		shape string
	}{
		{name: "openai", write: writeOpenAIRequestError, shape: `"server_error"`},
		{name: "gemini", write: writeGeminiRequestError, shape: `"DEADLINE_EXCEEDED"`},
		{name: "anthropic", write: writeAnthropicRequestError, shape: `"api_error"`},
	} {
		recorder := httptest.NewRecorder()
		test.write(recorder, err)
		body := recorder.Body.String()
		if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(body, officialDeadlineMessage) || !strings.Contains(body, test.shape) {
			t.Fatalf("%s 响应 = %d %s，期望 504 与 %s", test.name, recorder.Code, body, test.shape)
		}
	}
}
