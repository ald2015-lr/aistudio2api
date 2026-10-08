package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Ultra 路由：/ultra 前缀下的全部接口与主路由完全相同（同一个 API Key、同样的处理链与协议转换），
// 只是请求只使用 Ultra 号池（权益为 Ultra 的账户）。客户端把接口地址改成
// http://服务器:端口/ultra/v1（OpenAI）或 http://服务器:端口/ultra（Anthropic、Gemini）即可。
// ULTRA_EXCLUSIVE=true（默认）时普通路径只使用普通号池，为 false 时普通路径不限号池
const ultraPrefix = "/ultra"

// ultraEntry 处理 /ultra 前缀：去掉前缀后标记为 Ultra 号池的请求，交给与主路由相同的处理链；其余 /ultra/* 路径返回 404
func ultraEntry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, rawPath, ok := stripRoutePrefix(r.URL, ultraPrefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		inner := r.Clone(aistudio.ContextWithPoolScope(r.Context(), aistudio.PoolScopeUltra))
		inner.URL.Path = path
		inner.URL.RawPath = rawPath
		next.ServeHTTP(w, inner)
	})
}

// stripRoutePrefix 去掉 /ultra、/trace 等路由前缀，返回交给主路由的路径与其转义形式；不是 /v1/、/v1beta/ 路径时 ok 为 false。
// 前缀与 /v1/ 按转义后的路径判断，并保留客户端的转义（如 ID 中的 %2F）：主路由按转义后的路径段匹配，
// 去掉转义会让同一个请求与直接请求 /v1 时匹配到不同的路由
func stripRoutePrefix(u *url.URL, prefix string) (path string, rawPath string, ok bool) {
	escaped := strings.TrimPrefix(u.EscapedPath(), prefix)
	if !strings.HasPrefix(escaped, "/v1/") && !strings.HasPrefix(escaped, "/v1beta/") {
		return "", "", false
	}
	path = strings.TrimPrefix(u.Path, prefix)
	if u.RawPath != "" {
		rawPath = escaped
	}
	return path, rawPath, true
}

// poolScopeMiddleware 为没有经过 /ultra 入口的请求写入号池：独占模式下只用普通号池，否则不限号池。
// 放在请求日志之外，日志、选号与模型目录都能看到请求的号池
func poolScopeMiddleware(exclusive func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, marked := aistudio.LookupPoolScope(r.Context()); marked {
			next.ServeHTTP(w, r)
			return
		}
		scope := aistudio.PoolScopeAll
		if exclusive() {
			scope = aistudio.PoolScopeNormal
		}
		next.ServeHTTP(w, r.WithContext(aistudio.ContextWithPoolScope(r.Context(), scope)))
	})
}

// ultraExclusive 返回当前是否独占 Ultra 账户；没有配置读取函数时按默认独占
func (config Config) ultraExclusive() bool {
	if config.UltraExclusive == nil {
		return true
	}
	return config.UltraExclusive()
}

// ultraRequest 判断请求是否经 /ultra 入口进入
func ultraRequest(r *http.Request) bool {
	return aistudio.PoolScopeFromContext(r.Context()) == aistudio.PoolScopeUltra
}
