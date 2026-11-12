// Package webui 提供内嵌管理端静态资源
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var embedded embed.FS

// Files 返回管理端构建产物文件系统
func Files() fs.FS {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	return dist
}

// Handler 返回管理端静态文件处理器
func Handler() http.Handler {
	return newHandler(Files())
}

// newHandler 返回服务 files 中构建产物的处理器，files 根目录必须有 index.html
func newHandler(files fs.FS) http.Handler {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		panic(err)
	}
	return &spaHandler{
		files:  files,
		static: http.FileServer(http.FS(files)),
		index:  index,
	}
}

type spaHandler struct {
	files  fs.FS
	static http.Handler
	index  []byte
}

// assetCacheControl 是 /assets/ 下构建产物的缓存策略：Vite 输出的文件名带内容哈希，内容变了文件名就变，
// 浏览器可以长期缓存且不必重新验证
const assetCacheControl = "public, max-age=31536000, immutable"

// ServeHTTP 服务静态资源并为普通页面路径返回 SPA 入口。/assets/ 下的文件长期缓存，不存在时返回 404：
// 服务升级后，已打开的旧页面再去载入旧版的懒加载块，拿到 404 才会按载入失败处理（前端随后自动刷新一次），
// 原先回落为 200 的 index.html，浏览器只报 MIME 类型错误。入口 index.html、其他静态文件与 SPA 回落一律 no-store，
// 升级后刷新页面总能拿到引用新文件名的入口
func (handler *spaHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	requestPath := path.Clean(request.URL.Path)
	if reservedPath(requestPath) {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		http.NotFound(writer, request)
		return
	}

	name := strings.TrimPrefix(requestPath, "/")
	if name == "." {
		name = "index.html"
	}
	info, err := fs.Stat(handler.files, name)
	if assetPath(requestPath) {
		// 目录不列出内容，与不存在的文件一样返回 404
		if err != nil || info.IsDir() {
			writer.Header().Set("Cache-Control", "no-store")
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", assetCacheControl)
		handler.static.ServeHTTP(writer, request)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	if err == nil {
		handler.static.ServeHTTP(writer, request)
		return
	}
	if request.Method == http.MethodHead {
		http.NotFound(writer, request)
		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(handler.index)
}

// assetPath 判断清理后的路径是否在构建产物目录 /assets 下
func assetPath(requestPath string) bool {
	return requestPath == "/assets" || strings.HasPrefix(requestPath, "/assets/")
}

// reservedPath 判断路径是否属于服务 API
func reservedPath(requestPath string) bool {
	for _, prefix := range []string{"/api", "/v1", "/v1beta", "/health"} {
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return true
		}
	}
	return false
}
