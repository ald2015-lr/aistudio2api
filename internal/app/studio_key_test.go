package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

var testStudioKey = "AQ.TESTKEY" + strings.Repeat("k", 24)

// TestStudioKeyFileStorage 记录以 0600 写在账户目录，重新读取得到同样的内容
func TestStudioKeyFileStorage(t *testing.T) {
	directory := t.TempDir()
	at := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	record := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123", ProjectNumber: "123", Status: studioKeyOK, FetchedAt: &at}
	if err := writeStudioKeyFile(directory, record); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, studioKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %o，期望 600", info.Mode().Perm())
	}
	loaded, ok, err := readStudioKeyFile(directory)
	if err != nil || !ok || loaded.Key != testStudioKey || loaded.ProjectNumber != "123" || loaded.Version != 1 {
		t.Fatalf("读取 ok=%v err=%v project=%s", ok, err, loaded.ProjectNumber)
	}
	if _, ok, err := readStudioKeyFile(t.TempDir()); ok || err != nil {
		t.Fatalf("没有文件时 ok=%v err=%v", ok, err)
	}
}

// TestStudioKeyViewRedacted 列表项只有末四位，编码后的 JSON 里没有完整 Key（原因里的 Key 也打码）
func TestStudioKeyViewRedacted(t *testing.T) {
	record := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123", Status: studioKeyFailed, Reason: "页面 " + testStudioKey}
	view := studioKeyView("a@example.com", "a@example.com", record)
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), testStudioKey) || strings.Contains(string(encoded), "TESTKEY") {
		t.Fatalf("列表项泄露 Key: %s", encoded)
	}
	if !view.HasKey || view.Last4 != "kkkk" {
		t.Fatalf("view = %+v", view)
	}
}

// TestStudioKeyFailureReasonRedacted 失败原因去掉 Key 与页面快照，单行
func TestStudioKeyFailureReasonRedacted(t *testing.T) {
	err := &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonCreateFailed,
		Cause: errors.New("x " + testStudioKey + "\n第二行" + camoufoxnative.PageSnapshotMarker + `{"body":"secret"}`)}
	reason := studioKeyFailureReason(err)
	if strings.Contains(reason, testStudioKey) || strings.Contains(reason, "secret") || strings.Contains(reason, "\n") {
		t.Fatalf("reason = %q", reason)
	}
}

// TestStudioKeyJobQueue 队列去重、逐个取出，空了之后结束
func TestStudioKeyJobQueue(t *testing.T) {
	var job studioKeyJob
	if !job.enqueue([]string{"a@example.com", "b@example.com", "a@example.com"}) {
		t.Fatal("第一次排队应启动处理")
	}
	if job.enqueue([]string{"b@example.com", "c@example.com"}) {
		t.Fatal("处理中不应再次启动")
	}
	var order []string
	for {
		id, ok := job.next()
		if !ok {
			break
		}
		order = append(order, id)
	}
	if strings.Join(order, ",") != "a@example.com,b@example.com,c@example.com" {
		t.Fatalf("order = %v", order)
	}
	if running, _, queue, _ := job.snapshot(); running || len(queue) != 0 {
		t.Fatalf("结束后 running=%v queue=%v", running, queue)
	}
}

// TestStudioKeyJobClear 停止清空排队中的账户，正在处理的那个不受影响
func TestStudioKeyJobClear(t *testing.T) {
	var job studioKeyJob
	job.enqueue([]string{"a@example.com", "b@example.com", "c@example.com"})
	if id, _ := job.next(); id != "a@example.com" {
		t.Fatalf("first = %q", id)
	}
	job.clear()
	if job.pending() {
		t.Fatal("停止后仍有排队")
	}
	if _, current, queue, _ := job.snapshot(); current != "a@example.com" || len(queue) != 0 {
		t.Fatalf("current=%q queue=%v", current, queue)
	}
	if _, ok := job.next(); ok {
		t.Fatal("停止后不应再取出账户")
	}
}

// TestStudioKeySkipReason 未登录、停用、组织停用、收尾中的账户跳过
func TestStudioKeySkipReason(t *testing.T) {
	cases := map[string]aistudio.AccountStatus{
		"disabled": {Enabled: false, State: aistudio.AccountDisabled},
		"auth":     {Enabled: true, State: aistudio.AccountAuthRequired},
		"org":      {Enabled: true, State: aistudio.AccountOrgDisabled},
		"drain":    {Enabled: true, State: aistudio.AccountReady, Transition: "draining"},
		"unavail":  {Enabled: true, State: aistudio.AccountUnavailable},
	}
	for name, status := range cases {
		if studioKeySkipReason(status, false) == "" {
			t.Fatalf("%s 应跳过", name)
		}
	}
	if studioKeySkipReason(aistudio.AccountStatus{Enabled: true, State: aistudio.AccountReady}, true) == "" {
		t.Fatal("需要验证账号而整户暂停的账户应跳过")
	}
	if reason := studioKeySkipReason(aistudio.AccountStatus{Enabled: true, State: aistudio.AccountReady}, false); reason != "" {
		t.Fatalf("就绪账户不应跳过: %s", reason)
	}
}

// TestStudioKeyCacheNoticesExternalChange 文件在进程外被删除或替换后，列表缓存随之失效
func TestStudioKeyCacheNoticesExternalChange(t *testing.T) {
	dir := t.TempDir()
	job := &studioKeyJob{}
	if err := writeStudioKeyFile(dir, studioKeyFile{Key: "AQ.TESTKEYAAAAAAAAAAAAAAAAAAAA1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := job.load("a@example.com", dir); err != nil || !ok {
		t.Fatalf("首次读取: ok=%v err=%v", ok, err)
	}
	if err := os.Remove(filepath.Join(dir, studioKeyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := job.load("a@example.com", dir); err != nil || ok {
		t.Fatalf("删除后应读不到: ok=%v err=%v", ok, err)
	}
}
