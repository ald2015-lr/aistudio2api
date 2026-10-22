package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// deadlineSlack 为期限断言的上界余量（-race 与 GOMAXPROCS=2 下调度延迟）；下界余量为 deadlineEarly
const (
	deadlineSlack = 1500 * time.Millisecond
	deadlineEarly = 20 * time.Millisecond
)

// registeredFuncs 返回生成请求上下文上登记的结束回调数（已结束时为 0）
func registeredFuncs(c *generateContext) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.funcs)
}

// awaitDone 等 ctx 结束，超过 limit 时失败
func awaitDone(t *testing.T, ctx context.Context, limit time.Duration, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(limit):
		t.Fatalf("%s 在 %s 内没有结束", what, limit)
	}
}

// stillRunning 判断 ctx 此刻是否还没有结束
func stillRunning(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

type generateContextTestKey struct{}

// TestGenerateContextStartDeadline U1：首事件前到期时 Err 为 DeadlineExceeded 本身，阶段为首事件前；三种方式直接派生的子与
// 经 WithValue 包装后派生的子都以 DeadlineExceeded 结束（Err 与 Cause 都是），不会变成取消
func TestGenerateContextStartDeadline(t *testing.T) {
	const limit = 60 * time.Millisecond
	startedAt := time.Now()
	gc := newGenerateContext(context.Background(), startedAt, limit, time.Hour)
	defer gc.cancel()
	if deadline, ok := gc.Deadline(); !ok || !deadline.Equal(startedAt.Add(limit)) {
		t.Fatalf("Deadline = %v %v，期望首事件前期限 %v", deadline, ok, startedAt.Add(limit))
	}
	plain, cancelPlain := context.WithCancel(gc)
	defer cancelPlain()
	withCause, cancelCause := context.WithCancelCause(gc)
	defer cancelCause(nil)
	afterCalled := make(chan struct{})
	stopAfter := context.AfterFunc(gc, func() { close(afterCalled) })
	defer stopAfter()
	wrapped, cancelWrapped := context.WithCancel(context.WithValue(gc, generateContextTestKey{}, 1))
	defer cancelWrapped()
	if generateContextFrom(wrapped) != gc {
		t.Fatal("经 WithValue 包装后应仍能取到生成请求上下文")
	}

	awaitDone(t, gc, limit+deadlineSlack, "生成请求上下文")
	if elapsed := time.Since(startedAt); elapsed < limit-deadlineEarly {
		t.Fatalf("用时 %s 就结束，期望不早于首事件前期限 %s", elapsed, limit)
	}
	if err := gc.Err(); err != context.DeadlineExceeded {
		t.Fatalf("Err = %v，期望 context.DeadlineExceeded 本身", err)
	}
	if cause := gc.deadlineCause(); cause == nil || cause.stage != serviceDeadlineStart || cause.limit != limit || !gc.startExpired() {
		t.Fatalf("deadlineCause = %+v，期望首事件前 %s", cause, limit)
	}
	for name, child := range map[string]context.Context{"WithCancel": plain, "WithCancelCause": withCause, "WithValue+WithCancel": wrapped} {
		awaitDone(t, child, time.Second, name)
		if child.Err() != context.DeadlineExceeded || context.Cause(child) != context.DeadlineExceeded {
			t.Fatalf("%s：Err=%v Cause=%v，期望都是 DeadlineExceeded", name, child.Err(), context.Cause(child))
		}
	}
	select {
	case <-afterCalled:
	case <-time.After(time.Second):
		t.Fatal("context.AfterFunc 登记的函数没有被调用")
	}
}

// TestGenerateContextExtends U2：首事件前期限到期前 beginStreaming 成功，之前派生的子的期限随之变为总时长上限，
// 越过旧期限 3 倍时间仍未结束，到总时长上限时以总时长上限结束
func TestGenerateContextExtends(t *testing.T) {
	const start, total = 100 * time.Millisecond, 700 * time.Millisecond
	startedAt := time.Now()
	gc := newGenerateContext(context.Background(), startedAt, start, total)
	defer gc.cancel()
	child, cancelChild := context.WithCancel(gc)
	defer cancelChild()
	if !gc.beginStreaming() {
		t.Fatal("期限到期前 beginStreaming 应成功")
	}
	if deadline, _ := child.Deadline(); !deadline.Equal(startedAt.Add(total)) {
		t.Fatalf("子的期限 = %v，期望延长到总时长上限 %v", deadline, startedAt.Add(total))
	}
	select {
	case <-child.Done():
		t.Fatalf("延长后子在旧期限的 3 倍时间内结束: %v", child.Err())
	case <-time.After(time.Until(startedAt.Add(3 * start))):
	}
	if !stillRunning(gc) {
		t.Fatalf("延长后在旧期限的 3 倍时间内结束: %v", gc.Err())
	}
	awaitDone(t, child, time.Until(startedAt.Add(total))+deadlineSlack, "延长后的子")
	if elapsed := time.Since(startedAt); elapsed < total-deadlineEarly {
		t.Fatalf("用时 %s 就结束，期望不早于总时长上限 %s", elapsed, total)
	}
	if gc.Err() != context.DeadlineExceeded || child.Err() != context.DeadlineExceeded {
		t.Fatalf("Err = %v / %v，期望 DeadlineExceeded", gc.Err(), child.Err())
	}
	if cause := gc.deadlineCause(); cause == nil || cause.stage != serviceDeadlineTotal || cause.limit != total || gc.startExpired() {
		t.Fatalf("deadlineCause = %+v，期望总时长上限 %s", cause, total)
	}
}

// TestGenerateContextBeginAfterExpiry U3：首事件前期限到期后 beginStreaming 返回 false，Err 仍为 DeadlineExceeded
func TestGenerateContextBeginAfterExpiry(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), 10*time.Millisecond, time.Hour)
	awaitDone(t, gc, deadlineSlack, "生成请求上下文")
	if gc.beginStreaming() {
		t.Fatal("到期后 beginStreaming 应返回 false")
	}
	if gc.Err() != context.DeadlineExceeded || !gc.startExpired() {
		t.Fatalf("Err = %v cause=%+v，期望仍为首事件前到期", gc.Err(), gc.deadlineCause())
	}
}

