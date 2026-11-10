package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// 取值无效的配置项按默认值启动
//
// .env（或进程环境变量）里某一项取值无效时不再整个进程起不来（start.sh 守护每分钟拉起一次，原先会一直崩溃重启）：
// 这一项改用默认值，记入 Config.Invalid，启动日志逐项写一行 WARN，管理页的服务配置顶部列出（凭据键不记原值）。
// 单项写法无效（时长、整数、布尔值解析不了等）时这一项取默认值；几项组合起来不成立（MAX_ACTIVE_WORKERS 小于 WARM_WORKER_LIMIT、
// FIRST_EVENT_TIMEOUT 不小于 REQUEST_TIMEOUT 等）时把这项校验涉及的键改回默认值，直到全部校验通过。
// 决定发往 Google 的网络形态与通道的键例外：取值无效时仍然启动即报错，不按默认值启动——代理（PROXY、PROXY_POOL_*）绝不悄悄改成直连；
// WAA_BACKEND 写错时不悄悄从纯 Go 改成 Camoufox 浏览器（发送路径与 TLS、HTTP/2 指纹随之改变）；UPSTREAM_CHANNELS 写错时
// 不悄悄打开本想停用的 Build 通道（开始消耗 Build 额度）；TEMPORARY_CHAT 写错（如 yes、on）时不悄悄按 false 打开普通对话页
// （预热页不再带 &temporary=true，官网会把预热提示词自动保存进账号的历史）；AUTO_START 写错（如 off、no）时不悄悄按默认 true
// 自动启动生成服务、为账号预热 Worker 连官网（本想关掉自动启动，结果反而对外发请求）。
// 管理页保存设置仍按 Validate 拒绝无效值（不改用默认值）；管理页不编辑的键（REQUEST_BODY_LOG 等，见 Config.SaveKeeping）
// 取值无效时，保存其他设置沿用 .env 里的原行，不把按默认值生效的值写回

// InvalidValue 为加载时取值无效、已改用默认值的配置项
type InvalidValue struct {
	Key string `json:"key"`
	// Value 为原值（去空白，过长时截断）；凭据键（secretConfigKeys）不记录
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason"`
	// Environment 为取值来自进程环境变量：在管理页保存设置改不了它，要改启动环境
	Environment bool `json:"environment,omitempty"`
}

// secretConfigKeys 为取值可能含凭据的配置键：无效时不记录原值（服务地址可能误写进账号密码）
var secretConfigKeys = map[string]bool{
	"PROXY_API_KEY": true, "ADMIN_PASSWORD": true, "PROXY": true, "PROXY_POOL_URL": true, "MEDIA_BASE_URL": true,
}

// fatalConfigKeys 为决定网络出口、WAA 发送路径、上游通道、Worker 打开的官网页面与是否自动启动生成服务的配置键：取值无效时启动即报错，
// 不按默认值启动（默认值是直连、Camoufox 浏览器、playground 与 build 两个通道都开、普通对话页、自动启动，都会悄悄改变发往 Google 的
// 请求、页面交互或是否对外发请求）
var fatalConfigKeys = map[string]bool{
	"PROXY": true, "PROXY_POOL_MODE": true, "PROXY_POOL_URL": true, "PROXY_POOL_PORT_START": true, "PROXY_POOL_PORT_END": true,
	"WAA_BACKEND": true, "UPSTREAM_CHANNELS": true, "TEMPORARY_CHAT": true, "AUTO_START": true,
}

// maxInvalidValueRunes 为记录的原值长度上限
const maxInvalidValueRunes = 120

// envLoader 逐项读取 env 值并收集取值无效的项
type envLoader struct {
	values      map[string]string
	environment map[string]bool
	invalid     []InvalidValue
}

// field 读取一个键：键存在（skipEmpty 时还要求去空白后非空）时调用 parse，parse 只在成功时写入配置；
// parse 失败时这一项保持默认值并记入无效项，fatalConfigKeys 里的键返回错误
func (loader *envLoader) field(key string, skipEmpty bool, parse func(string) error) error {
	value, ok := loader.values[key]
	if !ok || skipEmpty && strings.TrimSpace(value) == "" {
		return nil
	}
	if err := parse(value); err != nil {
		if fatalConfigKeys[key] {
			return err
		}
		loader.record(key, err)
	}
	return nil
}

