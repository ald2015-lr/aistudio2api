package app

import (
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestStreamPrefersPlaygroundForTierModels 需要订阅权益的模型在 Build 通道只能一次性返回，流式请求优先走 Playground；
// 普通模型、非流式请求与目录里没有的模型不受影响
func TestStreamPrefersPlaygroundForTierModels(t *testing.T) {
	service := &trackedService{models: []aistudio.Model{
		{ID: "models/tier-flash", AccessModes: []int64{3}},
		{ID: "ultra-only", AccessModes: []int64{4}},
		{ID: "models/free-flash"},
	}}
	for _, test := range []struct {
		name   string
		model  string
		stream bool
		want   bool
	}{
		{name: "需要 Pro 权益的模型流式", model: "tier-flash", stream: true, want: true},
		{name: "带 models/ 前缀", model: "models/tier-flash", stream: true, want: true},
		{name: "需要 Ultra 权益的模型流式", model: "ultra-only", stream: true, want: true},
		{name: "需要权益的模型非流式", model: "tier-flash", stream: false, want: false},
		{name: "免费模型流式", model: "free-flash", stream: true, want: false},
		{name: "目录里没有的模型", model: "unknown", stream: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := aistudio.GenerateRequest{Model: test.model, Stream: test.stream}
			if got := service.streamPrefersPlayground(request, test.model); got != test.want {
				t.Fatalf("streamPrefersPlayground(%q, stream=%v) = %v，期望 %v", test.model, test.stream, got, test.want)
			}
		})
	}
}
