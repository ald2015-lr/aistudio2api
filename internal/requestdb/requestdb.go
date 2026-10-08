// Package requestdb 在本地 SQLite 中保存请求用量、小时与本地日汇总及可选的截断正文
package requestdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	// 纯 Go 的 SQLite 驱动：CGO_ENABLED=0 的发布构建同样可用
	_ "modernc.org/sqlite"
)

// usageRetention 是用量记录与汇总的保留时长，按服务器本地日整日清理
const usageRetention = 90 * 24 * time.Hour

// bodyRetention 是保留的最近正文条数
const bodyRetention = 1000

// batchLimit 是单个写事务合并的最大记录数
const batchLimit = 256

// queueLimit 是等待写入的最大记录数，队列已满时丢弃新记录
const queueLimit = 4096

// textLimit 是错误摘要、每次上游尝试错误与路径保存的最大字符数，避免个别超长错误撑大账本
const textLimit = 2000

// modelLimit 是模型名与实际服务模型保存的最大字符数。模型名来自客户端，没有通过模型目录校验的请求也会记录：
// 原样保存时一个超长模型名会写进汇总主键，并在之后 90 天的每次用量查询的分项、候选、组合与堆叠序列里重复出现
const modelLimit = 200

// duplicateWindow 是重复回复的比对窗口，与请求日志的重复回复检测一致：窗口内出现过完全相同的回复正文才计为重复
const duplicateWindow = 2 * time.Hour

// duplicateMinReplyTokens 是参与重复回复统计的最少回复 token，约等于请求日志重复检测的 200 字节下限：
// 过短的回复（固定答复、单个词）相同属于正常
const duplicateMinReplyTokens = 50

// duplicateQuery 查找窗口内是否已有相同回复指纹。指纹非空的条件不能省：SQLite 推断不出绑定参数非空，
// 少了它就用不上部分索引 requests_reply_hash，每条回复都要扫描窗口内的全部记录，请求多时写入跟不上、队列满后丢记录
const duplicateQuery = `SELECT EXISTS(SELECT 1 FROM requests WHERE reply_hash = ? AND reply_hash != '' AND time >= ? AND time <= ?)`

// hourMS 是一小时的毫秒数
const hourMS = int64(time.Hour / time.Millisecond)

// rollupFields 是小时与本地日汇总表除主键外的列，读取与写入都按这个顺序
const rollupFields = `requests, input_tokens, reasoning_tokens, reply_tokens, total_tokens,
 duration_ms, first_event_ms, first_events, queue_ms, downgrade_checked, downgrade_rejected, replies, duplicates,
 duration_hist, first_event_hist, last_time`

// rollupKeyFields 是汇总表的主键列
const rollupKeyFields = `start, model, account, channel, protocol, state, status, pool`

// rollupColumnsV1 是加入号池之前的小时与本地日汇总表的列（迁移旧账本时按它复制）
const rollupColumnsV1 = `start, model, account, channel, protocol, state, status, ` + rollupFields

// rollupColumns 是小时与本地日汇总表的列，start 是分段起点；pool 为请求的号池（ultra，普通号池为空），
// 放在最后，与旧账本迁移时追加的列位置一致
const rollupColumns = `
 start INTEGER NOT NULL, model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, protocol TEXT NOT NULL,
 state TEXT NOT NULL, status INTEGER NOT NULL, requests INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, first_events INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 downgrade_checked INTEGER NOT NULL, downgrade_rejected INTEGER NOT NULL, replies INTEGER NOT NULL, duplicates INTEGER NOT NULL,
 duration_hist BLOB NOT NULL, first_event_hist BLOB NOT NULL, last_time INTEGER NOT NULL, pool TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (start, model, account, channel, protocol, state, status, pool)`

// requestFields 是原始记录表的列，写入按这个顺序
const requestFields = `id, time, protocol, path, model, account, channel, status, state,
 duration_ms, first_event_ms, queue_ms, input_tokens, reasoning_tokens, reply_tokens, total_tokens, tool_calls,
 error, attempts, served_model, downgrade, reply_hash, duplicate, pool`

