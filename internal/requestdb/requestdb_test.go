package requestdb

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// openTestStore 打开临时目录中的账本，写入失败的运行日志直接让测试失败
func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path, func(level, message string) {
		if level != "INFO" {
			t.Errorf("%s %s", level, message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// reopen 关闭账本（写完排队的记录）后重新打开
func reopen(t *testing.T, store *Store, path string) *Store {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return openTestStore(t, path)
}

// TestLedgerUsageAndBodies 验证关闭前排队的记录落盘、当前与上一周期分开汇总、正文只保留最近条数
func TestLedgerUsageAndBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	store.Record(Row{ID: "a", Time: now.Add(-time.Hour), Model: "flash", State: "completed", Duration: 2 * time.Second, InputTokens: 10, TotalTokens: 30})
	store.Record(Row{ID: "b", Time: now.Add(-2 * time.Hour), Model: "flash", State: "failed", Status: 429, Duration: 4 * time.Second})
	store.Record(Row{ID: "c", Time: now.Add(-3 * time.Hour), Model: "pro", State: "completed", Duration: 6 * time.Second, TotalTokens: 5})
	store.Record(Row{ID: "old", Time: now.Add(-30 * time.Hour), Model: "pro", State: "completed", Duration: time.Second})
	for index := range bodyRetention + 2 {
		store.SaveBody(api.RequestBody{ID: fmt.Sprintf("body-%d", index), Time: now, Request: "in", Response: "out"})
	}
	store = reopen(t, store, path)
	defer store.Close()
	report, err := store.Usage(context.Background(), api.UsageQuery{
		From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour, Location: time.UTC, Stack: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	totals := report.Totals
	if totals.Requests != 3 || totals.Failed != 1 || totals.RateLimited != 1 || totals.TotalTokens != 35 || report.Previous.Requests != 1 {
		t.Fatalf("汇总不对: totals=%+v previous=%+v", totals, report.Previous)
	}
	// 耗时只统计成功请求：2s 与 6s
	if len(report.Buckets) != 25 || math.Abs(totals.Duration.P95MS-6000) > 60 || totals.Duration.AvgMS != 4000 {
		t.Fatalf("分桶或耗时不对: buckets=%d duration=%+v", len(report.Buckets), totals.Duration)
	}
	bucketed := int64(0)
	for _, bucket := range report.Buckets {
		bucketed += bucket.Requests
	}
	models := report.Groups["model"]
	if bucketed != 3 || len(models) != 2 || models[0].Key != "flash" || models[0].Requests != 2 {
		t.Fatalf("分桶合计或模型排行不对: bucketed=%d models=%+v", bucketed, models)
	}
	if _, err := store.RequestBody(context.Background(), "body-1"); !errors.Is(err, api.ErrRequestBodyNotFound) {
		t.Fatalf("超出保留条数的正文仍存在: %v", err)
	}
	body, err := store.RequestBody(context.Background(), fmt.Sprintf("body-%d", bodyRetention+1))
	if err != nil || body.Request != "in" || body.Response != "out" {
		t.Fatalf("最近的正文读取失败: body=%+v err=%v", body, err)
	}
}

// TestLedgerRollupsMatchRawRecords 跨越整小时的范围读取小时汇总，结果与只读原始记录的窄范围一致
func TestLedgerRollupsMatchRawRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	for index := range 6 {
		store.Record(Row{
			ID: fmt.Sprintf("r%d", index), Time: hour.Add(time.Duration(index*10) * time.Minute), Model: "flash",
			Account: "a@example.com", State: "completed", Duration: time.Duration(index+1) * time.Second, TotalTokens: 10,
			ReplyTokens: 80, ReplyHash: fmt.Sprintf("hash%d", index%2), Downgrade: []string{"", "passed", "rejected"}[index%3],
		})
	}
	store = reopen(t, store, path)
	defer store.Close()
	// 整小时 [hour, hour+1h) 走小时汇总；[hour, hour+59m) 只读原始记录
	whole, err := store.Usage(context.Background(), api.UsageQuery{From: hour, To: hour.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Usage(context.Background(), api.UsageQuery{From: hour, To: hour.Add(59 * time.Minute), Bucket: time.Hour, Location: time.UTC, Stack: "model"})
	if err != nil {
		t.Fatal(err)
	}
	for name, stats := range map[string]api.UsageStats{"小时汇总": whole.Totals, "原始记录": raw.Totals} {
		if stats.Requests != 6 || stats.TotalTokens != 60 || stats.DowngradeChecked != 4 || stats.DowngradeRejected != 2 ||
			stats.Replies != 6 || stats.DuplicateReplies != 4 {
			t.Fatalf("%s的汇总不对: %+v", name, stats)
		}
	}
	if whole.Totals.Duration.AvgMS != raw.Totals.Duration.AvgMS || whole.Totals.Duration.P95MS != raw.Totals.Duration.P95MS {
		t.Fatalf("两种来源的耗时不一致: 汇总=%+v 原始=%+v", whole.Totals.Duration, raw.Totals.Duration)
	}
}

// TestLedgerDuplicateReplies 窗口内回复指纹相同且回复足够长时才计为重复，可以按指纹列出同一回复的请求
func TestLedgerDuplicateReplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	start := time.Now().UTC().Add(-5 * time.Hour)
	store.Record(Row{ID: "first", Time: start, State: "completed", ReplyHash: "abc123", ReplyTokens: 120})
	store.Record(Row{ID: "again", Time: start.Add(time.Minute), State: "completed", ReplyHash: "abc123", ReplyTokens: 120})
	// 超出窗口：与 3 小时前的回复相同不算重复
	store.Record(Row{ID: "later", Time: start.Add(3 * time.Hour), State: "completed", ReplyHash: "abc123", ReplyTokens: 120})
	// 过短的回复不参与统计
	store.Record(Row{ID: "short1", Time: start.Add(2 * time.Minute), State: "completed", ReplyHash: "ok", ReplyTokens: 2})
	store.Record(Row{ID: "short2", Time: start.Add(3 * time.Minute), State: "completed", ReplyHash: "ok", ReplyTokens: 2})
	store = reopen(t, store, path)
	defer store.Close()
	query := api.UsageQuery{From: start.Add(-time.Minute), To: start.Add(4 * time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"}
	report, err := store.Usage(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Replies != 3 || report.Totals.DuplicateReplies != 1 {
		t.Fatalf("重复回复统计不对: replies=%d duplicates=%d", report.Totals.Replies, report.Totals.DuplicateReplies)
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: query.From, To: query.To, Search: "abc123", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	duplicates := map[string]bool{}
	for _, record := range page.Items {
		duplicates[record.ID] = record.Duplicate
	}
	if len(page.Items) != 3 || !duplicates["again"] || duplicates["first"] || duplicates["later"] {
		t.Fatalf("按指纹查询的记录不对: %+v", duplicates)
	}
}

// TestLedgerDuplicateQueryUsesReplyIndex 重复回复查找走回复指纹的部分索引，而不是扫描窗口内的全部记录
func TestLedgerDuplicateQueryUsesReplyIndex(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "requests.db"))
	defer store.Close()
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+duplicateQuery, "abc123", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		plan = append(plan, detail)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "requests_reply_hash") {
		t.Fatalf("重复回复查找没有使用回复指纹索引: %q", plan)
	}
}

// TestLedgerRecordsFiltersCursorAndExport 记录按维度、状态码与关键字筛选，游标分页不重不漏，CSV 防公式注入并带本地新增列
func TestLedgerRecordsFiltersCursorAndExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	for index := range 5 {
		store.Record(Row{
			ID: fmt.Sprintf("req_%d", index), Time: now.Add(-time.Duration(index) * time.Minute), Model: "flash", Protocol: "openai-chat",
			Path: "/v1/chat/completions", Account: "a@example.com", State: "completed", Status: 200,
		})
	}
	store.Record(Row{
		ID: "req_fail", Time: now.Add(-10 * time.Minute), Model: "pro", Protocol: "anthropic", Path: "/v1/messages",
		Account: "=cmd", State: "failed", Status: 503, Error: "上游暂时不可用", ServedModel: "other-model", Downgrade: "rejected",
		Attempts: []api.RequestAttempt{{Account: "b@example.com", Channel: "build", Error: "429", DurationMS: 12}},
	})
	store = reopen(t, store, path)
	defer store.Close()
	ctx := context.Background()
	base := api.UsageRecordQuery{From: now.Add(-time.Hour), To: now.Add(time.Minute), Limit: 2}

	seen := map[string]bool{}
	query := base
	for pages := 0; ; pages++ {
		page, err := store.Records(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Items {
			if seen[record.ID] {
				t.Fatalf("分页重复返回 %s", record.ID)
			}
			seen[record.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("分页没有结束")
		}
		// 游标的编码与解析由 API 层负责，这里按游标的含义从本页最后一条之后继续
		last := page.Items[len(page.Items)-1]
		if page.NextCursor != api.EncodeUsageCursor(api.UsageCursor{Time: last.Time, ID: last.ID}) {
			t.Fatalf("游标不是本页最后一条: %s", page.NextCursor)
		}
		query.Before = &api.UsageCursor{Time: last.Time, ID: last.ID}
	}
	if len(seen) != 6 {
		t.Fatalf("分页合计 %d 条，期望 6", len(seen))
	}

	filtered := base
	filtered.Limit = 50
	filtered.Filters = api.UsageFilters{"model": {"pro"}}
	filtered.Status = 503
	page, err := store.Records(ctx, filtered)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("按模型与状态码筛选: items=%d err=%v", len(page.Items), err)
	}
	record := page.Items[0]
	if record.ServedModel != "other-model" || record.Downgrade != "rejected" || len(record.Attempts) != 1 || record.Attempts[0].Channel != "build" {
		t.Fatalf("记录字段不对: %+v", record)
	}
	search := base
	search.Limit = 50
	search.Search = "暂时不可用"
	if page, err := store.Records(ctx, search); err != nil || len(page.Items) != 1 || page.Items[0].ID != "req_fail" {
		t.Fatalf("按错误关键字搜索: %+v err=%v", page.Items, err)
	}

	var output bytes.Buffer
	if err := store.ExportRecords(ctx, filtered, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(output.Bytes(), []byte("\xef\xbb\xbf")) {
		t.Fatal("CSV 缺少 UTF-8 BOM")
	}
	rows, err := csv.NewReader(bytes.NewReader(output.Bytes()[3:])).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV 行数=%d err=%v", len(rows), err)
	}
	header := strings.Join(rows[0], ",")
	if !strings.HasSuffix(header, "served_model,downgrade,reply_hash,duplicate") {
		t.Fatalf("CSV 表头缺少本地新增列: %s", header)
	}
	if rows[1][5] != "'=cmd" {
		t.Fatalf("账户列没有防公式注入: %q", rows[1][5])
	}
}

// TestLedgerRetentionPrunesOldRecords 超过保留期的原始记录与汇总被清理，保留期内的不受影响
func TestLedgerRetentionPrunesOldRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	old := now.Add(-usageRetention - 10*24*time.Hour)
	store.Record(Row{ID: "old", Time: old, Model: "flash", State: "completed"})
	store.Record(Row{ID: "recent", Time: now.Add(-time.Hour), Model: "flash", State: "completed"})
	store = reopen(t, store, path)
	defer store.Close()
	store.prune()
	ctx := context.Background()
	for _, table := range []string{"requests", "hours", "days"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("清理后 %s 剩 %d 行，期望只剩保留期内的 1 行", table, count)
		}
	}
	report, err := store.Usage(ctx, api.UsageQuery{From: old.Add(-time.Hour), To: old.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"})
	if err != nil || report.Totals.Requests != 0 {
		t.Fatalf("过期范围仍有请求: %d err=%v", report.Totals.Requests, err)
	}
}

// TestLedgerRecordBounds 空 ID 的记录不写入，超长错误、路径与模型名按字符截断，关闭后写入被忽略且不阻塞
func TestLedgerRecordBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	store.Record(Row{Time: now, State: "completed"})
	long := strings.Repeat("错", textLimit+50)
	// 模型名来自客户端：1 MiB 的模型名不能原样进入汇总主键与之后每次用量查询
	model := strings.Repeat("m", 1<<20)
	store.Record(Row{
		ID: "long", Time: now, State: "failed", Status: 500, Error: long, Path: "/v1beta/models/" + model + ":generateContent",
		Model: model, ServedModel: model, Attempts: []api.RequestAttempt{{Error: long}},
	})
	store = reopen(t, store, path)
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: now.Add(-time.Minute), To: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("记录数=%d err=%v，空 ID 的记录不应写入", len(page.Items), err)
	}
	record := page.Items[0]
	if utf8.RuneCountInString(record.Error) != textLimit+1 || utf8.RuneCountInString(record.Attempts[0].Error) != textLimit+1 {
		t.Fatalf("超长错误没有截断: %d %d", utf8.RuneCountInString(record.Error), utf8.RuneCountInString(record.Attempts[0].Error))
	}
	if utf8.RuneCountInString(record.Path) != textLimit+1 || utf8.RuneCountInString(record.Model) != modelLimit+1 ||
		utf8.RuneCountInString(record.ServedModel) != modelLimit+1 {
		t.Fatalf("超长路径或模型名没有截断: path=%d model=%d served=%d",
			utf8.RuneCountInString(record.Path), utf8.RuneCountInString(record.Model), utf8.RuneCountInString(record.ServedModel))
	}
	report, err := store.Usage(context.Background(), api.UsageQuery{
		From: now.Add(-time.Hour), To: now.Add(time.Minute), Bucket: time.Hour, Location: time.UTC, Stack: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if models := report.Groups["model"]; len(models) != 1 || models[0].Key != record.Model {
		t.Fatalf("用量分项的模型名应是截断后的取值: %d 项", len(models))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store.Record(Row{ID: "after-close", Time: now})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var nilStore *Store
	nilStore.Record(Row{ID: "nil"})
	nilStore.SetBodyCapture(true)
	if nilStore.BodyCapture() || nilStore.Close() != nil {
		t.Fatal("nil 账本的方法应当是空操作")
	}
}

// TestLedgerFilePermissions 账本文件只允许当前用户读写，已有的宽松权限在打开时收紧
func TestLedgerFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位")
	}
	path := filepath.Join(t.TempDir(), "requests.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	store.Record(Row{ID: "a", Time: time.Now().UTC(), State: "completed"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal"} {
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s 权限=%o，期望 600", filepath.Base(name), mode)
		}
	}
}
