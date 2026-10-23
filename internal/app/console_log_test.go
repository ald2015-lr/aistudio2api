package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// lockedBuffer 为可并发写入与读取的缓冲
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// newConsoleTestRegistry 返回没有写入协程、运行日志写进缓冲的注册表：console 与 consoleDurable 的长度分别为 queue 与 durable
func newConsoleTestRegistry(queue int, durable int) (*requestRegistry, *lockedBuffer) {
	buffer := &lockedBuffer{}
	registry := &requestRegistry{
		active:         make(map[string]trackedRequest),
		subscribers:    make(map[*eventSubscriber]struct{}),
		console:        make(chan api.AdminLog, queue),
		consoleDurable: make(chan api.AdminLog, durable),
		consoleHandler: slog.NewTextHandler(buffer, nil),
	}
	return registry, buffer
}

// startConsoleWriter 启动写入协程，返回取消函数与退出信号
func startConsoleWriter(registry *requestRegistry) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		registry.writeConsole(ctx)
	}()
	return cancel, done
}

// 控制台队列满时：普通日志丢弃并计数；logDurable 进入独立队列，调用方协程不直接写，写入协程启动后写出；两者都进入管理页面
func TestLogDurableQueuedWhenConsoleFull(t *testing.T) {
	registry, buffer := newConsoleTestRegistry(1, 4)
	registry.console <- api.AdminLog{Message: "占位"}

	registry.log("acct", "INFO", "普通行")
	registry.logDurable("acct", "INFO", "必须留下的行")

	if dropped := registry.consoleDropped.Load(); dropped != 1 {
		t.Fatalf("丢弃计数 = %d，期望 1（logDurable 不计入）", dropped)
	}
	if len(registry.consoleDurable) != 1 {
		t.Fatalf("logDurable 应进入独立队列，队列长度 %d", len(registry.consoleDurable))
	}
	if buffer.String() != "" {
		t.Fatalf("调用方协程不应直接写运行日志：%q", buffer.String())
	}
	if logCount(registry, "普通行") != 1 || logCount(registry, "必须留下的行") != 1 {
		t.Fatal("两行都应进入管理页面日志")
	}

	cancel, _ := startConsoleWriter(registry)
	defer cancel()
	waitFor(t, "写入协程写出独立队列的行", func() bool {
		return strings.Contains(buffer.String(), "必须留下的行") && strings.Contains(buffer.String(), "source=acct")
	})
	if strings.Contains(buffer.String(), "普通行") {
		t.Fatal("被丢弃的普通日志不应出现在运行日志里")
	}
}

// 独立队列也满时计入丢弃，不阻塞
func TestLogDurableDropsWhenDurableQueueFull(t *testing.T) {
	registry, _ := newConsoleTestRegistry(4, 1)
	registry.logDurable("acct", "INFO", "第一行")
	registry.logDurable("acct", "INFO", "第二行")
	if dropped := registry.consoleDropped.Load(); dropped != 1 {
		t.Fatalf("独立队列满时应计入丢弃，计数 %d", dropped)
	}
}

// 写入协程优先写出独立队列里的日志
func TestWriteConsolePrefersDurableQueue(t *testing.T) {
	registry, buffer := newConsoleTestRegistry(4, 4)
	registry.log("acct", "INFO", "普通日志甲")
	registry.log("acct", "INFO", "普通日志乙")
	registry.logDurable("acct", "INFO", "优先日志")

	cancel, _ := startConsoleWriter(registry)
	defer cancel()
	waitFor(t, "写出全部三行", func() bool { return strings.Contains(buffer.String(), "普通日志乙") })
	output := buffer.String()
	if !strings.Contains(output, "优先日志") || strings.Index(output, "优先日志") > strings.Index(output, "普通日志甲") {
		t.Fatalf("独立队列应先写：%q", output)
	}
}

// 写入协程定期汇总丢弃条数：没有新日志也会写；没有丢弃时不写
func TestWriteConsoleReportsDropsPeriodically(t *testing.T) {
	registry, buffer := newConsoleTestRegistry(4, 4)
	registry.dropReportInterval = 20 * time.Millisecond
	registry.consoleDropped.Store(5)

	cancel, _ := startConsoleWriter(registry)
	defer cancel()
	waitFor(t, "汇总丢弃条数", func() bool {
		return strings.Contains(buffer.String(), "控制台队列已满，丢弃 5 条日志 | 丢弃=5")
	})
	registry.consoleDropped.Store(3)
	waitFor(t, "下一次汇总", func() bool { return strings.Contains(buffer.String(), "丢弃 3 条日志") })
	if strings.Count(buffer.String(), "控制台队列已满") != 2 {
		t.Fatalf("没有丢弃时不应写汇总行：%q", buffer.String())
	}
}

// 生命周期结束时写完两个队列、汇总丢弃数并关闭 consoleDone；收尾开始后的日志在调用方协程直接写
func TestWriteConsoleFlushesOnShutdown(t *testing.T) {
	registry, buffer := newConsoleTestRegistry(4, 4)
	registry.log("acct", "INFO", "排队的普通行")
	registry.logDurable("acct", "INFO", "排队的独立行")
	registry.consoleDropped.Store(2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry.writeConsole(ctx)

	output := buffer.String()
	for _, text := range []string{"排队的普通行", "排队的独立行", "丢弃 2 条日志"} {
		if !strings.Contains(output, text) {
			t.Fatalf("关闭时应写出 %q：%q", text, output)
		}
	}
	if !registry.consoleStopped.Load() {
		t.Fatal("写入协程收尾时应置位 consoleStopped")
	}
	registry.log("service", "INFO", "关闭后的普通行")
	registry.logDurable("service", "WARN", "关闭后的独立行")
	output = buffer.String()
	if !strings.Contains(output, "关闭后的普通行") || !strings.Contains(output, "关闭后的独立行") {
		t.Fatalf("收尾开始后应直接写运行日志：%q", output)
	}
	if len(registry.console) != 0 || len(registry.consoleDurable) != 0 {
		t.Fatal("收尾开始后不应再进入队列")
	}
}

// 关闭与并发写日志交错时，每一条都写进运行日志：不会在队列写空后才入队、无人再读；waitConsole 等到收尾完成
func TestWriteConsoleShutdownRaceKeepsEveryLine(t *testing.T) {
	const writers, lines = 4, 20
	for round := 0; round < 200; round++ {
		registry, buffer := newConsoleTestRegistry(consoleQueueSize, consoleDurableQueueSize)
		registry.consoleDone = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go registry.writeConsole(ctx)
		var group sync.WaitGroup
		for writer := 0; writer < writers; writer++ {
			group.Add(1)
			go func() {
				defer group.Done()
				for line := 0; line < lines; line++ {
					registry.log("acct", "INFO", "并发行")
				}
			}()
		}
		cancel()
		group.Wait()
		registry.waitConsole(5 * time.Second)
		if got := strings.Count(buffer.String(), "并发行"); got != writers*lines {
			t.Fatalf("第 %d 轮：运行日志 %d 行，期望 %d 行（丢弃计数 %d）", round, got, writers*lines, registry.consoleDropped.Load())
		}
	}
}
