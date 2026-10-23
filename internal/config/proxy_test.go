package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearProxyEnvironment 去掉进程环境里的配置键，避免宿主机的 PROXY 等变量覆盖测试写的 .env
func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// TestNormalizeProxyFormats 支持的写法改写成规范 URL，账号密码往返一致且幂等；拒绝的写法返回对应的固定文案
func TestNormalizeProxyFormats(t *testing.T) {
	for _, test := range []struct {
		input, want    string
		user, password string
	}{
		{input: "", want: ""},
		{input: " http://127.0.0.1:7890 ", want: "http://127.0.0.1:7890"},
		{input: "HTTP://Proxy.example.com:8080/", want: "HTTP://Proxy.example.com:8080/"},
		{input: "http://h", want: "http://h"},
		{input: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{input: "http://user:pass@1.2.3.4:8080", want: "http://user:pass@1.2.3.4:8080", user: "user", password: "pass"},
		{input: "socks5://user:pass@host.example:1080", want: "socks5://user:pass@host.example:1080", user: "user", password: "pass"},
		{input: "1.2.3.4:8080:user:pass", want: "http://user:pass@1.2.3.4:8080", user: "user", password: "pass"},
		{input: "user:pass@1.2.3.4:8080", want: "http://user:pass@1.2.3.4:8080", user: "user", password: "pass"},
		{input: "socks5://1.2.3.4:1080:user:pass", want: "socks5://user:pass@1.2.3.4:1080", user: "user", password: "pass"},
		{input: "socks5h://u:p@h:1080", want: "socks5://u:p@h:1080", user: "u", password: "p"},
		{input: "socks5h://h:1080", want: "socks5://h:1080"},
		{input: "1.2.3.4:8080", want: "http://1.2.3.4:8080"},
		{input: "proxy.example.com:8080", want: "http://proxy.example.com:8080"},
		{
			input: "gate.example.com:7777:user-zone-us-session-abc:Secr3t:with:colons",
			want:  "http://user-zone-us-session-abc:Secr3t%3Awith%3Acolons@gate.example.com:7777",
			user:  "user-zone-us-session-abc", password: "Secr3t:with:colons",
		},
		{input: `h:1:u:p@ss/w?rd#x %y"\`, want: "http://u:p%40ss%2Fw%3Frd%23x%20%25y%22%5C@h:1", user: "u", password: `p@ss/w?rd#x %y"\`},
		{input: "h:1:u:p@ss", want: "http://u:p%40ss@h:1", user: "u", password: "p@ss"},
		{input: "h:1:u:p@x.com:22", want: "http://u:p%40x.com%3A22@h:1", user: "u", password: "p@x.com:22"},
		{input: "http://u:p/1@h:8080", want: "http://u:p%2F1@h:8080", user: "u", password: "p/1"},
		{input: "http://u:50%off@h:8080", want: "http://u:50%25off@h:8080", user: "u", password: "50%off"},
		{input: "http://u:a%40b@h:8080", want: "http://u:a%40b@h:8080", user: "u", password: "a@b"},
		{input: "http://u:a+b@h:8080", want: "http://u:a%2Bb@h:8080", user: "u", password: "a+b"},
		{input: "http://token@h:8080", want: "http://token@h:8080", user: "token"},
		{input: "http://u:p@h", want: "http://u:p@h:80", user: "u", password: "p"},
		{input: "https://u:p@h", want: "https://u:p@h:443", user: "u", password: "p"},
		{input: "http://u:p@h:1/", want: "http://u:p@h:1", user: "u", password: "p"},
		{input: "[::1]:8080:u:p", want: "http://u:p@[::1]:8080", user: "u", password: "p"},
		{input: "http://u:p@[2001:db8::1]:8080", want: "http://u:p@[2001:db8::1]:8080", user: "u", password: "p"},
		{input: "u:p@[::1]:1080", want: "http://u:p@[::1]:1080", user: "u", password: "p"},
		{input: "socks5://us%3Aer:pw@h:1", want: "socks5://us%3Aer:pw@h:1", user: "us:er", password: "pw"},
		// 带协议的冒号写法：纯数字或空密码不能被 url.Parse 读成主机名带冒号的无认证 URL，密码含 / ? # 不能被读成路径或查询
		{input: "http://1.2.3.4:8080:user:123456", want: "http://user:123456@1.2.3.4:8080", user: "user", password: "123456"},
		{input: "socks5://h:1080:u:1", want: "socks5://u:1@h:1080", user: "u", password: "1"},
		{input: "http://h:1:u:", want: "http://u:@h:1", user: "u", password: ""},
		{input: "https://h:443:u:p?x#y/z", want: "https://u:p%3Fx%23y%2Fz@h:443", user: "u", password: "p?x#y/z"},
		{input: "HTTP://h:1:?u:/p", want: "http://%3Fu:%2Fp@h:1", user: "?u", password: "/p"},
		// :// 前面不是协议名时按不写协议处理：密码里可以有 ://
		{input: "h:1:u:p://x", want: "http://u:p%3A%2F%2Fx@h:1", user: "u", password: "p://x"},
		{input: "u:p://x@h:1", want: "http://u:p%3A%2F%2Fx@h:1", user: "u", password: "p://x"},
		{input: "u:p@例子.测试:8080", want: "http://u:p@例子.测试:8080", user: "u", password: "p"},
		{input: "my_proxy-1.example:3128", want: "http://my_proxy-1.example:3128"},
		// host 段不含点时，只有 用户名:密码 段像 IPv4 或带点域名加端口才算歧义；其余照常按冒号写法处理
		{input: "localhost:8080:u:p", want: "http://u:p@localhost:8080", user: "u", password: "p"},
		{input: "proxy:3128:user:12345", want: "http://user:12345@proxy:3128", user: "user", password: "12345"},
		{input: "user:123:x@host:99", want: "http://x%40host:99@user:123", user: "x@host", password: "99"},
		{input: "gate.example.com:7777:1.2.3.4:8080", want: "http://1.2.3.4:8080@gate.example.com:7777", user: "1.2.3.4", password: "8080"},
		// @ 写法的 host 是 IP 或带点域名时，用户名:密码 段像 host:port 也照常接受（正常顺序不受 host:port@用户名:密码 判定影响）
		{input: "user:12345@1.2.3.4:8080", want: "http://user:12345@1.2.3.4:8080", user: "user", password: "12345"},
		{input: "http://1.2.3.4:8080@gate.example.com:7777", want: "http://1.2.3.4:8080@gate.example.com:7777", user: "1.2.3.4", password: "8080"},
		{input: "socks5://a.b:1234@[::1]:1080", want: "socks5://a.b:1234@[::1]:1080", user: "a.b", password: "1234"},
		{input: "u.v:pass@localhost:8080", want: "http://u.v:pass@localhost:8080", user: "u.v", password: "pass"},
	} {
		got, err := NormalizeProxy(test.input)
		if err != nil {
			t.Errorf("%q: %v", test.input, err)
			continue
		}
		if got != test.want {
			t.Errorf("%q: 得到 %q，期望 %q", test.input, got, test.want)
		}
		if again, err := NormalizeProxy(got); err != nil || again != got {
			t.Errorf("%q: 不幂等 %q -> %q (%v)", test.input, got, again, err)
		}
		if test.user == "" {
			continue
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Errorf("%q: url.Parse(%q): %v", test.input, got, err)
			continue
		}
		password, _ := parsed.User.Password()
		if parsed.User.Username() != test.user || password != test.password {
			t.Errorf("%q: 往返得到 %q/%q", test.input, parsed.User.Username(), password)
		}
	}

	for _, test := range []struct {
		input string
		want  error
	}{
		{"socks5://u:p@h", errProxySOCKS5Port},
		{"http://:p@h:1", errProxyUser},
		{"ftp://u:p@h:1", errProxyScheme},
		{"ftp://h:1", errProxyScheme},
		{"h:1:u", errProxyFormat},
		{"h:0:u:p", errProxyPort},
		{"h:99999:u:p", errProxyPort},
		{"h:01:u:p", errProxyPort},
		{"http://u:p@h:1/x", errProxyPath},
		{"http://u:p@h:1?x=1", errProxyPath},
		{"http://h:1/path", errProxyPath},
		{"h", errProxyNoPort},
		{"u:p@h", errProxyNoPort},
		{"a\nb", errProxyControl},
		{"http://u:p\t@h:1", errProxyControl},
		{"http://u:p\x7f@h:1", errProxyControl},
		{"http://u:p@h:abc", errProxyPort},
		{"http://u:p@h h:1", errProxyFormat},
		{"http://u:p@2001:db8::1:8080", errProxyIPv6},
		{"u:p@2001:db8::1", errProxyIPv6},
		{"2001:470::1:3128", errProxyIPv6},
		{"2001:db8::1:8080", errProxyIPv6},
		{"2001:470:1f0b::2:3128/", errProxyIPv6},
		// 用户名:密码:host:port 写法、密码是纯数字时，不能把用户名当 host、密码当端口（真实密码会进日志与页面）
		{"myuser:54321:1.2.3.4:8080", errProxyAmbiguous},
		{"http://myuser:54321:1.2.3.4:8080", errProxyAmbiguous},
		{"socks5://myuser:12345:gate.example.com:1080", errProxyAmbiguous},
		{"myuser:54321:1.2.3.4:8080/", errProxyAmbiguous},
		{"myuser:1:[::1]:8080", errProxyAmbiguous},
		{"myuser:54321:例子.测试:8080", errProxyAmbiguous},
		// host:port@用户名:密码 写法、密码是纯数字时，不能把真实主机当用户名、真实密码当端口
		{"1.2.3.4:8080@user:12345", errProxyAmbiguous},
		{"gate.example.com:7777@user-zone-us:54321", errProxyAmbiguous},
		{"http://1.2.3.4:8080@user:12345", errProxyAmbiguous},
		{"socks5://1.2.3.4:1080@user:12345", errProxyAmbiguous},
		{"https://1.2.3.4:8080@user", errProxyAmbiguous},
		{"[::1]:8080@user:12345", errProxyAmbiguous},
		// 按解码后的值判断：a%2Eb 解码为 a.b，规范输出 http://a.b:1234@localhost:8080 再规范化也得报错，否则不幂等
		{"a%2Eb:1234@localhost:8080", errProxyAmbiguous},
		{"http://a%2Eb:1234@localhost:8080", errProxyAmbiguous},
		// URL 写法里密码以数字开头又含 / ? #：url.Parse 把 用户名:数字 读成 host:port，提示改写法
		{"http://user:12/ab@h:8080", errProxyPathCredential},
		{"http://user:12#ab@h:8080", errProxyPathCredential},
		{"http://user:12?ab@h:8080", errProxyPathCredential},
		// 不写协议时端口为空不算写了端口
		{"1.2.3.4:", errProxyNoPort},
		{"u:p@h:", errProxyNoPort},
		{"[::1]:", errProxyNoPort},
		{"socks5://" + strings.Repeat("u", 256) + ":p@h:1", errProxySOCKS5Length},
		{"socks5://u:" + strings.Repeat("p", 256) + "@h:1", errProxySOCKS5Length},
		{"http://u:" + strings.Repeat("p", 1024) + "@h:1", errProxyCredentialLength},
		{"http://us%3Aer:pw@h:1", errProxyUserColon},
		{"http://u:p@[::1", errProxyFormat},
		{"h'x:1:u:p", errProxyFormat},
		{"u:p@h$x:1", errProxyFormat},
		{"socks5://u:p@h;x:1", errProxyFormat},
		{"u:p@\"h\":1", errProxyFormat},
		{"://h:1", errProxyScheme},
		{"1h://x:1", errProxyScheme},
	} {
		got, err := NormalizeProxy(test.input)
		if !errors.Is(err, test.want) {
			t.Errorf("%q: 得到 %q, %v，期望 %v", test.input, got, err, test.want)
		}
		if got != "" {
			t.Errorf("%q: 出错时应返回空串，得到 %q", test.input, got)
		}
		if ValidateProxy(test.input) == nil {
			t.Errorf("%q: ValidateProxy 应报错", test.input)
		}
	}
	// SOCKS5 的用户名、密码恰好 255 字节时接受
	if _, err := NormalizeProxy("socks5://" + strings.Repeat("u", 255) + ":" + strings.Repeat("p", 255) + "@h:1"); err != nil {
		t.Fatalf("255 字节的 SOCKS5 账号密码应接受: %v", err)
	}
}

// TestNormalizeProxyIdempotentAndRoundTrip 随机账号密码（含 URL 与 shell 特殊字符、多字节字符）走冒号写法：
// 规范化两次结果相同，url.Parse 取回的用户名、密码与原文一致，Host 正确，输出不含空格与 shell 元字符，写回 .env 不需要引号
func TestNormalizeProxyIdempotentAndRoundTrip(t *testing.T) {
	passwordRunes := []rune("aZ09-._~!$&'()*+,;=:@/?#[] %\"\\`^{}|<>密é")
	userRunes := []rune("abcXYZ019-._~!$&'()*+,;=@/?#[] %\"\\`^{}|<>密é")
	random := rand.New(rand.NewSource(20260601))
	pick := func(runes []rune, minimum int, maximum int) string {
		length := minimum + random.Intn(maximum-minimum+1)
		var builder strings.Builder
		for range length {
			builder.WriteRune(runes[random.Intn(len(runes))])
		}
		return builder.String()
	}
	schemes := []string{"", "http://", "https://", "socks5://"}
	for index := range 20000 {
		user := pick(userRunes, 1, 12)
		password := pick(passwordRunes, 0, 24)
		// 首尾空白会被去掉：密码不以空白结尾；用户名在中间，不受影响
		password = strings.TrimRight(password, " ")
		prefix := schemes[index%len(schemes)]
		input := prefix + "h.example.com:8080:" + user + ":" + password
		got, err := NormalizeProxy(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%q -> %q: %v", input, got, err)
		}
		wantScheme := strings.TrimSuffix(prefix, "://")
		if wantScheme == "" {
			wantScheme = "http"
		}
		decodedPassword, _ := parsed.User.Password()
		if parsed.Scheme != wantScheme || parsed.User.Username() != user || decodedPassword != password || parsed.Host != "h.example.com:8080" {
			t.Fatalf("%q -> %q 往返不一致: %q %q %q %q", input, got, parsed.Scheme, parsed.User.Username(), decodedPassword, parsed.Host)
		}
		if again, err := NormalizeProxy(got); err != nil || again != got {
			t.Fatalf("不幂等 %q -> %q (%v)", got, again, err)
		}
		if strings.ContainsAny(got, " #\"'\\$&;`") {
			t.Fatalf("输出含特殊字符 %q", got)
		}
		if formatEnvValue(got) != got {
			t.Fatalf("写回 .env 不应加引号: %q", got)
		}
	}
}

// TestNormalizeProxyMisorderedNeverLeaksPassword 账号密码与 host:port 写反（host:port@用户名:密码、用户名:密码:host:port）、
// 密码是纯数字时一律报错，不能把密码当端口写进 RedactProxy 的结果；正常顺序照常接受，RedactProxy 只剩真实的 host:port。
// 所有接受的值都幂等
func TestNormalizeProxyMisorderedNeverLeaksPassword(t *testing.T) {
	hosts := []string{"1.2.3.4", "gate.example.com", "例子.测试", "[::1]", "[2001:db8::1]"}
	users := []string{"user", "user-zone-us", "u_1", "customer-abc-session-9"}
	passwords := []string{"54321", "12345", "65432", "4242"}
	ports := []string{"8080", "1080", "7777"}
	schemes := []string{"", "http://", "https://", "socks5://", "socks5h://", "HTTP://"}
	for _, scheme := range schemes {
		for _, host := range hosts {
			for _, user := range users {
				for _, password := range passwords {
					for _, port := range ports {
						for _, input := range []string{
							scheme + host + ":" + port + "@" + user + ":" + password,
							scheme + user + ":" + password + ":" + host + ":" + port,
						} {
							if got, err := NormalizeProxy(input); !errors.Is(err, errProxyAmbiguous) {
								t.Errorf("%q: 写反的顺序应报歧义，得到 %q (%v)", input, got, err)
							}
						}
						for _, input := range []string{
							scheme + user + ":" + password + "@" + host + ":" + port,
							scheme + host + ":" + port + ":" + user + ":" + password,
						} {
							got, err := NormalizeProxy(input)
							if err != nil {
								t.Errorf("%q: 正常顺序应接受: %v", input, err)
								continue
							}
							if again, err := NormalizeProxy(got); err != nil || again != got {
								t.Errorf("%q: 不幂等 %q -> %q (%v)", input, got, again, err)
							}
							redacted := RedactProxy(got)
							if strings.Contains(redacted, password) || !strings.HasSuffix(redacted, host+":"+port) {
								t.Errorf("%q: RedactProxy = %q", input, redacted)
							}
						}
					}
				}
			}
		}
	}
}

// legacyValidateProxyForTest 为改动前的 ValidateProxy，只用于对照不带认证的旧值保持不变
func legacyValidateProxyForTest(value string) error {
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

// TestNormalizeProxyLegacyPassthrough 旧规则接受的值原样返回（只去首尾空白），旧规则拒绝的不带认证 URL 仍然拒绝。
// 唯一的例外是带协议的冒号写法 scheme://host:port:用户名:数字或空密码：旧规则把它读成主机名带冒号的无认证 URL（原来就连不上），
// 现在按账号密码处理，改写成带认证的 URL，或者按冒号写法报错（写在 .env 里时启动失败，见 README）
func TestNormalizeProxyLegacyPassthrough(t *testing.T) {
	for _, input := range []string{
		"http://h", "http://h:8080/", "HTTP://H:1", "https://h:443", "socks5://[::1]:1080", "http://h:0",
		"socks5://h", "http://127.0.0.1:7890", " http://127.0.0.1:7890\t", "HTTP://Proxy.example.com:8080/",
		"http://[::1]", "https://[2001:db8::1]:8443/", "Socks5://H:1080", "http://h:65536", "http://h:08080",
		"http://例子.测试:8080", "https://proxy.example.com",
		// 主机名带冒号、原来也连不上的旧值：第 3 段里没有冒号，不算冒号写法，仍按原规则原样返回
		"http://h:1:2",
		// 不加方括号的 IPv6：原规则原样接受，不能被当成冒号写法拆成账号密码
		"http://2001:470:1f0b::2:3128", "http://2001:470::1:3128", "http://2001:470::1", "http://2001:db8::1",
		"socks5://2001:db8::1:1080", "https://2001:470::1:3128/",
		// 冒号写法的用户名为空不可能是账号密码，原样返回
		"http://h:1::2", "socks5://x.com:801::1",
	} {
		if err := legacyValidateProxyForTest(input); err != nil {
			t.Fatalf("%q: 旧规则应接受: %v", input, err)
		}
		got, err := NormalizeProxy(input)
		if err != nil || got != strings.TrimSpace(input) {
			t.Errorf("%q: 应原样返回，得到 %q (%v)", input, got, err)
		}
		if CanonicalProxy(input) != strings.TrimSpace(input) {
			t.Errorf("%q: CanonicalProxy 应原样返回", input)
		}
	}
	for _, test := range []struct {
		input, want string
		err         error
	}{
		{input: "http://h:1:u:123", want: "http://u:123@h:1"},
		{input: "socks5://h:1080:u:1234", want: "socks5://u:1234@h:1080"},
		{input: "http://h:1:u:", want: "http://u:@h:1"},
		{input: "http://h.example:8080:1.2.3.4:80", want: "http://1.2.3.4:80@h.example:8080"},
		{input: "http://myuser:54321:1.2.3.4:8080", err: errProxyAmbiguous},
		{input: "socks5://myuser:12345:gate.example.com:1080", err: errProxyAmbiguous},
		{input: "http://h:0:u:1", err: errProxyPort},
		{input: "socks5://e.com8080:0:8080:", err: errProxyPort},
	} {
		if err := legacyValidateProxyForTest(test.input); err != nil {
			t.Fatalf("%q: 旧规则应接受: %v", test.input, err)
		}
		got, err := NormalizeProxy(test.input)
		if got != test.want || !errors.Is(err, test.err) {
			t.Errorf("%q: 得到 %q (%v)，期望 %q (%v)", test.input, got, err, test.want, test.err)
		}
	}
	// 旧规则因路径、查询或片段拒绝的不带认证 URL 仍然拒绝，文案不变
	for _, input := range []string{"http://h:1/x", "http://h:1?x=1", "http://h:1#x", "https://h/a/b"} {
		if legacyValidateProxyForTest(input) == nil {
			t.Fatalf("%q: 旧规则应拒绝", input)
		}
		if _, err := NormalizeProxy(input); !errors.Is(err, errProxyPath) {
			t.Errorf("%q: 应返回路径错误，得到 %v", input, err)
		}
	}
}

// TestNormalizeProxyRejectsColonInHTTPUser HTTP/HTTPS 的用户名解码后含冒号时拒绝（Basic 认证在第一个冒号处拆分，上游必然 407）；
// SOCKS5 按长度传用户名，照常接受
func TestNormalizeProxyRejectsColonInHTTPUser(t *testing.T) {
	for _, input := range []string{"http://us%3Aer:pw@h:1", "us%3Aer:pw@h:1", "https://us%3Aer@h:1", "http://us%3aer@h:1"} {
		if _, err := NormalizeProxy(input); !errors.Is(err, errProxyUserColon) {
			t.Errorf("%q: 应返回用户名冒号错误，得到 %v", input, err)
		}
	}
	got, err := NormalizeProxy("socks5://us%3Aer:pw@h:1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil || parsed.User.Username() != "us:er" {
		t.Fatalf("SOCKS5 用户名应解码为 us:er，得到 %q (%v)", got, err)
	}
}

// TestNormalizeProxyErrorsDoNotEchoInput 拒绝的输入里带上用户名与密码，错误文案不含两者，也不含 Basic 认证的 base64
func TestNormalizeProxyErrorsDoNotEchoInput(t *testing.T) {
	const user, password = "user-session-XYZ", "S3cr3t-PW"
	long := strings.Repeat("x", 1100)
	inputs := []string{
		"socks5://" + user + ":" + password + "@h",
		"http://:" + password + "@" + user + ":1",
		"ftp://" + user + ":" + password + "@h:1",
		"socks5h4://" + user + ":" + password + "@h:1",
		password + ":1:" + user,
		"h:0:" + user + ":" + password,
		"h:99999:" + user + ":" + password,
		"http://" + user + ":" + password + "@h:1/x",
		"http://" + user + ":" + password + "@h:1?x=1",
		"http://" + user + ":" + password + "@h:1#frag",
		user + ":" + password + "@h",
		"http://" + user + ":" + password + "\n@h:1",
		"http://" + user + ":" + password + "@h:abc",
		"http://" + user + ":" + password + "@h h:1",
		"http://" + user + ":" + password + "@[::1",
		"http://" + user + ":" + password + "@%zz:1",
		"socks5://" + user + ":" + password + strings.Repeat("p", 255) + "@h:1",
		"http://" + user + ":" + password + long + "@h:1",
		"http://" + user + "%3A1:" + password + "@h:1",
		"https://" + user + "%3A" + password + "@h:1",
		user + ":54321:1.2.3.4:8080",
		"http://" + user + ":54321:gate.example.com:8080",
		"http://" + user + ":12/" + password + "@h:8080",
		"http://" + user + ":" + password + "@2001:db8::1:8080",
	}
	secrets := []string{
		user, password,
		base64.StdEncoding.EncodeToString([]byte(user + ":" + password)),
		url.QueryEscape(user), url.QueryEscape(password),
	}
	for _, input := range inputs {
		got, err := NormalizeProxy(input)
		if err == nil {
			t.Errorf("%q: 应报错，得到 %q", input, got)
			continue
		}
		for _, check := range []error{err, ValidateProxy(input)} {
			for _, secret := range secrets {
				if strings.Contains(check.Error(), secret) {
					t.Errorf("%q: 错误文案含输入内容: %v", input, check)
				}
			}
		}
		if redacted := RedactProxy(input); strings.Contains(redacted, user) || strings.Contains(redacted, password) {
			t.Errorf("%q: RedactProxy 含账号密码: %q", input, redacted)
		}
	}
}

// TestValidateProxyAcceptsCredentials 带账号密码与简写的代理通过校验，配置整体也能通过校验
func TestValidateProxyAcceptsCredentials(t *testing.T) {
	for _, input := range []string{
		"http://user:pass@1.2.3.4:8080", "https://user:pass@proxy.example.com:443", "socks5://user:pass@host:1080",
		"1.2.3.4:8080:user:pass", "user:pass@1.2.3.4:8080", "socks5h://u:p@h:1080", "http://token@h:8080",
	} {
		if err := ValidateProxy(input); err != nil {
			t.Errorf("%q: %v", input, err)
		}
		cfg := Default()
		cfg.Proxy = CanonicalProxy(input)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q: 配置校验失败: %v", input, err)
		}
	}
	if ValidateProxy("ftp://h:1") == nil || ValidateProxy("h") == nil {
		t.Fatal("不支持的协议与缺端口的简写应报错")
	}
}

// TestLoadNormalizesProxy .env 与进程环境变量里手写的冒号写法在读取时改写成规范 URL；非法值时 Load 返回固定文案
func TestLoadNormalizesProxy(t *testing.T) {
	clearProxyEnvironment(t)
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ content, want string }{
		{"PROXY=1.2.3.4:8080:u:p\n", "http://u:p@1.2.3.4:8080"},
		{"PROXY=\"h.example.com:1:u:p w#x\" # 注释\n", "http://u:p%20w%23x@h.example.com:1"},
		{"PROXY=socks5h://u:p@h:1080\n", "socks5://u:p@h:1080"},
		{"PROXY= http://127.0.0.1:7890 \n", "http://127.0.0.1:7890"},
		{"PROXY=HTTP://H:1/\n", "HTTP://H:1/"},
		{"PROXY=\n", ""},
	} {
		write(test.content)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%q: %v", test.content, err)
		}
		if cfg.Proxy != test.want {
			t.Errorf("%q: Proxy = %q，期望 %q", test.content, cfg.Proxy, test.want)
		}
	}

	write("PROXY=1.2.3.4:8080:u:p\n")
	t.Setenv("PROXY", "socks5://9.9.9.9:1080:env-user:env:pass")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy != "socks5://env-user:env%3Apass@9.9.9.9:1080" {
		t.Fatalf("进程环境变量应覆盖并规范化，得到 %q", cfg.Proxy)
	}
	os.Unsetenv("PROXY")

	write("PROXY=ftp://user-session-XYZ:S3cr3t-PW@h:1\n")
	if _, err := Load(path); !errors.Is(err, errProxyScheme) || strings.Contains(err.Error(), "S3cr3t-PW") {
		t.Fatalf("非法代理应返回协议错误且不含密码，得到 %v", err)
	}
	write("PROXY=h:1:user-session-XYZ\n")
	if _, err := Load(path); !errors.Is(err, errProxyFormat) || strings.Contains(err.Error(), "user-session-XYZ") {
		t.Fatalf("缺密码应返回格式错误且不含用户名，得到 %v", err)
	}
}

