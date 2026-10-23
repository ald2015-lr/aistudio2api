package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAuthStates         = "auth"
	defaultListenAddr         = "127.0.0.1:2048"
	defaultInitTimeout        = 2 * time.Minute
	defaultRequestTimeout     = 5 * time.Minute
	defaultPrimaryQueueWait   = 10 * time.Second
	defaultWarmWorkerLimit    = 5
	defaultMaxActiveWorkers   = 10
	defaultWarmConcurrency    = 2
	defaultAccountConcurrency = 2
	// defaultUltraWarmWorkerLimit 与 defaultUltraMaxActiveWorkers 为 Ultra 分区的常驻与峰值 Worker 数
	defaultUltraWarmWorkerLimit  = 2
	defaultUltraMaxActiveWorkers = 5
	// defaultAccountRPMLimit 为单账号每分钟生成请求上限的默认值：上游每账号每分钟 12 次（所有模型、两个通道共用），留出余量；
	// maxAccountRPMLimit 为允许配置的最大值
	defaultAccountRPMLimit = 10
	maxAccountRPMLimit     = 1000
)

// defaultUltraPrimaryModels 为 Ultra 主用模型的默认列表：Build 独有、需要订阅的模型只有 Ultra 号池能服务，
// 默认让它在 /ultra 上没有额度时立即返回 429，不在号池里挨个换号、现场冷启动
var defaultUltraPrimaryModels = []string{"gemini-4-argon"}

var configKeys = [...]string{
	"AISTUDIO_AUTH_STATES",
	"LISTEN_ADDR",
	"PROXY_API_KEY",
	"PROXY",
	"PROXY_POOL_MODE",
	"PROXY_POOL_URL",
	"PROXY_POOL_PORT_START",
	"PROXY_POOL_PORT_END",
	"INIT_TIMEOUT",
	"REQUEST_TIMEOUT",
	"FIRST_EVENT_TIMEOUT",
	"WARM_WORKER_LIMIT",
	"MAX_ACTIVE_WORKERS",
	"ULTRA_EXCLUSIVE",
	"ULTRA_INCLUDE_PRO",
	"ULTRA_WARM_WORKER_LIMIT",
	"ULTRA_MAX_ACTIVE_WORKERS",
	"WARM_STARTUP_CONCURRENCY",
	"PER_ACCOUNT_CONCURRENCY",
	"ACCOUNT_RPM_LIMIT",
	"ROUTING_STRATEGY",
	"UPSTREAM_CHANNELS",
	"STREAM_PLAYGROUND_MODELS",
	"PRIMARY_MODELS",
	"PRIMARY_QUEUE_WAIT",
	"ULTRA_PRIMARY_MODELS",
	"ULTRA_PRIMARY_QUEUE_WAIT",
	"TEMPORARY_CHAT",
	"IGNORE_CLIENT_SEED",
	"REPEAT_PROMPT_NONCE",
	"MIN_OUTPUT_TOKENS",
	"DOWNGRADE_GUARD",
	"DOWNGRADE_GUARD_MODELS",
	"DOWNGRADE_SPEED_THRESHOLD",
	"DOWNGRADE_MIN_TOKENS",
	"DOWNGRADE_MIN_WINDOW_MS",
	"DOWNGRADE_FUZZY_LOW",
	"DOWNGRADE_FUZZY_HIGH",
	"DOWNGRADE_COUNT_TIMEOUT_MS",
	"DOWNGRADE_FAST_MODE",
	"DOWNGRADE_MAX_HOLD_MS",
	"DOWNGRADE_MEMORY_MINUTES",
	"DOWNGRADE_REJECT_STATUS",
	"WAA_BACKEND",
	"AUTO_START",
	"REQUEST_BODY_LOG",
	"ADMIN_PASSWORD",
}

// upstreamChannels 表示生成请求可启用的上游通道
var upstreamChannels = []string{"playground", "build"}

// WAABackendCamoufox 表示由 Camoufox 页面承担 WAA 生命周期
const WAABackendCamoufox = "camoufox"

// WAABackendGo 表示由纯 Go VM 承担 WAA 生命周期
const WAABackendGo = "go"

// Config 保存服务的全局配置
type Config struct {
	AuthStates             string        `json:"auth_states"`
	ListenAddr             string        `json:"listen_addr"`
	ProxyAPIKey            string        `json:"proxy_api_key"`
	Proxy                  string        `json:"proxy"`
	ProxyPool              ProxyPool     `json:"proxy_pool"` // IP 池（PROXY_POOL_*，见 ProxyPool）：开启时没有自己代理的账户不再用 Proxy；保存后立即生效
	InitTimeout            time.Duration `json:"-"`
	RequestTimeout         time.Duration `json:"-"`
	WarmWorkerLimit        int           `json:"warm_worker_limit"`
	MaxActiveWorkers       int           `json:"max_active_workers"`
	WarmStartupConcurrency int           `json:"warm_startup_concurrency"`
	PerAccountConcurrency  int           `json:"per_account_concurrency"`
	// AccountRPMLimit 为单账号最近 60 秒最多发出的生成请求数（ACCOUNT_RPM_LIMIT，默认 10，0 到 1000）：满了就算忙碌，
	// 请求换到其他账号，热 Worker 都满时启动新的 Worker；只统计本进程，429 冷却仍兜底；0 表示关闭；保存后立即生效
	AccountRPMLimit  int      `json:"account_rpm_limit"`
	RoutingStrategy  string   `json:"routing_strategy"`
	UpstreamChannels []string `json:"upstream_channels"`
	// StreamPlaygroundModels 为流式请求优先走 Playground 的模型 ID（STREAM_PLAYGROUND_MODELS，逗号分隔，默认为空）：
	// 列表中的模型流式请求先不等待地取 Playground 账号，没有立即可用的就改用其他通道；为空时 Playground 与 Build 照常轮询
	StreamPlaygroundModels []string `json:"stream_playground_models"`
	// PrimaryModels 为主用模型 ID（PRIMARY_MODELS，逗号分隔，默认为空），只作用于普通号池（以及 ULTRA_EXCLUSIVE=false 时的普通路径）：
	// 列表中的模型的生成请求等空闲账号或 Worker 最多 PrimaryQueueWait，超过即按 429 返回并带 Retry-After，由客户端稍后重试，
	// 不再排队到请求超时；为空时所有模型照常排队。Ultra 号池用 UltraPrimaryModels
	PrimaryModels []string `json:"primary_models"`
	// PrimaryQueueWait 为主用模型的排队上限（PRIMARY_QUEUE_WAIT，默认 10s，未配置时不超过 REQUEST_TIMEOUT）：只计等待时间
	// （含等待冷启动名额），不计浏览器启动本身与锁等待；
	// 0 表示不排队，没有立即可用的账号或 Worker 就返回 429；不能超过 RequestTimeout
	PrimaryQueueWait time.Duration `json:"-"`
	// UltraPrimaryModels 为 Ultra 主用模型 ID（ULTRA_PRIMARY_MODELS，逗号分隔，默认 gemini-4-argon）：只作用于 Ultra 号池
	// （/ultra 请求，以及 ULTRA_EXCLUSIVE=false 时普通路径上不在 PrimaryModels 里的这些模型），排队上限为 UltraPrimaryQueueWait；
	// 这些模型没有额度时立即返回 429（候选全部冷却立即返回；额度 429 后只用已启动的 Worker，每日额度用完最多再换 2 次账号，
	// 分钟限额与限流最多再换 5 次），不在号池里挨个重试。
	// 写空值表示关闭；进程环境变量的空值不生效（与其他配置项相同），关闭只能在 .env 里写空值或在管理页清空
	UltraPrimaryModels []string `json:"ultra_primary_models"`
	// UltraPrimaryQueueWait 为 Ultra 主用模型的排队上限（ULTRA_PRIMARY_QUEUE_WAIT，规则与 PrimaryQueueWait 相同：默认 10s，
	// 未配置时不超过 REQUEST_TIMEOUT，保存时写具体值）；0 表示不排队；不能超过 RequestTimeout
	UltraPrimaryQueueWait time.Duration `json:"-"`
	TemporaryChat         bool          `json:"temporary_chat"`
	// IgnoreClientSeed 为真时忽略客户端传入的 seed，每次请求都使用随机种子
	IgnoreClientSeed bool `json:"ignore_client_seed"`
	// RepeatPromptNonce 为真时，在最后一条用户消息末尾加入不可见随机后缀（客户端指定 seed 或温度为 0 时除外）。
	// 键名沿用早期"只对重复提示词加"时的叫法
	RepeatPromptNonce bool `json:"repeat_prompt_nonce"`
	// MinOutputTokens 为最大输出 token 的下限：客户端设置的更小值会提高到它（不超过模型上限）；0 表示不调整
	MinOutputTokens int `json:"min_output_tokens"`
	// FirstEventTimeout 为每次尝试等待首个上游事件的上限：超时只取消这一次尝试并按超时换号重试；0 表示关闭（默认），
	// 思考很长的模型可能很久才有第一个事件
	FirstEventTimeout time.Duration `json:"-"`
	// UltraExclusive 为真（默认）时 Ultra 号池的账户只服务 /ultra 前缀的请求，普通路径只用其余账户；为假时普通路径也可以用 Ultra 号池的账户
	UltraExclusive bool `json:"ultra_exclusive"`
	// UltraIncludePro 为真时权益为 Pro 的账户并入 Ultra 号池（付费号池）与 Ultra Worker 分区，按全部 ULTRA_* 设置调度；
	// 账户权益本身不变（权益请求头、输入上限、模型资格、更高权益重试仍按 Pro）。默认 false，保存后立即生效
	UltraIncludePro bool `json:"ultra_include_pro"`
	// UltraWarmWorkerLimit 与 UltraMaxActiveWorkers 为 Ultra 分区（Ultra 号池的账户）的常驻与峰值 Worker 数，
	// 与普通分区分开计算（WarmWorkerLimit、MaxActiveWorkers 只约束普通分区），浏览器总数最多为两个分区峰值之和；
	// 常驻数可以为 0（只按需启动）
	UltraWarmWorkerLimit  int `json:"ultra_warm_worker_limit"`
	UltraMaxActiveWorkers int `json:"ultra_max_active_workers"`
	// DowngradeGuard 为降级判定（拒绝被上游降级的回复）的设置，见 DowngradeGuard
	DowngradeGuard DowngradeGuard `json:"downgrade_guard"`
	WAABackend     string         `json:"waa_backend"`
	AutoStart      bool           `json:"auto_start"`
	// RequestBodyLog 为真时用量账本同时保存公开 API POST 请求与响应的截断正文（明文保存在本机，默认关闭）
	RequestBodyLog bool   `json:"request_body_log"`
	AdminPassword  string `json:"-"`
}

