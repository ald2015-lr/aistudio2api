package api

import (
	"encoding/json"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 排查路由：/trace/ 前缀下的全部接口与主路由完全相同（同一个 API Key、同样的调度与转换），
// 只是额外为每个 POST 请求写一份完整排查记录到 logs/trace/。主路由流量大、不适合记录大体积日志，
// 需要排查时把一个测试渠道的接口地址改成 http://服务器:端口/trace 即可，主路由完全不受影响
const (
	tracePrefix            = "/trace"
	defaultTraceDir        = "logs/trace"
	traceKeepFiles         = 100
	// traceResponseHeadLimit 与上游回复的记录上限相同，便于核对客户端实际收到的内容
	traceResponseHeadLimit = 1 << 20
)

var (
	traceFileName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.json$`)
	traceIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// traceHiddenHeaders 为不写入排查记录的鉴权头
var traceHiddenHeaders = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Cookie": true, "Proxy-Authorization": true,
}

// traceStore 保存排查记录文件，只保留最近 traceKeepFiles 个
type traceStore struct {
	dir string
	mu  sync.Mutex
}

type traceFile struct {
	Name  string    `json:"name"`
	Bytes int64     `json:"bytes"`
	Time  time.Time `json:"time"`
	// Summary 与 Flags 为记录里的自动结论
	Summary string   `json:"summary,omitempty"`
	Flags   []string `json:"flags,omitempty"`
}

var (
	traceSummaryField = regexp.MustCompile(`"summary":\s*("(?:[^"\\]|\\.)*")`)
	traceFlagsField   = regexp.MustCompile(`"flags":\s*(\[[^\]]*\])`)
)

// readTraceSummary 从记录文件开头读取自动结论（结论字段排在记录最前面）
func readTraceSummary(path string) (string, []string) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer file.Close()
	head := make([]byte, 32<<10)
	count, _ := io.ReadFull(file, head)
	head = head[:count]
	var summary string
	if match := traceSummaryField.FindSubmatch(head); match != nil {
		_ = json.Unmarshal(match[1], &summary)
	}
	var flags []string
	if match := traceFlagsField.FindSubmatch(head); match != nil {
		_ = json.Unmarshal(match[1], &flags)
	}
	return summary, flags
}

func newTraceStore(dir string) *traceStore {
	if strings.TrimSpace(dir) == "" {
		dir = defaultTraceDir
	}
	return &traceStore{dir: dir}
}

// save 写出一条排查记录并清理超出数量的旧文件
func (store *traceStore) save(trace *aistudio.Trace) (string, error) {
	data, err := trace.Finish()
	if err != nil {
		return "", err
	}
	id := traceIDUnsafe.ReplaceAllString(trace.RequestID(), "_")
	if id == "" {
		id = "request"
	}
	name := time.Now().Format("20060102-150405.000") + "_" + id + ".json"
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(store.dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(store.dir, name), data, 0o600); err != nil {
		return "", err
	}
	if files, err := store.list(); err == nil && len(files) > traceKeepFiles {
		for _, file := range files[traceKeepFiles:] {
			_ = os.Remove(filepath.Join(store.dir, file.Name))
		}
	}
	return name, nil
}

// list 返回全部排查记录，最新的在前
func (store *traceStore) list() ([]traceFile, error) {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []traceFile{}, nil
		}
		return nil, err
	}
	files := make([]traceFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !traceFileName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, traceFile{Name: entry.Name(), Bytes: info.Size(), Time: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files, nil
}

// traceEntry 处理 /trace/ 前缀：去掉前缀后交给与主路由相同的处理链，POST 请求结束后写出排查记录
func (s *server) traceEntry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, tracePrefix)
		if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/v1beta/") {
			http.NotFound(w, r)
			return
		}
		inner := r.Clone(r.Context())
		inner.URL.Path = path
		inner.URL.RawPath = ""
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, inner)
			return
		}
		trace := aistudio.NewTrace()
		next.ServeHTTP(w, inner.WithContext(aistudio.ContextWithTrace(inner.Context(), trace)))
		if !trace.Active() {
			return
		}
		if _, err := s.traces.save(trace); err != nil {
			// 写排查文件失败不影响已经完成的响应
			fmt.Fprintf(os.Stderr, "写入排查记录失败: %v\n", err)
		}
	})
}

