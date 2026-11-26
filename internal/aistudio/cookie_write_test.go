package aistudio

import (
	"context"
	"errors"
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

// TestStorageFileSynced 认证文件的修改时间与大小同本服务最近一次读入（载入账户）或写入时相同才算本服务自己的文件：
// 载入后与写回后都相同；外部原样重写（只改了修改时间）或改了修改时间都不同；账户不存在时为 false
func TestStorageFileSynced(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	synced := func() bool {
		t.Helper()
		account, err := pool.Account("alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(account.StoragePath)
		if err != nil {
			t.Fatal(err)
		}
		return pool.StorageFileSynced("alice@example.com", info.ModTime(), info.Size())
	}
	if !synced() {
		t.Fatal("载入账户后认证文件应算本服务读入的那一份")
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Account().StoragePath
	ageStorage(t, path, time.Hour)
	if synced() {
		t.Fatal("修改时间变了不应算本服务的文件")
	}
	if err := lease.SaveStorageState(testStorageState("B")); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !synced() {
		t.Fatal("本服务写回后应算本服务的文件")
	}
	state, err := LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	if synced() {
		t.Fatal("外部原样重写不应算本服务的文件")
	}
	if pool.StorageFileSynced("missing@example.com", time.Now(), 1) {
		t.Fatal("账户不存在时应为 false")
	}
}

// TestStorageFileOwned 认证文件内容与本服务最近一次读入或写入的逐字节相同、修改时间不晚于那一次，才算本服务自己的文件：
// 载入后只把修改时间改早仍算；外部原样重新写入（修改时间变新）不算；内容不同不算；本服务写回后又算；账户不存在时为 false
func TestStorageFileOwned(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	owned := func() bool {
		t.Helper()
		account, err := pool.Account("alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(account.StoragePath)
		if err != nil {
			t.Fatal(err)
		}
		_, digest, err := LoadStorageStateDigest(account.StoragePath)
		if err != nil {
			t.Fatal(err)
		}
		return pool.StorageFileOwned("alice@example.com", info.ModTime(), digest)
	}
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	path := account.StoragePath
	if !owned() {
		t.Fatal("载入账户后认证文件应算本服务读入的那一份")
	}
	ageStorage(t, path, time.Hour)
	if !owned() {
		t.Fatal("只把修改时间改早仍是本服务读入的那一份")
	}
	state, err := LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	if owned() {
		t.Fatal("外部原样重新写入（修改时间变新）不应算本服务的文件")
	}
	if err := WriteStorageState(path, testStorageState("OTHER")); err != nil {
		t.Fatal(err)
	}
	ageStorage(t, path, 2*time.Hour)
	if owned() {
		t.Fatal("内容不同不应算本服务的文件")
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveStorageState(testStorageState("B")), lease.Release()); err != nil {
		t.Fatal(err)
	}
	if !owned() {
		t.Fatal("本服务写回后应算本服务的文件")
	}
	if pool.StorageFileOwned("missing@example.com", time.Now(), StorageDigest{}) {
		t.Fatal("账户不存在时应为 false")
	}
}

// TestStorageFileKnown 认证文件逐字节是本服务从外部读入的（载入账户、SaveLoadedStorageState 记下的重新上传）或自己写过的那一份时为已知，
// 不看修改时间；本服务之后自己写回不会忘掉读入的那一份；其他内容与不存在的账户不是
func TestStorageFileKnown(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	_, loaded, err := LoadStorageStateDigest(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.StorageFileKnown(id, loaded) {
		t.Fatal("载入账户时读入的文件应为已知")
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveStorageState(testStorageState("B")), lease.Release()); err != nil {
		t.Fatal(err)
	}
	_, written, err := LoadStorageStateDigest(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.StorageFileKnown(id, written) || !pool.StorageFileKnown(id, loaded) {
		t.Fatal("本服务自己写过的与之前读入的都应为已知")
	}
	upload := StorageDigest{1, 2, 3}
	if pool.StorageFileKnown(id, upload) {
		t.Fatal("没读入过的内容不应为已知")
	}
	lease, err = pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveLoadedStorageState(testStorageState("C"), upload), lease.Release()); err != nil {
		t.Fatal(err)
	}
	if !pool.StorageFileKnown(id, upload) {
		t.Fatal("载入的重新上传应记下它原来的内容指纹")
	}
	if pool.StorageFileKnown(id, loaded) {
		t.Fatal("读入的外部文件只记最近一次")
	}
	if pool.StorageFileKnown("missing@example.com", loaded) {
		t.Fatal("账户不存在时应为 false")
	}
}

// TestSaveLoadedStorageStateSkipsIdenticalFile 载入重新上传的认证文件时，磁盘上已经逐字节是要写的内容就不重写文件（修改时间不变），
// 只推进认证代际、记下原始内容的指纹，文件仍算本服务读入的那一份；内容不同时照常写盘。原先总是重写：共用账户目录的另一个进程看到
// 修改时间变新，当成又一次重新上传，两边互相触发
func TestSaveLoadedStorageStateSkipsIdenticalFile(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	ageStorage(t, account.StoragePath, time.Hour)
	before, err := os.Stat(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	state, digest, err := LoadStorageStateDigest(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	generation := pool.AuthGeneration(id)
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveLoadedStorageState(state, digest), lease.Release()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("磁盘上已经是这份内容时不应重写文件")
	}
	if pool.AuthGeneration(id) == generation {
		t.Fatal("没有重写文件也应推进认证代际")
	}
	if !pool.StorageFileSynced(id, after.ModTime(), after.Size()) || !pool.StorageFileKnown(id, digest) {
		t.Fatal("文件应算本服务读入的那一份")
	}
	lease, err = pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveLoadedStorageState(testStorageState("NEW"), StorageDigest{9}), lease.Release()); err != nil {
		t.Fatal(err)
	}
	if diskSAPISID(t, account.StoragePath) != "NEW" {
		t.Fatal("内容不同时应照常写盘")
	}
}

// TestWriteBacksSkipUploadedNewLogin 磁盘上的认证文件被外部换成另一个能用的登录、还没载入时：浏览器 Cookie 写回（ReplaceCookies）与
// Set-Cookie 合并（MergeSetCookieHeaders）都不写，读认证状态（ReloadStorageState）照样按磁盘返回但不读进内存，并记下账户由扫描载入；
// 换上的文件不能签名时照原来的方式合并
func TestWriteBacksSkipUploadedNewLogin(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	TakeExternalLoginChanges()
	if err := WriteStorageState(account.StoragePath, testStorageState("UPLOADED")); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, reloadErr := lease.ReloadStorageState()
	err = errors.Join(
		reloadErr,
		lease.ReplaceCookies(testStorageState("OLD").Cookies),
		lease.MergeSetCookieHeaders([]string{"NID=old-session; Domain=.google.com; Path=/"}, aiStudioOrigin+"/", time.Now()),
		lease.Release(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := reloaded.CookieValue("SAPISID", aiStudioOrigin+"/", time.Now()); value != "UPLOADED" {
		t.Fatalf("读认证状态照样按磁盘返回，得到 %s", value)
	}
	if diskSAPISID(t, account.StoragePath) != "UPLOADED" {
		t.Fatal("写回不应覆盖换上的新登录")
	}
	if state, _ := pool.StorageStateOf(id); LoginIdentity(state) != LoginIdentity(testStorageState("OLD")) {
		t.Fatal("内存不应换成新文件")
	}
	if changes := TakeExternalLoginChanges(); len(changes) != 1 || changes[0] != id {
		t.Fatalf("应记下账户由扫描载入：%v", changes)
	}
	// 换上的文件缺登录签名 Cookie（不能签名）：照原来的方式合并，浏览器里能用的登录写回
	if err := WriteStorageState(account.StoragePath, StorageState{Cookies: []StateCookie{{Name: "NID", Value: "broken", Domain: ".google.com", Path: "/"}}, Origins: []StorageOrigin{}}); err != nil {
		t.Fatal(err)
	}
	lease, err = pool.AcquireFor(context.Background(), AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.ReplaceCookies(testStorageState("OLD").Cookies), lease.Release()); err != nil {
		t.Fatal(err)
	}
	if diskSAPISID(t, account.StoragePath) != "OLD" {
		t.Fatal("不能签名的文件上应照常合并浏览器里能用的登录")
	}
	TakeExternalLoginChanges()
}
