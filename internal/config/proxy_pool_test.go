package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// proxyPoolTestPassword 为测试用的占位密码：测试断言错误与摘要里都不出现它
const proxyPoolTestPassword = "S3cr3t-P@ss"

// writeProxyPoolEnv 写一份 .env 并载入
func writeProxyPoolEnv(t *testing.T, content string) (Config, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	return cfg, path, err
}

// TestProxyPoolDefaults C1：没有写 PROXY_POOL_* 时为关闭、10001-10200，Default 与 Load 一致
func TestProxyPoolDefaults(t *testing.T) {
	clearProxyEnvironment(t)
	want := ProxyPool{Mode: ProxyPoolOff, PortStart: DefaultProxyPoolPortStart, PortEnd: DefaultProxyPoolPortEnd}
	if DefaultProxyPool() != want || Default().ProxyPool != want {
		t.Fatalf("默认值 = %+v / %+v", DefaultProxyPool(), Default().ProxyPool)
	}
	for name, content := range map[string]string{
		"missing": "LISTEN_ADDR=127.0.0.1:2048\n",
		"empty":   "PROXY_POOL_MODE=\nPROXY_POOL_URL=\nPROXY_POOL_PORT_START=\nPROXY_POOL_PORT_END=\n",
	} {
		cfg, _, err := writeProxyPoolEnv(t, content)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.ProxyPool != want {
			t.Fatalf("%s: ProxyPool = %+v", name, cfg.ProxyPool)
		}
		if cfg.ProxyPool.Enabled() || cfg.ProxyPool.Size() != 0 || cfg.ProxyPool.Summary() != "关闭" {
			t.Fatalf("%s: 关闭时 Enabled/Size/Summary 错误", name)
		}
	}
}

// TestProxyPoolEnvRoundTrip C2：4 个键（含 {session}、密码里有 # 空格 @ :）Save → Load 逐字节不变，再 Save 文件不变
func TestProxyPoolEnvRoundTrip(t *testing.T) {
	clearProxyEnvironment(t)
	for _, pool := range []ProxyPool{
		{Mode: ProxyPoolStatic, URL: "isp.example.com:10000:user:" + proxyPoolTestPassword, PortStart: 10001, PortEnd: 10200},
		{Mode: ProxyPoolStatic, URL: "http://isp.example.com:10000", PortStart: 20001, PortEnd: 20500},
		{Mode: ProxyPoolDynamic, URL: "gate.example.com:58688:customer-a1-cc-US-sessid-{session}:p@ss #w:rd x", PortStart: 10001, PortEnd: 10200},
		{Mode: ProxyPoolDynamic, URL: "http://customer-a1:pw_session-{SESSION}@gate.example.com:7777", PortStart: 1, PortEnd: 5000},
		{Mode: ProxyPoolOff, URL: "socks5://u:" + proxyPoolTestPassword + "@[::1]:1080", PortStart: 65535, PortEnd: 65535},
	} {
		normalized, err := NormalizeProxyPool(pool)
		if err != nil {
			t.Fatalf("%+v: %v", pool.Mode, err)
		}
		cfg := Default()
		cfg.ProxyPool = normalized
		path := filepath.Join(t.TempDir(), ".env")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", pool.Mode, err, first)
		}
		if loaded.ProxyPool != normalized {
			t.Fatalf("往返不一致：%+v → %+v", normalized, loaded.ProxyPool)
		}
		if err := loaded.Save(path); err != nil {
			t.Fatal(err)
		}
		second, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Fatalf("再次保存 .env 改变：\n%s\n---\n%s", first, second)
		}
		// 4 个键紧跟在 PROXY 之后
		if !strings.Contains(string(first), "\nPROXY=\nPROXY_POOL_MODE="+normalized.Mode+"\nPROXY_POOL_URL=") {
			t.Fatalf(".env 键顺序错误：\n%s", first)
		}
	}
}

