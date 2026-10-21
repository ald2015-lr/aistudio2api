package aistudio

import (
	"context"
	"errors"
	"sync/atomic"
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

// reservationCounts 返回账户的独占等待登记数、进行中的租约数与独占状态
func reservationCounts(pool *AccountPool, accountID string) (int, int, bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	account := pool.byID[accountID]
	return account.exclusiveWaiters, account.active, account.exclusive
}

// TestAccountReservationRemove 登记后删除：进行中的租约照常使用，登记期间排队的请求取不到账户；租约释放后在同一次加锁内转为独占删除，
// 账户移出号池、目录删除函数只调用一次；已经空闲的账户不看 ctx 立即删除；登记的账户已被删除时返回 ErrAccountNotFound
func TestAccountReservationRemove(t *testing.T) {
	pool := testPoolWithAccount(t, reservationTestAccount)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inUse, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := pool.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	queued := queueLease(ctx, pool, reservationTestAccount)
	var deletes atomic.Int32
	removed := make(chan error, 1)
	go func() {
		_, err := reservation.Remove(ctx, func(*Account) error {
			deletes.Add(1)
			return nil
		})
		removed <- err
	}()
	select {
	case lease := <-queued:
		_ = lease.Release()
		t.Fatal("登记期间排队的请求取得了租约")
	case err := <-removed:
		t.Fatalf("还有进行中的租约时不应删除：%v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := inUse.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("租约释放后应删除账户")
	}
	if deletes.Load() != 1 {
		t.Fatalf("目录删除函数调用 %d 次，期望 1", deletes.Load())
	}
	if _, err := pool.Account(reservationTestAccount); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("删除后账户应移出号池：%v", err)
	}
	select {
	case lease := <-queued:
		_ = lease.Release()
		t.Fatal("删除后排队的请求不应取得租约")
	case <-time.After(50 * time.Millisecond):
	}
	reservation.Cancel()

	// 两个登记同时删除同一账户：先删除的成功，另一个返回 ErrAccountNotFound、不再调用删除函数；空闲时已取消的 ctx 也照常删除
	second := testPoolWithAccount(t, reservationTestAccount)
	first, err := second.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	late, err := second.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := first.Remove(done, func(*Account) error { return nil }); err != nil {
		t.Fatalf("空闲账户应立即删除，不看 ctx：%v", err)
	}
	if _, err := late.Remove(ctx, func(*Account) error {
		t.Error("已删除的账户不应再调用删除函数")
		return nil
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("账户已被删除时 err = %v，期望 ErrAccountNotFound", err)
	}
}

// TestAccountReservationRemoveRestores 等不到空闲（ctx 到期）或删除失败时撤销登记与独占：账户留在号池、恢复调度，不残留登记
func TestAccountReservationRemoveRestores(t *testing.T) {
	pool := testPoolWithAccount(t, reservationTestAccount)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inUse, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := pool.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer waitCancel()
	if _, err := reservation.Remove(waitCtx, func(*Account) error {
		t.Error("等不到空闲时不应调用删除函数")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望 ctx 到期", err)
	}
	if waiters, _, exclusive := reservationCounts(pool, reservationTestAccount); waiters != 0 || exclusive {
		t.Fatalf("等待到期后 exclusiveWaiters=%d exclusive=%v，期望登记已撤销", waiters, exclusive)
	}
	if err := inUse.Release(); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, AccountSelection{AccountID: reservationTestAccount})
	if err != nil {
		t.Fatalf("撤销登记后账户应照常调度：%v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	failing, err := pool.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Remove(ctx, func(*Account) error { return errors.New("删除失败") }); err == nil {
		t.Fatal("删除函数失败时应返回错误")
	}
	if waiters, active, exclusive := reservationCounts(pool, reservationTestAccount); waiters != 0 || active != 0 || exclusive {
		t.Fatalf("删除失败后 exclusiveWaiters=%d active=%d exclusive=%v，期望恢复原状", waiters, active, exclusive)
	}
	if _, err := pool.Account(reservationTestAccount); err != nil {
		t.Fatalf("删除失败的账户应留在号池：%v", err)
	}
}
