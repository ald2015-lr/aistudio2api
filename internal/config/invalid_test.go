package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInvalidValuesFallBackToDefaults .env 里取值无效的项按默认值启动、逐项记入无效项（原值、原因、来源），其余项照常生效：
// 单项写法无效取默认值；组合不成立时涉及的键改回默认值，没写在 .env 里的不记；凭据键不记原值
func TestInvalidValuesFallBackToDefaults(t *testing.T) {
	clearConfigEnv(t)
	content := strings.Join([]string{
		"INIT_TIMEOUT=abc",
		"REQUEST_TIMEOUT=3m",
		"FIRST_EVENT_TIMEOUT=10m",
		"MAX_ACTIVE_WORKERS=3",
		"ROUTING_STRATEGY=random",
		"PER_ACCOUNT_CONCURRENCY=4",
		"DOWNGRADE_SPEED_THRESHOLD=fast",
		"MEDIA_BASE_URL=https://user:secret-value@ai.example.com",
		"REPEAT_PROMPT_NONCE=sometimes",
		"",
	}, "\n")
	cfg, err := Load(writeEnv(t, content))
	if err != nil {
		t.Fatalf("取值无效应按默认值启动：%v", err)
	}
	defaults := Default()
	if cfg.InitTimeout != defaults.InitTimeout || cfg.FirstEventTimeout != 0 || cfg.MaxActiveWorkers != defaults.MaxActiveWorkers ||
		cfg.WarmWorkerLimit != defaults.WarmWorkerLimit || cfg.RoutingStrategy != defaults.RoutingStrategy ||
		cfg.DowngradeGuard.SpeedThreshold != DefaultDowngradeGuard().SpeedThreshold || cfg.Media.BaseURL != "" || !cfg.RepeatPromptNonce {
		t.Fatalf("无效项应改回默认值：%+v", cfg)
	}
	// 有效的项照常生效
	if cfg.RequestTimeout != 3*time.Minute || cfg.PerAccountConcurrency != 4 {
		t.Fatalf("有效项被改动：REQUEST_TIMEOUT=%s PER_ACCOUNT_CONCURRENCY=%d", cfg.RequestTimeout, cfg.PerAccountConcurrency)
	}
	keys := make([]string, 0, len(cfg.Invalid))
	for _, item := range cfg.Invalid {
		keys = append(keys, item.Key)
	}
	for _, key := range []string{
		"INIT_TIMEOUT", "FIRST_EVENT_TIMEOUT", "MAX_ACTIVE_WORKERS", "ROUTING_STRATEGY", "DOWNGRADE_SPEED_THRESHOLD", "MEDIA_BASE_URL", "REPEAT_PROMPT_NONCE",
	} {
		if !slices.Contains(keys, key) {
			t.Fatalf("无效项缺少 %s：%v", key, keys)
		}
	}
	// WARM_WORKER_LIMIT 没写在 .env 里：随 MAX_ACTIVE_WORKERS 一起改回默认值，但不记为无效项
	if slices.Contains(keys, "WARM_WORKER_LIMIT") || slices.Contains(keys, "REQUEST_TIMEOUT") || slices.Contains(keys, "PER_ACCOUNT_CONCURRENCY") {
		t.Fatalf("没写或有效的项不应记为无效：%v", keys)
	}
	for _, item := range cfg.Invalid {
		switch item.Key {
		case "INIT_TIMEOUT":
			if item.Value != "abc" || !strings.Contains(item.Reason, "INIT_TIMEOUT 必须是正数时长") || item.Environment {
				t.Fatalf("INIT_TIMEOUT 无效项 = %+v", item)
			}
		case "FIRST_EVENT_TIMEOUT":
			if item.Reason != "FIRST_EVENT_TIMEOUT 必须小于 REQUEST_TIMEOUT" {
				t.Fatalf("组合不成立的原因应写明：%+v", item)
			}
		case "MEDIA_BASE_URL":
			if item.Value != "" || strings.Contains(item.Summary(), "secret-value") || !strings.Contains(item.Summary(), "<已隐藏>") {
				t.Fatalf("服务地址可能带账号密码，不应记录原值：%+v %s", item, item.Summary())
			}
		}
	}
	summary := cfg.Invalid[0].Summary()
	if !strings.HasPrefix(summary, "配置项取值无效，已按默认值启动 | 键=") || !strings.Contains(summary, "来源=.env") {
		t.Fatalf("启动日志一行的写法不对：%s", summary)
	}
	// 按默认值生效的配置照常通过校验，可以原样保存
	if err := cfg.Validate(); err != nil {
		t.Fatalf("改回默认值后应通过校验：%v", err)
	}
}

