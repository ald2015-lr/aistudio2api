package mediastore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeAged 在 dir 下写一个大小为 size、修改时间为 now-age 的文件
func writeAged(t *testing.T, dir string, name string, size int, now time.Time, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	modified := now.Add(-age)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func mediaName(seed string, extension string) string {
	return strings.Repeat(seed, 64/len(seed)) + extension
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestCleanupExpired 删除超过保存期限的媒体文件与一小时前的临时文件，保留其余文件；保存期限为 0 时不按时间删除
func TestCleanupExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := writeAged(t, dir, mediaName("a", ".mp4"), 100, now, 8*24*time.Hour)
	fresh := writeAged(t, dir, mediaName("b", ".png"), 100, now, 24*time.Hour)
	staleTemp := writeAged(t, dir, "."+mediaName("c", ".mp4")+".123456.tmp", 50, now, 2*time.Hour)
	activeTemp := writeAged(t, dir, "."+mediaName("d", ".mp4")+".654321.tmp", 50, now, time.Minute)

	result, err := Cleanup(dir, 0, 0, now)
	if err != nil || result.Expired != 0 || result.Temps != 1 || !exists(old) || exists(staleTemp) {
		t.Fatalf("保存期限为 0 时只删残留临时文件: %+v %v", result, err)
	}
	result, err = Cleanup(dir, 7*24*time.Hour, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if exists(old) || !exists(fresh) || !exists(activeTemp) {
		t.Fatalf("过期文件应删除、其余保留: old=%v fresh=%v temp=%v", exists(old), exists(fresh), exists(activeTemp))
	}
	if result.Expired != 1 || result.Freed != 100 || result.Remaining != 1 || result.RemainingBytes != 100 {
		t.Fatalf("清理结果 = %+v", result)
	}
}

// TestCleanupMaxBytes 总量超过上限时从最旧的文件开始删除，直到不超过上限；最近 10 分钟内修改过的文件不删
func TestCleanupMaxBytes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldest := writeAged(t, dir, mediaName("1", ".mp4"), 400, now, 5*time.Hour)
	older := writeAged(t, dir, mediaName("2", ".mp4"), 400, now, 4*time.Hour)
	middle := writeAged(t, dir, mediaName("3", ".png"), 400, now, 3*time.Hour)
	recent := writeAged(t, dir, mediaName("4", ".mp4"), 400, now, time.Minute)

	result, err := Cleanup(dir, 0, 900, now)
	if err != nil {
		t.Fatal(err)
	}
	if exists(oldest) || exists(older) || !exists(middle) || !exists(recent) {
		t.Fatalf("应删掉最旧的两个: %v %v %v %v", exists(oldest), exists(older), exists(middle), exists(recent))
	}
	if result.Evicted != 2 || result.RemainingBytes != 800 || result.Remaining != 2 {
		t.Fatalf("清理结果 = %+v", result)
	}
	// 剩下的旧文件删掉之后仍超过上限，最近的文件也不删
	result, err = Cleanup(dir, 0, 100, now)
	if err != nil || exists(middle) || !exists(recent) || result.Evicted != 1 || result.RemainingBytes != 400 {
		t.Fatalf("最近的文件不应因总量删除: %+v %v", result, err)
	}
}

// TestCleanupOnlyValidNames 只删除名字符合格式的普通文件：其他名字、大写哈希、未知扩展名、目录、子目录里的文件与符号链接
// （以及它指向的文件）都不碰，即使已经过期或超过总量
func TestCleanupOnlyValidNames(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	now := time.Now()
	age := 30 * 24 * time.Hour
	kept := []string{
		writeAged(t, dir, "notes.txt", 10, now, age),
		writeAged(t, dir, mediaName("a", ".exe"), 10, now, age),
		writeAged(t, dir, strings.ToUpper(mediaName("b", ""))+".mp4", 10, now, age),
		writeAged(t, dir, mediaName("c", ".mp4")+".bak", 10, now, age),
		writeAged(t, dir, ".hidden.tmp", 10, now, age),
	}
	subdir := filepath.Join(dir, mediaName("d", ".mp4"))
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, subdir, writeAged(t, subdir, mediaName("e", ".mp4"), 10, now, age))
	target := writeAged(t, outsideDir, mediaName("f", ".mp4"), 10, now, age)
	link := filepath.Join(dir, mediaName("f", ".mp4"))
	if err := os.Symlink(target, link); err == nil {
		kept = append(kept, link, target)
	}
	removed := writeAged(t, dir, mediaName("9", ".webm"), 10, now, age)

	result, err := Cleanup(dir, time.Hour, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range kept {
		if !exists(path) {
			t.Fatalf("%s 不应被删除", path)
		}
	}
	if exists(removed) || result.Removed() != 1 {
		t.Fatalf("合法的过期文件应删除: %+v", result)
	}
}

// TestCleanupMissingDir 目录不存在时什么也不做
func TestCleanupMissingDir(t *testing.T) {
	result, err := Cleanup(filepath.Join(t.TempDir(), "missing"), time.Hour, 1, time.Now())
	if err != nil || result.Removed() != 0 {
		t.Fatalf("目录不存在: %+v %v", result, err)
	}
}

// recordingLog 记录清理器写的日志
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (log *recordingLog) write(level string, message string) {
	log.mu.Lock()
	log.lines = append(log.lines, level+" "+message)
	log.mu.Unlock()
}

func (log *recordingLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.lines...)
}

