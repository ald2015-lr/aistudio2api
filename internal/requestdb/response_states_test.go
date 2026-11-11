package requestdb

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// loadStates 读出续接状态，返回 “ID=内容” 的列表
func loadStates(t *testing.T, store *Store) []string {
	t.Helper()
	states, err := store.LoadResponseStates()
	if err != nil {
		t.Fatal(err)
	}
	described := make([]string, 0, len(states))
	for _, state := range states {
		described = append(described, state.ID+"="+string(state.Data))
	}
	return described
}

// TestResponseStatesPersist 续接状态的写入在关闭前落盘：同 ID 再写替换内容、保留原来的保存顺序，删除生效，重新打开后按保存顺序读出
func TestResponseStatesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store := openTestStore(t, path)
	store.SaveResponseStates([]api.SavedResponseState{{ID: "resp_a", Data: []byte("a1")}, {ID: "resp_b", Data: []byte("b1")}}, nil)
	store.SaveResponseStates([]api.SavedResponseState{{ID: "resp_c", Data: []byte("c1")}, {ID: "resp_a", Data: []byte("a2")}}, []string{"resp_b"})
	store.SaveResponseStates(nil, nil)
	store = reopen(t, store, path)
	defer store.Close()
	if got := strings.Join(loadStates(t, store), ","); got != "resp_a=a2,resp_c=c1" {
		t.Fatalf("读出的续接状态 = %s", got)
	}
}

// TestResponseStatesBackwardCompatible 表迁移向后兼容：没有 response_states 表的旧账本打开时补建、读出为空；回滚到旧版本时
// （按旧版本的建表语句与迁移打开同一个数据库）不报错、不动这张表，再升级回来时原来的续接状态还在
func TestResponseStatesBackwardCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	legacySchema := strings.TrimSuffix(schema, responseStatesSchema)
	if legacySchema == schema {
		t.Fatal("前提：续接状态表应追加在建表语句末尾")
	}
	openLegacy := func() *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+url.Values{"_pragma": {"busy_timeout(10000)"}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(legacySchema); err != nil {
			t.Fatalf("旧版本建表: %v", err)
		}
		if err := migrate(db, func(string, string) {}); err != nil {
			t.Fatalf("旧版本迁移: %v", err)
		}
		return db
	}
	legacy := openLegacy()
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	if got := loadStates(t, store); len(got) != 0 {
		t.Fatalf("旧账本补建的表应为空: %v", got)
	}
	store.SaveResponseStates([]api.SavedResponseState{{ID: "resp_keep", Data: []byte("kept")}}, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacy = openLegacy()
	var count int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM response_states`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("旧版本打开后续接状态应原样保留: count=%d err=%v", count, err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	defer store.Close()
	if got := strings.Join(loadStates(t, store), ","); got != "resp_keep=kept" {
		t.Fatalf("升级回来后读出 %s", got)
	}
}

// TestResponseStatesQueueLimit 排队的续接状态超过字节上限时丢弃这一次改动（不排队、不占内存），由写入协程写一行 WARN；之后的改动照常写入
func TestResponseStatesQueueLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	var warnings []string
	store, err := Open(path, func(level, message string) {
		if level == "WARN" {
			warnings = append(warnings, message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	store.SaveResponseStates([]api.SavedResponseState{{ID: "resp_huge", Data: make([]byte, responseStateQueueBytes+1)}}, nil)
	if pending := store.stateBytes.Load(); pending != 0 {
		t.Fatalf("丢弃的改动不应计入排队字节: %d", pending)
	}
	store.SaveResponseStates([]api.SavedResponseState{{ID: "resp_small", Data: []byte("ok")}}, nil)
	store = reopen(t, store, path)
	defer store.Close()
	if got := strings.Join(loadStates(t, store), ","); got != "resp_small=ok" {
		t.Fatalf("读出 %s", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "续接状态写入排队已满，丢弃 1 次改动") {
		t.Fatalf("WARN = %q", warnings)
	}
}

// TestResponseStatesQueueBytesReleased 续接状态落盘后释放排队字节：合计超过上限、但每次排队时都不超的几次写入都写进状态库，
// 不因累计写过的字节被当作排队已满丢弃
func TestResponseStatesQueueBytesReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	var mu sync.Mutex
	var warnings []string
	store, err := Open(path, func(level, message string) {
		if level == "WARN" {
			mu.Lock()
			warnings = append(warnings, message)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	store.stateLimit = 100
	ids := []string{"resp_one", "resp_two", "resp_three"}
	for _, id := range ids {
		store.SaveResponseStates([]api.SavedResponseState{{ID: id, Data: make([]byte, 40)}}, nil)
		deadline := time.Now().Add(10 * time.Second)
		for store.stateBytes.Load() != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("写入 %s 后排队字节没有释放: %d", id, store.stateBytes.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	store = reopen(t, store, path)
	defer store.Close()
	if got := len(loadStates(t, store)); got != len(ids) {
		t.Fatalf("应写入 %d 个节点，读出 %d 个", len(ids), got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 0 {
		t.Fatalf("不应丢弃: %q", warnings)
	}
}
