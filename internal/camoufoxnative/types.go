package camoufoxnative

import (
	"io"
	"net/http"
	"time"
)

// StartupStage 表示 Camoufox runtime 的启动阶段
type StartupStage string

const (
	// StartupPreparingBrowser 表示正在准备浏览器配置
	StartupPreparingBrowser StartupStage = "preparing_browser"
	// StartupLaunchingBrowser 表示正在启动浏览器进程
	StartupLaunchingBrowser StartupStage = "launching_browser"
	// StartupConnectingBiDi 表示正在连接 WebDriver BiDi
	StartupConnectingBiDi StartupStage = "connecting_bidi"
	// StartupLoadingAIStudio 表示正在载入 AI Studio 页面
	StartupLoadingAIStudio StartupStage = "loading_ai_studio"
	// StartupLocatingWAA 表示正在定位 WAA 服务
	StartupLocatingWAA StartupStage = "locating_waa"
	// StartupBootstrappingWAA 表示正在执行 WAA Bootstrap
	StartupBootstrappingWAA StartupStage = "bootstrapping_waa"
)

// Options 定义单个 AI Studio 账户的原生 Camoufox runtime
type Options struct {
	ExecutablePath   string
	StorageStatePath string
	Model            string
	BootstrapPrompt  string
	Locale           string
	Timezone         string
	Proxy            string
	ProxyBypass      string
	Headless         bool
	TemporaryChat    bool
	CacheDirectory   string
	ReadyTimeout     time.Duration
	Log              io.Writer
	StartupProgress  func(StartupStage)
	// CacheExpired 在启动锁定账户 HTTP 磁盘缓存、发现缓存超过 AccountCacheMaxAge 并清空时调用：size 为清空前的字节数；
	// err 非空表示清空失败：校验没有通过时什么都没删、照常使用这份缓存，只删了一部分时这次启动改用临时 profile 内的缓存
	CacheExpired func(size int64, err error)
	// CacheAttached 在启动锁定到账户 HTTP 磁盘缓存目录、这次启动的浏览器将使用它时调用（在启动浏览器之前）。
	// 同账户另一个 runtime 占用、改用临时 profile 内的缓存时不调用：这次启动失败与账户缓存无关，启动失败后的清理据此跳过
	CacheAttached func()
}

func (options Options) reportStartup(stage StartupStage) {
	if options.StartupProgress != nil {
		options.StartupProgress(stage)
	}
}

// State 返回原生 runtime 的当前页面与 bootstrap 结果
type State struct {
	PID         int
	PageURL     string
	UserAgent   string
	Platform    string
	Timezone    string
	SnapshotKey string
	Headers     http.Header
}

type storageState struct {
	Cookies []storageCookie `json:"cookies"`
	Origins []storageOrigin `json:"origins"`
}

type storageCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	HTTPOnly     bool    `json:"httpOnly"`
	Secure       bool    `json:"secure"`
	SameSite     string  `json:"sameSite"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

type storageOrigin struct {
	Origin       string             `json:"origin"`
	LocalStorage []localStorageItem `json:"localStorage"`
}

type localStorageItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