// record 记一项无效配置（同一键只记第一次）；键没有写在 .env 或环境变量里时不记（联动校验改回默认值的未设置项）
func (loader *envLoader) record(key string, err error) {
	value, ok := loader.values[key]
	if !ok || slices.ContainsFunc(loader.invalid, func(item InvalidValue) bool { return item.Key == key }) {
		return
	}
	value = strings.TrimSpace(value)
	if secretConfigKeys[key] {
		value = ""
	} else if utf8.RuneCountInString(value) > maxInvalidValueRunes {
		value = string([]rune(value)[:maxInvalidValueRunes]) + "…"
	}
	loader.invalid = append(loader.invalid, InvalidValue{
		Key: key, Value: value, Reason: err.Error(), Environment: loader.environment[key],
	})
}

// configCheck 为一项校验：check 失败时把 keys 里的键改回默认值（fatal 时直接报错）。Validate 按顺序执行全部校验
type configCheck struct {
	keys  []string
	fatal bool
	check func(Config) error
}

// settle 反复执行校验，把不成立的校验涉及的键改回默认值，直到全部通过；fatal 的校验（代理、WAA_BACKEND、UPSTREAM_CHANNELS）不成立时返回错误
// （TEMPORARY_CHAT 与 AUTO_START 只有写法一项，解析不了时在 field 里直接报错）。
// 改回默认值的键全部是默认值时每项校验都成立，最多重复校验项数次
func (loader *envLoader) settle(cfg *Config) error {
	for range len(configChecks) + 1 {
		failed := false
		for _, item := range configChecks {
			err := item.check(*cfg)
			if err == nil {
				continue
			}
			if item.fatal {
				return err
			}
			failed = true
			for _, key := range item.keys {
				cfg.resetKey(key)
				loader.record(key, err)
			}
		}
		if !failed {
			return nil
		}
	}
	return cfg.Validate()
}

// resetKey 把一个配置键改回默认值；依赖其他键的默认值（排队上限、预热并发）按其他键的当前值计算
func (c *Config) resetKey(key string) {
	defaults := Default()
	guard, media := DefaultDowngradeGuard(), DefaultMediaSettings()
	switch key {
	case "AISTUDIO_AUTH_STATES":
		c.AuthStates = defaults.AuthStates
	case "LISTEN_ADDR":
		c.ListenAddr = defaults.ListenAddr
	case "INIT_TIMEOUT":
		c.InitTimeout = defaults.InitTimeout
	case "REQUEST_TIMEOUT":
		c.RequestTimeout = defaults.RequestTimeout
	case "FIRST_EVENT_TIMEOUT":
		c.FirstEventTimeout = defaults.FirstEventTimeout
	case "WARM_WORKER_LIMIT":
		c.WarmWorkerLimit = defaults.WarmWorkerLimit
	case "MAX_ACTIVE_WORKERS":
		c.MaxActiveWorkers = defaults.MaxActiveWorkers
	case "ULTRA_WARM_WORKER_LIMIT":
		c.UltraWarmWorkerLimit = defaults.UltraWarmWorkerLimit
	case "ULTRA_MAX_ACTIVE_WORKERS":
		c.UltraMaxActiveWorkers = defaults.UltraMaxActiveWorkers
	case "WARM_STARTUP_CONCURRENCY":
		// 不超过当前的 WARM_WORKER_LIMIT：只设了 WARM_WORKER_LIMIT=1 时不必连它一起改回默认值
		c.WarmStartupConcurrency = defaults.WarmStartupConcurrency
		if c.WarmWorkerLimit > 0 {
			c.WarmStartupConcurrency = min(c.WarmStartupConcurrency, c.WarmWorkerLimit)
		}
	case "PER_ACCOUNT_CONCURRENCY":
		c.PerAccountConcurrency = defaults.PerAccountConcurrency
	case "ACCOUNT_RPM_LIMIT":
		c.AccountRPMLimit = defaults.AccountRPMLimit
	case "MIN_OUTPUT_TOKENS":
		c.MinOutputTokens = defaults.MinOutputTokens
	case "LONG_OUTPUT_MAX_TOKENS":
		c.LongOutputMaxTokens = defaults.LongOutputMaxTokens
	case "ROUTING_STRATEGY":
		c.RoutingStrategy = defaults.RoutingStrategy
	case "UPSTREAM_CHANNELS":
		c.UpstreamChannels = slices.Clone(defaults.UpstreamChannels)
	case "STREAM_PLAYGROUND_MODELS":
		c.StreamPlaygroundModels = slices.Clone(defaults.StreamPlaygroundModels)
	case "PRIMARY_MODELS":
		c.PrimaryModels = slices.Clone(defaults.PrimaryModels)
	case "PRIMARY_QUEUE_WAIT":
		c.PrimaryQueueWait = defaultPrimaryQueueWaitFor(c.RequestTimeout)
	case "ULTRA_PRIMARY_MODELS":
		c.UltraPrimaryModels = slices.Clone(defaults.UltraPrimaryModels)
	case "ULTRA_PRIMARY_QUEUE_WAIT":
		c.UltraPrimaryQueueWait = defaultPrimaryQueueWaitFor(c.RequestTimeout)
	case "MODEL_FALLBACKS":
		c.ModelFallbacks = defaults.ModelFallbacks
	case "MODEL_FALLBACK_SCOPE":
		c.ModelFallbackScope = defaults.ModelFallbackScope
	case "WAA_BACKEND":
		c.WAABackend = defaults.WAABackend
	case "DOWNGRADE_GUARD":
		c.DowngradeGuard.Enabled = guard.Enabled
	case "DOWNGRADE_GUARD_MODELS":
		c.DowngradeGuard.Models = slices.Clone(guard.Models)
	case "DOWNGRADE_SPEED_THRESHOLD":
		c.DowngradeGuard.SpeedThreshold = guard.SpeedThreshold
	case "DOWNGRADE_MIN_TOKENS":
		c.DowngradeGuard.MinTokens = guard.MinTokens
	case "DOWNGRADE_MIN_WINDOW_MS":
		c.DowngradeGuard.MinWindowMS = guard.MinWindowMS
	case "DOWNGRADE_FUZZY_LOW":
		c.DowngradeGuard.FuzzyLow = guard.FuzzyLow
	case "DOWNGRADE_FUZZY_HIGH":
		c.DowngradeGuard.FuzzyHigh = guard.FuzzyHigh
	case "DOWNGRADE_COUNT_TIMEOUT_MS":
		c.DowngradeGuard.CountTimeoutMS = guard.CountTimeoutMS
	case "DOWNGRADE_MEMORY_MINUTES":
		c.DowngradeGuard.MemoryMinutes = guard.MemoryMinutes
	case "DOWNGRADE_MAX_HOLD_MS":
		c.DowngradeGuard.MaxHoldMS = guard.MaxHoldMS
	case "DOWNGRADE_REJECT_STATUS":
		c.DowngradeGuard.RejectStatus = guard.RejectStatus
	case "MEDIA_OUTPUT":
		c.Media.Output = media.Output
	case "MEDIA_BASE_URL":
		c.Media.BaseURL = media.BaseURL
	case "MEDIA_RETENTION":
		c.Media.Retention = media.Retention
	case "MEDIA_MAX_SIZE":
		c.Media.MaxBytes = media.MaxBytes
	}
}