// DefaultProxyAPIKey 为没有设置 PROXY_API_KEY 时使用的公开 API 密钥。公开 API 始终要求密钥，
// 留空不再表示免密钥访问
const DefaultProxyAPIKey = "sk-onechat-fun-fun"

// EffectiveProxyAPIKey 返回实际生效的公开 API 密钥：空值使用默认密钥
func EffectiveProxyAPIKey(value string) string {
	if key := strings.TrimSpace(value); key != "" {
		return key
	}
	return DefaultProxyAPIKey
}

// Default 返回可直接启动的默认配置
func Default() Config {
	return Config{
		AuthStates:             defaultAuthStates,
		ListenAddr:             defaultListenAddr,
		ProxyAPIKey:            DefaultProxyAPIKey,
		ProxyPool:              DefaultProxyPool(),
		InitTimeout:            defaultInitTimeout,
		RequestTimeout:         defaultRequestTimeout,
		WarmWorkerLimit:        defaultWarmWorkerLimit,
		MaxActiveWorkers:       defaultMaxActiveWorkers,
		UltraExclusive:         true,
		UltraIncludePro:        false,
		UltraWarmWorkerLimit:   defaultUltraWarmWorkerLimit,
		UltraMaxActiveWorkers:  defaultUltraMaxActiveWorkers,
		WarmStartupConcurrency: defaultWarmConcurrency,
		PerAccountConcurrency:  defaultAccountConcurrency,
		AccountRPMLimit:        defaultAccountRPMLimit,
		RoutingStrategy:        "round-robin",
		UpstreamChannels:       append([]string(nil), upstreamChannels...),
		PrimaryQueueWait:       defaultPrimaryQueueWait,
		UltraPrimaryModels:     slices.Clone(defaultUltraPrimaryModels),
		UltraPrimaryQueueWait:  defaultPrimaryQueueWait,
		WAABackend:             WAABackendCamoufox,
		RepeatPromptNonce:      true,
		MinOutputTokens:        defaultMinOutputTokens,
		DowngradeGuard:         DefaultDowngradeGuard(),
		AutoStart:              true,
	}
}