// 本地相对上游新增 served_model（上游标明的实际模型）、downgrade（降级判定结论）、reply_hash（回复正文指纹）
// 与 duplicate（窗口内出现过相同回复），用于统计降级拦截率与重复回复率；指纹只用于比对，不保存回复内容。
// pool 为请求的号池（经 /ultra 进入的请求为 ultra，其余为空），旧账本打开时由 migrate 补上（见 migrate）
const schema = `
CREATE TABLE IF NOT EXISTS requests (
 id TEXT PRIMARY KEY, time INTEGER NOT NULL, protocol TEXT NOT NULL, path TEXT NOT NULL,
 model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, status INTEGER NOT NULL, state TEXT NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL,
 total_tokens INTEGER NOT NULL, tool_calls INTEGER NOT NULL, error TEXT NOT NULL, attempts TEXT NOT NULL,
 served_model TEXT NOT NULL, downgrade TEXT NOT NULL, reply_hash TEXT NOT NULL, duplicate INTEGER NOT NULL,
 pool TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS requests_time ON requests(time);
CREATE INDEX IF NOT EXISTS requests_model_time ON requests(model, time);
CREATE INDEX IF NOT EXISTS requests_account_time ON requests(account, time);
CREATE INDEX IF NOT EXISTS requests_state_time ON requests(state, time);
CREATE INDEX IF NOT EXISTS requests_reply_hash ON requests(reply_hash, time) WHERE reply_hash != '';
CREATE TABLE IF NOT EXISTS hours (` + rollupColumns + `) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS days (` + rollupColumns + `) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS bodies (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, time INTEGER NOT NULL,
 request TEXT NOT NULL, request_size INTEGER NOT NULL, response TEXT NOT NULL, response_size INTEGER NOT NULL);`

// Row 是一次完成请求的用量记录
type Row struct {
	ID              string
	Time            time.Time
	Protocol        string
	Path            string
	Model           string
	Account         string
	Channel         string
	Status          int
	State           string
	Duration        time.Duration
	FirstEvent      time.Duration
	Queue           time.Duration
	InputTokens     int64
	ReasoningTokens int64
	ReplyTokens     int64
	TotalTokens     int64
	ToolCalls       int
	Error           string
	Attempts        []api.RequestAttempt
	// ServedModel 为上游标明的实际服务模型（只在与请求的模型系列不同时有值）
	ServedModel string
	// Downgrade 为降级判定结论（rejected、passed、unjudged），没有经过判定时为空
	Downgrade string
	// ReplyHash 为回复正文指纹，只用于判断两次回复是否相同
	ReplyHash string
	// Pool 为请求的号池：经 /ultra 进入的请求为 ultra，其余为空
	Pool string
}

// countedReply 判断记录是否参与重复回复统计
func (row *Row) countedReply() bool {
	return row.ReplyHash != "" && row.ReplyTokens >= duplicateMinReplyTokens
}

// write 是写入协程处理的一条用量或正文
type write struct {
	row  *Row
	body *api.RequestBody
}

// Logger 输出账本写入失败与丢弃记录的运行日志
type Logger func(level string, message string)

// Store 串行批量写入请求账本与汇总，并提供聚合与明细查询
type Store struct {
	db      *sql.DB
	writes  chan write
	done    chan struct{}
	log     Logger
	capture atomic.Bool
	dropped atomic.Int64
	mu      sync.RWMutex
	closed  bool
}