// TestGenerateContextBeginRace U4：首事件前期限与 beginStreaming 在随机时刻相遇：成功时请求与之前派生的子都继续，
// 失败时一定是首事件前到期，子也已结束；两者不会交错（不会成功后又按旧期限结束）
func TestGenerateContextBeginRace(t *testing.T) {
	for i := 0; i < 500; i++ {
		gc := newGenerateContext(context.Background(), time.Now(), time.Millisecond, time.Hour)
		child, cancelChild := context.WithCancel(gc)
		time.Sleep(rand.N(2 * time.Millisecond))
		if gc.beginStreaming() {
			time.Sleep(5 * time.Millisecond)
			if !stillRunning(gc) || !stillRunning(child) {
				t.Fatalf("第 %d 次：beginStreaming 成功后仍结束了: %v / %v", i, gc.Err(), child.Err())
			}
		} else {
			if gc.Err() != context.DeadlineExceeded || !gc.startExpired() {
				t.Fatalf("第 %d 次：beginStreaming 失败时 Err=%v cause=%+v，期望首事件前到期", i, gc.Err(), gc.deadlineCause())
			}
			awaitDone(t, child, time.Second, "子上下文")
			if child.Err() != context.DeadlineExceeded {
				t.Fatalf("第 %d 次：子 Err = %v，期望 DeadlineExceeded", i, child.Err())
			}
		}
		cancelChild()
		gc.cancel()
	}
}

