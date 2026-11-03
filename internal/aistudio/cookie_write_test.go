package aistudio

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// rotatingTestCookies 返回登录 Cookie（值为 login）加上值为 sidcc 的三个轮换 Cookie
func rotatingTestCookies(login string, sidcc string) []StateCookie {
	cookies := testStorageState(login).Cookies
	for _, name := range []string{"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC"} {
		cookies = append(cookies, StateCookie{
			Name: name, Value: sidcc, Domain: ".google.com", Path: "/", Expires: float64(time.Now().Add(time.Hour).Unix()), Secure: true, SameSite: "None",
		})
	}
	return cookies
}

func diskCookie(t *testing.T, path string, name string) string {
	t.Helper()
	state, err := LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := state.CookieValue(name, aiStudioOrigin+"/", time.Now())
	return value
}

// ageStorage 把认证文件的修改时间改到 age 之前（模拟上一次写盘已经过去这么久）
func ageStorage(t *testing.T, path string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestOnlyRotatingCookiesChanged 只比较轮换 Cookie 以外的字段：轮换 Cookie 的值与过期时间变化算“只有轮换”，其他任何变化都不算
func TestOnlyRotatingCookiesChanged(t *testing.T) {
	base := rotatingTestCookies("A", "s1")
	if !onlyRotatingCookiesChanged(base, rotatingTestCookies("A", "s2")) {
		t.Fatal("只有 SIDCC 系列变化应判为只有轮换")
	}
	if onlyRotatingCookiesChanged(base, rotatingTestCookies("B", "s2")) {
		t.Fatal("登录 Cookie 变化不能算只有轮换")
	}
	if onlyRotatingCookiesChanged(base, append(rotatingTestCookies("A", "s1"), StateCookie{Name: "NID", Value: "x", Domain: ".google.com", Path: "/"})) {
		t.Fatal("新增 Cookie 不能算只有轮换")
	}
	changedDomain := rotatingTestCookies("A", "s2")
	changedDomain[3].Domain = "aistudio.google.com"
	if onlyRotatingCookiesChanged(base, changedDomain) {
		t.Fatal("轮换 Cookie 的 domain 变化不能算只有轮换")
	}
	changedFlag := rotatingTestCookies("A", "s2")
	changedFlag[4].HTTPOnly = true
	if onlyRotatingCookiesChanged(base, changedFlag) {
		t.Fatal("轮换 Cookie 的其他属性变化不能算只有轮换")
	}
	// Set-Cookie 合并把更新的 Cookie 挪到末尾：顺序不同不算变化
	reordered := rotatingTestCookies("A", "s2")
	reordered[0], reordered[len(reordered)-1] = reordered[len(reordered)-1], reordered[0]
	if !onlyRotatingCookiesChanged(base, reordered) {
		t.Fatal("只有顺序与轮换值不同应判为只有轮换")
	}
}

// TestRotatingCookieWriteThrottled 只有轮换 Cookie 变化、认证文件 30 秒内写过时不写盘：内存与 Go 通道读取（ReloadStorageState）
// 拿到最新值，其他 Cookie 变化立即写（连同暂存的轮换值），Worker 冷启动前与计时到期时写盘，上一次写盘超过 30 秒时立即写
func TestRotatingCookieWriteThrottled(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	// 第一次带上轮换 Cookie：新增 Cookie，立即写
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s1")); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s1" {
		t.Fatalf("新增 Cookie 应立即写盘，SIDCC=%q", got)
	}
	before, _ := os.Stat(path)
	// 30 秒内只有轮换 Cookie 变化：不写盘，内存与 Go 通道读到的是最新值
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s2")); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) || diskCookie(t, path, "SIDCC") != "s1" {
		t.Fatal("只有轮换 Cookie 变化时 30 秒内不应写盘")
	}
	reloaded, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := reloaded.CookieValue("SIDCC", aiStudioOrigin+"/", time.Now()); value != "s2" {
		t.Fatalf("Go 通道读取应拿到暂存的最新 SIDCC，得到 %q", value)
	}
	// Set-Cookie 里只有轮换 Cookie 同样暂存
	header := "SIDCC=s3; Domain=.google.com; Path=/; Secure; SameSite=None; Expires=" + time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat)
	if err := lease.MergeSetCookieHeaders([]string{header}, aiStudioOrigin+"/", time.Now()); err != nil {
		t.Fatal(err)
	}
	if diskCookie(t, path, "SIDCC") != "s1" {
		t.Fatal("Set-Cookie 只有轮换 Cookie 时 30 秒内不应写盘")
	}
	// Worker 冷启动前写盘：浏览器与纯 Go runtime 从文件读到最新值
	if err := pool.FlushDeferredStorage("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s3" {
		t.Fatalf("冷启动前应写出暂存的 SIDCC，得到 %q", got)
	}
	// 再次只有轮换变化（暂存），随后登录 Cookie 变化：立即写，连同暂存的轮换值
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s4")); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookies("NEW", "s5")); err != nil {
		t.Fatal(err)
	}
	if diskCookie(t, path, "SAPISID") != "NEW" || diskCookie(t, path, "SIDCC") != "s5" {
		t.Fatal("其他 Cookie 变化应立即写盘")
	}
	// 上一次写盘已超过 30 秒：只有轮换变化也立即写
	ageStorage(t, path, rotatingCookieWriteInterval+time.Second)
	if err := lease.ReplaceCookies(rotatingTestCookies("NEW", "s6")); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s6" {
		t.Fatalf("上次写盘超过 30 秒应立即写，SIDCC=%q", got)
	}
	// 计时到期写盘：把上一次写盘挪到 29.8 秒前，暂存约 200ms 后写出
	ageStorage(t, path, rotatingCookieWriteInterval-200*time.Millisecond)
	if err := lease.ReplaceCookies(rotatingTestCookies("NEW", "s7")); err != nil {
		t.Fatal(err)
	}
	if diskCookie(t, path, "SIDCC") != "s6" {
		t.Fatal("间隔未到时不应写盘")
	}
	deadline := time.Now().Add(3 * time.Second)
	for diskCookie(t, path, "SIDCC") != "s7" {
		if time.Now().After(deadline) {
			t.Fatal("到写盘间隔后暂存的 SIDCC 应写出")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRotatingCookieDeferralDroppedOnExternalChange 暂存之后认证文件被外部改过（重新上传等）：以磁盘为准，暂存丢弃，计时不会写回旧值
func TestRotatingCookieDeferralDroppedOnExternalChange(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s1")); err != nil {
		t.Fatal(err)
	}
	ageStorage(t, path, rotatingCookieWriteInterval-300*time.Millisecond)
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s2")); err != nil {
		t.Fatal(err)
	}
	external := StorageState{Cookies: rotatingTestCookies("UPLOADED", "x1"), Origins: []StorageOrigin{}}
	if err := WriteStorageState(path, external); err != nil {
		t.Fatal(err)
	}
	reloaded, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := reloaded.CookieValue("SAPISID", aiStudioOrigin+"/", time.Now()); value != "UPLOADED" {
		t.Fatalf("外部改过的认证文件应以磁盘为准，SAPISID=%q", value)
	}
	time.Sleep(600 * time.Millisecond)
	if diskCookie(t, path, "SAPISID") != "UPLOADED" || diskCookie(t, path, "SIDCC") != "x1" {
		t.Fatal("暂存丢弃后计时不应把旧值写回")
	}
}

