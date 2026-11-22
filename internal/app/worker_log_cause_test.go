package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWorkerLogCauses “WAA Worker 就绪”与“WAA Worker 停止/已停止”写起因（参照上游 c77dd89 的 trigger）：空槽位上的启动为“空槽位”，
// 按需启动换掉另一个账户的空闲 Worker 时被换下的停止行与新 Worker 的就绪行都是“替换”，超出常驻数的空闲 Worker 被回收为“空闲回收”，
// 生成服务停止时为“服务停止”；没有写明起因的调用不写这个键。来源键与原有键照旧，诊断脚本按标题与键取值不受影响
func TestWorkerLogCauses(t *testing.T) {
	ids := []string{"cause-a@example.com", "cause-b@example.com"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	full, _, requests := partitionTestManager(t, ids, nil, 1, 1, 0, 1)
	if _, err := full.promote(ctx, ids[0], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if countSourceLogs(requests, ids[0], "WAA Worker 就绪 | 页面模型=", "| 来源=按需 | 起因=空槽位") != 1 {
		t.Fatalf("空槽位上的启动应写起因=空槽位：%v", serviceLogMessages(requests))
	}
	if _, err := full.promote(ctx, ids[1], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{"WAA Worker 停止 | PID=", "WAA Worker 已停止 | PID="} {
		if countSourceLogs(requests, ids[0], head, "| 起因=替换") != 1 {
			t.Fatalf("被换下的 Worker 的 %q 应写起因=替换：%v", head, serviceLogMessages(requests))
		}
	}
	if countSourceLogs(requests, ids[1], "WAA Worker 就绪 | 页面模型=", "| 来源=按需 | 起因=替换") != 1 {
		t.Fatalf("换掉空闲 Worker 的启动应写起因=替换：%v", serviceLogMessages(requests))
	}

	spare, _, spareRequests := partitionTestManager(t, ids, nil, 1, 2, 0, 1)
	for _, accountID := range ids {
		if _, err := spare.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	if !spare.reapIdleWorker(time.Now().Add(time.Hour)) {
		t.Fatal("超出常驻数的空闲 Worker 应被回收")
	}
	if countSourceLogs(spareRequests, "", "WAA Worker 已停止 | PID=", "| 起因=空闲回收") != 1 {
		t.Fatalf("空闲回收应写起因=空闲回收：%v", serviceLogMessages(spareRequests))
	}
	if err := spare.ResetAll(); err != nil {
		t.Fatal(err)
	}
	if countSourceLogs(spareRequests, "", "WAA Worker 已停止 | PID=", "| 起因=服务停止") != 1 {
		t.Fatalf("生成服务停止时应写起因=服务停止：%v", serviceLogMessages(spareRequests))
	}

	plain, _, plainRequests := partitionTestManager(t, ids[:1], nil, 1, 1, 0, 1)
	if _, err := plain.promote(ctx, ids[0], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if err := plain.Reset(ids[0]); err != nil {
		t.Fatal(err)
	}
	if countSourceLogs(plainRequests, ids[0], "WAA Worker 已停止 | PID=") != 1 || hasLogContaining(plainRequests, "WAA Worker 已停止", "起因=") {
		t.Fatalf("没有写明起因的停止不应写起因键：%v", serviceLogMessages(plainRequests))
	}
}

// TestAuthRefreshResetLogsCause 认证续签（2/2 重置协议运行时）经生产环境的续签器关闭 Worker 时，停止行写“起因=认证续签”：
// 续签器用不写起因的 Reset 时，diag_403.sh 会把这类停止归为“未写起因（旧版本）”
func TestAuthRefreshResetLogsCause(t *testing.T) {
	ids := []string{"cause-auth@example.com"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager, _, requests := partitionTestManager(t, ids, nil, 1, 1, 0, 1)
	if _, err := manager.promote(ctx, ids[0], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	refresher := newAuthRuntimeRefresher(manager, nil, requests, "")
	if err := refresher.reset(ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{"WAA Worker 停止 | PID=", "WAA Worker 已停止 | PID="} {
		if countSourceLogs(requests, ids[0], head, "| 起因=认证续签") != 1 {
			t.Fatalf("认证续签关闭 Worker 的 %q 应写起因=认证续签：%v", head, serviceLogMessages(requests))
		}
	}
}

// TestDisableCompletionLogged 停用收尾结束写“账户停用完成”（原先只在失败时写“账户停用收尾失败”），关闭 Worker 的停止行写起因=停用；
// 收尾时账户已重新启用、收尾期间又启用时都不写完成行。每种情况各用一个子测试：测试账户共用同一个账户 ID，上一个管理端的 Worker
// 占着跨进程的运行租约，要等子测试结束清理后下一个才能启动
func TestDisableCompletionLogged(t *testing.T) {
	t.Run("收尾完成", func(t *testing.T) {
		admin, account := transitionTestAdmin(t)
		toggleWithin(t, admin, account.ID, false, time.Second)
		waitFor(t, "停用收尾结束", func() bool { return !admin.transitions.active(account.ID) })
		if !hasLogContaining(admin.requests, "账户停用完成 | 耗时=") || !hasLogContaining(admin.requests, "WAA Worker 已停止", "起因=停用") {
			t.Fatalf("停用收尾应写完成行与起因：%v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("收尾时已重新启用", func(t *testing.T) {
		admin, account := transitionTestAdmin(t)
		admin.finishDisable(context.Background(), account.ID, account.Config.Label)
		if hasLogContaining(admin.requests, "账户停用完成") || hasLogContaining(admin.requests, "起因=停用") {
			t.Fatalf("账户仍启用时不应关闭 Worker、写完成行：%v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("释放租约后配置被改写", func(t *testing.T) {
		// 收尾释放独占租约之后，别的租约（例如跟随默认代理的批量保存）可以立即取得账户并整份写配置：结束行按收尾在租约内读到的
		// 停用状态决定。原先释放之后再读 account.Config.Enabled（与写方没有先后关系），这里读到改写后的值就不写完成行
		admin, account := transitionTestAdmin(t)
		var hookErr error
		hook := func(accountID string) {
			reservation, err := admin.pool.ReserveAccount(accountID)
			if err != nil {
				hookErr = err
				return
			}
			lease, err := reservation.Wait(context.Background())
			if err != nil {
				hookErr = err
				return
			}
			config := lease.Account().Config
			config.Enabled = true
			hookErr = errors.Join(lease.SaveConfig(config), lease.Release())
		}
		finishDisableReleasedHook.Store(&hook)
		t.Cleanup(func() { finishDisableReleasedHook.Store(nil) })
		toggleWithin(t, admin, account.ID, false, time.Second)
		waitFor(t, "停用收尾结束", func() bool { return !admin.transitions.active(account.ID) })
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if !hasLogContaining(admin.requests, "账户停用完成 | 耗时=") || !hasLogContaining(admin.requests, "WAA Worker 已停止", "起因=停用") {
			t.Fatalf("Worker 已在租约内关闭，应写完成行：%v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("收尾期间又启用", func(t *testing.T) {
		admin, account := transitionTestAdmin(t)
		release := holdLease(t, admin.pool, account.ID)
		toggleWithin(t, admin, account.ID, false, time.Second)
		toggleWithin(t, admin, account.ID, true, 2*time.Second)
		waitFor(t, "启用收尾结束", func() bool { return !admin.transitions.active(account.ID) })
		release()
		time.Sleep(100 * time.Millisecond)
		if hasLogContaining(admin.requests, "账户停用完成") {
			t.Fatal("取消的停用收尾不应写完成行")
		}
	})
}
