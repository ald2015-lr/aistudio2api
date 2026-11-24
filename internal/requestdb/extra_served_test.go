package requestdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// sqliteObjects 返回数据库里全部表与索引（“类型:名字”，排好序）
func sqliteObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ':' || name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type || ':' || name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return names
}

// recordIDs 返回一页记录的 ID 与 extra 标记（“id” 或 “id+extra”）
func recordIDs(page api.UsageRecordPage) []string {
	ids := make([]string, 0, len(page.Items))
	for _, record := range page.Items {
		id := record.ID
		if record.Extra {
			id += "+extra"
		}
		ids = append(ids, id)
	}
	return ids
}

// exportIDs 导出 CSV，返回表头与各行的 ID
func exportIDs(t *testing.T, store *Store, query api.UsageRecordQuery) ([]string, []string) {
	t.Helper()
	var output bytes.Buffer
	if err := store.ExportRecords(context.Background(), query, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(output.Bytes(), []byte("\xef\xbb\xbf")) {
		t.Fatalf("CSV 应以 BOM 开头：%q", output.String())
	}
	rows, err := csv.NewReader(bytes.NewReader(output.Bytes()[3:])).ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatalf("CSV 行数=%d err=%v", len(rows), err)
	}
	column := slices.Index(rows[0], "id")
	ids := []string{}
	for _, row := range rows[1:] {
		ids = append(ids, row[column])
	}
	return rows[0], ids
}

// TestLedgerExtraServedTable 由补充池服务的请求附表 extra_served：从没写过补充池记录时没有附表（数据库里的表与索引和加入补充池之前一样），
// 记录不带 extra，按 extra 筛选为空页、导出只有表头；第一次写入补充池记录时建表（旧库重新打开后同样能建），记录 JOIN 后带 extra、
// 筛选只留补充池记录，CSV 的列不变；同一个请求 ID 重复写入不报错；过期的附表行随清理删除
func TestLedgerExtraServedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	before := sqliteObjects(t, store.db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	row := func(id string, at time.Time, extra bool) Row {
		return Row{ID: id, Time: at, Protocol: "openai-chat", Path: "/v1/chat/completions", Model: "gemini-flash-latest",
			Account: "a@example.com", Channel: "playground", Status: 200, State: "completed", Duration: time.Second, TotalTokens: 10, Extra: extra}
	}
	store.Record(row("main-1", now.Add(-3*time.Minute), false))
	store.Record(row("main-2", now.Add(-2*time.Minute), false))
	store = reopen(t, store, path)
	if got := sqliteObjects(t, store.db); !slices.Equal(got, before) || slices.Contains(got, "table:extra_served") {
		t.Fatalf("没写过补充池记录时数据库对象 = %v，期望与新建时相同 %v（没有附表）", got, before)
	}
	if store.extraTable.Load() {
		t.Fatal("没有附表时 extraTable 应为假")
	}
	ctx := context.Background()
	window := api.UsageRecordQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 10}
	page, err := store.Records(ctx, window)
	if err != nil || !slices.Equal(recordIDs(page), []string{"main-2", "main-1"}) {
		t.Fatalf("没有附表时的记录 = %v err=%v", recordIDs(page), err)
	}
	data, err := json.Marshal(page.Items[0])
	if err != nil || strings.Contains(string(data), `"extra"`) {
		t.Fatalf("没有由补充池服务的记录 JSON 不应出现 extra：%s err=%v", data, err)
	}
	onlyExtra := window
	onlyExtra.Extra = true
	if page, err := store.Records(ctx, onlyExtra); err != nil || len(page.Items) != 0 || page.Items == nil {
		t.Fatalf("没有附表时按 extra 筛选应为空页：%v err=%v", page.Items, err)
	}
	header, ids := exportIDs(t, store, onlyExtra)
	if !slices.Equal(header, csvHeader) || len(ids) != 0 {
		t.Fatalf("没有附表时按 extra 导出：表头 %v、行 %v，期望只有表头", header, ids)
	}

	// 旧库（没有附表）重新打开后第一次写入补充池记录：同一批里建表并写入，提交后立即可查
	store.Record(row("extra-1", now.Add(-time.Minute), true))
	store.Record(row("extra-1", now.Add(-time.Minute), true))
	store.Record(row("main-3", now.Add(-30*time.Second), false))
	deadline := time.Now().Add(5 * time.Second)
	for !store.extraTable.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !store.extraTable.Load() {
		t.Fatal("写入补充池记录后应建立附表")
	}
	store = reopen(t, store, path)
	defer store.Close()
	if !store.extraTable.Load() {
		t.Fatal("重新打开后应读出附表已存在")
	}
	objects := sqliteObjects(t, store.db)
	if want := append(slices.Clone(before), "index:extra_served_time", "table:extra_served"); !slices.Equal(objects, slices.Sorted(slices.Values(want))) {
		t.Fatalf("写过补充池记录后数据库对象 = %v，期望只多出附表与它的索引", objects)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM extra_served`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("附表行数 = %d err=%v，期望重复写入的同一个请求只有 1 行", count, err)
	}
	page, err = store.Records(ctx, window)
	if err != nil || !slices.Equal(recordIDs(page), []string{"main-3", "extra-1+extra", "main-2", "main-1"}) {
		t.Fatalf("JOIN 附表后的记录 = %v err=%v", recordIDs(page), err)
	}
	if data, _ := json.Marshal(page.Items[1]); !strings.Contains(string(data), `"extra":true`) {
		t.Fatalf("由补充池服务的记录 JSON 应带 extra：%s", data)
	}
	if page, err := store.Records(ctx, onlyExtra); err != nil || !slices.Equal(recordIDs(page), []string{"extra-1+extra"}) {
		t.Fatalf("按 extra 筛选 = %v err=%v", recordIDs(page), err)
	}
	// 分页游标与 JOIN 一起用
	paged := window
	paged.Limit = 1
	first, err := store.Records(ctx, paged)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("第一页 = %v err=%v", recordIDs(first), err)
	}
	cursor, err := decodeUsageCursorForTest(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	paged.Before = cursor
	if second, err := store.Records(ctx, paged); err != nil || !slices.Equal(recordIDs(second), []string{"extra-1+extra"}) {
		t.Fatalf("第二页 = %v err=%v", recordIDs(second), err)
	}
	header, ids = exportIDs(t, store, window)
	if !slices.Equal(header, csvHeader) || !slices.Equal(ids, []string{"main-3", "extra-1", "main-2", "main-1"}) {
		t.Fatalf("导出：表头 %v、行 %v", header, ids)
	}
	if _, ids := exportIDs(t, store, onlyExtra); !slices.Equal(ids, []string{"extra-1"}) {
		t.Fatalf("按 extra 导出的行 = %v", ids)
	}
	if !slices.Equal(csvHeader, []string{
		"time", "id", "protocol", "path", "model", "account", "channel", "pool", "status", "state", "duration_ms", "first_event_ms",
		"queue_ms", "input_tokens", "reasoning_tokens", "reply_tokens", "total_tokens", "tool_calls", "error", "attempts",
		"served_model", "downgrade", "reply_hash", "duplicate",
	}) {
		t.Fatalf("CSV 的列不应变化：%v", csvHeader)
	}
	// 汇总不分补充池：总数仍按全部请求
	report, err := store.Usage(ctx, api.UsageQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"})
	if err != nil || report.Totals.Requests != 4 {
		t.Fatalf("汇总请求数 = %d err=%v，期望 4", report.Totals.Requests, err)
	}

	// 清理：过期的附表行随原始记录一起删除
	old := now.Add(-usageRetention - 10*24*time.Hour)
	store.Record(row("extra-old", old, true))
	store = reopen(t, store, path)
	defer store.Close()
	store.prune()
	var stale int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM extra_served WHERE id = 'extra-old'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("过期的附表行 = %d err=%v，期望已清理", stale, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM extra_served`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("清理后附表行数 = %d err=%v，期望保留期内的 1 行", count, err)
	}
}