// TestInvalidValuesEnvironmentAndAdjustedDefaults 进程环境变量里的无效值同样按默认值并标明来源；只设 WARM_WORKER_LIMIT=1 时
// WARM_STARTUP_CONCURRENCY 的默认值按它收紧为 1，不报错也不记无效；凭据键不记原值；没有无效项时 Invalid 为空
func TestInvalidValuesEnvironmentAndAdjustedDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ACCOUNT_RPM_LIMIT", "lots")
	cfg, err := Load(writeEnv(t, "WARM_WORKER_LIMIT=1\nMAX_ACTIVE_WORKERS=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WarmWorkerLimit != 1 || cfg.WarmStartupConcurrency != 1 || cfg.AccountRPMLimit != Default().AccountRPMLimit {
		t.Fatalf("WARM=%d 预热并发=%d RPM=%d", cfg.WarmWorkerLimit, cfg.WarmStartupConcurrency, cfg.AccountRPMLimit)
	}
	if len(cfg.Invalid) != 1 || cfg.Invalid[0].Key != "ACCOUNT_RPM_LIMIT" || !cfg.Invalid[0].Environment || cfg.Invalid[0].Value != "lots" ||
		!strings.Contains(cfg.Invalid[0].Summary(), "来源=环境变量") {
		t.Fatalf("环境变量里的无效值 = %+v", cfg.Invalid)
	}
	clearConfigEnv(t)
	if cfg, err := Load(writeEnv(t, "WARM_WORKER_LIMIT=4\nROUTING_STRATEGY=fill-first\n")); err != nil || len(cfg.Invalid) != 0 {
		t.Fatalf("有效配置不应有无效项：%v %+v", err, cfg.Invalid)
	}
	item := InvalidValue{Key: "PROXY_API_KEY", Reason: "x"}
	if !strings.Contains(item.Summary(), "原值=<已隐藏>") {
		t.Fatalf("凭据键的日志不应带原值：%s", item.Summary())
	}
}

// TestInvalidProxyStillFails 网络出口相关的键（PROXY、PROXY_POOL_*）取值无效时仍然启动即报错，绝不按默认值（直连）启动；
// 错误不带代理密码
func TestInvalidProxyStillFails(t *testing.T) {
	clearConfigEnv(t)
	for _, content := range []string{
		"PROXY=ftp://user:secret-pass@proxy.example.com:21\n",
		"PROXY=http://user:secret-pass@proxy.example.com:notaport\n",
		"PROXY_POOL_MODE=weird\n",
		"PROXY_POOL_PORT_START=abc\n",
		"PROXY_POOL_MODE=static\n",
	} {
		_, err := Load(writeEnv(t, content))
		if err == nil {
			t.Fatalf("%q：代理配置无效应启动即报错", content)
		}
		if strings.Contains(err.Error(), "secret-pass") {
			t.Fatalf("%q：错误不应包含代理密码：%v", content, err)
		}
	}
	// 代理项无效时，即使其他项同样无效也报错，不会因为“按默认值启动”而跳过
	if _, err := Load(writeEnv(t, "INIT_TIMEOUT=abc\nPROXY=socks9://bad\n")); err == nil {
		t.Fatal("代理无效时应报错")
	}
}

