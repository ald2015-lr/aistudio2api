package api

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Config 定义公开 API 服务配置
type Config struct {
	APIKey string
	// APIKeyFunc 非空时每次请求读取最新密钥，优先于 APIKey
	APIKeyFunc    func() string
	Admin         AdminService
	AdminPassword string
	// AdminToken 为管理令牌：控制面请求必须携带（或通过 ADMIN_PASSWORD 认证）
	AdminToken string
	// Stopping 在服务开始优雅退出时关闭；管理页实时事件流据此主动结束，不拖住退出等待
	Stopping <-chan struct{}
	// TraceDir 为 /trace/ 排查路由写记录的目录，空值为 logs/trace
	TraceDir string
}

type server struct {
	service           aistudio.Service
	config            Config
	responseStates    *responseStateStore
	thoughtSignatures *thoughtSignatureStore
	// aliasModels 缓存模型后缀解析用的模型目录，避免每个请求都在账户池锁内重算
	aliasModels atomic.Pointer[aliasModelSnapshot]
	// aliasRefreshMu 保证没有快照时只有一个请求计算目录；aliasRefreshing 保证后台同时只有一个刷新
	aliasRefreshMu  sync.Mutex
	aliasRefreshing atomic.Bool
	// traces 保存 /trace/ 排查路由的请求记录
	traces *traceStore
}

var idSequence atomic.Uint64

// NewHandler 创建公开 API 路由
func NewHandler(service aistudio.Service, config Config) http.Handler {
	s := &server{service: service, config: config, responseStates: newResponseStateStore(), thoughtSignatures: newThoughtSignatureStore(), traces: newTraceStore(config.TraceDir)}
	public := http.NewServeMux()
	public.HandleFunc("GET /v1/models", s.handleOpenAIModels)
	public.HandleFunc("GET /v1/models/{model...}", s.handleOpenAIModel)
	public.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	public.HandleFunc("POST /v1/responses", s.handleResponses)
	public.HandleFunc("POST /v1/interactions", s.handleInteraction)
	public.HandleFunc("POST /v1beta/interactions", s.handleInteraction)
	public.HandleFunc("POST /v1/files", s.handleOpenAIFileUpload)
	public.HandleFunc("GET /v1/files/{file}", s.handleOpenAIFileGet)
	public.HandleFunc("GET /v1/files/{file}/content", s.handleOpenAIFileContent)
	public.HandleFunc("DELETE /v1/files/{file}", s.handleOpenAIFileDelete)
	public.HandleFunc("POST /v1/images/generations", s.handleOpenAIImages)
	public.HandleFunc("POST /v1/audio/speech", s.handleOpenAISpeech)
	public.HandleFunc("POST /v1/audio/transcriptions", s.handleOpenAITranscription)
	public.HandleFunc("GET /v1/live", s.handleGeminiLive)
	public.HandleFunc("GET /v1/robotics/stream", s.handleRoboticsStream)
	public.HandleFunc("POST /v1/videos", s.handleOpenAIVideoCreate)
	public.HandleFunc("GET /v1/videos/{video}", s.handleOpenAIVideoGet)
	public.HandleFunc("GET /v1/videos/{video}/content", s.handleOpenAIVideoContent)
	public.HandleFunc("POST /v1/messages", s.handleAnthropicMessages)
	public.HandleFunc("POST /v1/messages/count_tokens", s.handleAnthropicCountTokens)
	public.HandleFunc("GET /v1beta/models", s.handleGeminiModels)
	public.HandleFunc("GET /v1beta/models/{model}", s.handleGeminiModel)
	public.HandleFunc("POST /v1beta/models/{action}", s.handleGeminiAction)
	public.HandleFunc("GET /v1beta/operations/{operation}", s.handleGeminiVideoOperation)

	control := http.NewServeMux()
	control.HandleFunc("GET /api/status", s.handleStatus)
	control.HandleFunc("GET /api/models", s.handleAdminModels)
	control.HandleFunc("GET /api/debug/traces", s.handleTraceList)
	control.HandleFunc("GET /api/debug/traces/{name}", s.handleTraceFile)
	control.HandleFunc("DELETE /api/debug/traces", s.handleTraceClear)
	control.HandleFunc("GET /api/debug/duplicates", s.handleDuplicateReplies)
	control.HandleFunc("GET /api/debug/perf", s.handlePerformance)
	if config.Admin != nil {
		s.registerAdmin(control)
	}

	root := http.NewServeMux()
	root.Handle("GET /health", corsMiddleware(http.HandlerFunc(s.handleHealth)))
	publicHandler := bodyLimitMiddleware(browserOriginMiddleware(config.currentAPIKey, authMiddleware(config.currentAPIKey, traceCaptureMiddleware(public))))
	publicChain := requestLoggingMiddleware(config.Admin, corsMiddleware(publicHandler))
	root.Handle("/v1/", publicChain)
	root.Handle("/v1beta/", publicChain)
	// 排查路由：与主路由完全相同的处理链，额外为每个 POST 请求写完整排查记录（见 trace.go）
	root.Handle("/trace/", s.traceEntry(publicChain))
	// 先拒绝跨站请求，再校验令牌或密码：浏览器对跨站请求也会自动附带缓存的 Basic 凭据，不能让它们计入密码失败次数
	root.Handle("/api/", sameOriginMiddleware(adminAccessMiddleware(config.AdminPassword, config.AdminToken, controlPlaneMiddleware(control))))
	return root
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), idSequence.Add(1))
}