// TestCleanerLogsOnlyWhenRemoved 清理器只在真正删了文件时写一行汇总；同一错误不重复写；每次清理读取最新设置
func TestCleanerLogsOnlyWhenRemoved(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var mu sync.Mutex
	retention := time.Duration(0)
	settings := func() (time.Duration, int64) {
		mu.Lock()
		defer mu.Unlock()
		return retention, 0
	}
	logs := &recordingLog{}
	cleaner := NewCleaner(dir, settings, logs.write)
	expired := writeAged(t, dir, mediaName("a", ".mp4"), 2048, now, 10*24*time.Hour)
	cleaner.RunOnce(now)
	if len(logs.snapshot()) != 0 || !exists(expired) {
		t.Fatalf("保存期限为 0 时不应删除也不写日志: %v", logs.snapshot())
	}
	mu.Lock()
	retention = 7 * 24 * time.Hour
	mu.Unlock()
	cleaner.RunOnce(now)
	lines := logs.snapshot()
	if exists(expired) || len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO 媒体目录已清理 | 删除=1 个文件（过期 1") ||
		!strings.Contains(lines[0], "释放=2.0KB") {
		t.Fatalf("删除后应写一行汇总: %v", lines)
	}
	cleaner.RunOnce(now)
	if len(logs.snapshot()) != 1 {
		t.Fatalf("没有删除时不应写日志: %v", logs.snapshot())
	}

	broken := NewCleaner(writeAged(t, dir, "not-a-dir", 1, now, 0), settings, logs.write)
	broken.RunOnce(now)
	broken.RunOnce(now)
	warnings := 0
	for _, line := range logs.snapshot() {
		if strings.HasPrefix(line, "WARN ") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("同一错误只写一次: %v", logs.snapshot())
	}
}

// TestCleanerRunAndKick Run 启动时清理一次，Kick 立即再清理一次，ctx 结束后退出
func TestCleanerRunAndKick(t *testing.T) {
	dir := t.TempDir()
	logs := &recordingLog{}
	cleaner := NewCleaner(dir, func() (time.Duration, int64) { return time.Hour, 0 }, logs.write)
	first := writeAged(t, dir, mediaName("a", ".png"), 10, time.Now(), 2*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cleaner.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool { return !exists(first) })
	second := writeAged(t, dir, mediaName("b", ".png"), 10, time.Now(), 2*time.Hour)
	cleaner.Kick()
	waitFor(t, func() bool { return !exists(second) })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 结束后 Run 应退出")
	}
	var nilCleaner *Cleaner
	nilCleaner.Kick()
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("等待超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
