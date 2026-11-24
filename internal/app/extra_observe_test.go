package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// 补充池的观测（批 4）：账户列表的补充池关系、请求账本的附表

// accountExtras 返回账户列表里各账户的补充池关系（只列有值的）
func accountExtras(accounts []api.AdminAccount) map[string]string {
	extras := map[string]string{}
	for _, account := range accounts {
		if account.Extra != "" {
			extras[account.ID] = account.Extra
		}
	}
	return extras
}

// TestAdminAccountExtra 账户列表的 extra：补充池关闭且没有补充池 Worker 时所有账户都没有（JSON 里不出现）；开启后 Worker 计入补充池的为
// worker，其余成员为 member（Worker 驻留在主分区的成员也是），完全没冷却的账户没有；单个账户的视图与推送的账户列表同一口径；
// 关闭后对账归还，补充池 Worker 与成员缓存都没有了，又全部为空
func TestAdminAccountExtra(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA, partitionNormalB}, []string{proPartitionA, proPartitionB}, 1, 3, 1, 3, 2, 3)
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
	accounts := func() []api.AdminAccount {
		t.Helper()
		accounts, err := admin.Accounts(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return accounts
	}
	env.setExtra(false, 2, 3)
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	env.pass(t)
	list := accounts()
	if extras := accountExtras(list); len(extras) != 0 {
		t.Fatalf("补充池关闭时账户不应带 extra：%v", extras)
	}
	if data, err := json.Marshal(list); err != nil || strings.Contains(string(data), `"extra"`) {
		t.Fatalf("补充池关闭时账户列表 JSON 不应出现 extra：%s err=%v", data, err)
	}

	env.setExtra(true, 2, 3)
	env.member(t, aistudio.BenefitTierPro, proPartitionB)
	env.promote(t, proPartitionB)
	env.pass(t)
	want := map[string]string{partitionNormalA: adminAccountExtraWorker, proPartitionB: adminAccountExtraMember}
	if extras := accountExtras(accounts()); !equalStringMaps(extras, want) {
		t.Fatalf("开启后账户的补充池关系 = %v，期望 %v", extras, want)
	}
	if !slices.Equal(env.in(aistudio.PoolScopeExtra), []string{partitionNormalA}) || !slices.Equal(env.in(aistudio.PoolScopePro), []string{proPartitionB}) {
		t.Fatalf("前提：补充池 Worker %v、Pro 分区 %v", env.in(aistudio.PoolScopeExtra), env.in(aistudio.PoolScopePro))
	}
	for accountID, extra := range want {
		if account, err := admin.account(accountID); err != nil || account.Extra != extra {
			t.Fatalf("单个账户 %s 的 extra = %q err=%v，期望 %q", accountID, account.Extra, err, extra)
		}
	}
	subscribeCtx, unsubscribe := context.WithCancel(env.ctx)
	defer unsubscribe()
	subscription := env.requests.activateSubscriber(env.requests.subscribe(subscribeCtx), nil, nil)
	env.service.publishModelAccessNow()
	published := false
	for !published {
		select {
		case event := <-subscription:
			if event.Type != "accounts" {
				continue
			}
			published = true
			list := event.Data.(map[string]any)["accounts"].([]api.AdminAccount)
			if extras := accountExtras(list); !equalStringMaps(extras, want) {
				t.Fatalf("推送的账户列表 extra = %v，期望 %v", extras, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没有推送账户列表")
		}
	}

	env.setExtra(false, 2, 3)
	env.pass(t)
	env.pass(t)
	if extras := accountExtras(accounts()); len(extras) != 0 {
		t.Fatalf("关闭并归还后账户不应带 extra：%v（补充池 Worker %v）", extras, env.in(aistudio.PoolScopeExtra))
	}
}

// equalStringMaps 判断两个字符串映射相同
func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// TestLedgerExtraServedFromRequest 经公开 API 由补充池服务的默认地址请求写进账本：记录带 extra、号池仍按入口（普通号池），
// 按“由补充池服务”筛选只得到它；主号池服务的请求不带 extra
func TestLedgerExtraServedFromRequest(t *testing.T) {
	normal, lent := "ledger-normal@example.com", "ledger-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	handler, admin := borrowHandler(env)

	release := occupy(t, env.pool, normal)
	recorder, borrowed := postChat(t, handler, admin, testRuntimeModel)
	release()
	if recorder.Code != http.StatusOK || borrowed.Account != lent || !borrowed.Extra || borrowed.Pool != "" {
		t.Fatalf("由补充池服务：状态 %d、账户 %q、extra=%t、号池 %q", recorder.Code, borrowed.Account, borrowed.Extra, borrowed.Pool)
	}
	releaseLent := occupy(t, env.pool, lent)
	recorder, home := postChat(t, handler, admin, testRuntimeModel)
	releaseLent()
	if recorder.Code != http.StatusOK || home.Account != normal || home.Extra {
		t.Fatalf("主号池服务：状态 %d、账户 %q、extra=%t", recorder.Code, home.Account, home.Extra)
	}
	if row := requestRow(borrowed, time.Now()); !row.Extra || row.Pool != "" {
		t.Fatalf("账本行 extra=%t、号池 %q，期望 extra、号池按入口为空", row.Extra, row.Pool)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "runtime", "requests.db")
	manager := &runtimeManager{requests: newRequestRegistry(ctx), current: &runtimeGeneration{admin: recordingAdmin{}}}
	ledger := openRequestLedger(manager, path, false)
	if ledger == nil {
		t.Fatal("账本没有打开")
	}
	manager.RecordAccessLog(borrowed)
	manager.RecordAccessLog(home)
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	now := time.Now().UTC()
	window := api.UsageRecordQuery{From: now.Add(-time.Hour), To: now.Add(time.Minute), Limit: 10}
	page, err := reopened.Records(ctx, window)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("账本记录 = %+v err=%v", page.Items, err)
	}
	for _, record := range page.Items {
		if record.Pool != api.UsagePoolNormal || record.Extra != (record.ID == borrowed.RequestID) {
			t.Fatalf("记录 %s：号池 %q、extra=%t", record.ID, record.Pool, record.Extra)
		}
	}
	window.Extra = true
	if page, err := reopened.Records(ctx, window); err != nil || len(page.Items) != 1 || page.Items[0].ID != borrowed.RequestID {
		t.Fatalf("按由补充池服务筛选 = %+v err=%v", page.Items, err)
	}
}
