package aistudio

import (
	"reflect"
	"testing"
	"time"
)

func cookieNames(state StorageState) []string {
	names := make([]string, 0, len(state.Cookies))
	for _, cookie := range state.Cookies {
		names = append(names, cookie.Name)
	}
	return names
}

// 轮换已有 Cookie 时写回原下标：值、到期时间和属性按响应更新，Cookie 请求头里的顺序不变（与浏览器一致）
func TestMergeSetCookieHeadersKeepsRotatedCookiePosition(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := StorageState{Cookies: []StateCookie{
		{Name: "SID", Value: "sid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-1PSIDTS", Value: "old-ts", Domain: ".google.com", Path: "/", Expires: -1, HTTPOnly: true, Secure: true},
		{Name: "SAPISID", Value: "sapisid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "NID", Value: "nid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
	}}
	before, err := state.CookieHeader(aiStudioOrigin+"/", now)
	if err != nil {
		t.Fatal(err)
	}
	if before != "SID=sid; __Secure-1PSIDTS=old-ts; SAPISID=sapisid; NID=nid" {
		t.Fatalf("轮换前 Cookie 头 = %q", before)
	}
	if err := state.MergeSetCookieHeaders([]string{
		"__Secure-1PSIDTS=new-ts; Domain=.google.com; Path=/; Max-Age=3600; Secure; HttpOnly; SameSite=Lax",
	}, aiStudioOrigin+"/", now); err != nil {
		t.Fatal(err)
	}
	if got, want := cookieNames(state), []string{"SID", "__Secure-1PSIDTS", "SAPISID", "NID"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("轮换后 Cookie 顺序 = %v，期望 %v", got, want)
	}
	rotated := state.Cookies[1]
	want := StateCookie{
		Name: "__Secure-1PSIDTS", Value: "new-ts", Domain: ".google.com", Path: "/",
		Expires:  float64(now.Add(time.Hour).UnixNano()) / 1e9,
		HTTPOnly: true, Secure: true, SameSite: "Lax",
	}
	if rotated != want {
		t.Fatalf("轮换后的 Cookie = %+v，期望 %+v", rotated, want)
	}
	after, err := state.CookieHeader(aiStudioOrigin+"/", now)
	if err != nil {
		t.Fatal(err)
	}
	if after != "SID=sid; __Secure-1PSIDTS=new-ts; SAPISID=sapisid; NID=nid" {
		t.Fatalf("轮换后 Cookie 头 = %q（轮换的 Cookie 不应挪到最后）", after)
	}
}

// 新 Cookie 照旧追加到末尾；过期（Max-Age<=0 或 Expires 已过）照旧删除，其余 Cookie 相对顺序不变
func TestMergeSetCookieHeadersAppendsNewAndDeletesExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := StorageState{Cookies: []StateCookie{
		{Name: "A", Value: "a", Domain: ".google.com", Path: "/", Expires: -1},
		{Name: "B", Value: "b", Domain: ".google.com", Path: "/", Expires: -1},
		{Name: "C", Value: "c", Domain: ".google.com", Path: "/", Expires: -1},
		{Name: "D", Value: "d", Domain: ".google.com", Path: "/", Expires: -1},
		// 同名但路径不同的是另一个 Cookie，不受 B 的轮换影响
		{Name: "B", Value: "b-path", Domain: ".google.com", Path: "/app", Expires: -1},
	}}
	if err := state.MergeSetCookieHeaders([]string{
		"NEW=fresh; Domain=.google.com; Path=/",
		"B=b2; Domain=.google.com; Path=/",
		"C=; Domain=.google.com; Path=/; Max-Age=0",
		"A=gone; Domain=.google.com; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT",
		"MISSING=x; Domain=.google.com; Path=/; Max-Age=-1",
	}, aiStudioOrigin+"/", now); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(state.Cookies))
	for _, cookie := range state.Cookies {
		got = append(got, cookie.Name+"="+cookie.Value+"@"+cookie.Path)
	}
	want := []string{"B=b2@/", "D=d@/", "B=b-path@/app", "NEW=fresh@/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("合并结果 = %v，期望 %v", got, want)
	}
}
