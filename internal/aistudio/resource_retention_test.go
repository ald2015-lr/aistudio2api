package aistudio

import (
	"context"
	"errors"
	"testing"
	"time"
)

func retentionTestPool(t *testing.T, ids ...string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	for _, id := range ids {
		account, publish, err := store.Create(DefaultAccountConfig(id), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func boundResources(t *testing.T, pool *AccountPool, accountID string) map[string]string {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	result := map[string]string{}
	for resourceID, owner := range pool.resources {
		if owner == accountID {
			result[resourceID] = pool.byID[accountID].runtime.Resources[resourceID].Kind
		}
	}
	return result
}

func diskResources(t *testing.T, pool *AccountPool, accountID string) map[string]ResourceBinding {
	t.Helper()
	pool.mu.Lock()
	path := pool.byID[accountID].RuntimePath
	pool.mu.Unlock()
	state, err := readRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	return state.Resources
}

// TestReclaimExpiredResourceBindings 过了保留期的 Live 续接凭证（24 小时）与视频任务、结果文件（72 小时）的本地绑定被清理，
// 内存与 runtime-state.json 一致；上传文件与未标类型的绑定不按时间清理
func TestReclaimExpiredResourceBindings(t *testing.T) {
	const id = "alice@example.com"
	pool := retentionTestPool(t, id)
	ctx := context.Background()
	for resourceID, kind := range map[string]string{
		"live-token": "bidi-session", "operations/op1": "video-operation", "video-file-1": "video-file", "plain": "",
	} {
		if kind == "video-operation" {
			if _, err := pool.bindVideoOperation(ctx, resourceID, id, VideoResourceMetadata{Model: "veo", Seconds: "8", Size: "1280x720"}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := pool.BindResourceKind(resourceID, id, kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.bindFileResource(ctx, FileRef{ID: "upload-1", Name: "a.txt", MIME: "text/plain"}, id, 3, "user_data"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	if result := pool.ReclaimExpiredResourceBindings(ctx, now.Add(23*time.Hour), 0, ""); result.Bindings != 0 || result.Accounts != 0 {
		t.Fatalf("还没到保留期不应清理: %+v", result)
	}
	result := pool.ReclaimExpiredResourceBindings(ctx, now.Add(25*time.Hour), 0, "")
	if result.Accounts != 1 || result.Bindings != 1 || result.ByKind["bidi-session"] != 1 || result.Err != nil {
		t.Fatalf("24 小时后只清 Live 续接凭证: %+v", result)
	}
	if got := boundResources(t, pool, id); len(got) != 4 || got["live-token"] != "" {
		t.Fatalf("清理后的绑定 = %v", got)
	}
	// 官方只保存 2 天的视频，刚过 2 天时本地绑定仍要在（保留期留了余量，官方仍可下载期间不能先清掉）
	if result := pool.ReclaimExpiredResourceBindings(ctx, now.Add(49*time.Hour), 0, ""); result.Bindings != 0 {
		t.Fatalf("刚过官方 2 天时视频任务与结果文件的绑定应保留: %+v", result)
	}
	if got := boundResources(t, pool, id); got["operations/op1"] != "video-operation" || got["video-file-1"] != "video-file" {
		t.Fatalf("刚过官方 2 天时视频绑定应仍在，实际 %v", got)
	}
	result = pool.ReclaimExpiredResourceBindings(ctx, now.Add(73*time.Hour), 0, "")
	if result.Bindings != 2 || result.ByKind["video-operation"] != 1 || result.ByKind["video-file"] != 1 {
		t.Fatalf("72 小时后清视频任务与结果文件: %+v", result)
	}
	got := boundResources(t, pool, id)
	if len(got) != 2 || got["upload-1"] != "drive-file" || got["plain"] != "" {
		t.Fatalf("上传文件与未标类型的绑定应保留，实际 %v", got)
	}
	if _, ok := got["plain"]; !ok {
		t.Fatal("未标类型的绑定应保留")
	}
	disk := diskResources(t, pool, id)
	if len(disk) != 2 || disk["upload-1"].Kind != "drive-file" {
		t.Fatalf("runtime-state.json 应同步清理，实际 %v", disk)
	}
	if _, err := pool.FileMetadata(ctx, "upload-1"); err != nil {
		t.Fatalf("上传文件仍应可查: %v", err)
	}
	if err := pool.UnbindResource("operations/op1"); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("清理后的视频任务应不存在，实际 %v", err)
	}
	if result := pool.ReclaimExpiredResourceBindings(ctx, now.Add(1000*time.Hour), 0, ""); result.Accounts != 0 || result.Bindings != 0 {
		t.Fatalf("没有到期绑定时不应写回: %+v", result)
	}
}

// TestReclaimWhileAccountServesRequests 账户一直有普通请求（Ultra 常驻、Live 会话多的高负载账户）时到期绑定照常清理：
// 能清的都已过了官方有效期，不会再被续接、轮询或下载；还没到期的绑定与请求的租约都不受影响
func TestReclaimWhileAccountServesRequests(t *testing.T) {
	const id = "alice@example.com"
	pool := retentionTestPool(t, id)
	ctx := context.Background()
	if err := pool.BindResourceKind("live-token", id, "bidi-session"); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, AccountSelection{ResourceID: "live-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	later := time.Now().Add(48 * time.Hour)
	if err := pool.BindResourceKind("fresh-token", id, "bidi-session"); err != nil {
		t.Fatal(err)
	}
	if result := pool.ReclaimExpiredResourceBindings(ctx, later, 0, ""); result.Bindings != 2 || result.Busy != 0 {
		t.Fatalf("账户有普通请求时到期绑定应照常清理: %+v", result)
	}
	if got := boundResources(t, pool, id); len(got) != 0 {
		t.Fatalf("到期绑定应清掉，实际 %v", got)
	}
	if err := pool.BindResourceKind("new-token", id, "bidi-session"); err != nil {
		t.Fatal(err)
	}
	if result := pool.ReclaimExpiredResourceBindings(ctx, time.Now(), 0, ""); result.Bindings != 0 {
		t.Fatalf("没到期的绑定不应清理: %+v", result)
	}
	if got := boundResources(t, pool, id); got["new-token"] != "bidi-session" {
		t.Fatalf("新绑定应保留，实际 %v", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("清理不应影响请求的租约: %v", err)
	}
}

// TestReclaimDefersExclusiveAccount 账户正持有独占租约（登录、重新导入、删除、关闭 Worker）时不清理，推到下一轮；释放后清理
func TestReclaimDefersExclusiveAccount(t *testing.T) {
	const id = "alice@example.com"
	pool := retentionTestPool(t, id)
	ctx := context.Background()
	if err := pool.BindResourceKind("live-token", id, "bidi-session"); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	if result := pool.ReclaimExpiredResourceBindings(ctx, later, 0, ""); result.Busy != 1 || result.Bindings != 0 {
		t.Fatalf("独占操作期间应推后: %+v", result)
	}
	if got := boundResources(t, pool, id); got["live-token"] != "bidi-session" {
		t.Fatalf("独占操作期间绑定应保留，实际 %v", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if result := pool.ReclaimExpiredResourceBindings(ctx, later, 0, ""); result.Bindings != 1 || result.Busy != 0 {
		t.Fatalf("独占释放后应清理: %+v", result)
	}
}

// TestReclaimBatchLimitAndCursor 每轮最多处理 limit 个账户，下一轮从上一轮最后处理的账户之后继续
func TestReclaimBatchLimitAndCursor(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	pool := retentionTestPool(t, ids...)
	ctx := context.Background()
	for _, id := range ids {
		if err := pool.BindResourceKind("token-"+id, id, "bidi-session"); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(48 * time.Hour)
	first := pool.ReclaimExpiredResourceBindings(ctx, later, 2, "a@example.com")
	if first.Accounts != 2 || first.Remaining != 1 || first.Next != "c@example.com" {
		t.Fatalf("第一轮应从 a 之后开始、处理 b、c 后停在上限: %+v", first)
	}
	if got := boundResources(t, pool, "a@example.com"); len(got) != 1 {
		t.Fatalf("a 留到下一轮之前不应清理: %v", got)
	}
	second := pool.ReclaimExpiredResourceBindings(ctx, later, 2, first.Next)
	if second.Accounts != 1 || second.Remaining != 0 {
		t.Fatalf("第二轮处理剩下的 a: %+v", second)
	}
	for _, id := range ids {
		if got := boundResources(t, pool, id); len(got) != 0 {
			t.Fatalf("%s 的绑定应已清理: %v", id, got)
		}
	}
}