// Load 从指定 env 文件和进程环境加载配置
func Load(path string) (Config, error) {
	values, err := readEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	// 进程环境变量优先于 .env；空值视为未设置，避免 docker compose 的 ${VAR} 在宿主机没有设置时把 .env 里的值清空
	for _, key := range configKeys {
		if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}

	cfg := Default()
	if value, ok := values["AISTUDIO_AUTH_STATES"]; ok {
		cfg.AuthStates = strings.TrimSpace(value)
	}
	if value, ok := values["LISTEN_ADDR"]; ok {
		cfg.ListenAddr = strings.TrimSpace(value)
	}
	if value, ok := values["PROXY_API_KEY"]; ok {
		cfg.ProxyAPIKey = EffectiveProxyAPIKey(value)
	}
	if value, ok := values["PROXY"]; ok {
		// 手写的 host:port:用户名:密码 等写法在这里改写成规范 URL，保存侧与生效侧都从这里来；无法规范化的值留给 Validate 报错
		cfg.Proxy = CanonicalProxy(value)
	}
	if err := loadProxyPool(values, &cfg.ProxyPool); err != nil {
		return Config{}, err
	}
	if value, ok := values["INIT_TIMEOUT"]; ok {
		cfg.InitTimeout, err = parsePositiveDuration("INIT_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["REQUEST_TIMEOUT"]; ok {
		cfg.RequestTimeout, err = parsePositiveDuration("REQUEST_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["FIRST_EVENT_TIMEOUT"]; ok && strings.TrimSpace(value) != "" {
		cfg.FirstEventTimeout, err = parseOptionalDuration("FIRST_EVENT_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["WARM_WORKER_LIMIT"]; ok {
		cfg.WarmWorkerLimit, err = parsePositiveInt("WARM_WORKER_LIMIT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["MAX_ACTIVE_WORKERS"]; ok {
		cfg.MaxActiveWorkers, err = parsePositiveInt("MAX_ACTIVE_WORKERS", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["ULTRA_EXCLUSIVE"]; ok && strings.TrimSpace(value) != "" {
		cfg.UltraExclusive, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("ULTRA_EXCLUSIVE 必须是 true 或 false")
		}
	}
	if value, ok := values["ULTRA_INCLUDE_PRO"]; ok && strings.TrimSpace(value) != "" {
		cfg.UltraIncludePro, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("ULTRA_INCLUDE_PRO 必须是 true 或 false")
		}
	}
	if value, ok := values["ULTRA_WARM_WORKER_LIMIT"]; ok && strings.TrimSpace(value) != "" {
		cfg.UltraWarmWorkerLimit, err = parseNonNegativeInt("ULTRA_WARM_WORKER_LIMIT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["ULTRA_MAX_ACTIVE_WORKERS"]; ok && strings.TrimSpace(value) != "" {
		cfg.UltraMaxActiveWorkers, err = parsePositiveInt("ULTRA_MAX_ACTIVE_WORKERS", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["WARM_STARTUP_CONCURRENCY"]; ok {
		cfg.WarmStartupConcurrency, err = parsePositiveInt("WARM_STARTUP_CONCURRENCY", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["PER_ACCOUNT_CONCURRENCY"]; ok {
		cfg.PerAccountConcurrency, err = parsePositiveInt("PER_ACCOUNT_CONCURRENCY", value)
		if err != nil {
			return Config{}, err
		}
	}
	// 空值沿用默认值，关闭必须写 0
	if value, ok := values["ACCOUNT_RPM_LIMIT"]; ok && strings.TrimSpace(value) != "" {
		cfg.AccountRPMLimit, err = parseNonNegativeInt("ACCOUNT_RPM_LIMIT", value)
		if err != nil {
			return Config{}, err
		}
	}
	if value, ok := values["ROUTING_STRATEGY"]; ok {
		cfg.RoutingStrategy = strings.TrimSpace(value)
	}
	if value, ok := values["UPSTREAM_CHANNELS"]; ok {
		cfg.UpstreamChannels = ParseUpstreamChannels(value)
	}
	if value, ok := values["STREAM_PLAYGROUND_MODELS"]; ok {
		cfg.StreamPlaygroundModels = ParseModelList(value)
	}
	if value, ok := values["PRIMARY_MODELS"]; ok {
		cfg.PrimaryModels = ParseModelList(value)
	}
	if value, ok := values["PRIMARY_QUEUE_WAIT"]; ok && strings.TrimSpace(value) != "" {
		cfg.PrimaryQueueWait, err = parseOptionalDuration("PRIMARY_QUEUE_WAIT", value)
		if err != nil {
			return Config{}, err
		}
	} else {
		cfg.PrimaryQueueWait = defaultPrimaryQueueWaitFor(cfg.RequestTimeout)
	}
	// Ultra 主用模型：键存在时按写的值（空值表示关闭），键不存在时保持默认列表。进程环境变量的空值在上面已被忽略，
	// 只能经 .env 的空值或管理页清空来关闭
	if value, ok := values["ULTRA_PRIMARY_MODELS"]; ok {
		cfg.UltraPrimaryModels = ParseModelList(value)
	}
	if value, ok := values["ULTRA_PRIMARY_QUEUE_WAIT"]; ok && strings.TrimSpace(value) != "" {
		cfg.UltraPrimaryQueueWait, err = parseOptionalDuration("ULTRA_PRIMARY_QUEUE_WAIT", value)
		if err != nil {
			return Config{}, err
		}
	} else {
		cfg.UltraPrimaryQueueWait = defaultPrimaryQueueWaitFor(cfg.RequestTimeout)
	}
	if value, ok := values["TEMPORARY_CHAT"]; ok {
		cfg.TemporaryChat, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("TEMPORARY_CHAT 必须是 true 或 false")
		}
	}
	if value, ok := values["IGNORE_CLIENT_SEED"]; ok && strings.TrimSpace(value) != "" {
		cfg.IgnoreClientSeed, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("IGNORE_CLIENT_SEED 必须是 true 或 false")
		}
	}
	if value, ok := values["REPEAT_PROMPT_NONCE"]; ok && strings.TrimSpace(value) != "" {
		cfg.RepeatPromptNonce, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("REPEAT_PROMPT_NONCE 必须是 true 或 false")
		}
	}
	if value, ok := values["MIN_OUTPUT_TOKENS"]; ok && strings.TrimSpace(value) != "" {
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
		if parseErr != nil || parsed < 0 {
			return Config{}, fmt.Errorf("MIN_OUTPUT_TOKENS 必须是 0 或正整数")
		}
		cfg.MinOutputTokens = parsed
	}
	if err := loadDowngradeGuard(values, &cfg.DowngradeGuard); err != nil {
		return Config{}, err
	}
	if value, ok := values["WAA_BACKEND"]; ok {
		cfg.WAABackend = strings.ToLower(strings.TrimSpace(value))
	}
	if value, ok := values["AUTO_START"]; ok && strings.TrimSpace(value) != "" {
		cfg.AutoStart, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("AUTO_START 必须是 true 或 false")
		}
	}
	if value, ok := values["REQUEST_BODY_LOG"]; ok && strings.TrimSpace(value) != "" {
		cfg.RequestBodyLog, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("REQUEST_BODY_LOG 必须是 true 或 false")
		}
	}
	if value, ok := values["ADMIN_PASSWORD"]; ok {
		cfg.AdminPassword = strings.TrimSpace(value)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save 将配置原子写入指定 env 文件
func (c Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	values := map[string]string{
		"AISTUDIO_AUTH_STATES":     c.AuthStates,
		"LISTEN_ADDR":              c.ListenAddr,
		"PROXY_API_KEY":            c.ProxyAPIKey,
		"PROXY":                    c.Proxy,
		"INIT_TIMEOUT":             c.InitTimeout.String(),
		"REQUEST_TIMEOUT":          c.RequestTimeout.String(),
		"FIRST_EVENT_TIMEOUT":      c.FirstEventTimeout.String(),
		"WARM_WORKER_LIMIT":        strconv.Itoa(c.WarmWorkerLimit),
		"MAX_ACTIVE_WORKERS":       strconv.Itoa(c.MaxActiveWorkers),
		"ULTRA_EXCLUSIVE":          strconv.FormatBool(c.UltraExclusive),
		"ULTRA_INCLUDE_PRO":        strconv.FormatBool(c.UltraIncludePro),
		"ULTRA_WARM_WORKER_LIMIT":  strconv.Itoa(c.UltraWarmWorkerLimit),
		"ULTRA_MAX_ACTIVE_WORKERS": strconv.Itoa(c.UltraMaxActiveWorkers),
		"WARM_STARTUP_CONCURRENCY": strconv.Itoa(c.WarmStartupConcurrency),
		"PER_ACCOUNT_CONCURRENCY":  strconv.Itoa(c.PerAccountConcurrency),
		"ACCOUNT_RPM_LIMIT":        strconv.Itoa(c.AccountRPMLimit),
		"ROUTING_STRATEGY":         c.RoutingStrategy,
		"UPSTREAM_CHANNELS":        strings.Join(c.UpstreamChannels, ","),
		"STREAM_PLAYGROUND_MODELS": strings.Join(c.StreamPlaygroundModels, ","),
		"PRIMARY_MODELS":           strings.Join(c.PrimaryModels, ","),
		"PRIMARY_QUEUE_WAIT":       c.PrimaryQueueWait.String(),
		"ULTRA_PRIMARY_MODELS":     strings.Join(c.UltraPrimaryModels, ","),
		"ULTRA_PRIMARY_QUEUE_WAIT": c.UltraPrimaryQueueWait.String(),
		"TEMPORARY_CHAT":           strconv.FormatBool(c.TemporaryChat),
		"IGNORE_CLIENT_SEED":       strconv.FormatBool(c.IgnoreClientSeed),
		"REPEAT_PROMPT_NONCE":      strconv.FormatBool(c.RepeatPromptNonce),
		"MIN_OUTPUT_TOKENS":        strconv.Itoa(c.MinOutputTokens),
		"WAA_BACKEND":              c.WAABackend,
		"AUTO_START":               strconv.FormatBool(c.AutoStart),
		"REQUEST_BODY_LOG":         strconv.FormatBool(c.RequestBodyLog),
		"ADMIN_PASSWORD":           c.AdminPassword,
	}
	for key, value := range c.DowngradeGuard.envValues() {
		values[key] = value
	}
	for key, value := range c.ProxyPool.envValues() {
		values[key] = value
	}

	var output strings.Builder
	for _, key := range configKeys {
		output.WriteString(key)
		output.WriteByte('=')
		output.WriteString(formatEnvValue(values[key]))
		output.WriteByte('\n')
	}
	return atomicWrite(path, []byte(output.String()), 0o600)
}

// Validate 校验配置值是否能用于服务启动
func (c Config) Validate() error {
	if strings.TrimSpace(c.AuthStates) == "" {
		return fmt.Errorf("AISTUDIO_AUTH_STATES 不能为空")
	}
	if err := validateListenAddr(c.ListenAddr); err != nil {
		return err
	}
	if err := ValidateProxy(c.Proxy); err != nil {
		return err
	}
	if c.InitTimeout <= 0 {
		return fmt.Errorf("INIT_TIMEOUT 必须是正数时长")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("REQUEST_TIMEOUT 必须是正数时长")
	}
	if c.FirstEventTimeout < 0 {
		return fmt.Errorf("FIRST_EVENT_TIMEOUT 必须是 0 或正数时长")
	}
	if c.FirstEventTimeout > 0 && c.FirstEventTimeout >= c.RequestTimeout {
		return fmt.Errorf("FIRST_EVENT_TIMEOUT 必须小于 REQUEST_TIMEOUT")
	}
	if c.WarmWorkerLimit <= 0 {
		return fmt.Errorf("WARM_WORKER_LIMIT 必须是正整数")
	}
	if c.MaxActiveWorkers < c.WarmWorkerLimit {
		return fmt.Errorf("MAX_ACTIVE_WORKERS 必须大于或等于 WARM_WORKER_LIMIT")
	}
	if c.UltraWarmWorkerLimit < 0 {
		return fmt.Errorf("ULTRA_WARM_WORKER_LIMIT 必须是 0 或正整数")
	}
	if c.UltraMaxActiveWorkers <= 0 {
		return fmt.Errorf("ULTRA_MAX_ACTIVE_WORKERS 必须是正整数")
	}
	if c.UltraMaxActiveWorkers < c.UltraWarmWorkerLimit {
		return fmt.Errorf("ULTRA_MAX_ACTIVE_WORKERS 必须大于或等于 ULTRA_WARM_WORKER_LIMIT")
	}
	if c.WarmStartupConcurrency <= 0 || c.WarmStartupConcurrency > c.WarmWorkerLimit {
		return fmt.Errorf("WARM_STARTUP_CONCURRENCY 必须是 1 到 WARM_WORKER_LIMIT")
	}
	if c.PerAccountConcurrency <= 0 {
		return fmt.Errorf("PER_ACCOUNT_CONCURRENCY 必须是正整数")
	}
	if c.AccountRPMLimit < 0 || c.AccountRPMLimit > maxAccountRPMLimit {
		return fmt.Errorf("ACCOUNT_RPM_LIMIT 必须是 0 到 %d 的整数", maxAccountRPMLimit)
	}
	if c.MinOutputTokens < 0 {
		return fmt.Errorf("MIN_OUTPUT_TOKENS 必须是 0 或正整数")
	}
	if c.RoutingStrategy != "round-robin" && c.RoutingStrategy != "fill-first" {
		return fmt.Errorf("ROUTING_STRATEGY 必须是 round-robin 或 fill-first")
	}
	if err := validateUpstreamChannels(c.UpstreamChannels); err != nil {
		return err
	}
	if err := validateModelList("STREAM_PLAYGROUND_MODELS", c.StreamPlaygroundModels); err != nil {
		return err
	}
	if err := validateModelList("PRIMARY_MODELS", c.PrimaryModels); err != nil {
		return err
	}
	if c.PrimaryQueueWait < 0 {
		return fmt.Errorf("PRIMARY_QUEUE_WAIT 必须是 0 或正数时长")
	}
	if c.PrimaryQueueWait > c.RequestTimeout {
		return fmt.Errorf("PRIMARY_QUEUE_WAIT 不能超过 REQUEST_TIMEOUT")
	}
	if err := validateModelList("ULTRA_PRIMARY_MODELS", c.UltraPrimaryModels); err != nil {
		return err
	}
	if c.UltraPrimaryQueueWait < 0 {
		return fmt.Errorf("ULTRA_PRIMARY_QUEUE_WAIT 必须是 0 或正数时长")
	}
	if c.UltraPrimaryQueueWait > c.RequestTimeout {
		return fmt.Errorf("ULTRA_PRIMARY_QUEUE_WAIT 不能超过 REQUEST_TIMEOUT")
	}
	if c.WAABackend != WAABackendCamoufox && c.WAABackend != WAABackendGo {
		return fmt.Errorf("WAA_BACKEND 必须是 camoufox 或 go")
	}
	if err := c.DowngradeGuard.Validate(); err != nil {
		return err
	}
	if err := c.ProxyPool.Validate(); err != nil {
		return err
	}
	return nil
}

// defaultPrimaryQueueWaitFor 返回没有配置 PRIMARY_QUEUE_WAIT 时的排队上限：默认 10s，但不超过请求超时。
// 排队上限不能超过 REQUEST_TIMEOUT，旧配置的请求超时短于 10s 时若固定取 10s，升级后 .env 与保存的 JSON 都无法载入，整个服务起不来
func defaultPrimaryQueueWaitFor(requestTimeout time.Duration) time.Duration {
	if requestTimeout > 0 && requestTimeout < defaultPrimaryQueueWait {
		return requestTimeout
	}
	return defaultPrimaryQueueWait
}

// ParseUpstreamChannels 把逗号分隔的通道列表拆成去空白的小写名称
func ParseUpstreamChannels(value string) []string {
	var channels []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			channels = append(channels, item)
		}
	}
	return channels
}

// validateUpstreamChannels 要求至少一个已知且不重复的上游通道
func validateUpstreamChannels(channels []string) error {
	if len(channels) == 0 {
		return fmt.Errorf("UPSTREAM_CHANNELS 至少包含 playground 或 build")
	}
	seen := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		known := false
		for _, candidate := range upstreamChannels {
			known = known || channel == candidate
		}
		if !known {
			return fmt.Errorf("UPSTREAM_CHANNELS 只能包含 playground 与 build")
		}
		if _, exists := seen[channel]; exists {
			return fmt.Errorf("UPSTREAM_CHANNELS 通道 %s 重复", channel)
		}
		seen[channel] = struct{}{}
	}
	return nil
}

// MarshalJSON 将时长输出为 env 使用的文本格式
func (c Config) MarshalJSON() ([]byte, error) {
	// Ultra 主用模型为空时写空数组而不是 null：解析时 null 与缺字段一样取默认列表，清空后必须能原样往返
	ultraPrimaryModels := c.UltraPrimaryModels
	if ultraPrimaryModels == nil {
		ultraPrimaryModels = []string{}
	}
	type payload struct {
		AuthStates             string         `json:"auth_states"`
		ListenAddr             string         `json:"listen_addr"`
		ProxyAPIKey            string         `json:"proxy_api_key"`
		Proxy                  string         `json:"proxy"`
		ProxyPool              ProxyPool      `json:"proxy_pool"`
		InitTimeout            string         `json:"init_timeout"`
		RequestTimeout         string         `json:"request_timeout"`
		FirstEventTimeout      string         `json:"first_event_timeout"`
		WarmWorkerLimit        int            `json:"warm_worker_limit"`
		MaxActiveWorkers       int            `json:"max_active_workers"`
		UltraExclusive         bool           `json:"ultra_exclusive"`
		UltraIncludePro        bool           `json:"ultra_include_pro"`
		UltraWarmWorkerLimit   int            `json:"ultra_warm_worker_limit"`
		UltraMaxActiveWorkers  int            `json:"ultra_max_active_workers"`
		WarmStartupConcurrency int            `json:"warm_startup_concurrency"`
		PerAccountConcurrency  int            `json:"per_account_concurrency"`
		AccountRPMLimit        int            `json:"account_rpm_limit"`
		RoutingStrategy        string         `json:"routing_strategy"`
		UpstreamChannels       []string       `json:"upstream_channels"`
		StreamPlaygroundModels []string       `json:"stream_playground_models"`
		PrimaryModels          []string       `json:"primary_models"`
		PrimaryQueueWait       string         `json:"primary_queue_wait"`
		UltraPrimaryModels     []string       `json:"ultra_primary_models"`
		UltraPrimaryQueueWait  string         `json:"ultra_primary_queue_wait"`
		TemporaryChat          bool           `json:"temporary_chat"`
		IgnoreClientSeed       bool           `json:"ignore_client_seed"`
		RepeatPromptNonce      bool           `json:"repeat_prompt_nonce"`
		MinOutputTokens        int            `json:"min_output_tokens"`
		DowngradeGuard         DowngradeGuard `json:"downgrade_guard"`
		WAABackend             string         `json:"waa_backend"`
	}
	return json.Marshal(payload{
		AuthStates:             c.AuthStates,
		ListenAddr:             c.ListenAddr,
		ProxyAPIKey:            c.ProxyAPIKey,
		Proxy:                  c.Proxy,
		ProxyPool:              c.ProxyPool.withDefaults(),
		InitTimeout:            c.InitTimeout.String(),
		RequestTimeout:         c.RequestTimeout.String(),
		FirstEventTimeout:      c.FirstEventTimeout.String(),
		WarmWorkerLimit:        c.WarmWorkerLimit,
		MaxActiveWorkers:       c.MaxActiveWorkers,
		UltraExclusive:         c.UltraExclusive,
		UltraIncludePro:        c.UltraIncludePro,
		UltraWarmWorkerLimit:   c.UltraWarmWorkerLimit,
		UltraMaxActiveWorkers:  c.UltraMaxActiveWorkers,
		WarmStartupConcurrency: c.WarmStartupConcurrency,
		PerAccountConcurrency:  c.PerAccountConcurrency,
		AccountRPMLimit:        c.AccountRPMLimit,
		RoutingStrategy:        c.RoutingStrategy,
		UpstreamChannels:       c.UpstreamChannels,
		StreamPlaygroundModels: c.StreamPlaygroundModels,
		PrimaryModels:          c.PrimaryModels,
		PrimaryQueueWait:       c.PrimaryQueueWait.String(),
		UltraPrimaryModels:     ultraPrimaryModels,
		UltraPrimaryQueueWait:  c.UltraPrimaryQueueWait.String(),
		TemporaryChat:          c.TemporaryChat,
		IgnoreClientSeed:       c.IgnoreClientSeed,
		RepeatPromptNonce:      c.RepeatPromptNonce,
		MinOutputTokens:        c.MinOutputTokens,
		DowngradeGuard:         c.DowngradeGuard,
		WAABackend:             c.WAABackend,
	})
}

// UnmarshalJSON 从管理接口使用的文本时长解析配置
func (c *Config) UnmarshalJSON(data []byte) error {
	type payload struct {
		AuthStates             string          `json:"auth_states"`
		ListenAddr             string          `json:"listen_addr"`
		ProxyAPIKey            string          `json:"proxy_api_key"`
		Proxy                  string          `json:"proxy"`
		ProxyPool              *ProxyPool      `json:"proxy_pool"`
		InitTimeout            string          `json:"init_timeout"`
		RequestTimeout         string          `json:"request_timeout"`
		FirstEventTimeout      string          `json:"first_event_timeout"`
		WarmWorkerLimit        int             `json:"warm_worker_limit"`
		MaxActiveWorkers       int             `json:"max_active_workers"`
		UltraExclusive         *bool           `json:"ultra_exclusive"`
		UltraIncludePro        *bool           `json:"ultra_include_pro"`
		UltraWarmWorkerLimit   *int            `json:"ultra_warm_worker_limit"`
		UltraMaxActiveWorkers  *int            `json:"ultra_max_active_workers"`
		WarmStartupConcurrency int             `json:"warm_startup_concurrency"`
		PerAccountConcurrency  int             `json:"per_account_concurrency"`
		AccountRPMLimit        *int            `json:"account_rpm_limit"`
		RoutingStrategy        string          `json:"routing_strategy"`
		UpstreamChannels       []string        `json:"upstream_channels"`
		StreamPlaygroundModels []string        `json:"stream_playground_models"`
		PrimaryModels          []string        `json:"primary_models"`
		PrimaryQueueWait       string          `json:"primary_queue_wait"`
		UltraPrimaryModels     *[]string       `json:"ultra_primary_models"`
		UltraPrimaryQueueWait  string          `json:"ultra_primary_queue_wait"`
		TemporaryChat          bool            `json:"temporary_chat"`
		IgnoreClientSeed       bool            `json:"ignore_client_seed"`
		RepeatPromptNonce      *bool           `json:"repeat_prompt_nonce"`
		MinOutputTokens        *int            `json:"min_output_tokens"`
		DowngradeGuard         *DowngradeGuard `json:"downgrade_guard"`
		WAABackend             string          `json:"waa_backend"`
	}
	var value payload
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	initTimeout, err := parsePositiveDuration("INIT_TIMEOUT", value.InitTimeout)
	if err != nil {
		return err
	}
	requestTimeout, err := parsePositiveDuration("REQUEST_TIMEOUT", value.RequestTimeout)
	if err != nil {
		return err
	}
	// 旧版数据没有该字段时按关闭处理；没有 Ultra 字段时按默认值处理（ULTRA_INCLUDE_PRO 默认关闭）；没有主用模型排队上限时按默认 10s 处理；
	// 没有单账号每分钟上限时按默认 10 处理（显式 0 表示关闭）
	var firstEventTimeout time.Duration
	if strings.TrimSpace(value.FirstEventTimeout) != "" {
		if firstEventTimeout, err = parseOptionalDuration("FIRST_EVENT_TIMEOUT", value.FirstEventTimeout); err != nil {
			return err
		}
	}
	primaryQueueWait := defaultPrimaryQueueWaitFor(requestTimeout)
	if strings.TrimSpace(value.PrimaryQueueWait) != "" {
		if primaryQueueWait, err = parseOptionalDuration("PRIMARY_QUEUE_WAIT", value.PrimaryQueueWait); err != nil {
			return err
		}
	}
	// Ultra 主用模型：旧版数据没有该字段（或为 null）时取默认列表，显式的空数组表示关闭；排队上限缺省与普通主用模型同规则
	ultraPrimaryModels := slices.Clone(defaultUltraPrimaryModels)
	if value.UltraPrimaryModels != nil {
		ultraPrimaryModels = NormalizeModelList(*value.UltraPrimaryModels)
	}
	ultraPrimaryQueueWait := defaultPrimaryQueueWaitFor(requestTimeout)
	if strings.TrimSpace(value.UltraPrimaryQueueWait) != "" {
		if ultraPrimaryQueueWait, err = parseOptionalDuration("ULTRA_PRIMARY_QUEUE_WAIT", value.UltraPrimaryQueueWait); err != nil {
			return err
		}
	}
	parsed := Config{
		AuthStates:             strings.TrimSpace(value.AuthStates),
		ListenAddr:             strings.TrimSpace(value.ListenAddr),
		ProxyAPIKey:            EffectiveProxyAPIKey(value.ProxyAPIKey),
		Proxy:                  CanonicalProxy(value.Proxy),
		ProxyPool:              proxyPoolOrDefault(value.ProxyPool),
		InitTimeout:            initTimeout,
		RequestTimeout:         requestTimeout,
		FirstEventTimeout:      firstEventTimeout,
		WarmWorkerLimit:        value.WarmWorkerLimit,
		MaxActiveWorkers:       value.MaxActiveWorkers,
		UltraExclusive:         value.UltraExclusive == nil || *value.UltraExclusive,
		UltraIncludePro:        value.UltraIncludePro != nil && *value.UltraIncludePro,
		UltraWarmWorkerLimit:   intOrDefault(value.UltraWarmWorkerLimit, defaultUltraWarmWorkerLimit),
		UltraMaxActiveWorkers:  intOrDefault(value.UltraMaxActiveWorkers, defaultUltraMaxActiveWorkers),
		WarmStartupConcurrency: value.WarmStartupConcurrency,
		PerAccountConcurrency:  value.PerAccountConcurrency,
		AccountRPMLimit:        intOrDefault(value.AccountRPMLimit, defaultAccountRPMLimit),
		RoutingStrategy:        value.RoutingStrategy,
		UpstreamChannels:       value.UpstreamChannels,
		StreamPlaygroundModels: NormalizeModelList(value.StreamPlaygroundModels),
		PrimaryModels:          NormalizeModelList(value.PrimaryModels),
		PrimaryQueueWait:       primaryQueueWait,
		UltraPrimaryModels:     ultraPrimaryModels,
		UltraPrimaryQueueWait:  ultraPrimaryQueueWait,
		TemporaryChat:          value.TemporaryChat,
		IgnoreClientSeed:       value.IgnoreClientSeed,
		RepeatPromptNonce:      value.RepeatPromptNonce == nil || *value.RepeatPromptNonce,
		MinOutputTokens:        intOrDefault(value.MinOutputTokens, defaultMinOutputTokens),
		DowngradeGuard:         downgradeGuardOrDefault(value.DowngradeGuard),
		WAABackend:             strings.ToLower(strings.TrimSpace(value.WAABackend)),
	}
	if err := parsed.Validate(); err != nil {
		return err
	}
	*c = parsed
	return nil
}

// 代理规范化的错误文案：全部是固定文本，不含任何输入内容（输入里可能有账号密码）
var (
	errProxyFormat           = errors.New("PROXY 格式无效：支持 http://host:port、http://用户名:密码@host:port、socks5://用户名:密码@host:port、host:port:用户名:密码、用户名:密码@host:port（不写协议按 http）")
	errProxyScheme           = errors.New("PROXY 协议必须是 http、https 或 socks5（socks5h 按 socks5 处理）")
	errProxyPath             = errors.New("PROXY 不能包含路径、查询参数或片段")
	errProxyPort             = errors.New("PROXY 端口必须是 1 到 65535 的数字")
	errProxySOCKS5Port       = errors.New("SOCKS5 代理必须写端口")
	errProxyNoPort           = errors.New("PROXY 不写协议时必须写端口，例如 1.2.3.4:8080")
	errProxyUser             = errors.New("PROXY 的用户名不能为空")
	errProxyControl          = errors.New("PROXY 不能包含换行、制表符等控制字符")
	errProxySOCKS5Length     = errors.New("SOCKS5 代理的用户名和密码都不能超过 255 字节")
	errProxyCredentialLength = errors.New("PROXY 的用户名和密码合计不能超过 1024 字节")
	errProxyUserColon        = errors.New("HTTP 代理的用户名不能包含冒号（:），Basic 认证会在第一个冒号处拆开用户名和密码")
	errProxyAmbiguous        = errors.New("PROXY 无法判断账号密码与 host:port 的先后（host:port:用户名:密码、host:port@用户名:密码 还是 用户名:密码:host:port），请改用 http://用户名:密码@host:port 写法；代理主机名不含点时请写成 IP")
	errProxyIPv6             = errors.New("PROXY 的 IPv6 地址必须加方括号，例如 http://[2001:db8::1]:8080")
	errProxyPathCredential   = errors.New("PROXY 不能包含路径、查询参数或片段；密码里有 / ? # 时请写成 %2F、%3F、%23，或改用 host:port:用户名:密码 写法")
)

// unrecognizedProxy 为 RedactProxy 无法识别代理时的占位文本
const unrecognizedProxy = "<无法识别的代理>"

// proxySchemePattern 匹配 URL 的协议名（RFC 3986：字母开头，后面是字母、数字与 + - .）
var proxySchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// colonProxyPattern 匹配 host:port:用户名:密码（代理商面板最常见的写法），密码里可以有 : 和 @；IPv6 必须写成 [v6]
var colonProxyPattern = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[^:@\[\]/]+):([0-9]{1,5}):(.*)$`)

// hostPortLikePattern 匹配像 host:port 的 用户名:密码 段：IPv4、带点的域名或 [v6]，加数字端口。host 段又不像主机时
// （不是 IP、不含点），整串更像把账号密码与 host:port 写反了，数字密码会被当成端口、真实密码会进日志，所以报错
var hostPortLikePattern = regexp.MustCompile(`^(?:\[[0-9A-Fa-f:.]+\]|(?:[A-Za-z0-9_-]|[^\x00-\x7F])+(?:\.(?:[A-Za-z0-9_-]|[^\x00-\x7F])+)+):[0-9]{1,5}/?$`)

// NormalizeProxy 把代理写法统一成上游 URL：不带认证的合法 URL 只去首尾空白、原样返回；带账号密码或简写的写法
// 改写成 scheme://用户名:密码@host:port（账号密码按百分号编码，只保留字母、数字与 - . _ ~）。
// 函数幂等；错误文案不含输入内容（url.Parse 的错误里有原文和密码，不能用 %w 包装）
func NormalizeProxy(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", errProxyControl
		}
	}
	scheme, rest, explicit := "http", value, false
	// :// 前面是合法的协议名才算写了协议：不写协议的 host:port:用户名:密码 与 用户名:密码@host:port，密码里可以有 ://
	if index := strings.Index(value, "://"); index >= 0 && proxySchemePattern.MatchString(value[:index]) {
		scheme, rest, explicit = strings.ToLower(value[:index]), value[index+3:], true
		// 带协议的 host:port:用户名:密码 先按冒号写法处理，不走 url.Parse 的判定：密码是纯数字或为空时 url.Parse 会把它读成
		// 主机名带冒号的无认证 URL（http://h:1:u:123 的主机名是 h:1:u），密码含 / ? # 时又会读成带路径或查询的 URL。
		// 这类值主机名里有冒号，原来也连不上，不属于需要原样保留的旧值。
		// 例外是不加方括号的 IPv6（http://2001:db8::1:3128）与用户名为空的值（http://h:1::2，见 colonProxyCredentials）：
		// 原规则原样接受，这里照旧走直通分支
		if !colonProxyCredentials(rest) || bareIPv6URL(value) {
			// 不带认证、原规则就接受的 URL 原样返回：Firefox prefs、Go 传输与 chromeauth 拿到的字符串与改动前逐字节一致
			if handled, err := legacyProxyValid(value); handled {
				if err != nil {
					if pathHidesCredentials(rest) {
						return "", errProxyPathCredential
					}
					return "", err
				}
				return value, nil
			}
			if parsed, err := url.Parse(value); err == nil && (parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "") {
				if pathHidesCredentials(rest) {
					return "", errProxyPathCredential
				}
				return "", errProxyPath
			}
		}
	}
	switch scheme {
	case "http", "https", "socks5":
	case "socks5h":
		scheme = "socks5"
	default:
		return "", errProxyScheme
	}
	var user, password, hostPort string
	hasUser, hasPassword, atForm := false, false, false
	if !strings.Contains(rest, "@") && bareIPv6(strings.TrimSuffix(rest, "/")) {
		// 2001:db8::1:8080 不能按冒号写法拆成 host 2001、端口 db8……
		return "", errProxyIPv6
	}
	if match := colonProxyPattern.FindStringSubmatch(rest); match != nil {
		// host:port:用户名:密码：第 3 个冒号之后全部算密码，两段都按字面处理；不去掉末尾的 /，它可能属于密码
		credentials := strings.SplitN(match[3], ":", 2)
		if len(credentials) != 2 {
			return "", errProxyFormat
		}
		if ambiguousProxyOrder(match[1], match[3]) {
			return "", errProxyAmbiguous
		}
		hostPort = match[1] + ":" + match[2]
		user, password, hasUser, hasPassword = credentials[0], credentials[1], true, true
	} else if rest = strings.TrimSuffix(rest, "/"); strings.Contains(rest, "@") {
		// 用户名:密码@host:port：按最后一个 @ 拆分，密码里可以有 @；URL 写法末尾的 / 去掉
		at := strings.LastIndex(rest, "@")
		userinfo := rest[:at]
		hostPort, hasUser, atForm = rest[at+1:], true, true
		if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
			user, password, hasPassword = unescapeUserinfo(userinfo[:colon]), unescapeUserinfo(userinfo[colon+1:]), true
		} else {
			user = unescapeUserinfo(userinfo)
		}
	} else {
		hostPort = rest
	}
	if strings.Contains(hostPort, "://") {
		// :// 前面不是合法的协议名，例如 ://h:1
		return "", errProxyScheme
	}
	if strings.ContainsAny(hostPort, "/?#") {
		return "", errProxyPath
	}
	if !explicit && !hasExplicitPort(hostPort) {
		return "", errProxyNoPort
	}
	host, port, err := splitProxyHostPort(hostPort, scheme)
	if err != nil {
		return "", err
	}
	if !hasUser {
		// 不写协议的 host:port，或 socks5h 无认证：补协议后作为无认证 URL 返回
		return scheme + "://" + net.JoinHostPort(host, port), nil
	}
	if atForm {
		// host:port@用户名:密码（http://1.2.3.4:8080@user:12345）：按最后一个 @ 拆会把真实主机当用户名、数字密码当端口，
		// 真实密码进日志与页面。与冒号写法同一判定；用解码后的值判断，规范输出再规范化一次结果不变
		credentials := user
		if hasPassword {
			credentials += ":" + password
		}
		if ambiguousProxyOrder(host, credentials) {
			return "", errProxyAmbiguous
		}
	}
	if user == "" {
		return "", errProxyUser
	}
	// RFC 7617：Basic 认证的用户名不能含冒号，否则上游在第一个冒号处拆分、必然 407；SOCKS5（RFC 1929）按长度传，不受影响
	if (scheme == "http" || scheme == "https") && strings.Contains(user, ":") {
		return "", errProxyUserColon
	}
	if scheme == "socks5" && (len(user) > 255 || len(password) > 255) {
		return "", errProxySOCKS5Length
	}
	if len(user)+len(password) > 1024 {
		return "", errProxyCredentialLength
	}
	userinfo := escapeUserinfo(user)
	if hasPassword {
		userinfo += ":" + escapeUserinfo(password)
	}
	return scheme + "://" + userinfo + "@" + net.JoinHostPort(host, port), nil
}

// CanonicalProxy 返回规范化后的代理；无法规范化时返回去掉首尾空白的原值，留给 Validate 报错
func CanonicalProxy(value string) string {
	if normalized, err := NormalizeProxy(value); err == nil {
		return normalized
	}
	return strings.TrimSpace(value)
}

// ValidateProxy 校验账户或全局代理，接受 NormalizeProxy 支持的全部写法
func ValidateProxy(value string) error {
	_, err := NormalizeProxy(value)
	return err
}

// RedactProxy 返回去掉账号密码的 scheme://host:port，供日志与提示使用；空值返回空串，无法识别时返回 "<无法识别的代理>"。
// 不用 url.URL.Redacted()：它保留用户名，而动态 IP 的用户名里常带 session 参数
func RedactProxy(value string) string {
	normalized, err := NormalizeProxy(value)
	if err != nil {
		return unrecognizedProxy
	}
	if normalized == "" {
		return ""
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return unrecognizedProxy
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

// RedactArgument 用于把命令行位置参数写进错误：位置参数可能是忘了写 --proxy 的带账号密码代理。像代理写法（含 @、://，
// 或含 2 个及以上冒号，覆盖 host:port:用户名:密码 与 用户名:密码:host:port）时，带账号密码的只保留 scheme://host:port，
// 无法识别的整体隐藏；其余原样返回。位置参数在这里一律非法，多隐藏一些没有代价
func RedactArgument(value string) string {
	trimmed := strings.TrimSpace(value)
	if !strings.Contains(trimmed, "@") && !strings.Contains(trimmed, "://") && strings.Count(trimmed, ":") < 2 {
		return value
	}
	normalized, err := NormalizeProxy(trimmed)
	if err != nil {
		return "<已隐藏>"
	}
	if ProxyHasCredentials(normalized) {
		return RedactProxy(normalized)
	}
	return value
}

// ProxyHasCredentials 判断代理是否带账号密码（按规范化结果判断）；无法规范化时为 false
func ProxyHasCredentials(value string) bool {
	normalized, err := NormalizeProxy(value)
	if err != nil || normalized == "" {
		return false
	}
	parsed, err := url.Parse(normalized)
	return err == nil && parsed.User != nil
}

// legacyProxyValid 是原 ValidateProxy 去掉“不能包含认证信息”之后的判定：handled 为 true 表示按原规则处理
// （不带认证、能解析、有主机名、协议是 http/https/socks5），这时 err 为空就原样返回
func legacyProxyValid(value string) (handled bool, err error) {
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || parsed.User != nil || parsed.Hostname() == "" {
		return false, nil
	}
	switch parsed.Scheme {
	case "http", "https", "socks5":
	default:
		return false, nil
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return true, errProxyPath
	}
	return true, nil
}

// colonProxyCredentials 判断 rest（去掉协议之后的部分）是否为 host:port:用户名:密码 写法：前两段是 host 与数字端口，第 3 段里还有冒号，
// 而且用户名不为空。用户名为空（http://h:1::2）不可能是账号密码，照旧走直通分支，原规则接受的值原样返回
func colonProxyCredentials(rest string) bool {
	match := colonProxyPattern.FindStringSubmatch(rest)
	return match != nil && strings.Contains(match[3], ":") && !strings.HasPrefix(match[3], ":")
}

// ambiguousProxyOrder 判断账号密码与 host:port 是否可能写反了：host 段不是 IP、不含点、也不是 [v6]，而 用户名:密码 段本身像
// IPv4 或带点域名加端口。冒号写法（myuser:54321:1.2.3.4:8080 其实是 用户名:密码:host:port）与 @ 写法
// （1.2.3.4:8080@user:12345 其实是 host:port@用户名:密码）共用这一判定
func ambiguousProxyOrder(host, credentials string) bool {
	if strings.HasPrefix(host, "[") || strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	return hostPortLikePattern.MatchString(credentials)
}

// bareIPv6 判断没加方括号的 IPv6 地址，可以带端口（2001:db8::1、2001:db8::1:8080）
func bareIPv6(hostPort string) bool {
	if strings.Count(hostPort, ":") < 2 || strings.ContainsAny(hostPort, "[]") {
		return false
	}
	if net.ParseIP(hostPort) != nil {
		return true
	}
	return net.ParseIP(hostPort[:strings.LastIndexByte(hostPort, ':')]) != nil
}

// bareIPv6URL 判断带协议的值是否为不带认证、主机是不加方括号的 IPv6 的 URL
func bareIPv6URL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.User == nil && bareIPv6(parsed.Host)
}

// pathHidesCredentials 判断带协议的值里最后一个 @ 是否在第一个 / ? # 之后：密码以数字开头又含 / ? # 时，url.Parse 把
// 用户名:数字 读成 host:port、其余读成路径（http://user:12/ab@h:8080），提示改写法而不是只说“不能包含路径”
func pathHidesCredentials(rest string) bool {
	separator := strings.IndexAny(rest, "/?#")
	return separator >= 0 && strings.LastIndex(rest, "@") > separator
}

// hasExplicitPort 判断 host[:port] 是否写了端口；冒号后面为空（1.2.3.4:）不算写了端口
func hasExplicitPort(hostPort string) bool {
	if strings.HasPrefix(hostPort, "[") {
		index := strings.Index(hostPort, "]:")
		return index >= 0 && index+2 < len(hostPort)
	}
	index := strings.LastIndexByte(hostPort, ':')
	return index >= 0 && index+1 < len(hostPort)
}

// splitProxyHostPort 严格拆分 host[:port]：IPv6 必须加方括号，host 只能是 IP 或主机名（见 validProxyHost），端口为 1-65535 的
// 十进制数（不允许前导零）；没写端口时 http 补 80、https 补 443，socks5 报错
func splitProxyHostPort(hostPort string, scheme string) (string, string, error) {
	host, port := hostPort, ""
	if strings.HasPrefix(hostPort, "[") || strings.Count(hostPort, ":") == 1 {
		var err error
		if !strings.HasPrefix(hostPort, "[") || strings.Contains(hostPort, "]:") {
			host, port, err = net.SplitHostPort(hostPort)
			if err != nil {
				return "", "", errProxyFormat
			}
		} else {
			// [v6] 不带端口时必须以 ] 结尾：缺右方括号的 [::1 不能当成 ::1
			if !strings.HasSuffix(hostPort, "]") {
				return "", "", errProxyFormat
			}
			host = strings.TrimSuffix(strings.TrimPrefix(hostPort, "["), "]")
		}
	} else if strings.Contains(hostPort, ":") {
		if bareIPv6(hostPort) {
			return "", "", errProxyIPv6
		}
		return "", "", errProxyFormat
	}
	if !validProxyHost(host) {
		return "", "", errProxyFormat
	}
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", "", errProxySOCKS5Port
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", "", errProxyPort
	}
	return host, port, nil
}

// validProxyHost 判断代理主机：IP 字面量，或只含字母、数字、. - _ 与非 ASCII 字符（国际化域名）的主机名。
// 引号、$、&、; 等字符不是合法主机名，拒绝后规范输出写进 .env 不需要引号，也不含 shell 元字符
func validProxyHost(host string) bool {
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for index := 0; index < len(host); index++ {
		character := host[index]
		if character >= 0x80 || 'a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' ||
			'0' <= character && character <= '9' || character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

// unescapeUserinfo 按 URL 规则解码 @ 写法与 URL 写法里的百分号编码（不用 QueryUnescape，否则 + 会变成空格）；
// 不是合法编码时按字面保留
func unescapeUserinfo(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

// escapeUserinfo 对用户名或密码做百分号编码：只保留 RFC 3986 的非保留字符（字母、数字与 - . _ ~），
// 其余字节一律编码，结果写进 .env 不需要引号，也不含 shell 元字符；url.Parse 解码后与原文一致
func escapeUserinfo(value string) string {
	const hex = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if 'a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' || '0' <= character && character <= '9' ||
			character == '-' || character == '.' || character == '_' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hex[character>>4])
		builder.WriteByte(hex[character&15])
	}
	return builder.String()
}

func readEnvFile(path string) (map[string]string, error) {
	values := make(map[string]string)
	if strings.TrimSpace(path) == "" {
		return values, nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return values, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		text := scanner.Text()
		if lineNumber == 1 {
			// Windows 记事本保存的 UTF-8 文件开头带 BOM
			text = strings.TrimPrefix(text, "\ufeff")
		}
		line := strings.TrimSpace(text)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 兼容 shell 写法 export KEY=value
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d 缺少等号", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		if !isConfigKey(key) {
			continue
		}
		value, err := parseEnvValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}
	return values, nil
}

func isConfigKey(value string) bool {
	for _, key := range configKeys {
		if value == key {
			return true
		}
	}
	return false
}

func parseEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '\'':
		closing := strings.IndexByte(value[1:], '\'')
		if closing < 0 {
			return "", fmt.Errorf("单引号未闭合")
		}
		if err := checkEnvValueTail(value[closing+2:]); err != nil {
			return "", err
		}
		return value[1 : closing+1], nil
	case '"':
		// 双引号内只把 \\ 与 \" 当作转义，其余反斜杠原样保留：Windows 路径 "D:\accounts" 不会被当成转义
		var builder strings.Builder
		for index := 1; index < len(value); index++ {
			character := value[index]
			switch {
			case character == '\\' && index+1 < len(value) && (value[index+1] == '\\' || value[index+1] == '"'):
				builder.WriteByte(value[index+1])
				index++
			case character == '"':
				if err := checkEnvValueTail(value[index+1:]); err != nil {
					return "", err
				}
				return builder.String(), nil
			default:
				builder.WriteByte(character)
			}
		}
		return "", fmt.Errorf("双引号未闭合")
	}
	if index := strings.Index(value, " #"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value, nil
}

// checkEnvValueTail 校验引号值之后的内容：只允许空白与行内注释
func checkEnvValueTail(tail string) error {
	tail = strings.TrimSpace(tail)
	if tail == "" || strings.HasPrefix(tail, "#") {
		return nil
	}
	return fmt.Errorf("引号值之后只能是注释")
}

// formatEnvValue 写回 .env 时按需加双引号，只转义反斜杠与双引号（与 parseEnvValue 对应）；
// 换行等控制字符不是合法的配置值，写回时去掉
func formatEnvValue(value string) string {
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' {
			return -1
		}
		return character
	}, value)
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t#\"'\\") {
		escaped := strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "\"", "\\\"")
		return "\"" + escaped + "\""
	}
	return value
}

func parsePositiveDuration(key string, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s 必须是正数时长，例如 30s 或 5m", key)
	}
	return duration, nil
}

// parseOptionalDuration 解析可以为 0 的时长（0 表示关闭），例如 FIRST_EVENT_TIMEOUT
func parseOptionalDuration(key string, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("%s 必须是 0 或正数时长，例如 0、30s 或 2m", key)
	}
	return duration, nil
}

func parsePositiveInt(key string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数", key)
	}
	return parsed, nil
}

// parseNonNegativeInt 解析可以为 0 的整数，例如 ULTRA_WARM_WORKER_LIMIT（0 表示只按需启动）
func parseNonNegativeInt(key string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s 必须是 0 或正整数", key)
	}
	return parsed, nil
}

func validateListenAddr(value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || port == "" {
		return fmt.Errorf("LISTEN_ADDR 必须是 host:port")
	}
	if host == "" {
		host = "0.0.0.0"
	}
	if parsed, err := strconv.ParseUint(port, 10, 16); err != nil || parsed == 0 {
		return fmt.Errorf("LISTEN_ADDR 端口必须是 1 到 65535")
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	target, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("解析配置路径: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("设置配置权限: %w", err)
	}
	if _, err := bytes.NewReader(data).WriteTo(temporary); err != nil {
		temporary.Close()
		return fmt.Errorf("写入配置: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("同步配置: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭配置: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("替换配置: %w", err)
	}
	return nil
}

// defaultMinOutputTokens 为最大输出 token 下限的默认值
const defaultMinOutputTokens = 60000

func intOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// DowngradeGuard 为"拒绝被上游降级的回复"的设置：请求列表中的模型时，在把正文交给客户端之前按出字速度与上游标明的模型
// 判断这次是否被换成了其他模型（例如 3.1 Pro 被换成 3.1 Flash-Lite），判定为降级就返回 400。可在服务配置页修改，立即生效
type DowngradeGuard struct {
	// Enabled 为是否开启（DOWNGRADE_GUARD）
	Enabled bool `json:"enabled"`
	// Models 为拦截的模型 ID（DOWNGRADE_GUARD_MODELS，逗号分隔）；-nothinking、-128 等别名先解析到这里的模型
	Models []string `json:"models"`
	// SpeedThreshold 为判定降级的正文出字速度（DOWNGRADE_SPEED_THRESHOLD，tok/s）
	SpeedThreshold float64 `json:"speed_threshold"`
	// MinTokens 为判定所需的正文 token：第一块正文之后新增的数量（DOWNGRADE_MIN_TOKENS）
	MinTokens int `json:"min_tokens"`
	// MinWindowMS 为判定所需的最短时间窗口（DOWNGRADE_MIN_WINDOW_MS，毫秒）
	MinWindowMS int `json:"min_window_ms"`
	// FuzzyLow、FuzzyHigh 为模糊区间（DOWNGRADE_FUZZY_LOW / HIGH，tok/s）：按字数估算的速度落在区间内时调用 CountTokens 精确计算
	FuzzyLow  float64 `json:"fuzzy_low"`
	FuzzyHigh float64 `json:"fuzzy_high"`
	// CountTimeoutMS 为 CountTokens 的超时（DOWNGRADE_COUNT_TIMEOUT_MS，毫秒），0 表示不调用
	CountTimeoutMS int `json:"count_timeout_ms"`
	// FastMode 为快速模式（DOWNGRADE_FAST_MODE）：思考实时转发、只缓存正文，判定为降级时在流中发送错误事件
	FastMode bool `json:"fast_mode"`
	// MaxHoldMS 为流式请求内容的最长延后（DOWNGRADE_MAX_HOLD_MS，毫秒）：严格模式到时限还没判定就先返回 200 并发出思考，
	// 正文继续缓存；正文缓存到时限仍未判定则放行。0 表示不限（判定前扣住整个思考过程）
	MaxHoldMS int `json:"max_hold_ms"`
	// MemoryMinutes 为记住被降级对话的分钟数（DOWNGRADE_MEMORY_MINUTES），0 表示不记录
	MemoryMinutes int `json:"memory_minutes"`
	// RejectStatus 为因降级拒绝时返回给客户端的 HTTP 状态码（DOWNGRADE_REJECT_STATUS）：
	// 400 按 Google 输入被内容策略拦截的格式返回（默认，客户端不会重试）；503 按服务暂时不可用返回，客户端与中转可以重试或切换渠道
	RejectStatus int `json:"reject_status"`
}

// 因降级拒绝时可选的 HTTP 状态码
const (
	DowngradeRejectBlocked     = 400
	DowngradeRejectUnavailable = 503
)

// DefaultDowngradeGuard 返回默认设置：开启、只拦截 gemini-3.1-pro-preview、严格模式（判定前扣住全部内容，被降级时返回 400），
// 判定窗口 2.5 秒（较长的窗口测速更稳）
func DefaultDowngradeGuard() DowngradeGuard {
	return DowngradeGuard{
		Enabled: true, Models: []string{"gemini-3.1-pro-preview"},
		SpeedThreshold: 190, MinTokens: 150, MinWindowMS: 2500,
		FuzzyLow: 160, FuzzyHigh: 220, CountTimeoutMS: 1500, MemoryMinutes: 30, MaxHoldMS: 0,
		RejectStatus: DowngradeRejectBlocked,
	}
}

// NormalizeDowngradeRejectStatus 把未设置（0，旧版配置或页面）归为默认的 400
func NormalizeDowngradeRejectStatus(status int) int {
	if status == 0 {
		return DowngradeRejectBlocked
	}
	return status
}

// Equal 判断两份设置是否一致
func (g DowngradeGuard) Equal(other DowngradeGuard) bool {
	return g.Enabled == other.Enabled && slices.Equal(g.Models, other.Models) &&
		g.SpeedThreshold == other.SpeedThreshold && g.MinTokens == other.MinTokens && g.MinWindowMS == other.MinWindowMS &&
		g.FuzzyLow == other.FuzzyLow && g.FuzzyHigh == other.FuzzyHigh && g.CountTimeoutMS == other.CountTimeoutMS &&
		g.FastMode == other.FastMode && g.MemoryMinutes == other.MemoryMinutes && g.MaxHoldMS == other.MaxHoldMS &&
		NormalizeDowngradeRejectStatus(g.RejectStatus) == NormalizeDowngradeRejectStatus(other.RejectStatus)
}

// Validate 校验设置；关闭时不检查数值（未设置该项的旧配置等同于关闭）
func (g DowngradeGuard) Validate() error {
	if status := NormalizeDowngradeRejectStatus(g.RejectStatus); status != DowngradeRejectBlocked && status != DowngradeRejectUnavailable {
		return fmt.Errorf("DOWNGRADE_REJECT_STATUS 必须是 400 或 503")
	}
	if !g.Enabled {
		return nil
	}
	if len(g.Models) == 0 {
		return fmt.Errorf("开启降级判定时 DOWNGRADE_GUARD_MODELS 不能为空")
	}
	if g.SpeedThreshold <= 0 {
		return fmt.Errorf("DOWNGRADE_SPEED_THRESHOLD 必须是正数")
	}
	if g.MinTokens <= 0 {
		return fmt.Errorf("DOWNGRADE_MIN_TOKENS 必须是正整数")
	}
	if g.MinWindowMS < 0 || g.MinWindowMS > 60000 {
		return fmt.Errorf("DOWNGRADE_MIN_WINDOW_MS 必须在 0 到 60000 之间")
	}
	if g.FuzzyLow <= 0 || g.FuzzyHigh < g.FuzzyLow {
		return fmt.Errorf("DOWNGRADE_FUZZY_LOW 与 DOWNGRADE_FUZZY_HIGH 必须是正数，且下限不大于上限")
	}
	if g.CountTimeoutMS < 0 || g.CountTimeoutMS > 30000 {
		return fmt.Errorf("DOWNGRADE_COUNT_TIMEOUT_MS 必须在 0 到 30000 之间")
	}
	if g.MemoryMinutes < 0 {
		return fmt.Errorf("DOWNGRADE_MEMORY_MINUTES 必须是 0 或正整数")
	}
	if g.MaxHoldMS < 0 || g.MaxHoldMS > 600000 {
		return fmt.Errorf("DOWNGRADE_MAX_HOLD_MS 必须在 0 到 600000 之间")
	}
	return nil
}

// ParseModelList 把逗号或空白分隔的模型列表拆成去掉 models/ 前缀的名称，去重并保持顺序
func ParseModelList(value string) []string {
	var models []string
	separators := func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }
	for _, item := range strings.FieldsFunc(value, separators) {
		name := strings.TrimPrefix(strings.TrimSpace(item), "models/")
		if name != "" && !slices.Contains(models, name) {
			models = append(models, name)
		}
	}
	return models
}

// NormalizeModelList 整理模型列表：去空白、去 models/ 前缀、去重
func NormalizeModelList(models []string) []string {
	return ParseModelList(strings.Join(models, ","))
}

// validateModelList 校验已整理的模型列表：每项非空、不含空白与分隔符、不重复（ParseModelList 与 NormalizeModelList 的结果总能通过）
func validateModelList(key string, models []string) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == "" || strings.ContainsAny(model, ", \t\r\n，") {
			return fmt.Errorf("%s 的模型 ID %q 无效：不能为空，也不能包含空白或逗号", key, model)
		}
		if _, exists := seen[model]; exists {
			return fmt.Errorf("%s 模型 %s 重复", key, model)
		}
		seen[model] = struct{}{}
	}
	return nil
}

func downgradeGuardOrDefault(value *DowngradeGuard) DowngradeGuard {
	if value == nil {
		return DefaultDowngradeGuard()
	}
	guard := *value
	guard.Models = NormalizeModelList(guard.Models)
	guard.RejectStatus = NormalizeDowngradeRejectStatus(guard.RejectStatus)
	return guard
}

func (g DowngradeGuard) envValues() map[string]string {
	return map[string]string{
		"DOWNGRADE_GUARD":            strconv.FormatBool(g.Enabled),
		"DOWNGRADE_GUARD_MODELS":     strings.Join(g.Models, ","),
		"DOWNGRADE_SPEED_THRESHOLD":  strconv.FormatFloat(g.SpeedThreshold, 'f', -1, 64),
		"DOWNGRADE_MIN_TOKENS":       strconv.Itoa(g.MinTokens),
		"DOWNGRADE_MIN_WINDOW_MS":    strconv.Itoa(g.MinWindowMS),
		"DOWNGRADE_FUZZY_LOW":        strconv.FormatFloat(g.FuzzyLow, 'f', -1, 64),
		"DOWNGRADE_FUZZY_HIGH":       strconv.FormatFloat(g.FuzzyHigh, 'f', -1, 64),
		"DOWNGRADE_COUNT_TIMEOUT_MS": strconv.Itoa(g.CountTimeoutMS),
		"DOWNGRADE_FAST_MODE":        strconv.FormatBool(g.FastMode),
		"DOWNGRADE_MEMORY_MINUTES":   strconv.Itoa(g.MemoryMinutes),
		"DOWNGRADE_MAX_HOLD_MS":      strconv.Itoa(g.MaxHoldMS),
		"DOWNGRADE_REJECT_STATUS":    strconv.Itoa(NormalizeDowngradeRejectStatus(g.RejectStatus)),
	}
}

// loadDowngradeGuard 从 env 读取降级判定设置；没有写的项沿用默认值
func loadDowngradeGuard(values map[string]string, guard *DowngradeGuard) error {
	if err := envBool(values, "DOWNGRADE_GUARD", &guard.Enabled); err != nil {
		return err
	}
	if value, ok := values["DOWNGRADE_GUARD_MODELS"]; ok {
		guard.Models = ParseModelList(value)
	}
	if err := envFloat(values, "DOWNGRADE_SPEED_THRESHOLD", &guard.SpeedThreshold); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_MIN_TOKENS", &guard.MinTokens); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_MIN_WINDOW_MS", &guard.MinWindowMS); err != nil {
		return err
	}
	if err := envFloat(values, "DOWNGRADE_FUZZY_LOW", &guard.FuzzyLow); err != nil {
		return err
	}
	if err := envFloat(values, "DOWNGRADE_FUZZY_HIGH", &guard.FuzzyHigh); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_COUNT_TIMEOUT_MS", &guard.CountTimeoutMS); err != nil {
		return err
	}
	if err := envBool(values, "DOWNGRADE_FAST_MODE", &guard.FastMode); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_MEMORY_MINUTES", &guard.MemoryMinutes); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_MAX_HOLD_MS", &guard.MaxHoldMS); err != nil {
		return err
	}
	if err := envInt(values, "DOWNGRADE_REJECT_STATUS", &guard.RejectStatus); err != nil {
		return err
	}
	guard.RejectStatus = NormalizeDowngradeRejectStatus(guard.RejectStatus)
	return nil
}

func envBool(values map[string]string, key string, target *bool) error {
	value, ok := values[key]
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s 必须是 true 或 false", key)
	}
	*target = parsed
	return nil
}

func envInt(values map[string]string, key string, target *int) error {
	value, ok := values[key]
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s 必须是整数", key)
	}
	*target = parsed
	return nil
}

func envFloat(values map[string]string, key string, target *float64) error {
	value, ok := values[key]
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("%s 必须是数字", key)
	}
	*target = parsed
	return nil
}
