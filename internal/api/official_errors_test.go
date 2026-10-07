package api

import (
	"net/http"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestDowngradeRejectStatus 因降级拒绝按服务配置返回 400（默认，内容策略拦截格式）或 503（服务暂时不可用）
func TestDowngradeRejectStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		want    int
		rpc     string
		message string
	}{
		{name: "未设置按 400", status: 0, want: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: aistudio.DowngradePublicMessage},
		{name: "400", status: http.StatusBadRequest, want: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: aistudio.DowngradePublicMessage},
		{name: "503", status: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable, rpc: "UNAVAILABLE", message: aistudio.DowngradeUnavailableMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(&aistudio.ModelDowngradedError{Model: "gemini-3.1-pro-preview", Status: test.status}, "gemini-3.1-pro-preview")
			if public.Status != test.want || public.RPC != test.rpc || public.Message != test.message {
				t.Fatalf("得到 %+v", public)
			}
			if test.want == http.StatusServiceUnavailable && anthropicTypeForStatus(public.Status, public.Kind) != "overloaded_error" {
				t.Fatalf("503 应映射为 Anthropic overloaded_error，得到 %s", anthropicTypeForStatus(public.Status, public.Kind))
			}
		})
	}
}