// TestRotatingWriteWaitCapped 认证文件的修改时间在未来时按刚写过处理，但最多等一个写盘间隔（原先按修改时间算，未来 24 小时就暂存 24 小时）
func TestRotatingWriteWaitCapped(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		modTime time.Time
		want    time.Duration
	}{
		{name: "刚写过", modTime: now, want: rotatingCookieWriteInterval},
		{name: "10 秒前", modTime: now.Add(-10 * time.Second), want: rotatingCookieWriteInterval - 10*time.Second},
		{name: "未来 24 小时", modTime: now.Add(24 * time.Hour), want: rotatingCookieWriteInterval},
		{name: "超过间隔", modTime: now.Add(-rotatingCookieWriteInterval - time.Second), want: -time.Second},
	} {
		if got := rotatingWriteWait(test.modTime, now); got != test.want {
			t.Fatalf("%s：等待 %s，期望 %s", test.name, got, test.want)
		}
	}
	// 经 ReplaceCookies：修改时间在未来时照样暂存（不立即写），计时不超过一个写盘间隔
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s1")); err != nil {
		t.Fatal(err)
	}
	ageStorage(t, path, -24*time.Hour)
	startedAt := time.Now()
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s2")); err != nil {
		t.Fatal(err)
	}
	if diskCookie(t, path, "SIDCC") != "s1" {
		t.Fatal("修改时间在未来时按刚写过处理，暂存不立即写")
	}
	account := lease.Account()
	account.storageMu.Lock()
	deferred := account.deferredStorage
	account.storageMu.Unlock()
	if deferred == nil || deferred.due.Sub(startedAt) > rotatingCookieWriteInterval+time.Second {
		t.Fatalf("暂存的写盘时刻应在一个写盘间隔之内：%+v", deferred)
	}
}

