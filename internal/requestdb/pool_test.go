package requestdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// legacySchema 是加入号池列之前的账本结构（原始记录、小时与本地日汇总都没有 pool）
const legacySchema = `
CREATE TABLE requests (
 id TEXT PRIMARY KEY, time INTEGER NOT NULL, protocol TEXT NOT NULL, path TEXT NOT NULL,
 model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, status INTEGER NOT NULL, state TEXT NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL,
 total_tokens INTEGER NOT NULL, tool_calls INTEGER NOT NULL, error TEXT NOT NULL, attempts TEXT NOT NULL,
 served_model TEXT NOT NULL, downgrade TEXT NOT NULL, reply_hash TEXT NOT NULL, duplicate INTEGER NOT NULL);
CREATE INDEX requests_time ON requests(time);
CREATE TABLE hours (` + legacyRollupColumns + `) WITHOUT ROWID;
CREATE TABLE days (` + legacyRollupColumns + `) WITHOUT ROWID;
CREATE TABLE bodies (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, time INTEGER NOT NULL,
 request TEXT NOT NULL, request_size INTEGER NOT NULL, response TEXT NOT NULL, response_size INTEGER NOT NULL);`

const legacyRollupColumns = `
 start INTEGER NOT NULL, model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, protocol TEXT NOT NULL,
 state TEXT NOT NULL, status INTEGER NOT NULL, requests INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, first_events INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 downgrade_checked INTEGER NOT NULL, downgrade_rejected INTEGER NOT NULL, replies INTEGER NOT NULL, duplicates INTEGER NOT NULL,
 duration_hist BLOB NOT NULL, first_event_hist BLOB NOT NULL, last_time INTEGER NOT NULL,
 PRIMARY KEY (start, model, account, channel, protocol, state, status)`

// writeLegacyLedger 按旧结构写一个账本：一条原始记录及对应的小时与本地日汇总
func writeLegacyLedger(t *testing.T, path string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	millis := at.UnixMilli()
	if _, err := db.Exec(`INSERT INTO requests VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"legacy", millis, "openai-chat", "/v1/chat/completions", "flash", "a@example.com", "playground", 200, "completed",
		1000, 0, 0, 1, 0, 1, 2, 0, "", "[]", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	day, _ := localDay(millis)
	for table, start := range map[string]int64{"hours": floorDiv(millis, hourMS) * hourMS, "days": day} {
		if _, err := db.Exec(`INSERT INTO `+table+` VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			start, "flash", "a@example.com", "playground", "openai-chat", "completed", 200, 1,
			1, 0, 1, 2, 1000, 0, 0, 0, 0, 0, 0, 0, single(1000).encode(), counts(nil).encode(), millis); err != nil {
			t.Fatal(err)
		}
	}
}

// loggedStore 打开账本并收集运行日志
type loggedStore struct {
	mu       sync.Mutex
	messages []string
}

