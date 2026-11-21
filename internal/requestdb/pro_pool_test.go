package requestdb

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestLedgerProPoolRows 经 /pro 进入的请求（pool=pro）与普通号池、Ultra 号池的请求在原始记录、小时与本地日汇总中分开统计：
// 按号池分组与堆叠有 normal、pro、ultra 三组，号池候选含 pro，按 pool=pro 筛选只得到 Pro 的请求；记录与 CSV 的号池列为 pro。
// 同一小时内其余维度相同、只有号池不同的请求不会互相覆盖
func TestLedgerProPoolRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	at := time.Now().Add(-72 * time.Hour).Truncate(time.Hour).Add(10 * time.Minute)
	for index, pool := range []string{"pro", "", "ultra", "pro", "", "pro"} {
		prefix := map[string]string{"pro": "/pro", "ultra": "/ultra"}[pool]
		store.Record(Row{ID: fmt.Sprintf("r%d-%s", index, pool), Time: at.Add(time.Duration(index) * time.Minute), Protocol: "openai-chat",
			Path: prefix + "/v1/chat/completions", Model: "flash", Account: "a@example.com", Channel: "playground", Status: 200,
			State: "completed", Duration: time.Second, TotalTokens: 10, Pool: pool})
	}
	store = reopen(t, store, path)
	defer store.Close()

	ctx := context.Background()
	hour := at.Truncate(time.Hour)
	day, nextDay := localDay(at.UnixMilli())
	allPools := []string{api.UsagePoolNormal, api.UsagePoolPro, api.UsagePoolUltra}
	for name, query := range map[string]api.UsageQuery{
		"小时汇总":  {From: hour, To: hour.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "pool"},
		"原始记录":  {From: hour, To: hour.Add(59 * time.Minute), Bucket: time.Hour, Location: time.UTC, Stack: "pool"},
		"本地日汇总": {From: time.UnixMilli(day), To: time.UnixMilli(nextDay), Bucket: 24 * time.Hour, Location: time.Local, Stack: "pool"},
	} {
		report, err := store.Usage(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if got := poolGroups(report); got[api.UsagePoolPro] != 3 || got[api.UsagePoolNormal] != 2 || got[api.UsagePoolUltra] != 1 || len(got) != 3 {
			t.Fatalf("%s按号池分组 = %v，期望 pro 3、normal 2、ultra 1", name, got)
		}
		if !slices.Equal(report.Options["pool"], allPools) {
			t.Fatalf("%s号池候选 = %v", name, report.Options["pool"])
		}
		keys := []string{}
		for _, series := range report.Series {
			keys = append(keys, series.Key)
		}
		if slices.Sort(keys); !slices.Equal(keys, allPools) {
			t.Fatalf("%s按号池堆叠 = %v", name, keys)
		}
		filtered := query
		filtered.Filters = api.UsageFilters{"pool": {api.UsagePoolPro}}
		report, err = store.Usage(ctx, filtered)
		if err != nil {
			t.Fatal(err)
		}
		if report.Totals.Requests != 3 || report.Totals.TotalTokens != 30 {
			t.Fatalf("%s筛选 pool=pro 得到 %d 条（tokens %d），期望 3 条", name, report.Totals.Requests, report.Totals.TotalTokens)
		}
	}

	records, err := store.Records(ctx, api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Limit: 10, Filters: api.UsageFilters{"pool": {"pro"}}})
	if err != nil || len(records.Items) != 3 {
		t.Fatalf("按 pool=pro 筛选记录: %+v err=%v", records.Items, err)
	}
	for _, record := range records.Items {
		if record.Pool != api.UsagePoolPro || record.Path != "/pro/v1/chat/completions" {
			t.Fatalf("Pro 记录的号池与路径 = %q %q", record.Pool, record.Path)
		}
	}
	var output bytes.Buffer
	if err := store.ExportRecords(ctx, api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Filters: api.UsageFilters{"pool": {"pro"}}}, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(output.Bytes()[3:])).ReadAll()
	if err != nil || len(rows) != 4 {
		t.Fatalf("CSV 行数=%d err=%v", len(rows), err)
	}
	column := slices.Index(rows[0], "pool")
	for _, row := range rows[1:] {
		if column < 0 || row[column] != api.UsagePoolPro {
			t.Fatalf("CSV 号池列 = %v / %v", rows[0], row)
		}
	}
}
