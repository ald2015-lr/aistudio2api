package api

import (
	"net/http"
	"runtime/pprof"
)

// handleDebugGoroutines 输出全部 goroutine 调用栈（含阻塞时长），用于定位卡住的位置；
// 与其他管理接口一样只允许本机或通过管理密码访问
func handleDebugGoroutines(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
}