// traceCaptureMiddleware 在鉴权通过后记录客户端原始请求与返回给客户端的响应，只对排查路由的请求生效
func traceCaptureMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace := aistudio.TraceFromContext(r.Context())
		if trace == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			// 读取失败（如超过请求体上限）时把已读部分与原错误交还给处理函数，由它按原逻辑报错
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), traceErrorReader{err: readErr}))
		} else {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		trace.Activate()
		trace.SetClient(aistudio.TraceClient{
			Method: r.Method, Path: tracePrefix + r.URL.Path, Query: traceQuery(r),
			RemoteAddr: r.RemoteAddr, Headers: traceHeaders(r.Header), Body: aistudio.NewTracePayload(body),
		})
		capture := &traceResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		trace.SetResponse(capture.result())
	})
}

type traceErrorReader struct {
	err error
}

func (reader traceErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func traceQuery(r *http.Request) string {
	values := r.URL.Query()
	for _, key := range []string{"key", "api_key", "token", "access_token"} {
		if values.Has(key) {
			values.Set(key, "<已隐藏>")
		}
	}
	return values.Encode()
}

func traceHeaders(header http.Header) map[string]string {
	headers := make(map[string]string, len(header))
	for name, values := range header {
		canonical := http.CanonicalHeaderKey(name)
		if traceHiddenHeaders[canonical] {
			headers[canonical] = "<已隐藏>"
			continue
		}
		headers[canonical] = strings.Join(values, ", ")
	}
	return headers
}

// traceResponseWriter 在写给客户端的同时记录状态码与响应开头
type traceResponseWriter struct {
	http.ResponseWriter
	status int
	total  int64
	head   bytes.Buffer
}

func (writer *traceResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *traceResponseWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if room := traceResponseHeadLimit - writer.head.Len(); room > 0 {
		chunk := data
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		writer.head.Write(chunk)
	}
	writer.total += int64(len(data))
	return writer.ResponseWriter.Write(data)
}

// Flush 兼容直接断言 http.Flusher 的写法
func (writer *traceResponseWriter) Flush() {
	_ = writer.FlushError()
}

// FlushError 让流式响应照常逐块刷新
func (writer *traceResponseWriter) FlushError() error {
	return http.NewResponseController(writer.ResponseWriter).Flush()
}

func (writer *traceResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *traceResponseWriter) result() aistudio.TraceResponse {
	status := writer.status
	if status == 0 {
		status = http.StatusOK
	}
	return aistudio.TraceResponse{
		Status: status, ContentType: writer.Header().Get("Content-Type"), Bytes: writer.total,
		Truncated: writer.total > int64(writer.head.Len()), Head: writer.head.String(),
	}
}

// handleTraceList 列出排查记录
func (s *server) handleTraceList(w http.ResponseWriter, _ *http.Request) {
	files, err := s.traces.list()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "trace_list_failed", err.Error())
		return
	}
	for index := range files {
		files[index].Summary, files[index].Flags = readTraceSummary(filepath.Join(s.traces.dir, files[index].Name))
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": s.traces.dir, "keep": traceKeepFiles, "files": files})
}

// handleTraceFile 下载一条排查记录
func (s *server) handleTraceFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !traceFileName.MatchString(name) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "排查记录文件名无效")
		return
	}
	data, err := os.ReadFile(filepath.Join(s.traces.dir, name))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "排查记录不存在")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleTraceClear 删除全部排查记录
func (s *server) handleTraceClear(w http.ResponseWriter, _ *http.Request) {
	files, err := s.traces.list()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "trace_list_failed", err.Error())
		return
	}
	removed := 0
	for _, file := range files {
		if os.Remove(filepath.Join(s.traces.dir, file.Name)) == nil {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handlePerformance 返回账户池锁与浏览器命令统计
func (s *server) handlePerformance(w http.ResponseWriter, _ *http.Request) {
	provider, ok := s.config.Admin.(interface{ PerformanceStats() any })
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "当前运行时不支持性能统计")
		return
	}
	writeJSON(w, http.StatusOK, provider.PerformanceStats())
}

// handleDuplicateReplies 返回重复回复统计与最近的重复记录
func (s *server) handleDuplicateReplies(w http.ResponseWriter, _ *http.Request) {
	provider, ok := s.config.Admin.(interface{ DuplicateReplies() any })
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "当前运行时不支持重复回复统计")
		return
	}
	writeJSON(w, http.StatusOK, provider.DuplicateReplies())
}
