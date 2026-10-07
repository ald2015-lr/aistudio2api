package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
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
	defaultWarmWorkerLimit    = 5
	defaultMaxActiveWorkers   = 10
	defaultWarmConcurrency    = 2
	defaultAccountConcurrency = 2
)

var configKeys = [...]string{
	"AISTUDIO_AUTH_STATES",
	"LISTEN_ADDR",
	"PROXY_API_KEY",
	"PROXY",
	"INIT_TIMEOUT",
	"REQUEST_TIMEOUT",
	"WARM_WORKER_LIMIT",
	"MAX_ACTIVE_WORKERS",
	"WARM_STARTUP_CONCURRENCY",
	"PER_ACCOUNT_CONCURRENCY",
	"ROUTING_STRATEGY",
	"UPSTREAM_CHANNELS",
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
	InitTimeout            time.Duration `json:"-"`
	RequestTimeout         time.Duration `json:"-"`
	WarmWorkerLimit        int           `json:"warm_worker_limit"`
	MaxActiveWorkers       int           `json:"max_active_workers"`
	WarmStartupConcurrency int           `json:"warm_startup_concurrency"`
	PerAccountConcurrency  int           `json:"per_account_concurrency"`
	RoutingStrategy        string        `json:"routing_strategy"`
	UpstreamChannels       []string      `json:"upstream_channels"`
	TemporaryChat          bool          `json:"temporary_chat"`
	// IgnoreClientSeed 为真时忽略客户端传入的 seed，每次请求都使用随机种子
	IgnoreClientSeed bool `json:"ignore_client_seed"`
	// RepeatPromptNonce 为真时，在最后一条用户消息末尾加入不可见随机后缀（客户端指定 seed 或温度为 0 时除外）。
	// 键名沿用早期"只对重复提示词加"时的叫法
	RepeatPromptNonce bool `json:"repeat_prompt_nonce"`
	// MinOutputTokens 为最大输出 token 的下限：客户端设置的更小值会提高到它（不超过模型上限）；0 表示不调整
	MinOutputTokens int `json:"min_output_tokens"`
	// DowngradeGuard 为降级判定（拒绝被上游降级的回复）的设置，见 DowngradeGuard
	DowngradeGuard DowngradeGuard `json:"downgrade_guard"`
	WAABackend     string         `json:"waa_backend"`
	AutoStart      bool           `json:"auto_start"`
	AdminPassword  string         `json:"-"`
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
		InitTimeout:            defaultInitTimeout,
		RequestTimeout:         defaultRequestTimeout,
		WarmWorkerLimit:        defaultWarmWorkerLimit,
		MaxActiveWorkers:       defaultMaxActiveWorkers,
		WarmStartupConcurrency: defaultWarmConcurrency,
		PerAccountConcurrency:  defaultAccountConcurrency,
		RoutingStrategy:        "round-robin",
		UpstreamChannels:       append([]string(nil), upstreamChannels...),
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
		cfg.Proxy = strings.TrimSpace(value)
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
	if value, ok := values["ROUTING_STRATEGY"]; ok {
		cfg.RoutingStrategy = strings.TrimSpace(value)
	}
	if value, ok := values["UPSTREAM_CHANNELS"]; ok {
		cfg.UpstreamChannels = ParseUpstreamChannels(value)
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
		"WARM_WORKER_LIMIT":        strconv.Itoa(c.WarmWorkerLimit),
		"MAX_ACTIVE_WORKERS":       strconv.Itoa(c.MaxActiveWorkers),
		"WARM_STARTUP_CONCURRENCY": strconv.Itoa(c.WarmStartupConcurrency),
		"PER_ACCOUNT_CONCURRENCY":  strconv.Itoa(c.PerAccountConcurrency),
		"ROUTING_STRATEGY":         c.RoutingStrategy,
		"UPSTREAM_CHANNELS":        strings.Join(c.UpstreamChannels, ","),
		"TEMPORARY_CHAT":           strconv.FormatBool(c.TemporaryChat),
		"IGNORE_CLIENT_SEED":       strconv.FormatBool(c.IgnoreClientSeed),
		"REPEAT_PROMPT_NONCE":      strconv.FormatBool(c.RepeatPromptNonce),
		"MIN_OUTPUT_TOKENS":        strconv.Itoa(c.MinOutputTokens),
		"WAA_BACKEND":              c.WAABackend,
		"AUTO_START":               strconv.FormatBool(c.AutoStart),
		"ADMIN_PASSWORD":           c.AdminPassword,
	}
	for key, value := range c.DowngradeGuard.envValues() {
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
	if c.WarmWorkerLimit <= 0 {
		return fmt.Errorf("WARM_WORKER_LIMIT 必须是正整数")
	}
	if c.MaxActiveWorkers < c.WarmWorkerLimit {
		return fmt.Errorf("MAX_ACTIVE_WORKERS 必须大于或等于 WARM_WORKER_LIMIT")
	}
	if c.WarmStartupConcurrency <= 0 || c.WarmStartupConcurrency > c.WarmWorkerLimit {
		return fmt.Errorf("WARM_STARTUP_CONCURRENCY 必须是 1 到 WARM_WORKER_LIMIT")
	}
	if c.PerAccountConcurrency <= 0 {
		return fmt.Errorf("PER_ACCOUNT_CONCURRENCY 必须是正整数")
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
	if c.WAABackend != WAABackendCamoufox && c.WAABackend != WAABackendGo {
		return fmt.Errorf("WAA_BACKEND 必须是 camoufox 或 go")
	}
	if err := c.DowngradeGuard.Validate(); err != nil {
		return err
	}
	return nil
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
	type payload struct {
		AuthStates             string         `json:"auth_states"`
		ListenAddr             string         `json:"listen_addr"`
		ProxyAPIKey            string         `json:"proxy_api_key"`
		Proxy                  string         `json:"proxy"`
		InitTimeout            string         `json:"init_timeout"`
		RequestTimeout         string         `json:"request_timeout"`
		WarmWorkerLimit        int            `json:"warm_worker_limit"`
		MaxActiveWorkers       int            `json:"max_active_workers"`
		WarmStartupConcurrency int            `json:"warm_startup_concurrency"`
		PerAccountConcurrency  int            `json:"per_account_concurrency"`
		RoutingStrategy        string         `json:"routing_strategy"`
		UpstreamChannels       []string       `json:"upstream_channels"`
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
		InitTimeout:            c.InitTimeout.String(),
		RequestTimeout:         c.RequestTimeout.String(),
		WarmWorkerLimit:        c.WarmWorkerLimit,
		MaxActiveWorkers:       c.MaxActiveWorkers,
		WarmStartupConcurrency: c.WarmStartupConcurrency,
		PerAccountConcurrency:  c.PerAccountConcurrency,
		RoutingStrategy:        c.RoutingStrategy,
		UpstreamChannels:       c.UpstreamChannels,
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
		InitTimeout            string          `json:"init_timeout"`
		RequestTimeout         string          `json:"request_timeout"`
		WarmWorkerLimit        int             `json:"warm_worker_limit"`
		MaxActiveWorkers       int             `json:"max_active_workers"`
		WarmStartupConcurrency int             `json:"warm_startup_concurrency"`
		PerAccountConcurrency  int             `json:"per_account_concurrency"`
		RoutingStrategy        string          `json:"routing_strategy"`
		UpstreamChannels       []string        `json:"upstream_channels"`
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
	parsed := Config{
		AuthStates:             strings.TrimSpace(value.AuthStates),
		ListenAddr:             strings.TrimSpace(value.ListenAddr),
		ProxyAPIKey:            EffectiveProxyAPIKey(value.ProxyAPIKey),
		Proxy:                  strings.TrimSpace(value.Proxy),
		InitTimeout:            initTimeout,
		RequestTimeout:         requestTimeout,
		WarmWorkerLimit:        value.WarmWorkerLimit,
		MaxActiveWorkers:       value.MaxActiveWorkers,
		WarmStartupConcurrency: value.WarmStartupConcurrency,
		PerAccountConcurrency:  value.PerAccountConcurrency,
		RoutingStrategy:        value.RoutingStrategy,
		UpstreamChannels:       value.UpstreamChannels,
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

// ValidateProxy 校验账户或全局代理 URL
func ValidateProxy(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("PROXY 必须是 http、https 或 socks5 URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5":
	default:
		return fmt.Errorf("PROXY 必须是 http、https 或 socks5 URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("PROXY 不能包含认证信息")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("PROXY 不能包含路径、查询参数或片段")
	}
	return nil
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

func parsePositiveInt(key string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数", key)
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
