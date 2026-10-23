package setup

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// I13：IP 池开启、没传 --proxy 时 setup 登录用锁到的端口（与另一个持锁的进程不冲突），release 后释放；端口全被锁住时共用段内端口；
// 动态用临时会话号；提示只含端口号或会话号
func TestSetupPoolProxy(t *testing.T) {
	directory := t.TempDir()
	previous := setupLockDirectory
	setupLockDirectory = func() (string, error) { return directory, nil }
	t.Cleanup(func() { setupLockDirectory = previous })

	const password = "S3cr3t-P@ss"
	pool := config.ProxyPool{Mode: config.ProxyPoolStatic, URL: "http://user:" + password + "@isp.example.com:10000", PortStart: 10001, PortEnd: 10002}
	normalized, err := config.NormalizeProxyPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	// 另一个进程（这里是另一个句柄）锁住 10001：setup 只能用 10002
	other, locked, err := aistudio.TryLockProxyPort(directory, normalized.EndpointKey(), 10001)
	if err != nil || !locked {
		t.Fatalf("预先加锁 = %v %v", locked, err)
	}
	for range 5 {
		proxy, release, notice := setupPoolProxy(pool)
		if proxy != normalized.StaticURL(10002) || !strings.Contains(notice, "10002") || strings.Contains(notice, password) || strings.Contains(notice, "user") {
			t.Fatalf("出口 = %q 提示 = %q", config.RedactProxy(proxy), notice)
		}
		release()
	}
	// 两个端口都被占用：与其他进程共用段内端口（不加锁），不退回默认代理（PROXY 为空时那就是服务器真实 IP）
	second, locked, err := aistudio.TryLockProxyPort(directory, normalized.EndpointKey(), 10002)
	if err != nil || !locked {
		t.Fatalf("锁 10002 = %v %v", locked, err)
	}
	proxy, release, notice := setupPoolProxy(pool)
	port, inRange := normalized.HostMatches(proxy)
	if !inRange || port < 10001 || port > 10002 || proxy != normalized.StaticURL(port) ||
		!strings.Contains(notice, "共用端口 "+strconv.Itoa(port)) || strings.Contains(notice, password) {
		t.Fatalf("端口都被占用时 = %q %q", config.RedactProxy(proxy), notice)
	}
	release()
	_ = other.Release()
	_ = second.Release()

	dynamic := config.ProxyPool{Mode: config.ProxyPoolDynamic, URL: "http://customer-acc-sessid-{session}:" + password + "@gate.example.com:7000"}
	proxy, release, notice = setupPoolProxy(dynamic)
	defer release()
	session := regexp.MustCompile(`sessid-(n[0-9a-f]{11}):`).FindStringSubmatch(proxy)
	if session == nil || !strings.HasSuffix(notice, session[1]) || strings.Contains(notice, password) {
		t.Fatalf("动态出口 = %q 提示 = %q", config.RedactProxy(proxy), notice)
	}
	if proxy, _, notice := setupPoolProxy(config.DefaultProxyPool()); proxy != "" || notice != "" {
		t.Fatal("IP 池关闭时不应选出口")
	}
}