// TestSaveLoadProxyRoundTrip 带特殊字符密码的规范值保存后，.env 里这一行不加引号，读回来逐字节相同
func TestSaveLoadProxyRoundTrip(t *testing.T) {
	clearProxyEnvironment(t)
	for _, input := range []string{
		"h.example.com:8080:user-1:p@ss w0rd#$&;'\"\\`!",
		"socks5://[::1]:1080:u~._-:%41%zz+=",
		"https://u:p@proxy.example.com",
	} {
		normalized, err := NormalizeProxy(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		cfg := Default()
		cfg.Proxy = normalized
		path := filepath.Join(t.TempDir(), ".env")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "\nPROXY="+normalized+"\n") {
			t.Fatalf("%q: .env 里 PROXY 应不加引号写成 %q，实际:\n%s", input, normalized, content)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Proxy != normalized {
			t.Fatalf("%q: 往返不一致 %q -> %q", input, normalized, loaded.Proxy)
		}
	}
}

// TestUnmarshalJSONNormalizesProxy 管理接口提交的配置 JSON 里的代理同样规范化，非法值返回固定文案
func TestUnmarshalJSONNormalizesProxy(t *testing.T) {
	cfg := Default()
	cfg.Proxy = " 1.2.3.4:8080:u:p "
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Proxy != "http://u:p@1.2.3.4:8080" {
		t.Fatalf("Proxy = %q", decoded.Proxy)
	}
	cfg.Proxy = "HTTP://H:1/"
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Proxy != "HTTP://H:1/" {
		t.Fatalf("不带认证的旧值应原样保留，得到 %q (%v)", decoded.Proxy, err)
	}
	cfg.Proxy = "socks5://user-session-XYZ:S3cr3t-PW@h"
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); !errors.Is(err, errProxySOCKS5Port) || strings.Contains(err.Error(), "S3cr3t-PW") {
		t.Fatalf("非法代理应返回 SOCKS5 端口错误且不含密码，得到 %v", err)
	}
}

