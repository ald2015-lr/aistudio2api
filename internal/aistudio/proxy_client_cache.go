package aistudio

import (
	"container/list"
	"net/http"
	"sync"
)

// proxyClientCacheLimit 为按代理缓存的 Go 侧 *http.Client 上限：IP 池开启后每个端口（静态）或每个账户的会话（动态）都是不同的
// 代理地址，不设上限时缓存只增不减。超出时淘汰最久未用的一个并关闭它的空闲连接（进行中的请求不受影响）；
// 只影响连接复用，不改 TLS 指纹与请求形态
const proxyClientCacheLimit = 1024

// proxyClientCache 为按代理地址缓存的 client（最近使用的在前）
type proxyClientCache struct {
	mu      sync.Mutex
	limit   int
	entries map[string]*list.Element
	order   list.List
}

type proxyClientEntry struct {
	proxy  string
	client *http.Client
}

func newProxyClientCache(limit int) *proxyClientCache {
	return &proxyClientCache{limit: limit, entries: make(map[string]*list.Element)}
}

// get 返回代理 proxy 的 client，没有时用 create 新建并放进缓存；超出上限时淘汰最久未用的
func (cache *proxyClientCache) get(proxy string, create func(string) (*http.Client, error)) (*http.Client, error) {
	cache.mu.Lock()
	if element := cache.entries[proxy]; element != nil {
		cache.order.MoveToFront(element)
		client := element.Value.(*proxyClientEntry).client
		cache.mu.Unlock()
		return client, nil
	}
	client, err := create(proxy)
	if err != nil {
		cache.mu.Unlock()
		return nil, err
	}
	cache.entries[proxy] = cache.order.PushFront(&proxyClientEntry{proxy: proxy, client: client})
	var evicted []*http.Client
	for cache.limit > 0 && cache.order.Len() > cache.limit {
		oldest := cache.order.Back()
		entry := oldest.Value.(*proxyClientEntry)
		cache.order.Remove(oldest)
		delete(cache.entries, entry.proxy)
		evicted = append(evicted, entry.client)
	}
	cache.mu.Unlock()
	for _, old := range evicted {
		old.CloseIdleConnections()
	}
	return client, nil
}

// closeIdle 关闭全部缓存 client 的空闲连接
func (cache *proxyClientCache) closeIdle() {
	cache.mu.Lock()
	clients := make([]*http.Client, 0, cache.order.Len())
	for element := cache.order.Front(); element != nil; element = element.Next() {
		clients = append(clients, element.Value.(*proxyClientEntry).client)
	}
	cache.mu.Unlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

// len 返回缓存的 client 数
func (cache *proxyClientCache) len() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.order.Len()
}
