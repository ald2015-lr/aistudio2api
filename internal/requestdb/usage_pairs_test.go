package requestdb

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// recordPairRows 写入跨越几个整小时的记录：三个账户（含未分配的空账户）、两个模型，结果有成功、失败与 429，
// 范围内既有完整小时（读小时汇总）也有零散分钟（读原始记录），另有一条落在上一周期
func recordPairRows(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	accounts := []string{"a@example.test", "b@example.test", ""}
	models := []string{"flash", "pro"}
	for index := range 36 {
		row := Row{
			ID: fmt.Sprintf("p%d", index), Time: now.Add(-time.Duration(20+index*9) * time.Minute),
			Account: accounts[index%len(accounts)], Model: models[index%len(models)], Channel: "playground", Protocol: "openai",
			State: "completed", Duration: time.Duration(index+1) * 300 * time.Millisecond, FirstEvent: time.Duration(index+1) * 100 * time.Millisecond,
			InputTokens: int64(index), ReplyTokens: 60, TotalTokens: int64(index) + 60, ReplyHash: fmt.Sprintf("h%d", index%4),
		}
		if index%5 == 0 {
			row.State, row.Status = "failed", 429
		}
		if index%7 == 0 {
			row.Pool = api.UsagePoolUltra
		}
		store.Record(row)
	}
	store.Record(Row{ID: "previous", Time: now.Add(-9 * time.Hour), Account: "a@example.test", Model: "flash", State: "completed", Duration: time.Second})
}

// pairsOf 取出报告里某一行（账户或模型）的组合，保持报告里的顺序
func pairsOf(report api.UsageReport, dimension, key string) []api.UsagePair {
	selected := []api.UsagePair{}
	for _, pair := range report.Pairs {
		if (dimension == "account" && pair.Account == key) || (dimension == "model" && pair.Model == key) {
			selected = append(selected, pair)
		}
	}
	return selected
}

// TestLedgerUsageSkipPairs 不带组合参数时与原先一样带全部组合；SkipPairs 只去掉组合，其余字段逐项相同
func TestLedgerUsageSkipPairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	recordPairRows(t, store, now)
	store = reopen(t, store, path)
	defer store.Close()
	query := api.UsageQuery{From: now.Add(-6 * time.Hour), To: now, Bucket: 15 * time.Minute, Location: time.UTC, Stack: "account"}
	full, err := store.Usage(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	// 3 个账户 × 2 个模型，36 条记录按下标轮换，6 种组合都出现
	if len(full.Pairs) != 6 || full.Totals.Requests != 36 {
		t.Fatalf("不带参数时应带全部组合: pairs=%d requests=%d", len(full.Pairs), full.Totals.Requests)
	}
	query.SkipPairs = true
	skipped, err := store.Usage(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Pairs == nil || len(skipped.Pairs) != 0 {
		t.Fatalf("SkipPairs 时 pairs 应为空数组: %#v", skipped.Pairs)
	}
	full.Pairs, full.GeneratedAt, skipped.GeneratedAt = []api.UsagePair{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(full, skipped) {
		t.Fatalf("SkipPairs 不应改变组合以外的字段:\n带组合=%+v\n不带组合=%+v", full, skipped)
	}
}

// TestLedgerUsagePairsOfMatchesFullReport 只读一行的组合与同一查询主报告里这一行的组合逐项相同，
// 包括未分配（空账户）那一行与带筛选的查询；行不在筛选范围内时为空
func TestLedgerUsagePairsOfMatchesFullReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	recordPairRows(t, store, now)
	store = reopen(t, store, path)
	defer store.Close()
	base := api.UsageQuery{From: now.Add(-6 * time.Hour), To: now, Bucket: 15 * time.Minute, Location: time.UTC, Stack: "model"}
	for _, filters := range []api.UsageFilters{
		nil,
		{"model": {"flash"}},
		{"account": {"a@example.test", "b@example.test"}, "pool": {api.UsagePoolNormal}},
	} {
		query := base
		query.Filters = filters
		full, err := store.Usage(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, dimension := range []string{"account", "model"} {
			keys := []string{"", "a@example.test", "b@example.test", "flash", "pro", "missing"}
			for _, key := range keys {
				query := query
				query.PairsOf = &api.UsagePairsOf{Dimension: dimension, Key: key}
				only, err := store.Usage(context.Background(), query)
				if err != nil {
					t.Fatal(err)
				}
				want := pairsOf(full, dimension, key)
				if !reflect.DeepEqual(only.Pairs, want) {
					t.Fatalf("筛选=%v %s=%q 的组合与主报告不一致:\n只读这一行=%+v\n主报告=%+v", filters, dimension, key, only.Pairs, want)
				}
				if only.Totals.Requests != 0 || len(only.Groups) != 0 || len(only.Buckets) != 0 || only.Options == nil {
					t.Fatalf("只读组合时不应计算其他字段: %+v", only)
				}
				checked += len(want)
			}
		}
		// 每个组合在按账户与按模型两种读取里各出现一次
		if checked != 2*len(full.Pairs) || len(full.Pairs) == 0 {
			t.Fatalf("筛选=%v 核对的组合数=%d，主报告组合数=%d", filters, checked, len(full.Pairs))
		}
	}
	// 未分配（空账户）这一行有组合，且不被当成“不筛选”而带出其他账户
	query := base
	query.PairsOf = &api.UsagePairsOf{Dimension: "account", Key: ""}
	only, err := store.Usage(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Pairs) != 2 {
		t.Fatalf("空账户应有 2 个组合: %+v", only.Pairs)
	}
	for _, pair := range only.Pairs {
		if pair.Account != "" {
			t.Fatalf("空账户的组合里出现了其他账户: %+v", only.Pairs)
		}
	}
}