// TestNormalizeProxyPoolURL C3：静态的多种写法得到同一规范值；动态占位的各种大小写与百分号写法在用户名、在密码里都统一成 {session}；幂等
func TestNormalizeProxyPoolURL(t *testing.T) {
	staticWant := "http://user:S3cr3t-P%40ss@isp.example.com:10000"
	for _, input := range []string{
		"isp.example.com:10000:user:S3cr3t-P@ss",
		"http://user:S3cr3t-P@ss@isp.example.com:10000",
		"user:S3cr3t-P@ss@isp.example.com:10000",
		" http://user:S3cr3t-P%40ss@isp.example.com:10000/ ",
	} {
		got, err := NormalizeProxyPoolURL(input)
		if err != nil || got != staticWant {
			t.Fatalf("%q → %q (%v)，期望 %q", input, got, err, staticWant)
		}
	}
	for _, test := range []struct{ input, want string }{
		{"http://customer-a1-sessid-{session}:pw@gate.example.com:58688", "http://customer-a1-sessid-{session}:pw@gate.example.com:58688"},
		{"http://customer-a1-sessid-{SESSION}:pw@gate.example.com:58688", "http://customer-a1-sessid-{session}:pw@gate.example.com:58688"},
		{"http://customer-a1-sessid-%7Bsession%7D:pw@gate.example.com:58688", "http://customer-a1-sessid-{session}:pw@gate.example.com:58688"},
		{"http://customer-a1-sessid-%7bSession%7D:pw@gate.example.com:58688", "http://customer-a1-sessid-{session}:pw@gate.example.com:58688"},
		{"gate.example.com:58688:customer-a1-sessid-{Session}:p@ss", "http://customer-a1-sessid-{session}:p%40ss@gate.example.com:58688"},
		{"socks5h://u:pw_session-{session}-x@gate.example.com:1080", "socks5://u:pw_session-{session}-x@gate.example.com:1080"},
		{"http://u-{session}:pw-{session}@gate.example.com:1", "http://u-{session}:pw-{session}@gate.example.com:1"},
	} {
		got, err := NormalizeProxyPoolURL(test.input)
		if err != nil || got != test.want {
			t.Fatalf("%q → %q (%v)，期望 %q", test.input, got, err, test.want)
		}
		again, err := NormalizeProxyPoolURL(got)
		if err != nil || again != got {
			t.Fatalf("%q 不幂等：%q (%v)", got, again, err)
		}
	}
	if got, err := NormalizeProxyPoolURL(""); err != nil || got != "" {
		t.Fatalf("空值 → %q (%v)", got, err)
	}
	// 输入本身含内部令牌按格式错误处理
	if _, err := NormalizeProxyPoolURL("http://aistudio2apisessiontoken-{session}:p@h:1"); err == nil {
		t.Fatal("含内部令牌应报错")
	}
	if got := CanonicalProxyPoolURL(" ::bad "); got != "::bad" {
		t.Fatalf("CanonicalProxyPoolURL 应返回去空白的原值，得到 %q", got)
	}
}