func (logs *loggedStore) open(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path, func(level, message string) {
		logs.mu.Lock()
		logs.messages = append(logs.messages, level+" "+message)
		logs.mu.Unlock()
		if level != "INFO" {
			t.Errorf("%s %s", level, message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (logs *loggedStore) contains(text string) bool {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, message := range logs.messages {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func poolGroups(report api.UsageReport) map[string]int64 {
	groups := map[string]int64{}
	for _, group := range report.Groups["pool"] {
		groups[group.Key] = group.Requests
	}
	return groups
}

// TestLedgerMigratesPoolColumn 旧账本打开时补上号池列（原始记录追加列，汇总表按新主键重建），旧数据归入普通号池；
// 之后 Ultra 与普通请求在原始记录、小时与本地日汇总中分开统计，可以按号池筛选与分组；再次打开不再迁移
func TestLedgerMigratesPoolColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	at := time.Now().Add(-72 * time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	writeLegacyLedger(t, path, at)

	logs := &loggedStore{}
	store := logs.open(t, path)
	if !logs.contains("请求账本已加入号池列") {
		t.Fatal("旧账本打开时应迁移并记录日志")
	}
	for _, table := range []string{"requests", "hours", "days"} {
		if exists, err := hasColumn(store.db, table, "pool"); err != nil || !exists {
			t.Fatalf("表 %s 缺少 pool 列: %v", table, err)
		}
	}
	var index string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='requests_pool_time'`).Scan(&index); err != nil {
		t.Fatalf("缺少号池索引: %v", err)
	}
	store.Record(Row{ID: "ultra", Time: at.Add(time.Minute), Protocol: "openai-chat", Path: "/ultra/v1/chat/completions",
		Model: "flash", Account: "u@example.com", Channel: "playground", Status: 200, State: "completed", Duration: time.Second,
		TotalTokens: 5, Pool: "ultra"})
	store.Record(Row{ID: "normal", Time: at.Add(2 * time.Minute), Protocol: "openai-chat", Path: "/v1/chat/completions",
		Model: "flash", Account: "a@example.com", Channel: "playground", Status: 200, State: "completed", Duration: time.Second,
		TotalTokens: 3})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	logs = &loggedStore{}
	store = logs.open(t, path)
	defer store.Close()
	if logs.contains("请求账本已加入号池列") {
		t.Fatal("已迁移的账本再次打开不应重复迁移")
	}

	ctx := context.Background()
	hour := at.Truncate(time.Hour)
	day, nextDay := localDay(at.UnixMilli())
	for name, query := range map[string]api.UsageQuery{
		"小时汇总":  {From: hour, To: hour.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "pool"},
		"原始记录":  {From: hour, To: hour.Add(59 * time.Minute), Bucket: time.Hour, Location: time.UTC, Stack: "pool"},
		"本地日汇总": {From: time.UnixMilli(day), To: time.UnixMilli(nextDay), Bucket: 24 * time.Hour, Location: time.Local, Stack: "pool"},
	} {
		report, err := store.Usage(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if got := poolGroups(report); got[api.UsagePoolNormal] != 2 || got[api.UsagePoolUltra] != 1 || len(got) != 2 {
			t.Fatalf("%s按号池分组 = %v，期望 normal 2、ultra 1", name, got)
		}
		if !slices.Equal(report.Options["pool"], []string{api.UsagePoolNormal, api.UsagePoolUltra}) {
			t.Fatalf("%s号池候选 = %v", name, report.Options["pool"])
		}
		keys := []string{}
		for _, series := range report.Series {
			keys = append(keys, series.Key)
		}
		if slices.Sort(keys); !slices.Equal(keys, []string{api.UsagePoolNormal, api.UsagePoolUltra}) {
			t.Fatalf("%s按号池堆叠 = %v", name, keys)
		}
		for pool, want := range map[string]int64{api.UsagePoolUltra: 1, api.UsagePoolNormal: 2} {
			filtered := query
			filtered.Filters = api.UsageFilters{"pool": {pool}}
			report, err := store.Usage(ctx, filtered)
			if err != nil {
				t.Fatal(err)
			}
			if report.Totals.Requests != want {
				t.Fatalf("%s筛选 pool=%s 得到 %d 条，期望 %d", name, pool, report.Totals.Requests, want)
			}
			if !slices.Equal(report.Options["pool"], []string{api.UsagePoolNormal, api.UsagePoolUltra}) {
				t.Fatalf("%s带筛选时号池候选 = %v", name, report.Options["pool"])
			}
		}
	}

	records, err := store.Records(ctx, api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	pools := map[string]string{}
	for _, record := range records.Items {
		pools[record.ID] = record.Pool
	}
	if pools["legacy"] != api.UsagePoolNormal || pools["normal"] != api.UsagePoolNormal || pools["ultra"] != api.UsagePoolUltra {
		t.Fatalf("记录的号池 = %v", pools)
	}
	ultraOnly, err := store.Records(ctx, api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Limit: 10, Filters: api.UsageFilters{"pool": {"ultra"}}})
	if err != nil || len(ultraOnly.Items) != 1 || ultraOnly.Items[0].ID != "ultra" {
		t.Fatalf("按号池筛选记录: %+v err=%v", ultraOnly.Items, err)
	}
	var output bytes.Buffer
	if err := store.ExportRecords(ctx, api.UsageRecordQuery{From: hour, To: hour.Add(time.Hour), Filters: api.UsageFilters{"pool": {"ultra"}}}, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(output.Bytes()[3:])).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV 行数=%d err=%v", len(rows), err)
	}
	column := slices.Index(rows[0], "pool")
	if column < 0 || rows[1][column] != api.UsagePoolUltra {
		t.Fatalf("CSV 号池列 = %v / %v", rows[0], rows[1])
	}
}

// TestLedgerPoolRollupsDoNotCollide 同一小时内其余维度都相同、只有号池不同的请求分别汇总，不会互相覆盖
func TestLedgerPoolRollupsDoNotCollide(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for index, pool := range []string{"ultra", "", "ultra"} {
		store.Record(Row{ID: fmt.Sprintf("r%d", index), Time: hour.Add(time.Duration(index) * time.Minute), Model: "flash",
			State: "completed", Status: 200, Duration: time.Second, TotalTokens: 10, Pool: pool})
	}
	store = reopen(t, store, path)
	defer store.Close()
	report, err := store.Usage(context.Background(), api.UsageQuery{From: hour, To: hour.Add(time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if got := poolGroups(report); got[api.UsagePoolUltra] != 2 || got[api.UsagePoolNormal] != 1 || report.Totals.TotalTokens != 30 {
		t.Fatalf("小时汇总按号池分组 = %v，tokens=%d", got, report.Totals.TotalTokens)
	}
}