// TestStorageDeferralStoppedWithService 生成服务停止后（SetStorageDeferral(false)）暂存全部写盘，之后只有轮换 Cookie 变化也直接写：
// 停止再启动时新一代生成服务从磁盘载入认证文件，读到的是最新的 Cookie；重新启动后恢复暂存
func TestStorageDeferralStoppedWithService(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s1")); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s2")); err != nil {
		t.Fatal(err)
	}
	if diskCookie(t, path, "SIDCC") != "s1" {
		t.Fatal("30 秒内只有轮换 Cookie 变化时应暂存")
	}
	if err := pool.SetStorageDeferral(false); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s2" {
		t.Fatalf("停止时应写出暂存的 SIDCC，得到 %q", got)
	}
	// 停止之后：只有轮换变化也直接写，旧账户池不再留下没写盘的 Cookie
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s3")); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s3" {
		t.Fatalf("停止之后应直接写盘，SIDCC=%q", got)
	}
	// 重新启动：恢复暂存
	if err := pool.SetStorageDeferral(true); err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s4")); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s3" {
		t.Fatalf("重新启动后应恢复暂存，SIDCC=%q", got)
	}
}

// TestRotatingCookieDeferralDroppedOnAuthGeneration 暂存之后账户认证代际变了（新登录、续签）：计时到期不写回暂存的旧状态，
// Go 通道读取以磁盘为准
func TestRotatingCookieDeferralDroppedOnAuthGeneration(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s1")); err != nil {
		t.Fatal(err)
	}
	ageStorage(t, path, rotatingCookieWriteInterval-300*time.Millisecond)
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s2")); err != nil {
		t.Fatal(err)
	}
	account := lease.Account()
	pool.mu.Lock()
	account.authGeneration++
	pool.mu.Unlock()
	time.Sleep(700 * time.Millisecond)
	if got := diskCookie(t, path, "SIDCC"); got != "s1" {
		t.Fatalf("认证代际变了之后计时不应写回暂存的 SIDCC，磁盘为 %q", got)
	}
	account.storageMu.Lock()
	deferred := account.deferredStorage
	account.storageMu.Unlock()
	if deferred != nil {
		t.Fatal("认证代际变了之后暂存应丢弃")
	}
	// ReloadStorageState 只在认证代际一致时读文件，这里直接按账户池的读取路径确认以磁盘为准
	account.storageMu.Lock()
	state, err := pool.loadStorageLocked(account)
	account.storageMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := state.CookieValue("SIDCC", aiStudioOrigin+"/", time.Now()); value != "s1" {
		t.Fatalf("应以磁盘为准，SIDCC=%q", value)
	}
}
