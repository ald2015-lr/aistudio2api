package aistudio

import (
	"context"
	"testing"
	"time"
)

// reservationTestAccount 为独占等待测试的账户
const reservationTestAccount = "alice@example.com"

// queueLease 在后台为账户排队取一个共享租约，返回取得的租约
func queueLease(ctx context.Context, pool *AccountPool, accountID string) <-chan *AccountLease {
	leases := make(chan *AccountLease, 1)
	go func() {
		lease, err := pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
		if err == nil {
			leases <- lease
		}
	}()
	return leases
}

// TestReleaseToExclusiveReservationBlocksQueuedLease 失败的请求把租约原子地转成独占等待：释放唤醒的排队请求看到的已经是登记后的账户，
// 不会在释放与登记之间取得租约（原先先释放、再由 AcquireAccount 另行登记，排队的请求可能抢在中间落到坏掉的 Worker 上）；
// 进行中的其他租约照常使用，它们都释放后 Wait 取得独占租约，独占租约释放后排队的请求才取得租约
func TestReleaseToExclusiveReservationBlocksQueuedLease(t *testing.T) {
	pool := testPoolWithAccount(t, reservationTestAccount)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sibling, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	// 两个并发槽位都被占用：排队的请求等槽位
	queued := queueLease(ctx, pool, reservationTestAccount)
	time.Sleep(50 * time.Millisecond)

	reservation, err := failed.ReleaseToExclusiveReservation()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-queued:
		_ = lease.Release()
		t.Fatal("释放与登记之间排队的请求取得了租约")
	case <-time.After(200 * time.Millisecond):
	}
	// 已转换的租约不能再转换；再释放是空操作
	if again, err := failed.ReleaseToExclusiveReservation(); err == nil || again != nil {
		t.Fatalf("已释放的租约再次转换：reservation=%v err=%v，期望报错且不登记", again, err)
	}
	if err := failed.Release(); err != nil {
		t.Fatalf("转换后再释放应为空操作: %v", err)
	}

	exclusive := make(chan *AccountLease, 1)
	go func() {
		lease, err := reservation.Wait(ctx)
		if err == nil {
			exclusive <- lease
		}
	}()
	select {
	case lease := <-exclusive:
		_ = lease.Release()
		t.Fatal("还有进行中的租约时不应取得独占租约")
	case <-time.After(100 * time.Millisecond):
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	var owner *AccountLease
	select {
	case owner = <-exclusive:
	case <-time.After(5 * time.Second):
		t.Fatal("进行中的租约都释放后应取得独占租约")
	}
	select {
	case lease := <-queued:
		_ = lease.Release()
		t.Fatal("独占期间排队的请求取得了租约")
	case <-time.After(100 * time.Millisecond):
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-queued:
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("独占租约释放后排队的请求应取得租约")
	}
	pool.mu.Lock()
	waiters, active, exclusiveHeld := pool.byID[reservationTestAccount].exclusiveWaiters, pool.byID[reservationTestAccount].active, pool.byID[reservationTestAccount].exclusive
	pool.mu.Unlock()
	if waiters != 0 || active != 0 || exclusiveHeld {
		t.Fatalf("结束后 exclusiveWaiters=%d active=%d exclusive=%v，期望都已归还", waiters, active, exclusiveHeld)
	}
}

// TestAccountReservationCancel 撤销独占等待登记后排队的请求立即取得租约；重复撤销与 Wait 之后的撤销都是空操作
func TestAccountReservationCancel(t *testing.T) {
	pool := testPoolWithAccount(t, reservationTestAccount)
	pool.SetPerAccountConcurrency(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failed, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	queued := queueLease(ctx, pool, reservationTestAccount)
	time.Sleep(50 * time.Millisecond)
	reservation, err := failed.ReleaseToExclusiveReservation()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-queued:
		_ = lease.Release()
		t.Fatal("登记期间排队的请求取得了租约")
	case <-time.After(100 * time.Millisecond):
	}
	reservation.Cancel()
	reservation.Cancel()
	select {
	case lease := <-queued:
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("撤销登记后排队的请求应取得租约")
	}

	// ReserveAccount 登记后 Wait 取得独占租约，返回时已撤销登记，之后的撤销不再改动计数
	reserved, err := pool.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := reserved.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reserved.Cancel()
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	waiters := pool.byID[reservationTestAccount].exclusiveWaiters
	pool.mu.Unlock()
	if waiters != 0 {
		t.Fatalf("exclusiveWaiters=%d，期望 0", waiters)
	}
	if _, err := pool.ReserveAccount("missing@example.com"); err == nil {
		t.Fatal("不存在的账户不应登记独占等待")
	}
}
