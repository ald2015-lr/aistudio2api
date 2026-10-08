package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/setup"
	"github.com/Mag1cFall/AIStudio2API/internal/webui"
)

// commandOptions 保存只影响本次启动的命令行选项
type commandOptions struct {
	openUI    bool
	overrides dataConfigOverrides
}

// Run 执行单二进制命令入口
func Run(args []string) int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := runCommand(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		slog.Error("AIStudio2API 启动失败", "error", err)
		return 1
	}
	return 0
}

// runCommand 分派首次配置与默认服务
func runCommand(args []string) error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) != 0 && args[0] == "setup" {
		return setup.Run(ctx, cfg, args[1:])
	}
	options, err := parseFlags(args, &cfg)
	if err != nil {
		return err
	}
	// 运行时（生成服务、浏览器 Worker）的生命周期不直接跟随退出信号：原先一收到停止信号，
	// 所有正在生成的回复立即被取消，每次重启都会把进行中的回复全部截断。
	// 现在收到信号后先停止接收新请求、等进行中的请求完成（见 runServer），再结束运行时
	lifecycle, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	manager, err := newRuntimeManager(ctx, lifecycle, ".env", cfg, options.overrides)
	if err != nil {
		if ctx.Err() != nil {
			// 装配初始生成服务（如首次下载 Camoufox）期间收到退出信号：正常退出
			return nil
		}
		return err
	}
	serveErr := runServer(ctx, cfg, options, manager)
	cancelLifecycle()
	return errors.Join(serveErr, manager.Close())
}

// shutdownGrace 返回退出时等待进行中请求完成的最长时间，可用 SHUTDOWN_GRACE 调整（如 90s；默认 60s）
func shutdownGrace() time.Duration {
	if value := strings.TrimSpace(os.Getenv("SHUTDOWN_GRACE")); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration >= 0 {
			return duration
		}
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 60 * time.Second
}

// parseFlags 使用命令行参数覆盖本次启动配置
func parseFlags(args []string, cfg *config.Config) (commandOptions, error) {
	flags := flag.NewFlagSet("aistudio2api", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "首次配置: aistudio2api setup")
		fmt.Fprintln(flags.Output(), "日常启动: aistudio2api [参数]")
		flags.PrintDefaults()
	}
	authStates := flags.String("auth", cfg.AuthStates, "账户状态文件、目录或逗号分隔的多个路径")
	listenAddr := flags.String("listen", cfg.ListenAddr, "服务监听地址")
	proxy := flags.String("proxy", cfg.Proxy, "本次启动使用的 HTTP、HTTPS 或 SOCKS5 代理")
	openUI := flags.Bool("open-ui", len(args) == 0, "启动后打开管理界面")
	if err := flags.Parse(args); err != nil {
		return commandOptions{}, err
	}
	if flags.NArg() != 0 {
		return commandOptions{}, fmt.Errorf("未知参数 %q", flags.Arg(0))
	}

	cfg.AuthStates = strings.TrimSpace(*authStates)
	cfg.ListenAddr = strings.TrimSpace(*listenAddr)
	cfg.Proxy = strings.TrimSpace(*proxy)
	if err := cfg.Validate(); err != nil {
		return commandOptions{}, err
	}
	options := commandOptions{openUI: *openUI}
	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "auth":
			override := cfg.AuthStates
			options.overrides.authStates = &override
		case "proxy":
			override := cfg.Proxy
			options.overrides.proxy = &override
		}
	})
	return options, nil
}