// TestProxyPoolValidate C4：模式、端口、各模式下地址的组合；错误文案不含输入
func TestProxyPoolValidate(t *testing.T) {
	staticURL := "http://user:" + proxyPoolTestPassword + "@isp.example.com:10000"
	dynamicURL := "http://user-sessid-{session}:" + proxyPoolTestPassword + "@gate.example.com:7777"
	for _, test := range []struct {
		name string
		pool ProxyPool
		want string // 错误文案的片段；为空表示应通过
	}{
		{name: "zero", pool: ProxyPool{}},
		{name: "off-with-url", pool: ProxyPool{Mode: "off", URL: staticURL}},
		{name: "off-with-dynamic-url", pool: ProxyPool{Mode: "off", URL: dynamicURL}},
		{name: "static", pool: ProxyPool{Mode: "static", URL: staticURL}},
		{name: "static-upper", pool: ProxyPool{Mode: " STATIC ", URL: staticURL}},
		{name: "dynamic", pool: ProxyPool{Mode: "dynamic", URL: dynamicURL}},
		{name: "mode", pool: ProxyPool{Mode: "rotate"}, want: "PROXY_POOL_MODE 必须是 off、static 或 dynamic"},
		{name: "port-negative", pool: ProxyPool{PortStart: -1, PortEnd: 10}, want: "必须是 1 到 65535 的端口"},
		{name: "port-high", pool: ProxyPool{PortStart: 10001, PortEnd: 65536}, want: "必须是 1 到 65535 的端口"},
		{name: "port-order", pool: ProxyPool{PortStart: 10200, PortEnd: 10001}, want: "起始端口不大于结束端口"},
		{name: "port-count", pool: ProxyPool{PortStart: 1, PortEnd: 5001}, want: "最多 5000 个端口"},
		{name: "port-count-off", pool: ProxyPool{Mode: "off", PortStart: 1000, PortEnd: 7000}, want: "最多 5000 个端口"},
		{name: "static-empty", pool: ProxyPool{Mode: "static"}, want: "static 模式的 PROXY_POOL_URL 不能为空"},
		{name: "static-placeholder", pool: ProxyPool{Mode: "static", URL: dynamicURL}, want: "不能包含 {session}"},
		{name: "dynamic-empty", pool: ProxyPool{Mode: "dynamic"}, want: "dynamic 模式的 PROXY_POOL_URL 不能为空"},
		{name: "dynamic-missing", pool: ProxyPool{Mode: "dynamic", URL: staticURL}, want: "必须包含 {session}"},
		{name: "dynamic-host", pool: ProxyPool{Mode: "dynamic", URL: "http://user:" + proxyPoolTestPassword + "@{session}.gate.example.com:7777"}, want: "只能写在用户名或密码里"},
		{name: "dynamic-port", pool: ProxyPool{Mode: "dynamic", URL: "http://user:" + proxyPoolTestPassword + "@gate.example.com:{session}"}, want: "PROXY_POOL_URL"},
		{name: "bad-url", pool: ProxyPool{Mode: "static", URL: "ftp://user:" + proxyPoolTestPassword + "@h:1"}, want: "PROXY_POOL_URL 协议必须是"},
		{name: "bad-url-off", pool: ProxyPool{Mode: "off", URL: "http://user:" + proxyPoolTestPassword + "@h:99999"}, want: "PROXY_POOL_URL 端口必须是"},
	} {
		err := test.pool.Validate()
		if test.want == "" {
			if err != nil {
				t.Fatalf("%s: 应通过，得到 %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: 应报 %q，得到 %v", test.name, test.want, err)
		}
		if strings.Contains(err.Error(), proxyPoolTestPassword) || strings.Contains(err.Error(), "user-sessid") {
			t.Fatalf("%s: 错误文案含输入：%v", test.name, err)
		}
		if _, normalizeErr := NormalizeProxyPool(test.pool); normalizeErr == nil {
			t.Fatalf("%s: NormalizeProxyPool 应报错", test.name)
		}
	}
	// Config.Validate 带上 IP 池；Load 时端口不是整数报端口错误
	cfg := Default()
	cfg.ProxyPool = ProxyPool{Mode: "dynamic", URL: staticURL}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "必须包含 {session}") {
		t.Fatalf("Config.Validate 应检查 IP 池，得到 %v", err)
	}
	clearProxyEnvironment(t)
	for content, want := range map[string]string{
		"PROXY_POOL_PORT_START=abc\n":  "PROXY_POOL_PORT_START 必须是 1 到 65535 的整数",
		"PROXY_POOL_PORT_END=0\n":      "PROXY_POOL_PORT_END 必须是 1 到 65535 的整数",
		"PROXY_POOL_PORT_END=70000\n":  "PROXY_POOL_PORT_END 必须是 1 到 65535 的整数",
		"PROXY_POOL_MODE=Rotating\n":   "PROXY_POOL_MODE 必须是",
		"PROXY_POOL_MODE=STATIC\n":     "static 模式的 PROXY_POOL_URL 不能为空",
		"PROXY_POOL_PORT_START=1000\n": "最多 5000 个端口",
	} {
		if _, _, err := writeProxyPoolEnv(t, content); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: 应报 %q，得到 %v", content, want, err)
		}
	}
}