// TestRedactProxy RedactProxy 不含用户名和密码，各写法都得到 scheme://host:port；ProxyHasCredentials 按规范化结果判断
func TestRedactProxy(t *testing.T) {
	for _, test := range []struct {
		input, want string
		credentials bool
	}{
		{"", "", false},
		{"http://user-session-XYZ:S3cr3t-PW@1.2.3.4:8080", "http://1.2.3.4:8080", true},
		{"1.2.3.4:8080:user-session-XYZ:S3cr3t-PW", "http://1.2.3.4:8080", true},
		{"user-session-XYZ:S3cr3t-PW@proxy.example.com:3128", "http://proxy.example.com:3128", true},
		{"socks5h://user-session-XYZ:S3cr3t-PW@h:1080", "socks5://h:1080", true},
		{"https://user-session-XYZ:S3cr3t-PW@h", "https://h:443", true},
		{"[::1]:8080:user-session-XYZ:S3cr3t-PW", "http://[::1]:8080", true},
		{"http://user-session-XYZ@h:8080", "http://h:8080", true},
		{"HTTP://H:1/", "http://H:1", false},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", false},
		{"1.2.3.4:8080", "http://1.2.3.4:8080", false},
		{"ftp://user-session-XYZ:S3cr3t-PW@h:1", "<无法识别的代理>", false},
		{"h:1:user-session-XYZ", "<无法识别的代理>", false},
		// 用户名:密码:host:port、密码是纯数字：不能显示成 http://用户名:密码
		{"myuser:54321:1.2.3.4:8080", "<无法识别的代理>", false},
		{"http://user:12/ab@h:8080", "<无法识别的代理>", false},
		{"http://2001:470::1:3128", "http://2001:470::1:3128", false},
	} {
		got := RedactProxy(test.input)
		if got != test.want {
			t.Errorf("%q: RedactProxy = %q，期望 %q", test.input, got, test.want)
		}
		if strings.Contains(got, "user-session-XYZ") || strings.Contains(got, "S3cr3t-PW") {
			t.Errorf("%q: RedactProxy 含账号密码: %q", test.input, got)
		}
		if ProxyHasCredentials(test.input) != test.credentials {
			t.Errorf("%q: ProxyHasCredentials = %t", test.input, !test.credentials)
		}
	}
}