// Open 打开或创建账本数据库并启动写入协程
func Open(path string, log Logger) (*Store, error) {
	if log == nil {
		log = func(string, string) {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 账本含账户名与错误摘要，开启正文记录时还有提示词与回复：数据库文件只允许当前用户读写，
	// SQLite 创建的 -wal 与 -shm 文件沿用数据库文件的权限
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(file.Close(), os.Chmod(path, 0o600)); err != nil {
		return nil, err
	}
	options := url.Values{"_pragma": {"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)"}}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+options.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(fmt.Errorf("初始化请求账本: %w", err), db.Close())
	}
	if err := migrate(db, log); err != nil {
		return nil, errors.Join(fmt.Errorf("升级请求账本: %w", err), db.Close())
	}
	store := &Store{db: db, writes: make(chan write, queueLimit), done: make(chan struct{}), log: log}
	if err := store.alignDays(); err != nil {
		return nil, errors.Join(fmt.Errorf("重建本地日汇总: %w", err), db.Close())
	}
	go store.run()
	return store, nil
}

// Close 写完已排队的记录后关闭数据库
func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return nil
	}
	store.closed = true
	close(store.writes)
	store.mu.Unlock()
	<-store.done
	return store.db.Close()
}

// SetBodyCapture 设置是否保存请求与响应正文
func (store *Store) SetBodyCapture(enabled bool) {
	if store != nil {
		store.capture.Store(enabled)
	}
}

// BodyCapture 返回是否保存请求与响应正文
func (store *Store) BodyCapture() bool {
	return store != nil && store.capture.Load()
}

// Record 排队写入一条用量记录，不等待落盘
func (store *Store) Record(row Row) {
	if row.ID == "" {
		return
	}
	row.Error, row.Path = truncateText(row.Error, textLimit), truncateText(row.Path, textLimit)
	row.Model, row.ServedModel = truncateText(row.Model, modelLimit), truncateText(row.ServedModel, modelLimit)
	if len(row.Attempts) > 0 {
		attempts := make([]api.RequestAttempt, len(row.Attempts))
		for index, attempt := range row.Attempts {
			attempt.Error = truncateText(attempt.Error, textLimit)
			attempts[index] = attempt
		}
		row.Attempts = attempts
	}
	store.enqueue(write{row: &row})
}

// SaveBody 排队写入一条截断正文，不等待落盘
func (store *Store) SaveBody(body api.RequestBody) {
	if body.ID != "" {
		store.enqueue(write{body: &body})
	}
}

// truncateText 按字符截断超过 limit 的文本
func truncateText(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}

// enqueue 把记录交给写入协程，队列已满时计入丢弃数而不阻塞请求
func (store *Store) enqueue(item write) {
	if store == nil {
		return
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return
	}
	select {
	case store.writes <- item:
	default:
		store.dropped.Add(1)
	}
}

// run 合并连续到达的记录为单个事务，并每天清理过期用量
func (store *Store) run() {
	defer close(store.done)
	store.prune()
	prune := time.NewTicker(24 * time.Hour)
	defer prune.Stop()
	for {
		select {
		case item, ok := <-store.writes:
			if !ok {
				return
			}
			batch := []write{item}
			for len(batch) < batchLimit {
				select {
				case next, ok := <-store.writes:
					if !ok {
						store.commit(batch)
						return
					}
					batch = append(batch, next)
					continue
				default:
				}
				break
			}
			store.commit(batch)
		case <-prune.C:
			store.prune()
		}
	}
}

// commit 写入一批记录，记录写入失败与队列丢弃
func (store *Store) commit(batch []write) {
	if err := store.write(batch); err != nil {
		store.log("ERROR", fmt.Sprintf("请求账本写入失败 | 记录=%d | 错误=%v", len(batch), err))
	}
	if dropped := store.dropped.Swap(0); dropped > 0 {
		store.log("WARN", fmt.Sprintf("请求账本写入队列已满 | 丢弃=%d", dropped))
	}
}

// rollupKey 是汇总表的主键
type rollupKey struct {
	start    int64
	model    string
	account  string
	channel  string
	protocol string
	state    string
	status   int
	pool     string
}

// keyOf 返回条目在指定分段起点下的汇总键
func keyOf(start int64, value *entry) rollupKey {
	return rollupKey{
		start: start, model: value.model, account: value.account, channel: value.channel,
		protocol: value.protocol, state: value.state, status: value.status, pool: value.pool,
	}
}

