package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	lockFileName         = ".lock"
	snapshotDirName      = "snapshots"
	sharedLockTimeout    = 5 * time.Second
	exclusiveLockTimeout = 10 * time.Second
	lockRetryDelay       = 20 * time.Millisecond
)

var errBusy = errors.New("vault busy")

// Options 为 Store 的可替换依赖（测试固定时间与随机数、接收日志）
type Options struct {
	Now  func() time.Time
	Rand io.Reader
	// Warn 接收文件读不出、主文件缺失而有备份、写入失败的 WARN（只含路径、原因类别与计数，不含内容）
	Warn func(string)
	// Info 接收文件恢复可读的 INFO
	Info func(string)
}

// Store 为账号资料与家庭组的存储：两份文件各自一把进程内状态（受 mu 保护）、共用一个跨进程文件锁 .lock。
// 锁序固定：先 mu 再文件锁；持有文件锁时从不再拿 mu。dir 为空时只在内存
type Store struct {
	dir      string
	opts     Options
	mu       sync.Mutex
	profiles *slot[*profileDoc]
	families *slot[*familyDoc]
	// readFile 读主文件与 .bak（测试注入读失败用；平时为 os.ReadFile）
	readFile func(string) ([]byte, error)
}

// Open 创建 Store，不做任何 I/O（第一次调用时才读文件，第一次真正有改动的写操作才创建目录与文件）
func Open(dir string, options Options) *Store {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Rand == nil {
		options.Rand = rand.Reader
	}
	if dir != "" {
		if absolute, err := filepath.Abs(dir); err == nil {
			dir = absolute
		}
	}
	return &Store{
		dir: dir, opts: options, readFile: os.ReadFile,
		profiles: &slot[*profileDoc]{name: fileProfiles, label: "账号资料", decode: decodeProfiles, encode: encodeProfiles, empty: emptyProfileDoc, records: profileRecords},
		families: &slot[*familyDoc]{name: fileFamilies, label: "家庭组", decode: decodeFamilies, encode: encodeFamilies, empty: emptyFamilyDoc, records: familyRecords},
	}
}

// Dir 为数据目录（只在内存时为空）
func (store *Store) Dir() string { return store.dir }

func (store *Store) path(name string) string { return filepath.Join(store.dir, name+".json") }
func (store *Store) lockPath() string        { return filepath.Join(store.dir, lockFileName) }
func (store *Store) snapshotDir() string     { return filepath.Join(store.dir, snapshotDirName) }

func (store *Store) now() time.Time { return store.opts.Now() }

func (store *Store) warn(message string) {
	if store.opts.Warn != nil {
		store.opts.Warn(message)
	}
}

func (store *Store) info(message string) {
	if store.opts.Info != nil {
		store.opts.Info(message)
	}
}

// newHexID 为 12 位 hex（write_id 与家庭组编号用）
func (store *Store) newHexID() string {
	var buffer [6]byte
	if _, err := io.ReadFull(store.opts.Rand, buffer[:]); err != nil {
		sum := sha256.Sum256([]byte(store.now().String()))
		copy(buffer[:], sum[:])
	}
	return hex.EncodeToString(buffer[:])
}

func (store *Store) newGroupID(exists func(string) bool) string {
	for {
		if id := "fam-" + store.newHexID(); !exists(id) {
			return id
		}
	}
}

// dryRunGroupIDs 为预览里新建的组的临时编号（new-1、new-2…，确认时换成正式编号）
func dryRunGroupIDs() func(func(string) bool) string {
	next := 0
	return func(exists func(string) bool) string {
		for {
			next++
			if id := fmt.Sprintf("new-%d", next); !exists(id) {
				return id
			}
		}
	}
}