// TestGenerateContextCancelAndParent U5：cancel 为 Canceled，直接子在 cancel 返回时已结束；父上下文取消时按父的错误结束，
// 父在构造前已取消时构造后立即结束；expire 与 cancel 先到先得
func TestGenerateContextCancelAndParent(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	child, cancelChild := context.WithCancel(gc)
	defer cancelChild()
	gc.cancel()
	if gc.Err() != context.Canceled || gc.deadlineCause() != nil {
		t.Fatalf("Err = %v cause=%+v，期望 Canceled、没有本服务期限", gc.Err(), gc.deadlineCause())
	}
	if stillRunning(child) || child.Err() != context.Canceled {
		t.Fatalf("直接子在 cancel 返回时应已结束: %v", child.Err())
	}

	parent, cancelParent := context.WithCancel(context.Background())
	gc = newGenerateContext(parent, time.Now(), time.Hour, time.Hour)
	cancelParent()
	awaitDone(t, gc, time.Second, "父上下文取消后的生成请求上下文")
	if gc.Err() != parent.Err() {
		t.Fatalf("Err = %v，期望父上下文的错误 %v", gc.Err(), parent.Err())
	}

	gc = newGenerateContext(parent, time.Now(), time.Hour, time.Hour)
	if gc.Err() != context.Canceled || stillRunning(gc) {
		t.Fatalf("父上下文已取消时构造后应立即结束: %v", gc.Err())
	}

	gc = newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	if !gc.expire(&serviceDeadlineError{stage: serviceDeadlineIdle, limit: time.Second}) {
		t.Fatal("首次 expire 应生效")
	}
	gc.cancel()
	if gc.Err() != context.DeadlineExceeded || gc.deadlineCause().stage != serviceDeadlineIdle {
		t.Fatalf("先 expire 再 cancel：Err=%v cause=%+v，期望仍为空闲到期", gc.Err(), gc.deadlineCause())
	}
	gc = newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	gc.cancel()
	if gc.expire(&serviceDeadlineError{stage: serviceDeadlineIdle, limit: time.Second}) || gc.Err() != context.Canceled {
		t.Fatalf("先 cancel 再 expire：expire 应返回 false，Err=%v", gc.Err())
	}
	var none *generateContext
	if none.expire(&serviceDeadlineError{}) || !none.beginStreaming() || none.deadlineCause() != nil || none.startExpired() || none.idleTimeout() != 0 {
		t.Fatal("nil 生成请求上下文应按普通 context 处理")
	}
}

// TestGenerateContextAfterFunc U6：结束前 stop 成功则 f 不会被调用；结束后 stop 失败且 f 恰好调用一次；结束后登记的 f 在
// goroutine 里调用；并发登记、stop 与 cancel 交错时 stop 返回 true 当且仅当 f 没有被调用
func TestGenerateContextAfterFunc(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	var stoppedCalls, firedCalls atomic.Int32
	stop := gc.AfterFunc(func() { stoppedCalls.Add(1) })
	fired := gc.AfterFunc(func() { firedCalls.Add(1) })
	if !stop() {
		t.Fatal("结束前 stop 应返回 true")
	}
	gc.cancel()
	if stoppedCalls.Load() != 0 {
		t.Fatal("已 stop 的函数不应被调用")
	}
	if fired() || firedCalls.Load() != 1 {
		t.Fatalf("结束后 stop 应返回 false，函数调用 %d 次，期望恰好 1 次", firedCalls.Load())
	}
	late := make(chan struct{})
	if gc.AfterFunc(func() { close(late) })() {
		t.Fatal("结束后登记的 stop 应返回 false")
	}
	select {
	case <-late:
	case <-time.After(time.Second):
		t.Fatal("结束后登记的函数应在 goroutine 里被调用")
	}

	gc = newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	const workers = 100
	calls := make([]atomic.Int32, workers)
	stopped := make([]bool, workers)
	var wait sync.WaitGroup
	release := make(chan struct{})
	for i := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-release
			stop := gc.AfterFunc(func() { calls[i].Add(1) })
			if i%2 == 0 {
				stopped[i] = stop()
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-release
		gc.cancel()
	}()
	close(release)
	wait.Wait()
	for i := range workers {
		if stopped[i] {
			if calls[i].Load() != 0 {
				t.Fatalf("第 %d 个：stop 返回 true 但函数被调用了", i)
			}
			continue
		}
		if !waitUntil(time.Second, func() bool { return calls[i].Load() == 1 }) {
			t.Fatalf("第 %d 个：stop 返回 false（或没有 stop），函数调用 %d 次，期望恰好 1 次", i, calls[i].Load())
		}
	}
	time.Sleep(20 * time.Millisecond)
	for i := range workers {
		if calls[i].Load() > 1 {
			t.Fatalf("第 %d 个函数调用了 %d 次", i, calls[i].Load())
		}
	}
}