// TestInvalidSendPathStillFails WAA_BACKEND、UPSTREAM_CHANNELS、TEMPORARY_CHAT 与 AUTO_START 写错时同样启动即报错，不按默认值启动：
// 纯 Go 部署不能悄悄改成 Camoufox 浏览器发送（TLS、HTTP/2 指纹与发送路径都会改变），本想停用的 Build 通道不能被悄悄打开，临时对话
// 不能悄悄改成会自动保存历史的普通对话页，本想关掉的自动启动不能悄悄按 true 启动生成服务、预热 Worker 连官网；写对时照常生效，
// AUTO_START 留空按默认值
func TestInvalidSendPathStillFails(t *testing.T) {
	clearConfigEnv(t)
	for _, test := range []struct {
		content string
		key     string
	}{
		{content: "WAA_BACKEND=golang\n", key: "WAA_BACKEND"},
		{content: "WAA_BACKEND=\n", key: "WAA_BACKEND"},
		{content: "UPSTREAM_CHANNELS=playgroud\n", key: "UPSTREAM_CHANNELS"},
		{content: "UPSTREAM_CHANNELS=playground,playground\n", key: "UPSTREAM_CHANNELS"},
		// TEMPORARY_CHAT 决定预热页带不带 &temporary=true：yes 不是有效写法，不悄悄按 false 打开会自动保存历史的普通对话页
		{content: "TEMPORARY_CHAT=yes\n", key: "TEMPORARY_CHAT"},
		{content: "TEMPORARY_CHAT=\n", key: "TEMPORARY_CHAT"},
		// AUTO_START=off 本想关掉自动启动：不悄悄按默认 true 自动启动生成服务
		{content: "AUTO_START=off\n", key: "AUTO_START"},
		{content: "INIT_TIMEOUT=abc\nAUTO_START=no\n", key: "AUTO_START"},
		// 其他项同样无效时也报错，不会因为“按默认值启动”而跳过
		{content: "INIT_TIMEOUT=abc\nWAA_BACKEND=chrome\n", key: "WAA_BACKEND"},
	} {
		cfg, err := Load(writeEnv(t, test.content))
		if err == nil || !strings.Contains(err.Error(), test.key) {
			t.Fatalf("%q：应启动即报错（%s），得到 err=%v WAA_BACKEND=%s UPSTREAM_CHANNELS=%v", test.content, test.key, err, cfg.WAABackend, cfg.UpstreamChannels)
		}
	}
	cfg, err := Load(writeEnv(t, "WAA_BACKEND=Go\nUPSTREAM_CHANNELS=playground\nTEMPORARY_CHAT=true\nAUTO_START=false\n"))
	if err != nil || cfg.WAABackend != WAABackendGo || !slices.Equal(cfg.UpstreamChannels, []string{"playground"}) || !cfg.TemporaryChat ||
		cfg.AutoStart || len(cfg.Invalid) != 0 {
		t.Fatalf("写对时应照常生效：err=%v WAA_BACKEND=%s UPSTREAM_CHANNELS=%v AUTO_START=%v 无效项=%+v",
			err, cfg.WAABackend, cfg.UpstreamChannels, cfg.AutoStart, cfg.Invalid)
	}
	if cfg, err := Load(writeEnv(t, "AUTO_START=\n")); err != nil || !cfg.AutoStart || len(cfg.Invalid) != 0 {
		t.Fatalf("AUTO_START 留空应按默认值 true：err=%v AUTO_START=%v 无效项=%+v", err, cfg.AutoStart, cfg.Invalid)
	}
}

// TestSaveKeepingInvalidLine SaveKeeping 沿用现有文件里 keep 键的原行（取值无效、按默认值生效的 REQUEST_BODY_LOG 不被默认值抹掉，
// 下次加载照旧记为无效项），其余键照常写 c 的值；文件里没有这一行时照常写 c 的值
func TestSaveKeepingInvalidLine(t *testing.T) {
	clearConfigEnv(t)
	path := writeEnv(t, "REQUEST_BODY_LOG=on\nINIT_TIMEOUT=abc\nADMIN_PASSWORD=secret\n")
	cfg, err := Load(path)
	if err != nil || cfg.RequestBodyLog || len(cfg.Invalid) != 2 {
		t.Fatalf("加载：err=%v REQUEST_BODY_LOG=%v 无效项=%+v", err, cfg.RequestBodyLog, cfg.Invalid)
	}
	if err := cfg.SaveKeeping(path, []string{"REQUEST_BODY_LOG", "AUTO_START"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"REQUEST_BODY_LOG=on\n", "INIT_TIMEOUT=" + Default().InitTimeout.String() + "\n", "AUTO_START=true\n", "ADMIN_PASSWORD=secret\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf("保存后的文件应含 %q：\n%s", line, data)
		}
	}
	reloaded, err := Load(path)
	if err != nil || len(reloaded.Invalid) != 1 || reloaded.Invalid[0].Key != "REQUEST_BODY_LOG" || reloaded.Invalid[0].Value != "on" {
		t.Fatalf("重新加载：err=%v 无效项=%+v，期望只剩沿用原行的 REQUEST_BODY_LOG", err, reloaded.Invalid)
	}
}