// collect 把条目累加到同键汇总，首次出现时保存副本
func collect(target map[rollupKey]*entry, key rollupKey, value *entry) {
	if current := target[key]; current != nil {
		current.combine(value)
		return
	}
	copied := value.clone()
	copied.time = key.start
	target[key] = copied
}

// write 在一个事务内写入记录、累加小时与本地日汇总并裁剪正文条数
func (store *Store) write(batch []write) (err error) {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	hours, days := map[rollupKey]*entry{}, map[rollupKey]*entry{}
	bodies := false
	for _, item := range batch {
		if row := item.row; row != nil {
			attempts := row.Attempts
			if attempts == nil {
				attempts = []api.RequestAttempt{}
			}
			encoded, err := json.Marshal(attempts)
			if err != nil {
				return err
			}
			// 重复回复：插入之前查找窗口内的相同指纹（同一批内先写入的记录在同一事务中可见）
			duplicate := false
			if row.countedReply() {
				at := row.Time.UnixMilli()
				if err := tx.QueryRow(duplicateQuery, row.ReplyHash, at-duplicateWindow.Milliseconds(), at).Scan(&duplicate); err != nil {
					return err
				}
			}
			result, err := tx.Exec(`INSERT OR IGNORE INTO requests (`+requestFields+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				row.ID, row.Time.UnixMilli(), row.Protocol, row.Path, row.Model, row.Account, row.Channel, row.Status, row.State,
				row.Duration.Milliseconds(), row.FirstEvent.Milliseconds(), row.Queue.Milliseconds(),
				row.InputTokens, row.ReasoningTokens, row.ReplyTokens, row.TotalTokens, row.ToolCalls, row.Error, string(encoded),
				row.ServedModel, row.Downgrade, row.ReplyHash, duplicate, row.Pool)
			if err != nil {
				return err
			}
			if inserted, err := result.RowsAffected(); err != nil {
				return err
			} else if inserted == 1 {
				value := rowEntry(row, duplicate)
				day, _ := localDay(value.time)
				collect(hours, keyOf(floorDiv(value.time, hourMS)*hourMS, value), value)
				collect(days, keyOf(day, value), value)
			}
		}
		if body := item.body; body != nil {
			bodies = true
			if _, err := tx.Exec(`INSERT OR REPLACE INTO bodies (id,time,request,request_size,response,response_size) VALUES (?,?,?,?,?,?)`,
				body.ID, body.Time.UnixMilli(), body.Request, body.RequestSize, body.Response, body.ResponseSize); err != nil {
				return err
			}
		}
	}
	for key, value := range hours {
		if err := mergeRollup(tx, "hours", key, value); err != nil {
			return err
		}
	}
	for key, value := range days {
		if err := mergeRollup(tx, "days", key, value); err != nil {
			return err
		}
	}
	if bodies {
		if _, err := tx.Exec(`DELETE FROM bodies WHERE seq <= (SELECT MAX(seq) FROM bodies) - ?`, bodyRetention); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mergeRollup 把本批记录累加到汇总表中已有的同键汇总
func mergeRollup(tx *sql.Tx, table string, key rollupKey, value *entry) error {
	existing := &entry{}
	var durations, firstEvents []byte
	err := tx.QueryRow(`SELECT `+rollupFields+` FROM `+table+`
 WHERE start=? AND model=? AND account=? AND channel=? AND protocol=? AND state=? AND status=? AND pool=?`,
		key.start, key.model, key.account, key.channel, key.protocol, key.state, key.status, key.pool).Scan(
		&existing.requests, &existing.inputTokens, &existing.reasoningTokens, &existing.replyTokens, &existing.totalTokens,
		&existing.durationMS, &existing.firstEventMS, &existing.firstEvents, &existing.queueMS,
		&existing.downgradeChecked, &existing.downgradeRejected, &existing.replies, &existing.duplicates,
		&durations, &firstEvents, &existing.lastTime)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if existing.duration, err = decodeCounts(durations); err != nil {
			return err
		}
		if existing.firstEvent, err = decodeCounts(firstEvents); err != nil {
			return err
		}
		value.combine(existing)
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO `+table+` (`+rollupKeyFields+`, `+rollupFields+`)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		key.start, key.model, key.account, key.channel, key.protocol, key.state, key.status, key.pool, value.requests,
		value.inputTokens, value.reasoningTokens, value.replyTokens, value.totalTokens,
		value.durationMS, value.firstEventMS, value.firstEvents, value.queueMS,
		value.downgradeChecked, value.downgradeRejected, value.replies, value.duplicates,
		value.duration.encode(), value.firstEvent.encode(), value.lastTime)
	return err
}

// prune 删除超过保留期的数据，记录与汇总都从截止时间所在本地日的起点开始保留，使三层数据覆盖同一范围
func (store *Store) prune() {
	store.pruneBefore(time.Now().Add(-usageRetention))
}

// pruneBefore 删除截止时间所在本地日之前的记录与汇总
func (store *Store) pruneBefore(cutoff time.Time) {
	day, _ := localDay(cutoff.UnixMilli())
	hour := floorDiv(day, hourMS) * hourMS
	_, requestsErr := store.db.Exec(`DELETE FROM requests WHERE time < ?`, hour)
	_, hoursErr := store.db.Exec(`DELETE FROM hours WHERE start < ?`, hour)
	_, daysErr := store.db.Exec(`DELETE FROM days WHERE start < ?`, day)
	if err := errors.Join(requestsErr, hoursErr, daysErr); err != nil {
		store.log("ERROR", fmt.Sprintf("请求账本清理失败 | 错误=%v", err))
	}
}

// alignDays 在本地日汇总的起点与当前服务器时区不一致时，从小时汇总与原始记录重建全部本地日汇总
func (store *Store) alignDays() (err error) {
	rows, err := store.db.Query(`SELECT start FROM days GROUP BY start`)
	if err != nil {
		return err
	}
	aligned := true
	for rows.Next() {
		var start int64
		if err := rows.Scan(&start); err != nil {
			return errors.Join(err, rows.Close())
		}
		if day, _ := localDay(start); day != start {
			aligned = false
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || aligned {
		return err
	}
	var earliest sql.NullInt64
	if err := store.db.QueryRow(`SELECT MIN(start) FROM hours`).Scan(&earliest); err != nil {
		return err
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if _, err := tx.Exec(`DELETE FROM days`); err != nil {
		return err
	}
	rebuilt, now := 0, time.Now().UnixMilli()
	for start, end := localDay(earliest.Int64); earliest.Valid && start <= now; start, end = localDay(end) {
		days := map[rollupKey]*entry{}
		for _, part := range planHours(start, end, nil) {
			if err := store.scan(context.Background(), part, nil, func(value *entry) { collect(days, keyOf(start, value), value) }); err != nil {
				return err
			}
		}
		for key, value := range days {
			if err := mergeRollup(tx, "days", key, value); err != nil {
				return err
			}
		}
		rebuilt++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	store.log("INFO", fmt.Sprintf("请求账本已按服务器时区重建本地日汇总 | 天数=%d", rebuilt))
	return nil
}

// RequestBody 返回请求保存的截断正文
func (store *Store) RequestBody(ctx context.Context, id string) (api.RequestBody, error) {
	body := api.RequestBody{ID: id}
	var at int64
	err := store.db.QueryRowContext(ctx, `SELECT time,request,request_size,response,response_size FROM bodies WHERE id=?`, id).
		Scan(&at, &body.Request, &body.RequestSize, &body.Response, &body.ResponseSize)
	if errors.Is(err, sql.ErrNoRows) {
		return api.RequestBody{}, api.ErrRequestBodyNotFound
	}
	if err != nil {
		return api.RequestBody{}, err
	}
	body.Time = time.UnixMilli(at).UTC()
	return body, nil
}

// floorDiv 返回向负无穷取整的整数除法结果
func floorDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor != 0 && (value < 0) != (divisor < 0) {
		quotient--
	}
	return quotient
}
