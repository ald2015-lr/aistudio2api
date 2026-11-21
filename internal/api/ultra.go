package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Ultra 路由：/ultra 前缀下的全部接口与主路由完全相同（同一个 API Key、同样的处理链与协议转换），
// 只是请求只使用 Ultra 号池（权益为 Ultra 的账户，PRO_POOL=false、ULTRA_INCLUDE_PRO=true 时也含权益为 Pro 的账户；号池成员由账户池判定，
// 这里只标记请求的号池）。客户端把接口地址改成
// http://服务器:端口/ultra/v1（OpenAI）或 http://服务器:端口/ultra（Anthropic、Gemini）即可。
// ULTRA_EXCLUSIVE=true（默认）时普通路径只使用普通号池，为 false 时普通路径不限号池（PRO_POOL=true 时为普通号池与 Ultra 号池）
const ultraPrefix = "/ultra"

// Pro 路由：/pro 前缀与 /ultra 的挂载方式完全相同，请求只使用 Pro 号池（PRO_POOL=true 时权益为 Pro 的账户），不借用 Ultra 与普通号池的账户。
// 客户端地址为 http://服务器:端口/pro/v1（OpenAI）或 http://服务器:端口/pro（Anthropic、Gemini）。PRO_POOL=false 时 /pro 仍挂载，
// 由处理链里的 proPoolGate 按协议返回 404（在密钥校验、CORS 与请求日志之后，见 NewHandler）
const proPrefix = "/pro"

// poolEntry 处理号池前缀（/ultra、/pro）：去掉前缀后标记为该号池的请求，交给与主路由相同的处理链；其余 prefix/* 路径返回 404。
// 号池与前缀一一对应：经 /pro 进入的请求号池总是 Pro，普通路径不会标记为 Pro 或 Ultra（见 poolScopeMiddleware），
// 日志路径、视频下载地址与排查记录按号池还原前缀（routePoolPrefix）
func poolEntry(prefix string, scope aistudio.PoolScope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, rawPath, ok := stripRoutePrefix(r.URL, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		inner := r.Clone(aistudio.ContextWithPoolScope(r.Context(), scope))
		inner.URL.Path = path
		inner.URL.RawPath = rawPath
		next.ServeHTTP(w, inner)
	})
}

// ultraEntry 处理 /ultra 前缀：标记为 Ultra 号池的请求
func ultraEntry(next http.Handler) http.Handler {
	return poolEntry(ultraPrefix, aistudio.PoolScopeUltra, next)
}

// proEntry 处理 /pro 前缀：标记为 Pro 号池的请求；PRO_POOL=false 时的 404 不在这里返回，由处理链里的 proPoolGate 返回
func proEntry(next http.Handler) http.Handler {
	return poolEntry(proPrefix, aistudio.PoolScopePro, next)
}

// stripRoutePrefix 去掉 /ultra、/pro、/trace 等路由前缀，返回交给主路由的路径与其转义形式；不是 /v1/、/v1beta/ 路径时 ok 为 false。
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

// poolScopeMiddleware 为没有经过 /ultra、/pro 入口的请求写入号池：独占模式下只用普通号池；不独占时不限号池，
// PRO_POOL=true 时为普通号池与 Ultra 号池（Pro 账户只服务 /pro）。放在请求日志之外，日志、选号与模型目录都能看到请求的号池
func poolScopeMiddleware(exclusive func() bool, proPool func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, marked := aistudio.LookupPoolScope(r.Context()); marked {
			next.ServeHTTP(w, r)
			return
		}
		scope := aistudio.PoolScopeAll
		switch {
		case exclusive():
			scope = aistudio.PoolScopeNormal
		case proPool():
			scope = aistudio.PoolScopeNormalUltra
		}
		next.ServeHTTP(w, r.WithContext(aistudio.ContextWithPoolScope(r.Context(), scope)))
	})
}

// proPoolGate 在 PRO_POOL=false 时拒绝经 /pro 进入的请求：按请求协议的错误体返回 404，说明 /pro 接口在本服务关闭。
// 放在密钥校验之后、读取请求体之前：没有密钥的请求照常返回 401，CORS 头与请求日志（路径 /pro/...、号池 pro）照常写；
// 开关每个请求读一次（proPool），关闭后新请求立即 404，已经进入的请求由账户池按 Pro 号池未启用返回 503
func proPoolGate(proPool func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aistudio.PoolScopeFromContext(r.Context()) == aistudio.PoolScopePro && !proPool() {
			writeProPoolDisabled(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeProPoolDisabled 按请求协议（按去掉前缀后的路径判断，与 writeRouteNotFound 相同）返回 /pro 关闭时的 404，
// 消息为 "Invalid URL (POST /pro/v1/chat/completions): the /pro endpoints are disabled on this server."（排查路由带 /trace 前缀）
func writeProPoolDisabled(w http.ResponseWriter, r *http.Request) {
	path := routePoolPrefix(r) + r.URL.EscapedPath()
	if aistudio.TraceFromContext(r.Context()) != nil {
		path = tracePrefix + path
	}
	message := fmt.Sprintf("Invalid URL (%s %s): the %s endpoints are disabled on this server.", r.Method, path, proPrefix)
	switch protocolForRequest(r) {
	case "gemini":
		writeGeminiError(w, http.StatusNotFound, "NOT_FOUND", message)
	case "anthropic":
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", message)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", message)
	}
}

// ultraExclusive 返回当前是否独占 Ultra 号池的账户；没有配置读取函数时按默认独占
func (config Config) ultraExclusive() bool {
	if config.UltraExclusive == nil {
		return true
	}
	return config.UltraExclusive()
}

// proPool 返回当前是否启用 Pro 独立号池（PRO_POOL）；没有配置读取函数时按默认开启
func (config Config) proPool() bool {
	if config.ProPool == nil {
		return true
	}
	return config.ProPool()
}

// routePoolPrefix 返回请求原路径中的号池前缀：Ultra 号池的请求为 /ultra，Pro 号池的请求为 /pro，其余（普通路径的各种号池）为空
func routePoolPrefix(r *http.Request) string {
	switch aistudio.PoolScopeFromContext(r.Context()) {
	case aistudio.PoolScopeUltra:
		return ultraPrefix
	case aistudio.PoolScopePro:
		return proPrefix
	default:
		return ""
	}
}

// routePoolLabel 返回请求日志与用量账本里的号池标记：经 /ultra 进入为 ultra，经 /pro 进入为 pro，普通路径为空
// （不论普通路径按 ULTRA_EXCLUSIVE、PRO_POOL 用的是哪个号池，账本里都是普通号池）
func routePoolLabel(r *http.Request) string {
	switch scope := aistudio.PoolScopeFromContext(r.Context()); scope {
	case aistudio.PoolScopeUltra, aistudio.PoolScopePro:
		return scope.String()
	default:
		return ""
	}
}