// TestProxyPoolURLs C5：StaticURL 与手工替换端口后 NormalizeProxy 的结果逐字节相同；SessionURL 再规范化不变；
// Endpoint 不含端口与账号密码；EndpointKey 随主机或用户名变化；HostMatches 只比较协议与主机
func TestProxyPoolURLs(t *testing.T) {
	for _, test := range []struct {
		input, port10023 string
	}{
		{"isp.example.com:10000:user:" + proxyPoolTestPassword, "isp.example.com:10023:user:" + proxyPoolTestPassword},
		{"HTTP://isp.example.com:10000/", "http://isp.example.com:10023"},
		{"http://isp.example.com", "http://isp.example.com:10023"},
		{"socks5://u:p%3Aq@[2001:db8::1]:1080", "socks5://u:p%3Aq@[2001:db8::1]:10023"},
		{"http://token@isp.example.com:1", "http://token@isp.example.com:10023"},
		{"http://u:@isp.example.com:1", "http://u:@isp.example.com:10023"},
	} {
		pool, err := NormalizeProxyPool(ProxyPool{Mode: ProxyPoolStatic, URL: test.input, PortStart: 10001, PortEnd: 10200})
		if err != nil {
			t.Fatalf("%q: %v", test.input, err)
		}
		want, err := NormalizeProxy(test.port10023)
		if err != nil {
			t.Fatal(err)
		}
		got := pool.StaticURL(10023)
		if got != want {
			t.Fatalf("%q: StaticURL = %q，期望 %q", test.input, got, want)
		}
		if again, _ := NormalizeProxy(got); again != got {
			t.Fatalf("%q: StaticURL 不是规范值：%q → %q", test.input, got, again)
		}
		urls := pool.StaticURLs()
		if len(urls) != 200 || urls[22] != got || urls[0] != pool.StaticURL(10001) || urls[199] != pool.StaticURL(10200) {
			t.Fatalf("%q: StaticURLs 与 StaticURL 不一致", test.input)
		}
		if port, ok := pool.HostMatches(got); !ok || port != 10023 {
			t.Fatalf("%q: HostMatches(%q) = %d %v", test.input, got, port, ok)
		}
		endpoint := pool.Endpoint()
		if strings.Contains(endpoint, "1000") || strings.Contains(endpoint, "@") || strings.Contains(endpoint, "u:") ||
			strings.Contains(endpoint, proxyPoolTestPassword) || !strings.HasPrefix(endpoint, strings.ToLower(test.input[:4])) && !strings.HasPrefix(endpoint, "http://") {
			t.Fatalf("%q: Endpoint = %q", test.input, endpoint)
		}
		if summary := pool.Summary(); strings.Contains(summary, proxyPoolTestPassword) || !strings.Contains(summary, "端口 10001-10200（200 个）") {
			t.Fatalf("%q: Summary = %q", test.input, summary)
		}
	}
	static, err := NormalizeProxyPool(ProxyPool{Mode: ProxyPoolStatic, URL: "http://user:" + proxyPoolTestPassword + "@ISP.example.com:10000"})
	if err != nil {
		t.Fatal(err)
	}
	if static.Endpoint() != "http://ISP.example.com" || static.Summary() != "静态 http://ISP.example.com 端口 10001-10200（200 个）" {
		t.Fatalf("Endpoint/Summary = %q / %q", static.Endpoint(), static.Summary())
	}
	if static.Size() != 200 || static.SessionURL("abc") != "" || static.SessionInPassword() {
		t.Fatal("static 的 Size/SessionURL/SessionInPassword 错误")
	}
	// HostMatches：主机不区分大小写、不比较账号密码；协议或主机不同、空值、无法规范化时为 false
	for proxy, want := range map[string]int{
		"isp.example.com:10050:other:pw":         10050,
		"http://isp.example.com:80":              80,
		"http://isp.example.com":                 80,
		"https://isp.example.com:10050":          -1,
		"http://isp2.example.com:10050":          -1,
		"socks5://user:pw@isp.example.com:10050": -1,
		"":                                       -1,
		"ftp://isp.example.com:1":                -1,
	} {
		port, ok := static.HostMatches(proxy)
		if want < 0 && ok || want >= 0 && (!ok || port != want) {
			t.Fatalf("HostMatches(%q) = %d %v，期望 %d", proxy, port, ok, want)
		}
	}
	// EndpointKey：16 个十六进制字符，随主机或用户名变化，不随密码、端口、模式变化
	key := static.EndpointKey()
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("EndpointKey = %q", key)
	}
	same := ProxyPool{Mode: ProxyPoolOff, URL: "isp.example.com:9999:user:other", PortStart: 1, PortEnd: 2}
	same.URL = strings.Replace(same.URL, "isp.example.com", "ISP.example.com", 1)
	if same.EndpointKey() != key {
		t.Fatal("只换密码、端口与模式时 EndpointKey 不应变化")
	}
	for _, other := range []string{"http://user2:pw@ISP.example.com:10000", "http://user:pw@isp2.example.com:10000", "https://user:pw@ISP.example.com:10000"} {
		if (ProxyPool{Mode: ProxyPoolStatic, URL: other}).EndpointKey() == key {
			t.Fatalf("%q 的 EndpointKey 应不同", other)
		}
	}
	if (ProxyPool{}).EndpointKey() != "" || (ProxyPool{}).Endpoint() != "" || (ProxyPool{}).StaticURL(1) != "" {
		t.Fatal("没有地址时 Endpoint/EndpointKey/StaticURL 应为空")
	}

	// 动态：会话号在用户名或密码里
	inUser, err := NormalizeProxyPool(ProxyPool{Mode: ProxyPoolDynamic, URL: "http://customer-a1-cc-US-sessid-{session}:" + proxyPoolTestPassword + "@gate.example.com:58688"})
	if err != nil {
		t.Fatal(err)
	}
	session := "3fa9c01b7d2er1z"
	got := inUser.SessionURL(session)
	if got != "http://customer-a1-cc-US-sessid-3fa9c01b7d2er1z:S3cr3t-P%40ss@gate.example.com:58688" {
		t.Fatalf("SessionURL = %q", got)
	}
	if again, err := NormalizeProxy(got); err != nil || again != got {
		t.Fatalf("SessionURL 再规范化改变：%q (%v)", again, err)
	}
	if inUser.SessionInPassword() || inUser.Size() != 0 || inUser.StaticURLs() != nil {
		t.Fatal("会话号在用户名里时 SessionInPassword 应为 false，Size 为 0")
	}
	if inUser.SessionURL("Bad_1") != "" || inUser.SessionURL("") != "" {
		t.Fatal("会话号只能是小写字母与数字")
	}
	if inUser.Endpoint() != "http://gate.example.com" || inUser.Summary() != "动态 http://gate.example.com" {
		t.Fatalf("Endpoint/Summary = %q / %q", inUser.Endpoint(), inUser.Summary())
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(inUser.EndpointKey()) {
		t.Fatalf("动态 EndpointKey = %q", inUser.EndpointKey())
	}
	inPassword, err := NormalizeProxyPool(ProxyPool{Mode: ProxyPoolDynamic, URL: "gate.example.com:7777:customer-a1:pw_session-{session}"})
	if err != nil {
		t.Fatal(err)
	}
	if !inPassword.SessionInPassword() || inPassword.SessionURL("abc123") != "http://customer-a1:pw_session-abc123@gate.example.com:7777" {
		t.Fatalf("会话号在密码里：SessionInPassword=%v SessionURL=%q", inPassword.SessionInPassword(), inPassword.SessionURL("abc123"))
	}
	if inPassword.EndpointKey() == inUser.EndpointKey() {
		t.Fatal("不同主机的 EndpointKey 应不同")
	}
}