// decodeUsageCursorForTest 解析 api.EncodeUsageCursor 生成的分页位置（“毫秒|ID” 的 URL 安全 base64；api 的解析函数不导出）
func decodeUsageCursorForTest(value string) (*api.UsageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	at, id, _ := strings.Cut(string(raw), "|")
	milliseconds, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return nil, err
	}
	return &api.UsageCursor{Time: time.UnixMilli(milliseconds).UTC(), ID: id}, nil
}

// TestLedgerPoolStillEntry 由补充池服务的请求，pool 列仍按入口记录：默认地址借来的 Pro 账户记为普通号池（空），/pro 请求借来的普通账户
// 记为 pro；按号池筛选与汇总都按入口，与是否由补充池服务无关
func TestLedgerPoolStillEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	at := time.Now().Add(-48 * time.Hour).Truncate(time.Hour).Add(5 * time.Minute)
	for index, item := range []struct {
		id, pool, path, account string
		extra                   bool
	}{
		{"default-borrowed", "", "/v1/chat/completions", "lent-pro@example.com", true},
		{"pro-borrowed", "pro", "/pro/v1/chat/completions", "lent-free@example.com", true},
		{"default-home", "", "/v1/chat/completions", "home@example.com", false},
	} {
		store.Record(Row{ID: item.id, Time: at.Add(time.Duration(index) * time.Minute), Protocol: "openai-chat", Path: item.path,
			Model: "gemini-flash-latest", Account: item.account, Channel: "playground", Status: 200, State: "completed",
			Duration: time.Second, TotalTokens: 10, Pool: item.pool, Extra: item.extra})
	}
	store = reopen(t, store, path)
	defer store.Close()
	ctx := context.Background()
	hour := at.Truncate(time.Hour)
	window := api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Limit: 10}
	page, err := store.Records(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	pools := map[string]string{}
	for _, record := range page.Items {
		pools[record.ID] = record.Pool
		if record.Extra != (record.ID != "default-home") {
			t.Fatalf("%s 的 extra = %t", record.ID, record.Extra)
		}
	}
	if pools["default-borrowed"] != api.UsagePoolNormal || pools["pro-borrowed"] != api.UsagePoolPro || pools["default-home"] != api.UsagePoolNormal {
		t.Fatalf("号池列应按入口记录：%v", pools)
	}
	normal := window
	normal.Filters = api.UsageFilters{"pool": {api.UsagePoolNormal}}
	normal.Extra = true
	if page, err := store.Records(ctx, normal); err != nil || !slices.Equal(recordIDs(page), []string{"default-borrowed+extra"}) {
		t.Fatalf("普通号池里由补充池服务的记录 = %v err=%v", recordIDs(page), err)
	}
	report, err := store.Usage(ctx, api.UsageQuery{From: hour, To: hour.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	if got := poolGroups(report); got[api.UsagePoolNormal] != 2 || got[api.UsagePoolPro] != 1 || len(got) != 2 {
		t.Fatalf("按号池汇总 = %v，期望 normal 2、pro 1（按入口）", got)
	}
}