// TestGenerateContextChildRegistration U7：直接派生的子登记在生成请求上下文上（不起 goroutine），子取消后经 removeChild 解除登记
func TestGenerateContextChildRegistration(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	defer gc.cancel()
	cancels := make([]context.CancelFunc, 0, 1000)
	for range 1000 {
		_, cancel := context.WithCancel(gc)
		cancels = append(cancels, cancel)
	}
	if got := registeredFuncs(gc); got != 1000 {
		t.Fatalf("登记数 = %d，期望 1000（子直接登记在生成请求上下文上）", got)
	}
	for _, cancel := range cancels {
		cancel()
	}
	if got := registeredFuncs(gc); got != 0 {
		t.Fatalf("子全部取消后登记数 = %d，期望 0", got)
	}
}

// TestGenerateContextWithTimeoutChild U8：首事件前派生的 WithTimeout 自己的期限更短时按自己的结束；
// 长于生成请求上下文当时的期限时标准库直接返回 WithCancel，延长后跟着延长、不再有自己的期限（记录标准库行为）
func TestGenerateContextWithTimeoutChild(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), time.Second, time.Hour)
	defer gc.cancel()
	short, cancelShort := context.WithTimeout(gc, 50*time.Millisecond)
	defer cancelShort()
	awaitDone(t, short, deadlineSlack, "期限更短的子")
	if short.Err() != context.DeadlineExceeded || !stillRunning(gc) {
		t.Fatalf("期限更短的子应按自己的期限结束、不影响请求: %v / %v", short.Err(), gc.Err())
	}

	startedAt := time.Now()
	gc = newGenerateContext(context.Background(), startedAt, 100*time.Millisecond, time.Hour)
	defer gc.cancel()
	long, cancelLong := context.WithTimeout(gc, 200*time.Millisecond)
	defer cancelLong()
	if !gc.beginStreaming() {
		t.Fatal("beginStreaming 应成功")
	}
	if deadline, _ := long.Deadline(); !deadline.Equal(startedAt.Add(time.Hour)) {
		t.Fatalf("期限更长的子 Deadline = %v，期望跟着延长到总时长上限", deadline)
	}
	select {
	case <-long.Done():
		t.Fatalf("期限更长的子在延长后不应按自己的 200ms 结束: %v", long.Err())
	case <-time.After(time.Until(startedAt.Add(600 * time.Millisecond))):
	}
}