// runServer 管理 HTTP 监听与优雅退出
func runServer(ctx context.Context, cfg config.Config, options commandOptions, manager *runtimeManager) error {
	manager.requests.log("service", "INFO", fmt.Sprintf("管理监听启动 | 地址=%s", cfg.ListenAddr))
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.ListenAddr, err)
	}
	// 管理令牌：控制面与管理页面不再按来源是否为本机放行，必须携带令牌（或 ADMIN_PASSWORD）
	tokenPath := adminTokenPath(manager.configPath)
	adminToken, err := loadAdminToken(tokenPath)
	if adminToken == "" {
		listener.Close()
		return err
	}
	if err != nil {
		manager.requests.log("service", "WARN", "管理令牌保存失败，本次运行使用临时令牌，重启后需要重新打开带令牌的地址 | "+err.Error())
	}
	// stopping 在开始优雅退出时关闭，让管理页的实时事件流主动结束，不拖住退出等待
	stopping := make(chan struct{})
	apiHandler := api.NewHandler(manager, api.Config{
		APIKey: cfg.ProxyAPIKey, APIKeyFunc: manager.activeAPIKey,
		Admin: manager, AdminPassword: cfg.AdminPassword, AdminToken: adminToken, Stopping: stopping,
	})
	if cfg.AdminPassword != "" {
		manager.requests.log("service", "INFO", "远程管理已开启 | HTTP Basic 认证 | 未配置 HTTPS 时密码为明文传输")
	}
	if cfg.ProxyAPIKey == config.DefaultProxyAPIKey {
		level := "INFO"
		if !loopbackListenAddr(cfg.ListenAddr) {
			level = "WARN"
		}
		manager.requests.log("service", level, "公开 API 正在使用默认密钥 "+config.DefaultProxyAPIKey+
			" | 默认密钥随源码公开，对外监听时请在服务配置中改为自定义密钥")
	}
	server := &http.Server{
		Handler:           rootHandler(apiHandler, cfg.AdminPassword, adminToken),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	server.RegisterOnShutdown(func() { close(stopping) })
	serveError := make(chan error, 1)
	go func() {
		serveError <- server.Serve(listener)
	}()

	// AUTO_START 为真时在后台启动生成服务；返回前取消并等待其结束，避免与 manager.Close 并发
	autoStartCtx, cancelAutoStart := context.WithCancel(ctx)
	autoStartDone := make(chan struct{})
	go func() {
		defer close(autoStartDone)
		if cfg.AutoStart {
			runAutoStart(autoStartCtx, manager)
		}
	}()
	defer func() {
		cancelAutoStart()
		<-autoStartDone
	}()

	address := browserAddress(listener.Addr().String())
	adminURL := "http://" + address + "/?admin_token=" + adminToken
	manager.requests.log("service", "INFO", "管理服务就绪 | 地址=http://"+address)
	manager.requests.log("service", "INFO", "管理页面登录地址（打开一次后浏览器记住登录；令牌保存在 "+tokenPath+"）| "+adminURL)
	if options.openUI {
		if err := openBrowser(adminURL); err != nil {
			manager.requests.log("service", "WARN", "管理页面打开失败 | "+err.Error())
		} else {
			manager.requests.log("service", "INFO", "管理页面已打开 | 地址=http://"+address)
		}
	}

	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// 正在启动生成服务（自动启动或页面点击）时立即中断启动，不必等浏览器启动完成才能退出
		cancelAutoStart()
		manager.beginShutdown()
		// 停止接收新请求，等进行中的请求完成（原先只等 10 秒，而一次请求中位要二十多秒）
		grace := shutdownGrace()
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"收到停止信号 | 停止接收新请求，等待进行中的 %d 个请求完成（最多 %s）", manager.requests.count(), grace,
		))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			manager.requests.log("service", "WARN", fmt.Sprintf(
				"等待超时，仍有 %d 个请求未完成，将被中断 | %v", manager.requests.count(), err,
			))
			return nil
		}
		manager.requests.log("service", "INFO", "进行中的请求已全部完成")
		if err := <-serveError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// rootHandler 将公开 API 与内嵌管理端挂载到同一服务
func rootHandler(apiHandler http.Handler, adminPassword string, adminToken string) http.Handler {
	root := http.NewServeMux()
	root.Handle("/health", apiHandler)
	root.Handle("/api/", apiHandler)
	root.Handle("/v1/", apiHandler)
	root.Handle("/v1beta/", apiHandler)
	root.Handle("/trace/", apiHandler)
	root.Handle("/", api.AdminPageMiddleware(adminPassword, adminToken, webui.Handler()))
	return securityHeaders(root)
}

// securityHeaders 禁止页面被其他站点嵌入并关闭 MIME 嗅探与 Referer
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Frame-Options", "DENY")
		header.Set("Content-Security-Policy", "frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// loopbackListenAddr 判断监听地址是否只绑定本机回环地址
func loopbackListenAddr(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// browserAddress 将通配监听地址转换为本机可访问地址
func browserAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// openBrowser 使用当前平台的系统命令打开管理界面
func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("打开管理界面: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("释放管理界面启动进程: %w", err)
	}
	return nil
}