// hashToken 为文件内容的标记（revision 的一部分）：内容变了就变，人工改了文件而 write_id 没变也能看出
func hashToken(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

func (store *Store) revisionLocked() string {
	return store.profiles.token + "." + store.families.token
}

func (store *Store) loadAll(ctx context.Context) error {
	if err := store.profiles.load(ctx, store); err != nil {
		return err
	}
	return store.families.load(ctx, store)
}

// splitRevision 返回 revision 里资料与家庭组的部分
func splitRevision(revision string) (profiles string, families string) {
	index := strings.LastIndexByte(revision, '.')
	if index < 0 {
		return revision, ""
	}
	return revision[:index], revision[index+1:]
}

type document interface {
	meta() *docMeta
}

// slot 为一份文件的缓存与状态（只在持有 Store.mu 时访问）：缓存的 doc 不可变，写操作产生新的 doc 整体替换
type slot[D document] struct {
	name    string
	label   string
	decode  func([]byte) (D, *loadProblem)
	encode  func(D, string, time.Time) []byte
	empty   func() D
	records func(D) int

	loaded bool
	// info 为载入时主文件的 stat（主文件不存在或读不出时为 nil）；missingCached 为当前状态来自“主文件不存在”，missKey 为当时的备份情况
	info          os.FileInfo
	missingCached bool
	missKey       string
	doc           D
	token         string
	fileErr       *FileError
	warned        string
	inError       bool
	// bakKey 为上次解析 .bak 时的情况（backupState.key），bakCount/bakOK 为当时的条数与能否解析
	bakKey   string
	bakCount int
	bakOK    bool
}

func (sl *slot[D]) emptyDoc() D {
	doc := sl.empty()
	doc.meta().token = "0"
	return doc
}

// readFailed 为当前错误来自读文件失败（而不是内容解析不了）：内容没变时解析结果不会变，可以按 stat 缓存；读失败要重试
func (sl *slot[D]) readFailed() bool {
	return sl.fileErr != nil && sl.fileErr.Reason == problemRead
}

func sameStat(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.ModTime().Equal(right.ModTime()) && left.Size() == right.Size()
}

// load 按 stat 判断是否需要重读：主文件没变用缓存；变了就在共享锁下重读、查重复键、解析、校验；不存在时看 .bak 与快照
func (sl *slot[D]) load(ctx context.Context, store *Store) error {
	if store.dir == "" {
		if !sl.loaded {
			sl.setOK(store, sl.emptyDoc(), nil)
		}
		return nil
	}
	path := store.path(sl.name)
	info, err := os.Stat(path)
	switch {
	case err == nil:
		// 读失败（权限、属主或 I/O）不走缓存：修权限只改 ctime，stat 看不出变化，要每次重试才能在“重新读取”时恢复
		if sl.loaded && sl.info != nil && sameStat(sl.info, info) && !sl.readFailed() {
			return nil
		}
		data, readInfo, readErr := store.readShared(ctx, path)
		switch {
		case readErr == nil:
			sl.accept(store, data, readInfo)
		case errors.Is(readErr, errBusy):
			return busy()
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(readErr, fs.ErrNotExist):
			sl.loadMissing(store)
		default:
			sl.setError(store, readProblem(readErr), info)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		sl.loadMissing(store)
		return nil
	default:
		sl.setError(store, readProblem(err), nil)
		return nil
	}
}

// accept 解析读到的主文件：解析不了进入错误状态，否则换上新的缓存
func (sl *slot[D]) accept(store *Store, data []byte, info os.FileInfo) {
	doc, problem := sl.decode(data)
	if problem != nil {
		sl.setError(store, problem, info)
		return
	}
	doc.meta().token = hashToken(data)
	sl.setOK(store, doc, info)
}

func (sl *slot[D]) loadMissing(store *Store) {
	backup := store.backupState(sl.name)
	key := backup.key()
	if sl.loaded && sl.missingCached && sl.missKey == key {
		return
	}
	if backup.present() {
		sl.setMissing(store, backup)
	} else {
		sl.setOK(store, sl.emptyDoc(), nil)
	}
	sl.missingCached, sl.missKey = true, key
}

func (sl *slot[D]) setOK(store *Store, doc D, info os.FileInfo) {
	if sl.inError {
		store.info(fmt.Sprintf("%s文件已恢复可读 | 文件=%s", sl.label, store.path(sl.name)))
	}
	sl.inError, sl.warned, sl.fileErr = false, "", nil
	sl.doc, sl.token, sl.info, sl.loaded, sl.missingCached = doc, doc.meta().token, info, true, false
}

// setError 进入只读错误状态：不改名、不覆盖、不从空开始；同一文件、同一（修改时间、大小）只写一次 WARN
func (sl *slot[D]) setError(store *Store, problem *loadProblem, info os.FileInfo) {
	backup := store.backupState(sl.name)
	path := store.path(sl.name)
	fileErr := &FileError{File: sl.name, Code: CodeUnreadable, Reason: problem.reason, Message: problem.text, Path: path}
	count, countOK := sl.fillBackup(store, fileErr, backup)
	token := "e0-0"
	if info != nil {
		token = fmt.Sprintf("e%d-%d", info.ModTime().UnixNano(), info.Size())
	}
	if key := problem.reason + "|" + problem.text + "|" + token; sl.warned != key {
		sl.warned = key
		store.warn(fmt.Sprintf("%s文件无法读取，已停止写入以免覆盖 | 文件=%s | 原因=%s | 备份=%s | 快照=%d 份",
			sl.label, path, problem.text, backupText(backup, count, countOK, false), backup.snapshots))
	}
	sl.inError, sl.fileErr = true, fileErr
	sl.doc, sl.token, sl.info, sl.loaded, sl.missingCached = sl.emptyDoc(), token, info, true, false
}

// setMissing 为主文件不见了而 .bak 或快照还在：同样停止写入（要用户恢复或明确点“从空开始”），免得下一次写把备份换成新建的小文件
func (sl *slot[D]) setMissing(store *Store, backup backupState) {
	path := store.path(sl.name)
	fileErr := &FileError{File: sl.name, Code: CodeUnreadable, Reason: problemMissingWithBackup, Path: path}
	count, countOK := sl.fillBackup(store, fileErr, backup)
	var parts []string
	if backup.bakExists {
		if countOK {
			parts = append(parts, fmt.Sprintf("%s：%d 条，修改于 %s", filepath.Base(backup.bakPath), count, localTime(backup.bakTime)))
		} else {
			parts = append(parts, fmt.Sprintf("%s 读不出来，修改于 %s", filepath.Base(backup.bakPath), localTime(backup.bakTime)))
		}
	}
	if backup.snapshots > 0 {
		parts = append(parts, fmt.Sprintf("快照 %d 份，最新 %s", backup.snapshots, localTime(backup.latestTime)))
	}
	fileErr.Message = fmt.Sprintf("%s 不见了，但还有备份（%s）。已停止写入以免覆盖备份", filepath.Base(path), strings.Join(parts, "；"))
	latest := backup.bakTime
	if backup.latestTime.After(latest) {
		latest = backup.latestTime
	}
	if key := "missing|" + backup.key(); sl.warned != key {
		sl.warned = key
		store.warn(fmt.Sprintf("%s文件不见了，但还有备份，已停止写入以免覆盖备份 | 文件=%s | 备份=%s | 快照=%d 份",
			sl.label, path, backupText(backup, count, countOK, true), backup.snapshots))
	}
	sl.inError, sl.fileErr = true, fileErr
	sl.doc, sl.token, sl.info, sl.loaded = sl.emptyDoc(), fmt.Sprintf("m%d", latest.UnixNano()), nil, true
}

// fillBackup 填写 .bak 的路径、修改时间与条数（.bak 读不出时不给条数）和快照份数。
// 条数按 .bak 的情况（大小、修改时间）缓存：读失败时每次“重新读取”都会走到这里，.bak 没变就不再重新解析（2 万条约 200ms）
func (sl *slot[D]) fillBackup(store *Store, fileErr *FileError, backup backupState) (int, bool) {
	fileErr.Snapshots = backup.snapshots
	if !backup.bakExists {
		return 0, false
	}
	at := backup.bakTime
	fileErr.Backup, fileErr.BackupTime = backup.bakPath, &at
	if key := backup.key(); sl.bakKey != key {
		sl.bakKey, sl.bakCount, sl.bakOK = key, 0, false
		if data, err := store.readFile(backup.bakPath); err == nil {
			if doc, problem := sl.decode(data); problem == nil {
				sl.bakCount, sl.bakOK = sl.records(doc), true
			}
		}
	}
	if !sl.bakOK {
		return 0, false
	}
	count := sl.bakCount
	fileErr.BackupCount = &count
	return count, true
}

func backupText(backup backupState, count int, countOK bool, withCount bool) string {
	if !backup.bakExists {
		return "没有"
	}
	switch {
	case withCount && countOK:
		return fmt.Sprintf("%s（%d 条，%s）", backup.bakPath, count, localTime(backup.bakTime))
	case withCount:
		return fmt.Sprintf("%s（读不出来，%s）", backup.bakPath, localTime(backup.bakTime))
	default:
		return fmt.Sprintf("%s（%s）", backup.bakPath, localTime(backup.bakTime))
	}
}

func localTime(at time.Time) string { return at.Local().Format("2006-01-02 15:04:05") }

// readProblem 只写系统错误类别（如 permission denied），不写路径以外的任何内容
func readProblem(err error) *loadProblem {
	detail := "读取失败"
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		detail = pathErr.Err.Error()
	}
	return &loadProblem{reason: problemRead, text: "读取失败（" + detail + "）"}
}

// readShared 在 .lock 的共享锁下读主文件（原子改名保证读到完整版本，共享锁另外让 Windows 上的改名不撞上正在读的进程）。
// 没有 .lock（本程序从没写过这个目录）时不创建它，直接读
func (store *Store) readShared(ctx context.Context, path string) ([]byte, os.FileInfo, error) {
	lock := flock.New(store.lockPath(), flock.SetFlag(os.O_RDONLY))
	waitCtx, cancel := context.WithTimeout(ctx, sharedLockTimeout)
	defer cancel()
	locked, err := lock.TryRLockContext(waitCtx, lockRetryDelay)
	switch {
	case err == nil && locked:
		defer lock.Unlock()
	case ctx.Err() != nil:
		return nil, nil, ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return nil, nil, errBusy
	}
	data, err := store.readFile(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// backupState 为一个文件的 .bak 与快照情况
type backupState struct {
	bakPath    string
	bakExists  bool
	bakSize    int64
	bakTime    time.Time
	snapshots  int
	latestName string
	latestTime time.Time
}

// present 为“还有备份”：.bak 存在且非空，或快照目录里有这个文件的快照
func (backup backupState) present() bool {
	return (backup.bakExists && backup.bakSize > 0) || backup.snapshots > 0
}

func (backup backupState) key() string {
	return fmt.Sprintf("%v|%d|%d|%d|%s|%d", backup.bakExists, backup.bakSize, backup.bakTime.UnixNano(), backup.snapshots, backup.latestName, backup.latestTime.UnixNano())
}

func (store *Store) backupState(name string) backupState {
	backup := backupState{bakPath: store.path(name) + ".bak"}
	if info, err := os.Stat(backup.bakPath); err == nil && info.Mode().IsRegular() {
		backup.bakExists, backup.bakSize, backup.bakTime = true, info.Size(), info.ModTime()
	}
	entries, err := os.ReadDir(store.snapshotDir())
	if err != nil {
		return backup
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), name+"-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backup.snapshots++
		if info.ModTime().After(backup.latestTime) || (info.ModTime().Equal(backup.latestTime) && entry.Name() > backup.latestName) {
			backup.latestName, backup.latestTime = entry.Name(), info.ModTime()
		}
	}
	return backup
}

// lockExclusive 拿 .lock 的跨进程排他锁（每次新建 Flock 对象，从不在同一对象上由共享锁升级）；等不到时 503
func (store *Store) lockExclusive(ctx context.Context, label string) (*flock.Flock, error) {
	lock := flock.New(store.lockPath())
	waitCtx, cancel := context.WithTimeout(ctx, exclusiveLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(waitCtx, lockRetryDelay)
	switch {
	case err == nil && locked:
		return lock, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err == nil || errors.Is(err, context.DeadlineExceeded):
		return nil, busy()
	default:
		return nil, store.writeFailed(label, store.lockPath(), err)
	}
}

// writeFailed 记一行 WARN 并返回 500（文案只含路径与系统错误）
func (store *Store) writeFailed(label string, path string, err error) *Error {
	store.warn(fmt.Sprintf("%s写入失败 | 文件=%s | 错误=%s", label, path, err.Error()))
	return &Error{Status: http.StatusInternalServerError, Code: CodeWriteFailed, Message: fmt.Sprintf("写入失败（%s：%s），原有数据没有改动", path, err.Error())}
}

// applyFunc 在当前内容上计算一次操作：返回新的 doc 与是否有改动；revision 为当前（锁内重读之后）的 revision，用于冲突时附带的预览。必须是纯函数
type applyFunc[D document] func(cur D, revision string) (D, bool, error)

// mutate 执行一次写操作（调用方持有 Store.mu）：目录不存在且在空状态上没有改动时直接返回（不建目录、不建 .lock）；
// 否则拿 .lock 的排他锁 → 锁内重新读盘（内容与缓存相同才沿用缓存的解析结果）→ 读不出、主文件缺失而有备份时 409 且不写任何东西 →
// 只把本次操作应用到读到的内容 → 没有改动不写盘 → 批量操作先存快照 → 写 .bak（上一版原样字节）→ 原子写主文件
func (sl *slot[D]) mutate(ctx context.Context, store *Store, snapshot bool, apply applyFunc[D]) (string, error) {
	if store.dir == "" {
		next, changed, err := apply(sl.doc, store.revisionLocked())
		if err != nil || !changed {
			return store.revisionLocked(), err
		}
		id := store.newHexID()
		next.meta().writeID, next.meta().token = id, id
		sl.setOK(store, next, nil)
		return store.revisionLocked(), nil
	}
	// 调用方刚在同一把 Store.mu 下 loadAll 过：已知读不出（或主文件缺失而有备份）就直接 409，不再拿锁重读，
	// 免得读失败时这里与 load 交替改写 WARN 去重键、每次写都多记 WARN；.lock 打不开时也同样是 409
	if sl.fileErr != nil {
		return "", unreadable(sl.name)
	}
	if _, err := os.Stat(store.dir); errors.Is(err, fs.ErrNotExist) {
		if _, changed, err := apply(sl.emptyDoc(), store.revisionLocked()); err != nil || !changed {
			return store.revisionLocked(), err
		}
	}
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return "", store.writeFailed(sl.label, store.dir, err)
	}
	lock, err := store.lockExclusive(ctx, sl.label)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	path := store.path(sl.name)
	old, err := store.readFile(path)
	var cur D
	switch {
	case err == nil:
		info, statErr := os.Stat(path)
		if statErr != nil {
			info = nil
		}
		if sl.fileErr == nil && sl.info != nil && sl.doc.meta().token == hashToken(old) {
			cur, sl.info = sl.doc, info
		} else {
			doc, problem := sl.decode(old)
			if problem != nil {
				sl.setError(store, problem, info)
				return "", unreadable(sl.name)
			}
			doc.meta().token = hashToken(old)
			sl.setOK(store, doc, info)
			cur = doc
		}
	case errors.Is(err, fs.ErrNotExist):
		old = nil
		backup := store.backupState(sl.name)
		if backup.present() {
			sl.setMissing(store, backup)
			sl.missingCached, sl.missKey = true, backup.key()
			return "", unreadable(sl.name)
		}
		cur = sl.emptyDoc()
		sl.setOK(store, cur, nil)
		sl.missingCached, sl.missKey = true, backup.key()
	default:
		// 与 load 一样带上 stat（拿得到时），WARN 去重键对同一文件状态保持不变
		info, statErr := os.Stat(path)
		if statErr != nil {
			info = nil
		}
		sl.setError(store, readProblem(err), info)
		return "", unreadable(sl.name)
	}
	next, changed, err := apply(cur, store.revisionLocked())
	if err != nil {
		return "", err
	}
	if !changed {
		return store.revisionLocked(), nil
	}
	writeID := store.newHexID()
	data := sl.encode(next, writeID, store.now())
	if old != nil && snapshot {
		if snapshotPath, err := store.writeSnapshot(sl.name, old, snapshotID(cur.meta().writeID, old)); err != nil {
			return "", store.writeFailed(sl.label, snapshotPath, err)
		}
	}
	if old != nil {
		if err := atomicWrite(path+".bak", old, 0o600); err != nil {
			return "", store.writeFailed(sl.label, path+".bak", err)
		}
	}
	if err := atomicWrite(path, data, 0o600); err != nil {
		return "", store.writeFailed(sl.label, path, err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		info = nil
	}
	next.meta().writeID, next.meta().token = writeID, hashToken(data)
	sl.setOK(store, next, info)
	return store.revisionLocked(), nil
}