// TestServiceDeadlineErrorText U9：三种本服务期限的错误文本；只满足 DeadlineExceeded、不满足 Canceled，状态记 failed；
// startDeadlineError 的规则；标注后的错误不换号
func TestServiceDeadlineErrorText(t *testing.T) {
	queued := fmt.Errorf("排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: %w", context.DeadlineExceeded)
	for _, test := range []struct {
		err  *serviceDeadlineError
		want string
	}{
		{&serviceDeadlineError{stage: serviceDeadlineStart, limit: 5 * time.Minute, where: "等待上游首事件"},
			"本服务首事件前期限 5m0s 到期（停在 等待上游首事件）: context deadline exceeded"},
		{&serviceDeadlineError{stage: serviceDeadlineStart, limit: 5 * time.Minute, inner: queued},
			"本服务首事件前期限 5m0s 到期: 排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded"},
		{&serviceDeadlineError{stage: serviceDeadlineStart, limit: 5 * time.Minute, where: "WAA proof", inner: queued},
			"本服务首事件前期限 5m0s 到期（停在 WAA proof）: 排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded"},
		{(&serviceDeadlineError{stage: serviceDeadlineIdle, limit: 5 * time.Minute}).withOutput(812, 40),
			"本服务首事件后空闲期限 5m0s 到期（已输出 推理=812 正文=40）: context deadline exceeded"},
		{(&serviceDeadlineError{stage: serviceDeadlineTotal, limit: time.Hour}).withOutput(812, 4033),
			"本服务总时长上限 1h0m0s 到期（已输出 推理=812 正文=4033）: context deadline exceeded"},
	} {
		if got := test.err.Error(); got != test.want {
			t.Fatalf("错误文本 = %q，期望 %q", got, test.want)
		}
		if !errors.Is(test.err, context.DeadlineExceeded) || errors.Is(test.err, context.Canceled) || finalRequestState(test.err) != "failed" {
			t.Fatalf("%q 应只满足 DeadlineExceeded、状态 failed", test.err)
		}
	}
	if serviceDeadlineStart.label() != "首事件前" || serviceDeadlineIdle.label() != "首事件后空闲" || serviceDeadlineTotal.label() != "总时长上限" {
		t.Fatal("阶段名不对")
	}

	running := newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	defer running.cancel()
	if _, ok := running.startDeadlineError(context.DeadlineExceeded, ""); ok {
		t.Fatal("期限没有到期时不应标注")
	}
	var none *generateContext
	if _, ok := none.startDeadlineError(context.DeadlineExceeded, ""); ok {
		t.Fatal("nil 生成请求上下文不应标注")
	}
	expired := newGenerateContext(context.Background(), time.Now(), time.Millisecond, time.Hour)
	awaitDone(t, expired, deadlineSlack, "生成请求上下文")
	if _, ok := expired.startDeadlineError(nil, ""); ok {
		t.Fatal("没有错误时不应标注")
	}
	canceled, ok := expired.startDeadlineError(fmt.Errorf("尝试被取消: %w", context.Canceled), "等待上游首事件")
	if !ok || canceled.inner != nil || errors.Is(canceled, context.Canceled) || !errors.Is(canceled, context.DeadlineExceeded) ||
		canceled.Error() != "本服务首事件前期限 1ms 到期（停在 等待上游首事件）: context deadline exceeded" {
		t.Fatalf("Canceled 应丢弃原错误: ok=%v err=%v", ok, canceled)
	}
	joined, ok := expired.startDeadlineError(errors.Join(context.DeadlineExceeded, context.Canceled), "")
	if !ok || joined.inner != nil || errors.Is(joined, context.Canceled) {
		t.Fatalf("同时满足 Canceled 时应丢弃原错误: ok=%v err=%v", ok, joined)
	}
	kept, ok := expired.startDeadlineError(queued, "")
	if !ok || kept.inner != queued || !errors.Is(kept, context.DeadlineExceeded) ||
		kept.Error() != "本服务首事件前期限 1ms 到期: "+queued.Error() {
		t.Fatalf("DeadlineExceeded 应保留原错误: ok=%v err=%v", ok, kept)
	}
	rpc := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Message: "internal"}
	if _, ok := expired.startDeadlineError(rpc, ""); ok {
		t.Fatal("上游的真实错误应保留原样")
	}
	if _, ok := expired.startDeadlineError(fmt.Errorf("再包一层: %w", kept), ""); ok {
		t.Fatal("已经标注过的错误不应重复包装")
	}
	if retryableGenerateAccountError(expired, kept) || retryableGenerateAccountError(expired, canceled) {
		t.Fatal("首事件前期限到期后不应换号")
	}
	if !retryableGenerateAccountError(context.Background(), &firstEventTimeoutError{timeout: time.Second}) {
		t.Fatal("请求没有结束时首事件超时仍应换号")
	}
}