// InvalidSummary 返回启动日志里一项无效配置的说明：键=原值（凭据键为 <已隐藏>）| 原因
func (item InvalidValue) Summary() string {
	value := item.Value
	switch {
	case secretConfigKeys[item.Key]:
		value = "<已隐藏>"
	case value == "":
		value = "<空>"
	}
	source := ".env"
	if item.Environment {
		source = "环境变量"
	}
	return fmt.Sprintf("配置项取值无效，已按默认值启动 | 键=%s | 原值=%s | 来源=%s | 原因=%s", item.Key, value, source, item.Reason)
}

// configChecks 为配置校验，顺序即 Validate 报告第一个错误的顺序。keys 为校验不成立时改回默认值的键
var configChecks = []configCheck{
	{keys: []string{"AISTUDIO_AUTH_STATES"}, check: func(c Config) error {
		if strings.TrimSpace(c.AuthStates) == "" {
			return fmt.Errorf("AISTUDIO_AUTH_STATES 不能为空")
		}
		return nil
	}},
	{keys: []string{"LISTEN_ADDR"}, check: func(c Config) error { return validateListenAddr(c.ListenAddr) }},
	{keys: []string{"PROXY"}, fatal: true, check: func(c Config) error { return ValidateProxy(c.Proxy) }},
	{keys: []string{"INIT_TIMEOUT"}, check: func(c Config) error {
		if c.InitTimeout <= 0 {
			return fmt.Errorf("INIT_TIMEOUT 必须是正数时长")
		}
		return nil
	}},
	{keys: []string{"REQUEST_TIMEOUT"}, check: func(c Config) error {
		if c.RequestTimeout <= 0 {
			return fmt.Errorf("REQUEST_TIMEOUT 必须是正数时长")
		}
		return nil
	}},
	{keys: []string{"FIRST_EVENT_TIMEOUT"}, check: func(c Config) error {
		if c.FirstEventTimeout < 0 {
			return fmt.Errorf("FIRST_EVENT_TIMEOUT 必须是 0 或正数时长")
		}
		if c.FirstEventTimeout > 0 && c.FirstEventTimeout >= c.RequestTimeout {
			return fmt.Errorf("FIRST_EVENT_TIMEOUT 必须小于 REQUEST_TIMEOUT")
		}
		return nil
	}},
	{keys: []string{"WARM_WORKER_LIMIT"}, check: func(c Config) error {
		if c.WarmWorkerLimit <= 0 {
			return fmt.Errorf("WARM_WORKER_LIMIT 必须是正整数")
		}
		return nil
	}},
	{keys: []string{"MAX_ACTIVE_WORKERS", "WARM_WORKER_LIMIT"}, check: func(c Config) error {
		if c.MaxActiveWorkers < c.WarmWorkerLimit {
			return fmt.Errorf("MAX_ACTIVE_WORKERS 必须大于或等于 WARM_WORKER_LIMIT")
		}
		return nil
	}},
	{keys: []string{"ULTRA_WARM_WORKER_LIMIT"}, check: func(c Config) error {
		if c.UltraWarmWorkerLimit < 0 {
			return fmt.Errorf("ULTRA_WARM_WORKER_LIMIT 必须是 0 或正整数")
		}
		return nil
	}},
	{keys: []string{"ULTRA_MAX_ACTIVE_WORKERS"}, check: func(c Config) error {
		if c.UltraMaxActiveWorkers <= 0 {
			return fmt.Errorf("ULTRA_MAX_ACTIVE_WORKERS 必须是正整数")
		}
		return nil
	}},
	{keys: []string{"ULTRA_MAX_ACTIVE_WORKERS", "ULTRA_WARM_WORKER_LIMIT"}, check: func(c Config) error {
		if c.UltraMaxActiveWorkers < c.UltraWarmWorkerLimit {
			return fmt.Errorf("ULTRA_MAX_ACTIVE_WORKERS 必须大于或等于 ULTRA_WARM_WORKER_LIMIT")
		}
		return nil
	}},
	{keys: []string{"WARM_STARTUP_CONCURRENCY"}, check: func(c Config) error {
		if c.WarmStartupConcurrency <= 0 || c.WarmStartupConcurrency > c.WarmWorkerLimit {
			return fmt.Errorf("WARM_STARTUP_CONCURRENCY 必须是 1 到 WARM_WORKER_LIMIT")
		}
		return nil
	}},
	{keys: []string{"PER_ACCOUNT_CONCURRENCY"}, check: func(c Config) error {
		if c.PerAccountConcurrency <= 0 {
			return fmt.Errorf("PER_ACCOUNT_CONCURRENCY 必须是正整数")
		}
		return nil
	}},
	{keys: []string{"ACCOUNT_RPM_LIMIT"}, check: func(c Config) error {
		if c.AccountRPMLimit < 0 || c.AccountRPMLimit > maxAccountRPMLimit {
			return fmt.Errorf("ACCOUNT_RPM_LIMIT 必须是 0 到 %d 的整数", maxAccountRPMLimit)
		}
		return nil
	}},
	{keys: []string{"MIN_OUTPUT_TOKENS"}, check: func(c Config) error {
		if c.MinOutputTokens < 0 {
			return fmt.Errorf("MIN_OUTPUT_TOKENS 必须是 0 或正整数")
		}
		return nil
	}},
	{keys: []string{"LONG_OUTPUT_MAX_TOKENS"}, check: func(c Config) error {
		if c.LongOutputMaxTokens < 0 || c.LongOutputMaxTokens > maxLongOutputMaxTokens {
			return fmt.Errorf("LONG_OUTPUT_MAX_TOKENS 必须是 0 到 %d 的整数", maxLongOutputMaxTokens)
		}
		return nil
	}},
	{keys: []string{"ROUTING_STRATEGY"}, check: func(c Config) error {
		if c.RoutingStrategy != "round-robin" && c.RoutingStrategy != "fill-first" {
			return fmt.Errorf("ROUTING_STRATEGY 必须是 round-robin 或 fill-first")
		}
		return nil
	}},
	// 写错时不按默认值（两个通道都开）启动：本想停用的 Build 通道不能被悄悄打开
	{keys: []string{"UPSTREAM_CHANNELS"}, fatal: true, check: func(c Config) error { return validateUpstreamChannels(c.UpstreamChannels) }},
	{keys: []string{"STREAM_PLAYGROUND_MODELS"}, check: func(c Config) error {
		return validateModelList("STREAM_PLAYGROUND_MODELS", c.StreamPlaygroundModels)
	}},
	{keys: []string{"PRIMARY_MODELS"}, check: func(c Config) error { return validateModelList("PRIMARY_MODELS", c.PrimaryModels) }},
	{keys: []string{"PRIMARY_QUEUE_WAIT"}, check: func(c Config) error {
		if c.PrimaryQueueWait < 0 {
			return fmt.Errorf("PRIMARY_QUEUE_WAIT 必须是 0 或正数时长")
		}
		if c.PrimaryQueueWait > c.RequestTimeout {
			return fmt.Errorf("PRIMARY_QUEUE_WAIT 不能超过 REQUEST_TIMEOUT")
		}
		return nil
	}},
	{keys: []string{"ULTRA_PRIMARY_MODELS"}, check: func(c Config) error {
		return validateModelList("ULTRA_PRIMARY_MODELS", c.UltraPrimaryModels)
	}},
	{keys: []string{"ULTRA_PRIMARY_QUEUE_WAIT"}, check: func(c Config) error {
		if c.UltraPrimaryQueueWait < 0 {
			return fmt.Errorf("ULTRA_PRIMARY_QUEUE_WAIT 必须是 0 或正数时长")
		}
		if c.UltraPrimaryQueueWait > c.RequestTimeout {
			return fmt.Errorf("ULTRA_PRIMARY_QUEUE_WAIT 不能超过 REQUEST_TIMEOUT")
		}
		return nil
	}},
	{keys: []string{"MODEL_FALLBACKS"}, check: func(c Config) error { return validateModelFallbacks(c.ModelFallbacks) }},
	{keys: []string{"MODEL_FALLBACK_SCOPE"}, check: func(c Config) error { return validateModelFallbackScope(c.ModelFallbackScope) }},
	// 写错时不按默认值（Camoufox）启动：纯 Go 部署不能悄悄改成浏览器发送，TLS、HTTP/2 指纹与发送路径都会改变
	{keys: []string{"WAA_BACKEND"}, fatal: true, check: func(c Config) error {
		if c.WAABackend != WAABackendCamoufox && c.WAABackend != WAABackendGo {
			return fmt.Errorf("WAA_BACKEND 必须是 camoufox 或 go")
		}
		return nil
	}},
	{keys: []string{"DOWNGRADE_REJECT_STATUS"}, check: func(c Config) error { return c.DowngradeGuard.validateRejectStatus() }},
	{keys: []string{"DOWNGRADE_GUARD_MODELS", "DOWNGRADE_GUARD"}, check: func(c Config) error {
		return c.DowngradeGuard.validateModels()
	}},
	{keys: []string{"DOWNGRADE_SPEED_THRESHOLD"}, check: func(c Config) error { return c.DowngradeGuard.validateSpeedThreshold() }},
	{keys: []string{"DOWNGRADE_MIN_TOKENS"}, check: func(c Config) error { return c.DowngradeGuard.validateMinTokens() }},
	{keys: []string{"DOWNGRADE_MIN_WINDOW_MS"}, check: func(c Config) error { return c.DowngradeGuard.validateMinWindow() }},
	{keys: []string{"DOWNGRADE_FUZZY_LOW", "DOWNGRADE_FUZZY_HIGH"}, check: func(c Config) error {
		return c.DowngradeGuard.validateFuzzy()
	}},
	{keys: []string{"DOWNGRADE_COUNT_TIMEOUT_MS"}, check: func(c Config) error { return c.DowngradeGuard.validateCountTimeout() }},
	{keys: []string{"DOWNGRADE_MEMORY_MINUTES"}, check: func(c Config) error { return c.DowngradeGuard.validateMemory() }},
	{keys: []string{"DOWNGRADE_MAX_HOLD_MS"}, check: func(c Config) error { return c.DowngradeGuard.validateMaxHold() }},
	{keys: []string{"PROXY_POOL_MODE", "PROXY_POOL_URL", "PROXY_POOL_PORT_START", "PROXY_POOL_PORT_END"}, fatal: true,
		check: func(c Config) error { return c.ProxyPool.Validate() }},
	{keys: []string{"MEDIA_OUTPUT"}, check: func(c Config) error { return c.Media.validateOutput() }},
	{keys: []string{"MEDIA_BASE_URL"}, check: func(c Config) error { return c.Media.validateBaseURL() }},
	{keys: []string{"MEDIA_RETENTION"}, check: func(c Config) error { return c.Media.validateRetention() }},
	{keys: []string{"MEDIA_MAX_SIZE"}, check: func(c Config) error { return c.Media.validateMaxSize() }},
}
