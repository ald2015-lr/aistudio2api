package aistudio

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestEgressKeyContract 出口键按分组口径规范化：直连为空串；scheme 与 host 小写、socks5h 归为 socks5、没有 scheme 时补 http://；
// 去掉密码、保留用户名；解析失败时取去空白的原文。标签从不含用户名与密码原文，编号稳定且不含代理原文
func TestEgressKeyContract(t *testing.T) {
	cases := []struct {
		proxy, key string
	}{
		{"", ""},
		{"   ", ""},
		{"HTTP://Proxy.Example:8080", "http://proxy.example:8080"},
		{"proxy.example:3128", "http://proxy.example:3128"},
		{"socks5h://gw.example:1080", "socks5://gw.example:1080"},
		{" socks5://alice:secret@GW.example:1080 ", "socks5://alice@gw.example:1080"},
		{"http://:8080", "http://:8080"},
	}
	for _, item := range cases {
		if got := EgressKey(item.proxy); got != item.key {
			t.Fatalf("EgressKey(%q) = %q，期望 %q", item.proxy, got, item.key)
		}
	}
	if EgressKey("socks5h://alice:one@gw.example:1080") != EgressKey("socks5://alice:two@GW.EXAMPLE:1080") {
		t.Fatal("同一用户名、同一网关的不同写法与密码应归到同一出口")
	}
	if EgressKey("socks5://alice@gw.example:1080") == EgressKey("socks5://bob@gw.example:1080") {
		t.Fatal("同一网关的不同用户名应是不同出口")
	}

	if got := EgressLabel(""); got != "直连" {
		t.Fatalf("直连标签 = %q", got)
	}
	if got := EgressLabel("http://proxy.example:8080"); got != "http://proxy.example:8080" {
		t.Fatalf("无用户名标签 = %q", got)
	}
	label := EgressLabel(EgressKey("socks5://alice:secret@gw.example:1080"))
	if !strings.HasPrefix(label, "socks5://gw.example:1080#") || len(label) != len("socks5://gw.example:1080#")+4 ||
		strings.Contains(label, "alice") || strings.Contains(label, "secret") {
		t.Fatalf("带用户名标签 = %q", label)
	}
	unknown := EgressLabel("http://:8080")
	if !strings.HasPrefix(unknown, "<无法识别的代理>#") || strings.Contains(unknown, "8080") {
		t.Fatalf("无法解析的标签 = %q", unknown)
	}

	id := EgressID("socks5://alice@gw.example:1080")
	if len(id) != 12 || id != EgressID("socks5://alice@gw.example:1080") || strings.Contains(id, "alice") {
		t.Fatalf("EgressID = %q", id)
	}
	if EgressID("") == id {
		t.Fatal("不同出口的编号应不同")
	}
}

// TestEgressWaitLimitContext 出口等待上限经 context 传递；没有设置或不为正时为 0（不限）
func TestEgressWaitLimitContext(t *testing.T) {
	ctx := context.Background()
	if got := egressWaitLimitFrom(ctx); got != 0 {
		t.Fatalf("没有设置时 = %v", got)
	}
	if got := egressWaitLimitFrom(WithEgressWaitLimit(ctx, 0)); got != 0 {
		t.Fatalf("上限为 0 时 = %v", got)
	}
	if got := egressWaitLimitFrom(WithEgressWaitLimit(ctx, 30*time.Second)); got != 30*time.Second {
		t.Fatalf("上限 = %v", got)
	}
}
