package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestReclaimResourceBindingsOnceLogs 一轮到期绑定清理写一行汇总（按类型计数），没有清理时不写
func TestReclaimResourceBindingsOnceLogs(t *testing.T) {
	pool, _ := testAccountPool(t, "alice@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &trackedService{pool: pool, requests: newRequestRegistry(ctx)}
	if err := pool.BindResourceKind("live-token", "alice@example.com", "bidi-session"); err != nil {
		t.Fatal(err)
	}
	reclaimLines := func() []api.AdminLog {
		service.requests.mu.Lock()
		defer service.requests.mu.Unlock()
		var lines []api.AdminLog
		for _, entry := range service.requests.logs {
			if strings.HasPrefix(entry.Message, "资源绑定到期清理") {
				lines = append(lines, entry)
			}
		}
		return lines
	}
	if _, backlog := service.reclaimResourceBindingsOnce(ctx, time.Now(), ""); backlog || len(reclaimLines()) != 0 {
		t.Fatal("没有到期绑定时不应写日志")
	}
	next, backlog := service.reclaimResourceBindingsOnce(ctx, time.Now().Add(25*time.Hour), "")
	lines := reclaimLines()
	if backlog || next != "alice@example.com" || len(lines) != 1 {
		t.Fatalf("next=%q backlog=%v 日志=%+v", next, backlog, lines)
	}
	if lines[0].Level != "INFO" || lines[0].Message != "资源绑定到期清理 | 账户=1 | 绑定=1（Live 续接 1） | 账户忙推后=0 | 待处理=0" {
		t.Fatalf("日志 = %+v", lines[0])
	}
}
