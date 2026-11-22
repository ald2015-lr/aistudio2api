package aistudio

import (
	"context"
	"testing"
)

// deviceBoundTestState 返回带或不带设备绑定 Cookie（只看名字）、带或不带 Chrome 续签材料的认证状态；值都是假数据
func deviceBoundTestState(t *testing.T, value string, bound bool, renewal bool) StorageState {
	t.Helper()
	state := testStorageState(value)
	if bound {
		state.Cookies = append(state.Cookies, StateCookie{Name: DeviceBoundCookie, Value: "bound-" + value, Domain: ".google.com", Path: "/"})
	}
	if renewal {
		if err := state.SetAuthExtension(AuthExtension{
			Source: AuthSource{Browser: "chrome", Email: "dbsc@example.com"},
			OAuth:  &ChromeOAuthMaterial{GaiaID: "1", RefreshToken: "refresh-" + value},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// saveDeviceBoundState 在独占租约里保存新的认证状态（与管理端重新登录、导入保存新登录同一条路径）
func saveDeviceBoundState(t *testing.T, pool *AccountPool, id string, state StorageState) {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(state); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestAccountStatusDeviceBound 账户状态带出设备绑定会话的两项判断：有没有 __Secure-1PSIDRTS（只看名字）、认证扩展里有没有 Chrome
// 续签材料。续签材料的判断按认证扩展缓存在账户上：认证扩展没变时构建状态不再解析，保存新的认证状态后按新的一份重新判断
func TestAccountStatusDeviceBound(t *testing.T) {
	const id = "dbsc@example.com"
	pool := testPoolWithAccount(t, id)
	status, ok := pool.StatusOf(id)
	if !ok || status.DeviceBoundSession || status.ChromeRenewal {
		t.Fatalf("普通认证状态不是设备绑定会话：%+v", status)
	}

	saveDeviceBoundState(t, pool, id, deviceBoundTestState(t, "COPIED", true, false))
	if status, _ = pool.StatusOf(id); !status.DeviceBoundSession || status.ChromeRenewal {
		t.Fatalf("带 __Secure-1PSIDRTS、没有续签材料：%+v", status)
	}

	saveDeviceBoundState(t, pool, id, deviceBoundTestState(t, "CHROME", true, true))
	if status, _ = pool.StatusOf(id); !status.DeviceBoundSession || !status.ChromeRenewal {
		t.Fatalf("带 __Secure-1PSIDRTS 与 Chrome 续签材料：%+v", status)
	}
	// 认证扩展没变：改掉缓存的结论，构建状态应沿用缓存（不重新解析），Status 与 StatusOf 一致
	pool.mu.Lock()
	pool.byID[id].deviceBound.renewal = false
	pool.mu.Unlock()
	if status, _ = pool.StatusOf(id); status.ChromeRenewal {
		t.Fatal("认证扩展没变时应沿用账户上缓存的判断，不应重新解析")
	}
	if statuses := pool.Status(); len(statuses) != 1 || statuses[0].ChromeRenewal {
		t.Fatalf("Status 应与 StatusOf 同样沿用缓存：%+v", statuses)
	}

	// 保存新的认证状态（认证扩展是新的一份，长度与原来相同，只能按身份看出换了）：重新判断
	saveDeviceBoundState(t, pool, id, deviceBoundTestState(t, "CHROMX", true, true))
	if status, _ = pool.StatusOf(id); !status.DeviceBoundSession || !status.ChromeRenewal {
		t.Fatalf("新的认证扩展应重新判断：%+v", status)
	}
	// 重新登录后没有设备绑定 Cookie：两项都为假
	saveDeviceBoundState(t, pool, id, deviceBoundTestState(t, "RELOGIN", false, true))
	if status, _ = pool.StatusOf(id); status.DeviceBoundSession || status.ChromeRenewal {
		t.Fatalf("没有设备绑定 Cookie 时两项都应为假：%+v", status)
	}
}