// TestProxyPoolJSONRoundTrip C6：JSON 往返；旧 JSON 没有 proxy_pool（或为 null）时取默认
func TestProxyPoolJSONRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.ProxyPool = ProxyPool{Mode: "Dynamic", URL: "gate.example.com:7777:customer-a1-sessid-%7BSession%7D:" + proxyPoolTestPassword, PortStart: 20001, PortEnd: 20100}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	want := ProxyPool{Mode: ProxyPoolDynamic, URL: "http://customer-a1-sessid-{session}:S3cr3t-P%40ss@gate.example.com:7777", PortStart: 20001, PortEnd: 20100}
	if decoded.ProxyPool != want {
		t.Fatalf("ProxyPool = %+v", decoded.ProxyPool)
	}
	again, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var twice Config
	if err := json.Unmarshal(again, &twice); err != nil || twice.ProxyPool != want {
		t.Fatalf("第二次往返 = %+v (%v)", twice.ProxyPool, err)
	}

	// 零值 ProxyPool 输出补齐后的默认值
	zero := Default()
	zero.ProxyPool = ProxyPool{}
	if data, err = json.Marshal(zero); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"proxy_pool":{"mode":"off","url":"","port_start":10001,"port_end":10200}`) {
		t.Fatalf("零值输出：%s", data)
	}

	// 旧 JSON：去掉 proxy_pool 字段或写 null
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, "missing"} {
		if value == "missing" {
			delete(raw, "proxy_pool")
		} else {
			raw["proxy_pool"] = nil
		}
		legacy, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var parsed Config
		if err := json.Unmarshal(legacy, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.ProxyPool != DefaultProxyPool() {
			t.Fatalf("旧数据 %v: ProxyPool = %+v", value, parsed.ProxyPool)
		}
	}

	// 非法组合在解析时报错，文案不含密码
	raw["proxy_pool"] = map[string]any{"mode": "static", "url": "http://u-{session}:" + proxyPoolTestPassword + "@h:1"}
	invalid, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rejected Config
	if err := json.Unmarshal(invalid, &rejected); err == nil || strings.Contains(err.Error(), proxyPoolTestPassword) {
		t.Fatalf("非法组合应报错且不含密码，得到 %v", err)
	}
}

// TestProxyPoolEnvironmentOverride C7：进程环境变量覆盖 .env，空值不生效
func TestProxyPoolEnvironmentOverride(t *testing.T) {
	clearProxyEnvironment(t)
	content := "PROXY_POOL_MODE=static\nPROXY_POOL_URL=http://isp.example.com:10000\nPROXY_POOL_PORT_START=10001\nPROXY_POOL_PORT_END=10200\n"
	t.Setenv("PROXY_POOL_PORT_END", "10100")
	t.Setenv("PROXY_POOL_MODE", " ")
	t.Setenv("PROXY_POOL_URL", "")
	cfg, _, err := writeProxyPoolEnv(t, content)
	if err != nil {
		t.Fatal(err)
	}
	want := ProxyPool{Mode: ProxyPoolStatic, URL: "http://isp.example.com:10000", PortStart: 10001, PortEnd: 10100}
	if cfg.ProxyPool != want {
		t.Fatalf("ProxyPool = %+v", cfg.ProxyPool)
	}
	t.Setenv("PROXY_POOL_MODE", "DYNAMIC")
	t.Setenv("PROXY_POOL_URL", "gate.example.com:7777:u-{session}:pw")
	if cfg, _, err = writeProxyPoolEnv(t, content); err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyPool.Mode != ProxyPoolDynamic || cfg.ProxyPool.URL != "http://u-{session}:pw@gate.example.com:7777" {
		t.Fatalf("环境变量覆盖后 = %+v", cfg.ProxyPool)
	}
}

// TestProxyPoolZeroValue C8：没有写 ProxyPool 的 Config 字面量通过校验，并等同默认设置
func TestProxyPoolZeroValue(t *testing.T) {
	var zero ProxyPool
	if err := zero.Validate(); err != nil {
		t.Fatal(err)
	}
	if !zero.Equal(DefaultProxyPool()) || zero.Enabled() {
		t.Fatal("零值应等同默认设置（关闭）")
	}
	if start, end := zero.Ports(); start != DefaultProxyPoolPortStart || end != DefaultProxyPoolPortEnd {
		t.Fatalf("Ports = %d-%d", start, end)
	}
	cfg := Default()
	cfg.ProxyPool = ProxyPool{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeProxyPool(ProxyPool{})
	if err != nil || normalized != DefaultProxyPool() {
		t.Fatalf("NormalizeProxyPool(零值) = %+v (%v)", normalized, err)
	}
	// Equal 按规范值比较地址与模式
	left := ProxyPool{Mode: "STATIC", URL: "isp.example.com:10000:u:p"}
	right := ProxyPool{Mode: "static", URL: "http://u:p@isp.example.com:10000", PortStart: 10001, PortEnd: 10200}
	if !left.Equal(right) {
		t.Fatal("写法不同但规范值相同应视为一致")
	}
	if right.Equal(ProxyPool{Mode: "static", URL: right.URL, PortStart: 10001, PortEnd: 10201}) {
		t.Fatal("端口段不同应视为不一致")
	}
}
