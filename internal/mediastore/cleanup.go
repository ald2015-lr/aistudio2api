package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// CleanupInterval 为定期清理的间隔
	CleanupInterval = time.Hour
	// recentGrace 内修改过的文件不因总量超限删除：刚生成的链接可能还没被客户端打开
	recentGrace = 10 * time.Minute
	// tempMaxAge 之前的临时文件视为进程中断留下的残留：一次写入只需要几毫秒到几秒
	tempMaxAge = time.Hour
)

// CleanupResult 为一次清理的结果
type CleanupResult struct {
	// Expired 与 Evicted 为因超过保存期限、因超过总量上限删除的文件数，Temps 为删除的残留临时文件数
	Expired int
	Evicted int
	Temps   int
	// Freed 为删除的字节数，Remaining 与 RemainingBytes 为清理后的文件数与总字节数
	Freed          int64
	Remaining      int
	RemainingBytes int64
	// Failed 为删除失败的文件数，FailErr 为第一个删除错误
	Failed  int
	FailErr error
}

// Removed 返回删除的文件总数
func (result CleanupResult) Removed() int {
	return result.Expired + result.Evicted + result.Temps
}

// mediaFile 为一个保存的媒体文件
type mediaFile struct {
	name     string
	size     int64
	modified time.Time
}

// Cleanup 清理媒体目录：删除修改时间早于 now-retention 的文件（retention 为 0 时不按时间删除），总量仍超过 maxBytes 时
// 从最旧的文件开始删除（maxBytes 为 0 时不限；最近 10 分钟内修改过的文件不删），并删除一小时前留下的临时文件。
// 只处理名字符合格式的普通文件，不跟随符号链接，不进入子目录；目录不存在时什么也不做
func Cleanup(dir string, retention time.Duration, maxBytes int64, now time.Time) (CleanupResult, error) {
	var result CleanupResult
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("读取媒体目录: %w", err)
	}
	remove := func(name string, keep func(fs.FileInfo) bool) (int64, bool) {
		path := filepath.Join(dir, name)
		// 删除前按当前状态再判断一次：列出目录之后文件可能被重新生成（修改时间更新）或换成了其他类型
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || keep(info) {
			return 0, false
		}
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				result.Failed++
				if result.FailErr == nil {
					result.FailErr = err
				}
			}
			return 0, false
		}
		return info.Size(), true
	}
	files := make([]mediaFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		media, temp := ValidName(name), validTempName(name)
		if !media && !temp || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if temp {
			young := func(info fs.FileInfo) bool { return now.Sub(info.ModTime()) < tempMaxAge }
			if !young(info) {
				if size, ok := remove(name, young); ok {
					result.Temps++
					result.Freed += size
				}
			}
			continue
		}
		if retention > 0 && now.Sub(info.ModTime()) > retention {
			fresh := func(info fs.FileInfo) bool { return now.Sub(info.ModTime()) <= retention }
			if size, ok := remove(name, fresh); ok {
				result.Expired++
				result.Freed += size
				continue
			}
		}
		files = append(files, mediaFile{name: name, size: info.Size(), modified: info.ModTime()})
	}
	var total int64
	for _, file := range files {
		total += file.size
	}
	if maxBytes > 0 && total > maxBytes {
		sort.Slice(files, func(left, right int) bool {
			if !files[left].modified.Equal(files[right].modified) {
				return files[left].modified.Before(files[right].modified)
			}
			return files[left].name < files[right].name
		})
		recent := func(info fs.FileInfo) bool { return now.Sub(info.ModTime()) < recentGrace }
		kept := files[:0]
		for _, file := range files {
			if total > maxBytes && now.Sub(file.modified) >= recentGrace {
				if size, ok := remove(file.name, recent); ok {
					result.Evicted++
					result.Freed += size
					total -= file.size
					continue
				}
			}
			kept = append(kept, file)
		}
		files = kept
	}
	result.Remaining = len(files)
	result.RemainingBytes = total
	return result, nil
}

// Cleaner 定期清理媒体目录：启动时清理一次，之后每 CleanupInterval 一次，设置变化时调用 Kick 立即再清理一次
type Cleaner struct {
	dir string
	// settings 每次清理读取当前的保存期限与总量上限（热更新）
	settings func() (retention time.Duration, maxBytes int64)
	// log 写服务日志；只在真正删了文件或出错时调用
	log func(level string, message string)
	// interval 为清理间隔，测试可以调短
	interval time.Duration
	kick     chan struct{}
	// lastErr 为上一次写进日志的错误，同一错误不重复写
	mu      sync.Mutex
	lastErr string
}

// NewCleaner 创建目录 dir 的清理器
func NewCleaner(dir string, settings func() (time.Duration, int64), log func(level string, message string)) *Cleaner {
	return &Cleaner{dir: dir, settings: settings, log: log, interval: CleanupInterval, kick: make(chan struct{}, 1)}
}

// Kick 请求立即清理一次（不等待清理完成）；清理器为 nil 时什么也不做
func (cleaner *Cleaner) Kick() {
	if cleaner == nil {
		return
	}
	select {
	case cleaner.kick <- struct{}{}:
	default:
	}
}

// Run 运行清理循环，直到 ctx 结束
func (cleaner *Cleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(cleaner.interval)
	defer ticker.Stop()
	for {
		cleaner.RunOnce(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-cleaner.kick:
		}
	}
}

// RunOnce 按当前设置清理一次，删了文件时写一行汇总日志
func (cleaner *Cleaner) RunOnce(now time.Time) CleanupResult {
	retention, maxBytes := cleaner.settings()
	result, err := Cleanup(cleaner.dir, retention, maxBytes, now)
	if err != nil {
		cleaner.warnOnce(err.Error())
		return result
	}
	if result.Removed() > 0 {
		message := fmt.Sprintf("媒体目录已清理 | 删除=%d 个文件（过期 %d，超出总量 %d，残留临时文件 %d）| 释放=%s | 剩余=%d 个文件 %s",
			result.Removed(), result.Expired, result.Evicted, result.Temps, formatBytes(result.Freed),
			result.Remaining, formatBytes(result.RemainingBytes))
		if result.Failed > 0 {
			message += fmt.Sprintf(" | 删除失败=%d 个（%v）", result.Failed, result.FailErr)
		}
		cleaner.log("INFO", message)
		cleaner.mu.Lock()
		cleaner.lastErr = ""
		cleaner.mu.Unlock()
		return result
	}
	if result.Failed > 0 {
		cleaner.warnOnce(fmt.Sprintf("媒体文件删除失败 %d 个: %v", result.Failed, result.FailErr))
	}
	return result
}

// warnOnce 写一次 WARN，同一错误连续出现时不重复写
func (cleaner *Cleaner) warnOnce(message string) {
	cleaner.mu.Lock()
	repeated := cleaner.lastErr == message
	cleaner.lastErr = message
	cleaner.mu.Unlock()
	if !repeated {
		cleaner.log("WARN", "媒体目录清理失败 | "+message)
	}
}

// formatBytes 把字节数写成便于阅读的文本
func formatBytes(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(size)/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%dB", size)
	}
}
