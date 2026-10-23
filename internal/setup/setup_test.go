package setup

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// captureStderr 运行 run 并返回期间写到标准错误的内容（flag 的 -h 输出写在这里）
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	var output []byte
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		output, _ = io.ReadAll(reader)
	}()
	func() {
		defer func() { os.Stderr = previous }()
		run()
	}()
	writer.Close()
	wait.Wait()
	reader.Close()
	return string(output)
}

// TestParseSetupFlagsProxy 不传 --proxy 时用 .env 的代理；传了就规范化（"" 表示直连）；-h 与参数错误的输出、不合法时的错误都不含账号密码
func TestParseSetupFlagsProxy(t *testing.T) {
	const user, password = "user-session-XYZ", "S3cr3t-PW"
	cfg := config.Default()
	cfg.Proxy = "http://" + user + ":" + password + "@1.2.3.4:8080"

	options, err := parseSetupFlags(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if options.proxy != cfg.Proxy || setupAccountConfig("a@example.com", options).Proxy != cfg.Proxy {
		t.Fatalf("不传 --proxy 时应使用 .env 的代理，得到 %q", options.proxy)
	}

	for _, test := range []struct{ input, want string }{
		{"h.example.com:1:u:p@ss", "http://u:p%40ss@h.example.com:1"},
		{"socks5h://u:p@h:1080", "socks5://u:p@h:1080"},
		{" HTTP://H:1/ ", "HTTP://H:1/"},
		{"", ""},
	} {
		options, err := parseSetupFlags([]string{"--proxy", test.input}, cfg)
		if err != nil {
			t.Fatalf("%q: %v", test.input, err)
		}
		if options.proxy != test.want || setupAccountConfig("a@example.com", options).Proxy != test.want {
			t.Errorf("%q: proxy = %q，期望 %q", test.input, options.proxy, test.want)
		}
	}

	for _, args := range [][]string{{"-h"}, {"-no-such-flag"}} {
		var err error
		output := captureStderr(t, func() { _, err = parseSetupFlags(args, cfg) })
		if err == nil {
			t.Fatalf("%v 应返回错误", args)
		}
		if args[0] == "-h" && !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("-h 应返回 flag.ErrHelp，得到 %v", err)
		}
		if !strings.Contains(output, "-proxy") {
			t.Fatalf("%v 的输出应列出 -proxy:\n%s", args, output)
		}
		if strings.Contains(output, user) || strings.Contains(output, password) {
			t.Fatalf("%v 的输出含代理账号密码:\n%s", args, output)
		}
	}

	_, err = parseSetupFlags([]string{"--proxy", "ftp://" + user + ":" + password + "@h:1"}, cfg)
	if err == nil {
		t.Fatal("不合法的 --proxy 应报错")
	}
	if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), password) {
		t.Fatalf("错误含代理账号密码: %v", err)
	}

	// 忘了写 --proxy、把带账号密码的代理写成位置参数：错误只带去掉账号密码的 scheme://host:port
	_, err = parseSetupFlags([]string{"http://" + user + ":" + password + "@h.example:1"}, cfg)
	if err == nil || !strings.Contains(err.Error(), `未知参数 "http://h.example:1"`) {
		t.Fatalf("位置参数应报未知参数，得到 %v", err)
	}
	if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), password) {
		t.Fatalf("未知参数错误含代理账号密码: %v", err)
	}

	// 用户名:密码:host:port 顺序的位置参数，以及 --proxy 的值有空格又没加引号时拆出的后半段：都不回显
	for _, args := range [][]string{
		{user + ":" + password + ":gate.example.com:7777"},
		{"--proxy", "h.example:1:" + user + ":pre", password},
	} {
		_, err = parseSetupFlags(args, cfg)
		if err == nil || !strings.Contains(err.Error(), "未知参数") {
			t.Fatalf("%v: 应报未知参数，得到 %v", args, err)
		}
		if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), password) {
			t.Fatalf("%v: 未知参数错误含代理账号密码: %v", args, err)
		}
	}
}