// TestRedactArgument 命令行位置参数写进错误前：像带账号密码的代理时只保留 scheme://host:port，像代理却无法识别时整体隐藏，
// 其余原样返回
func TestRedactArgument(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"extra", "extra"},
		{`C:\data\auth`, `C:\data\auth`},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"1.2.3.4:8080:user-session-XYZ:S3cr3t-PW", "http://1.2.3.4:8080"},
		{"http://user-session-XYZ:S3cr3t-PW@h:1", "http://h:1"},
		{"user-session-XYZ:S3cr3t-PW@h:1", "http://h:1"},
		{"http://user-session-XYZ:S3cr3t-PW@h:1/x", "<已隐藏>"},
		{"ftp://user-session-XYZ:S3cr3t-PW@h:1", "<已隐藏>"},
		{"h:1:user-session-XYZ", "<已隐藏>"},
		{"myuser:54321:1.2.3.4:8080", "<已隐藏>"},
		// 用户名:密码:host:port 顺序：不含 @ 与 ://，第 2 段也不是数字，同样不能原样写进错误
		{"user-session-XYZ:S3cr3t-PW:gate.example.com:7777", "<已隐藏>"},
		{"user-session-XYZ:S3cr3t-PW:1.2.3.4:8080", "<已隐藏>"},
		{"user-session-XYZ:12345:gate.example.com:7777", "<已隐藏>"},
		{"1.2.3.4:8080@user-session-XYZ:12345", "<已隐藏>"},
		{"127.0.0.1:2048", "127.0.0.1:2048"},
	} {
		got := RedactArgument(test.input)
		if got != test.want {
			t.Errorf("%q: RedactArgument = %q，期望 %q", test.input, got, test.want)
		}
		if strings.Contains(got, "user-session-XYZ") || strings.Contains(got, "S3cr3t-PW") {
			t.Errorf("%q: 含账号密码: %q", test.input, got)
		}
	}
}
