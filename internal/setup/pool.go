package setup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// setupLockDirectory 返回跨进程端口锁目录（与主进程相同），测试替换
var setupLockDirectory = aistudio.ProxyPortLockDirectory

// setupPoolProxy 为 setup 的登录与导入选 IP 池出口：静态从随机起点依次尝试端口锁，用第一个锁到的端口（与主进程及其他 setup 互斥），
// 返回的 release 在 setup 结束时释放；端口全被其他进程锁住时与它们共用随机起点的端口（不加锁，与主进程端口不足时共用相同），
// 不退回默认代理（PROXY 为空时那就是服务器真实 IP）。动态用一个 "n" 开头的临时会话号。setup 是独立进程，不记亲和（主进程第一次
// 启动这个账户的 Worker 时再选端口）；一次 setup（包括批量导入 Chrome 的多个账号）只用这一个出口。notice 为写到标准错误的说明
// （只含端口号或会话号）
func setupPoolProxy(pool config.ProxyPool) (proxy string, release func(), notice string) {
	release = func() {}
	normalized, err := config.NormalizeProxyPool(pool)
	if err != nil || !normalized.Enabled() {
		return "", release, ""
	}
	if normalized.Mode == config.ProxyPoolDynamic {
		session := "n" + randomHex(11)
		url := normalized.SessionURL(session)
		if url == "" {
			return "", release, ""
		}
		return url, release, "登录使用 IP 池会话 " + session
	}
	urls := normalized.StaticURLs()
	if len(urls) == 0 {
		return "", release, ""
	}
	start, _ := normalized.Ports()
	offset := randomIndex(len(urls))
	directory, err := setupLockDirectory()
	if err != nil {
		return urls[offset], release, fmt.Sprintf("IP 池跨进程端口锁不可用，登录使用端口 %d（不与主进程互斥）", start+offset)
	}
	key := normalized.EndpointKey()
	for step := range urls {
		index := (offset + step) % len(urls)
		lock, locked, err := aistudio.TryLockProxyPort(directory, key, start+index)
		if err != nil {
			return urls[index], release, fmt.Sprintf("IP 池跨进程端口锁不可用，登录使用端口 %d（不与主进程互斥）", start+index)
		}
		if locked {
			return urls[index], func() { _ = lock.Release() }, fmt.Sprintf("登录使用 IP 池端口 %d", start+index)
		}
	}
	return urls[offset], release, fmt.Sprintf("IP 池端口都在使用中，登录与其他浏览器共用端口 %d（不加锁）", start+offset)
}

// randomIndex 返回 [0, size) 的随机数
func randomIndex(size int) int {
	if size <= 1 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(size)))
	if err != nil {
		return 0
	}
	return int(value.Int64())
}

// randomHex 返回 length 位随机小写十六进制
func randomHex(length int) string {
	buffer := make([]byte, (length+1)/2)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)[:length]
}