// TestGenerateContextRequestTimeoutAboveTotal U10：REQUEST_TIMEOUT 不短于总时长上限时总上限取 REQUEST_TIMEOUT；
// 首事件前到期标为首事件前，首事件后到期标为总时长上限
func TestGenerateContextRequestTimeoutAboveTotal(t *testing.T) {
	if got := generateMaxDurationFor(2 * time.Hour); got != 2*time.Hour {
		t.Fatalf("REQUEST_TIMEOUT=2h 时总上限 = %s", got)
	}
	if got := generateMaxDurationFor(time.Second); got != generateMaxDuration {
		t.Fatalf("REQUEST_TIMEOUT=1s 时总上限 = %s，期望 %s", got, generateMaxDuration)
	}
	const limit = 80 * time.Millisecond
	start := newGenerateContext(context.Background(), time.Now(), limit, limit/2)
	if start.maxDuration != limit || start.startLimit != limit {
		t.Fatalf("总上限 = %s，期望取 REQUEST_TIMEOUT %s", start.maxDuration, limit)
	}
	awaitDone(t, start, limit+deadlineSlack, "首事件前")
	if cause := start.deadlineCause(); cause == nil || cause.stage != serviceDeadlineStart {
		t.Fatalf("cause = %+v，期望首事件前", cause)
	}
	streaming := newGenerateContext(context.Background(), time.Now(), limit, limit/2)
	if !streaming.beginStreaming() {
		t.Fatal("beginStreaming 应成功")
	}
	awaitDone(t, streaming, limit+deadlineSlack, "首事件后")
	if cause := streaming.deadlineCause(); cause == nil || cause.stage != serviceDeadlineTotal || cause.limit != limit {
		t.Fatalf("cause = %+v，期望总时长上限 %s", cause, limit)
	}
}

// TestFirstGenerateEventClosedAfterContextEnds U11：尝试的上下文已结束、上游没发错误就关闭了事件流时按 ctx 的错误返回，
// 不当成上游在首事件前断流；ctx 没有结束时关闭事件流仍是首事件前断流
func TestFirstGenerateEventClosedAfterContextEnds(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		source := make(chan aistudio.Event)
		close(source)
		if _, err := firstGenerateEvent(ctx, source, nil, nil); err != context.Canceled {
			t.Fatalf("第 %d 次：err = %v，期望 ctx 的错误", i, err)
		}
	}
	source := make(chan aistudio.Event)
	close(source)
	if _, err := firstGenerateEvent(context.Background(), source, nil, nil); !errors.Is(err, errStreamClosedBeforeFirstEvent) {
		t.Fatalf("err = %v，期望首事件前断流", err)
	}
}

// TestUpstreamActivityMonotonic U12：最近网络字节按单调时钟计算；quietFor 取事件与网络字节中较近的一次；nil 安全，日志格式不变
func TestUpstreamActivityMonotonic(t *testing.T) {
	var none *upstreamActivity
	if _, ok := none.sinceLastByte(time.Now()); ok || none.logFields(time.Now()) != "网络字节=0" {
		t.Fatal("nil 应当作没有网络字节")
	}
	if now := time.Now(); none.quietFor(now, now.Add(-time.Second)) != time.Second {
		t.Fatal("nil 时 quietFor 应按最近事件计")
	}
	activity := &upstreamActivity{}
	if _, ok := activity.sinceLastByte(time.Now()); ok || activity.logFields(time.Now()) != "网络字节=0" {
		t.Fatal("没有字节时 sinceLastByte 应为 ok=false")
	}
	activity.observe(0)
	if _, ok := activity.sinceLastByte(time.Now()); ok {
		t.Fatal("0 字节不应计为活动")
	}
	observedAt := time.Now()
	activity.observe(10)
	time.Sleep(30 * time.Millisecond)
	now := time.Now()
	since, ok := activity.sinceLastByte(now)
	if !ok || since < now.Sub(observedAt)-deadlineEarly || since > now.Sub(observedAt)+deadlineSlack {
		t.Fatalf("sinceLastByte = %s %v，期望约 %s", since, ok, now.Sub(observedAt))
	}
	if quiet := activity.quietFor(now, now.Add(-time.Hour)); quiet != since {
		t.Fatalf("网络字节更近时 quietFor = %s，期望 %s", quiet, since)
	}
	if quiet := activity.quietFor(now, now.Add(-time.Millisecond)); quiet != time.Millisecond {
		t.Fatalf("事件更近时 quietFor = %s，期望 1ms", quiet)
	}
	if fields := activity.logFields(now); !regexp.MustCompile(`^网络字节=10 \| 最近网络=[0-9.]+m?s$`).MatchString(fields) {
		t.Fatalf("logFields = %q，格式应不变", fields)
	}
}
