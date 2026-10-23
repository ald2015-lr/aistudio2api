package aistudio

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// I5：client 缓存同一代理返回同一个 client；超过上限时淘汰最久未用的；创建失败不进缓存；并发安全
func TestProxyClientCache(t *testing.T) {
	cache := newProxyClientCache(2)
	created := map[string]int{}
	create := func(proxy string) (*http.Client, error) {
		if proxy == "bad" {
			return nil, fmt.Errorf("代理地址无效")
		}
		created[proxy]++
		return &http.Client{}, nil
	}
	a, _ := cache.get("a", create)
	b, _ := cache.get("b", create)
	if again, _ := cache.get("a", create); again != a {
		t.Fatal("同一代理应返回同一个 client")
	}
	// a 刚用过：再放进 c 时淘汰最久未用的 b
	if _, err := cache.get("c", create); err != nil || cache.len() != 2 {
		t.Fatalf("放进 c 后缓存 = %d（%v）", cache.len(), err)
	}
	if again, _ := cache.get("a", create); again != a {
		t.Fatal("最近用过的 a 不应被淘汰")
	}
	if again, _ := cache.get("b", create); again == b || created["b"] != 2 {
		t.Fatalf("b 应已被淘汰并重新创建：创建次数 %d", created["b"])
	}
	if _, err := cache.get("bad", create); err == nil || cache.len() != 2 {
		t.Fatalf("创建失败不应进缓存：%d %v", cache.len(), err)
	}
	cache.closeIdle()

	shared := newProxyClientCache(proxyClientCacheLimit)
	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range 400 {
				if _, err := shared.get(fmt.Sprintf("http://h:%d", (worker*400+index)%1500), func(string) (*http.Client, error) {
					return &http.Client{}, nil
				}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wait.Wait()
	if shared.len() > proxyClientCacheLimit {
		t.Fatalf("缓存超过上限：%d", shared.len())
	}
}
