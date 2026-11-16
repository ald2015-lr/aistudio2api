package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 获取 AI Studio API Key：管理页按账户（单个或批量选中）手动触发，绝不在启动时或后台自动运行。
// 每个账户用已保存的登录态启动一次无头隔离浏览器（与“验证”相同：先取得账户的独占租约，正在处理的请求结束后才开始，
// 期间这个账户不接新请求），打开 API Keys 页面，从页面自己的响应里读取 Key，页面上没有 Key 时经页面自己的创建按钮创建一个
// （没有 Google Cloud 项目时先经页面新建一个项目，见 camoufoxnative.FetchStudioKey）。账户逐个处理，打开过浏览器的账户之后等
// STUDIO_KEY_DELAY 秒（每次等待前重新读取配置，保存后立即生效）；未登录、停用、组织停用、不可用、正在启停收尾的账户直接跳过、不等待。
//
// 需要验证账号（403 verify-account）而整户暂停的账户照常获取：这种账户的 API Key 页面通常仍可用。获取只取得管理操作的独占租约
// （AcquireAccount 不看调度状态，暂停中也能取得），不清 403 记录与冷却、不改账户状态：结束后账户仍暂停调度，直到验证通过等原有路径解除。
//
// Key 保存在账户目录的 studioKeyFileName（0600，账户删除时随目录删除，重启后仍在）。Key 是机密：不写运行日志、错误与列表接口，
// 列表只给末四位，完整 Key 只经 reveal 接口返回（管理页的复制按钮）。
//
// 补项目信息（fill）：只给保存着 Key 的账户排队，只读打开页面找这条已保存的 Key（完整相等）读它的项目名、项目编号与项目 ID，
// 从不点“Create API key”、不新建项目；页面上没有这条 Key 时记为失败（Key 保留）。同一账户既按普通方式又按补项目信息排队时按普通方式。
// 读到的是与记录里同一条 Key、这次却没读到项目信息时沿用记录里的项目字段，不会清空。
//
// 清除记录（ClearStudioKeys）：没有 Key 的记录删掉文件，保存着 Key 的只清掉失败或跳过的信息（状态回到成功，Key、项目与时间不动），
// 从不删除 Key；正在处理的账户不动。清除与写记录互斥（studioKeyJob.files），删之前在锁内重新读一次文件，有 Key 就只清信息。

const studioKeyFileName = "studio-key.json"

// studioKey 记录的结论
const (
	studioKeyOK      = "ok"
	studioKeyNone    = "none"
	studioKeyFailed  = "failed"
	studioKeySkipped = "skipped"
)

// studioKeyFile 为 studioKeyFileName 的内容
type studioKeyFile struct {
	Version        int        `json:"version"`
	Key            string     `json:"key,omitempty"`
	ProjectName    string     `json:"project_name,omitempty"`
	ProjectNumber  string     `json:"project_number,omitempty"`
	ProjectDisplay string     `json:"project_display,omitempty"`
	ProjectID      string     `json:"project_id,omitempty"`
	Created        bool       `json:"created,omitempty"`
	KeyCount       int        `json:"key_count,omitempty"`
	FetchedAt      *time.Time `json:"fetched_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	// ProjectCreated 表示这个 Key 所在的项目是获取时经页面新建的
	ProjectCreated bool       `json:"project_created,omitempty"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	AttemptAt      *time.Time `json:"attempt_at,omitempty"`
}

// readStudioKeyFile 读取账户目录里的记录；没有文件时返回 false
func readStudioKeyFile(directory string) (studioKeyFile, bool, error) {
	data, err := os.ReadFile(filepath.Join(directory, studioKeyFileName))
	if errors.Is(err, os.ErrNotExist) {
		return studioKeyFile{}, false, nil
	}
	if err != nil {
		return studioKeyFile{}, false, err
	}
	var record studioKeyFile
	if err := json.Unmarshal(data, &record); err != nil {
		// 错误文案不带文件内容
		return studioKeyFile{}, false, errors.New("解析 " + studioKeyFileName + " 失败")
	}
	return record, true, nil
}

// writeStudioKeyFile 以 0600 原子写入记录
func writeStudioKeyFile(directory string, record studioKeyFile) error {
	record.Version = 1
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(directory, studioKeyFileName), append(data, '\n'), 0o600)
}

// studioKeyView 把记录转成列表项（不含完整 Key）
func studioKeyView(accountID string, label string, record studioKeyFile) api.StudioKeyRecord {
	view := api.StudioKeyRecord{
		AccountID: accountID, Label: label, Status: record.Status, Reason: camoufoxnative.RedactStudioKeys(record.Reason),
		HasKey: record.Key != "", ProjectName: record.ProjectName, ProjectNumber: record.ProjectNumber,
		ProjectDisplay: record.ProjectDisplay, ProjectID: record.ProjectID, Created: record.Created, ProjectCreated: record.ProjectCreated,
		KeyCount: record.KeyCount, FetchedAt: record.FetchedAt, CreatedAt: record.CreatedAt, AttemptAt: record.AttemptAt,
	}
	if record.Key != "" {
		view.Last4 = camoufoxnative.StudioKeyLast4(record.Key)
	}
	return view
}

// studioKeyJob 为获取 Key 的队列（进程级，挂在 runtimeManager 上，跨生成服务重建保留）
type studioKeyJob struct {
	mu    sync.Mutex
	queue []string
	// fill 为排队中按补项目信息处理的账户（不在里面的按普通方式）；currentFill 表示正在处理的账户是补项目信息
	fill        map[string]bool
	current     string
	currentFill bool
	running     bool
	nextAt      time.Time
	// files 让写记录与清除记录互斥：清除在锁内重新读文件再决定删还是只清信息，不会删掉刚写进去的 Key
	files sync.Mutex
	// cache 为读过的记录（按账户，连同读取时的账户目录）：管理页每几秒读一次列表，账户上万时不必每次读上万个文件；
	// 写入时更新缓存；账户目录变了，或文件的修改时间、大小变了（进程外替换或删除）时重新读取
	cache map[string]studioKeyCached
}

type studioKeyCached struct {
	directory string
	stamp     studioKeyStamp
	record    studioKeyFile
	ok        bool
}

// load 读取账户的记录（先看缓存）；读取出错不缓存。缓存记的是读内容之前取得的 stamp：读内容之后文件又被清除或写入时，
// 下次的 stamp 对不上，重新读文件（不会把读到的旧内容配上新文件的 stamp 一直留在缓存里）
func (job *studioKeyJob) load(accountID string, directory string) (studioKeyFile, bool, error) {
	job.mu.Lock()
	cached, hit := job.cache[accountID]
	job.mu.Unlock()
	stamp := statStudioKeyFile(directory)
	if hit && cached.directory == directory && cached.stamp == stamp {
		return cached.record, cached.ok, nil
	}
	record, ok, err := readStudioKeyFile(directory)
	if err != nil {
		return studioKeyFile{}, false, err
	}
	job.remember(accountID, directory, stamp, record, ok)
	return record, ok, nil
}

// forget 让账户的缓存失效（下次读列表时重新读文件）
func (job *studioKeyJob) forget(accountID string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	delete(job.cache, accountID)
}

// remember 缓存账户的记录；stamp 须是读到（或写入）这份内容时文件的 stamp：读文件时为读之前取得的，写文件时为在 files 锁内写完之后取得的
func (job *studioKeyJob) remember(accountID string, directory string, stamp studioKeyStamp, record studioKeyFile, ok bool) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.cache == nil {
		job.cache = make(map[string]studioKeyCached)
	}
	job.cache[accountID] = studioKeyCached{directory: directory, stamp: stamp, record: record, ok: ok}
}

// studioKeyStamp 为记录文件的修改时间与大小：文件在进程外被替换或删除时缓存随之失效（一次 stat，不读内容）
type studioKeyStamp struct {
	exists bool
	mtime  int64
	size   int64
}

func statStudioKeyFile(directory string) studioKeyStamp {
	info, err := os.Stat(filepath.Join(directory, studioKeyFileName))
	if err != nil {
		return studioKeyStamp{}
	}
	return studioKeyStamp{exists: true, mtime: info.ModTime().UnixNano(), size: info.Size()}
}

// studioKeySnapshot 为队列状态
type studioKeySnapshot struct {
	running     bool
	current     string
	currentFill bool
	queue       []string
	nextAt      *time.Time
}

// snapshot 返回队列状态
func (job *studioKeyJob) snapshot() studioKeySnapshot {
	job.mu.Lock()
	defer job.mu.Unlock()
	snapshot := studioKeySnapshot{running: job.running, current: job.current, currentFill: job.currentFill, queue: slices.Clone(job.queue)}
	if snapshot.queue == nil {
		snapshot.queue = []string{}
	}
	if !job.nextAt.IsZero() {
		at := job.nextAt
		snapshot.nextAt = &at
	}
	return snapshot
}

// enqueue 把账户加入队列（fill 为补项目信息），返回是否需要启动处理协程。已在队列里的不重复加入：普通方式覆盖补项目信息
// （同一账户两种都排了时按普通方式）；正在处理的不重复加入，只有它在补项目信息、这次是普通方式时再排一次
func (job *studioKeyJob) enqueue(accountIDs []string, fill bool) bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.fill == nil {
		job.fill = make(map[string]bool)
	}
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		switch {
		case id == "":
		case slices.Contains(job.queue, id):
			if !fill {
				delete(job.fill, id)
			}
		case id == job.current && (fill || !job.currentFill):
		default:
			job.queue = append(job.queue, id)
			if fill {
				job.fill[id] = true
			}
		}
	}
	if job.running || len(job.queue) == 0 {
		return false
	}
	job.running = true
	return true
}

// next 取出下一个账户与它是否补项目信息；队列空时结束处理
func (job *studioKeyJob) next() (string, bool, bool) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.nextAt = time.Time{}
	if len(job.queue) == 0 {
		job.running = false
		job.current = ""
		job.currentFill = false
		return "", false, false
	}
	id := job.queue[0]
	job.queue = job.queue[1:]
	job.current = id
	job.currentFill = job.fill[id]
	delete(job.fill, id)
	return id, job.currentFill, true
}

func (job *studioKeyJob) waiting(at time.Time) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.current = ""
	job.currentFill = false
	job.nextAt = at
}

// clear 清空排队中的账户（正在处理的不受影响）；间隔等待中时处理协程醒来后发现队列空就结束
func (job *studioKeyJob) clear() {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.queue = nil
	job.fill = nil
}

// processing 返回正在处理的账户
func (job *studioKeyJob) processing() string {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.current
}

// write 写入账户的记录并更新缓存（与清除互斥）
func (job *studioKeyJob) write(accountID string, directory string, record studioKeyFile) error {
	job.files.Lock()
	defer job.files.Unlock()
	if err := writeStudioKeyFile(directory, record); err != nil {
		job.forget(accountID)
		return err
	}
	job.remember(accountID, directory, statStudioKeyFile(directory), record, true)
	return nil
}

func (job *studioKeyJob) pending() bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	return len(job.queue) > 0
}

// studioKeyDelay 读取当前保存的 STUDIO_KEY_DELAY（每次等待前读取，保存配置后立即生效）；读取失败时用当前生效配置
func (manager *runtimeManager) studioKeyDelay() time.Duration {
	seconds := config.Default().StudioKeyDelaySeconds
	if generation := manager.generation(); generation != nil {
		seconds = generation.config.StudioKeyDelaySeconds
	}
	if manager.configPath != "" {
		if cfg, err := config.Load(manager.configPath); err == nil {
			seconds = cfg.StudioKeyDelaySeconds
		}
	}
	return time.Duration(max(seconds, 0)) * time.Second
}

// studioKeyAdmin 返回当前生成服务的管理实现
func (manager *runtimeManager) studioKeyAdmin() (*runtimeAdmin, error) {
	generation := manager.generation()
	if generation == nil {
		return nil, errors.New("服务未就绪")
	}
	admin, ok := generation.admin.(*runtimeAdmin)
	if !ok {
		return nil, errors.New("当前服务不支持获取 AI Studio API Key")
	}
	return admin, nil
}

// StudioKeys 返回获取任务状态与全部账户的记录（不含完整 Key）
func (manager *runtimeManager) StudioKeys(ctx context.Context) (api.StudioKeyStatus, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyStatus{}, err
	}
	snapshot := manager.studioKeys.snapshot()
	status := api.StudioKeyStatus{
		DelaySeconds: int(manager.studioKeyDelay() / time.Second),
		Running:      snapshot.running, Current: snapshot.current, CurrentFill: snapshot.currentFill, Queue: snapshot.queue, NextAt: snapshot.nextAt,
		Records: []api.StudioKeyRecord{},
	}
	for _, id := range admin.pool.AccountIDs() {
		if ctx.Err() != nil {
			return api.StudioKeyStatus{}, ctx.Err()
		}
		account, err := admin.pool.Account(id)
		if err != nil || account == nil {
			continue
		}
		record, ok, err := manager.studioKeys.load(id, account.Directory)
		if err != nil {
			status.Records = append(status.Records, api.StudioKeyRecord{
				AccountID: id, Label: account.Config.Label, Status: studioKeyFailed, Reason: err.Error(),
			})
			continue
		}
		if ok {
			status.Records = append(status.Records, studioKeyView(id, account.Config.Label, record))
		}
	}
	return status, nil
}

// FetchStudioKeys 把账户加入获取队列（逐个处理；FillProject、MissingProject 时按补项目信息排队），返回最新状态
func (manager *runtimeManager) FetchStudioKeys(ctx context.Context, input api.StudioKeyFetchInput) (api.StudioKeyStatus, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyStatus{}, err
	}
	fill := input.FillProject || input.MissingProject
	ids := input.AccountIDs
	if input.All {
		ids = admin.pool.AccountIDs()
	}
	var accepted []string
	known := 0
	for _, id := range ids {
		if _, ok := admin.pool.StatusOf(id); !ok {
			continue
		}
		known++
		if input.MissingOnly || fill {
			var record studioKeyFile
			hasRecord := false
			if account, err := admin.pool.Account(id); err == nil && account != nil {
				if loaded, ok, err := manager.studioKeys.load(id, account.Directory); err == nil && ok {
					record, hasRecord = loaded, true
				}
			}
			hasKey := hasRecord && record.Key != ""
			switch {
			case input.MissingOnly && hasKey:
				continue
			case fill && !hasKey:
				// 补项目信息只给保存着 Key 的账户
				continue
			case input.MissingProject && record.ProjectNumber != "":
				continue
			}
		}
		accepted = append(accepted, id)
	}
	if len(accepted) == 0 && (input.All || fill && known > 0) {
		// 全部账户都已经有 Key、没有缺项目信息的 Key（或没有账户）：不算错误，返回现状
		return manager.StudioKeys(ctx)
	}
	if len(accepted) == 0 {
		return api.StudioKeyStatus{}, accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, strings.Join(input.AccountIDs, ",")))
	}
	if manager.studioKeys.enqueue(accepted, fill) {
		lifecycle := manager.lifecycle
		if lifecycle == nil {
			lifecycle = context.Background()
		}
		go manager.runStudioKeys(lifecycle)
	}
	return manager.StudioKeys(ctx)
}

// RevealStudioKey 返回账户保存的完整 Key（只给管理页复制按钮用）
func (manager *runtimeManager) RevealStudioKey(_ context.Context, accountID string) (api.StudioKeySecret, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeySecret{}, err
	}
	account, err := admin.pool.Account(accountID)
	if err != nil || account == nil {
		return api.StudioKeySecret{}, accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, accountID))
	}
	record, ok, err := readStudioKeyFile(account.Directory)
	if err != nil {
		return api.StudioKeySecret{}, err
	}
	if !ok || record.Key == "" {
		return api.StudioKeySecret{}, &adminOperationError{status: http.StatusNotFound, code: "studio_key_not_found", message: "账户还没有获取到 API Key"}
	}
	return studioKeySecret(accountID, record), nil
}

// studioKeySecret 为 reveal 与 export 返回的完整 Key 与项目
func studioKeySecret(accountID string, record studioKeyFile) api.StudioKeySecret {
	return api.StudioKeySecret{
		AccountID: accountID, Key: record.Key, ProjectName: record.ProjectName, ProjectNumber: record.ProjectNumber,
		ProjectDisplay: record.ProjectDisplay, ProjectID: record.ProjectID,
	}
}

// StopStudioKeys 清空排队中的账户；正在处理的那个照常做完（中途打断浏览器可能留下建了一半的 Key）
func (manager *runtimeManager) StopStudioKeys(ctx context.Context) (api.StudioKeyStatus, error) {
	if _, err := manager.studioKeyAdmin(); err != nil {
		return api.StudioKeyStatus{}, err
	}
	manager.studioKeys.clear()
	return manager.StudioKeys(ctx)
}

// ExportStudioKeys 返回全部保存着的完整 Key（按账户列表顺序，只给“复制全部 Key”用）
func (manager *runtimeManager) ExportStudioKeys(ctx context.Context) (api.StudioKeyExport, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyExport{}, err
	}
	export := api.StudioKeyExport{Keys: []api.StudioKeySecret{}}
	for _, id := range admin.pool.AccountIDs() {
		if ctx.Err() != nil {
			return api.StudioKeyExport{}, ctx.Err()
		}
		account, err := admin.pool.Account(id)
		if err != nil || account == nil {
			continue
		}
		record, ok, err := manager.studioKeys.load(id, account.Directory)
		if err != nil || !ok || record.Key == "" {
			continue
		}
		export.Keys = append(export.Keys, studioKeySecret(id, record))
	}
	return export, nil
}

// ClearStudioKeys 清除账户的记录：没有 Key 的删掉记录文件；保存着 Key 的只清掉失败或跳过的信息（状态回到成功、原因去掉、最近尝试
// 时间回到读到 Key 的时间），Key、项目、读取与创建时间都不动；正在处理的账户不动（排队中的照清）；不认识的账户只计数。
// 与写记录互斥，每个文件在锁内重新读一次再决定，从不删除保存着 Key 的文件。运行日志只记计数
func (manager *runtimeManager) ClearStudioKeys(ctx context.Context, input api.StudioKeyClearInput) (api.StudioKeyClearResult, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyClearResult{}, err
	}
	var counts api.StudioKeyClearCounts
	job := &manager.studioKeys
	job.files.Lock()
	for _, id := range input.AccountIDs {
		account, err := admin.pool.Account(id)
		if err != nil || account == nil {
			counts.Unknown++
			continue
		}
		if id == job.processing() {
			counts.Busy++
			continue
		}
		// 在锁内重新读文件（不看缓存），清除后把新状态记进缓存（stamp 取自锁内，读写都在这之前）
		directory := account.Directory
		stamp := statStudioKeyFile(directory)
		record, ok, err := readStudioKeyFile(directory)
		switch {
		case err != nil:
			job.forget(id)
			counts.Errors++
		case !ok:
			job.remember(id, directory, stamp, studioKeyFile{}, false)
			counts.Unchanged++
		case record.Key == "":
			if err := os.Remove(filepath.Join(directory, studioKeyFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
				job.forget(id)
				counts.Errors++
				continue
			}
			job.remember(id, directory, statStudioKeyFile(directory), studioKeyFile{}, false)
			counts.Deleted++
		case record.Status == studioKeyOK && record.Reason == "":
			job.remember(id, directory, stamp, record, true)
			counts.Unchanged++
		default:
			record.Status, record.Reason, record.AttemptAt = studioKeyOK, "", record.FetchedAt
			if err := writeStudioKeyFile(directory, record); err != nil {
				job.forget(id)
				counts.Errors++
				continue
			}
			job.remember(id, directory, statStudioKeyFile(directory), record, true)
			counts.Reset++
		}
	}
	job.files.Unlock()
	admin.requests.log("auth", "INFO", fmt.Sprintf("AI Studio API Key | 清除记录 | 删除=%d | 清掉失败信息=%d | 正在处理没动=%d | 未知账户=%d | 出错=%d",
		counts.Deleted, counts.Reset, counts.Busy, counts.Unknown, counts.Errors))
	status, err := manager.StudioKeys(ctx)
	if err != nil {
		return api.StudioKeyClearResult{}, err
	}
	return api.StudioKeyClearResult{StudioKeyStatus: status, Cleared: counts}, nil
}

// runStudioKeys 逐个处理队列；打开过浏览器的账户之后等 STUDIO_KEY_DELAY
func (manager *runtimeManager) runStudioKeys(ctx context.Context) {
	for {
		id, fill, ok := manager.studioKeys.next()
		if !ok {
			return
		}
		opened := manager.fetchStudioKey(ctx, id, fill)
		if ctx.Err() != nil {
			// 进程退出：清空队列
			for {
				if _, _, more := manager.studioKeys.next(); !more {
					return
				}
			}
		}
		if !opened || !manager.studioKeys.pending() {
			continue
		}
		delay := manager.studioKeyDelay()
		if delay <= 0 {
			continue
		}
		deadline := time.Now().Add(delay)
		manager.studioKeys.waiting(deadline)
		// 每秒看一次队列：点了“停止”清空队列后不必等满间隔
		for ctx.Err() == nil && manager.studioKeys.pending() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			timer := time.NewTimer(min(remaining, time.Second))
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		}
	}
}

// studioKeySkipReason 返回不处理这个账户的原因（未登录、停用、组织停用、不可用、正在启停收尾）；可以处理时为空。
// 需要验证账号而整户暂停的账户不跳过（暂停只是一条全局冷却，账户状态显示为冷却，这里不看冷却）
func studioKeySkipReason(status aistudio.AccountStatus) string {
	switch {
	case !status.Enabled || status.State == aistudio.AccountDisabled:
		return "账户已停用"
	case status.State == aistudio.AccountAuthRequired:
		return "账户需要登录"
	case status.OrgDisabled || status.State == aistudio.AccountOrgDisabled:
		return "账户所属组织停用了 AI Studio"
	case status.State == aistudio.AccountUnavailable:
		return "账户不可用"
	case status.Transition != "":
		return "账户正在启停收尾（暂停中）"
	}
	return ""
}

// fetchStudioKey 处理一个账户并写回记录（fill 为补项目信息：只读，不创建）；返回是否打开过浏览器（决定之后是否等待间隔）
func (manager *runtimeManager) fetchStudioKey(ctx context.Context, accountID string, fill bool) bool {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return false
	}
	status, ok := admin.pool.StatusOf(accountID)
	if !ok {
		return false
	}
	account, err := admin.pool.Account(accountID)
	if err != nil || account == nil {
		return false
	}
	label := status.Label
	directory := account.Directory
	previous, _, _ := readStudioKeyFile(directory)
	now := time.Now().UTC()
	// 运行日志的开头：获取 API Key、补 API Key 项目信息（各带完成、失败的说法）
	title, done, failed := "获取 API Key", "获取 API Key 完成", "获取 API Key 失败"
	if fill {
		title, done, failed = "补 API Key 项目信息", "补 API Key 项目信息完成", "补 API Key 项目信息失败"
	}
	save := func(record studioKeyFile) {
		record.AttemptAt = &now
		if err := manager.studioKeys.write(accountID, directory, record); err != nil {
			admin.requests.log(label, "WARN", title+" | 保存记录失败 | 错误="+camoufoxnative.RedactStudioKeys(err.Error()))
		}
	}
	if fill && previous.Key == "" {
		// 排队之后记录被清除或 Key 没了：没有可补的，不写记录
		admin.requests.log(label, "INFO", title+" | 跳过 | 原因=没有保存的 Key")
		return false
	}
	if reason := studioKeySkipReason(status); reason != "" {
		previous.Status, previous.Reason = studioKeySkipped, reason
		save(previous)
		admin.requests.log(label, "INFO", title+" | 跳过 | 原因="+reason)
		return false
	}
	driver, ok := admin.login.(aistudio.StudioKeyDriver)
	if !ok {
		previous.Status, previous.Reason = studioKeyFailed, "当前登录驱动不支持获取 API Key"
		save(previous)
		return false
	}
	options := camoufoxnative.StudioKeyOptions{ReadOnly: fill, SavedKey: previous.Key}
	startedAt := time.Now()
	admin.requests.log(label, "INFO", title+" | 打开 API Keys 页面")
	result, err := admin.fetchStudioKeyWithLease(ctx, accountID, driver, options)
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	if err == nil && fill && result.Key != previous.Key {
		// 只读时驱动只认已保存的 Key；万一不是同一条也不改记录里的 Key
		err = &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonSavedMissing}
	}
	if err != nil {
		reason := studioKeyFailureReason(err)
		status := studioKeyFailed
		if !fill && camoufoxnative.ErrStudioKeyNone(err) {
			status = studioKeyNone
		}
		previous.Status, previous.Reason = status, reason
		save(previous)
		admin.requests.log(label, "WARN", fmt.Sprintf("%s | 耗时=%s | 原因=%s", failed, elapsed, reason))
		return true
	}
	record := studioKeyFile{
		Key: result.Key, ProjectName: result.ProjectName, ProjectNumber: result.ProjectNumber,
		ProjectDisplay: result.ProjectDisplay, ProjectID: result.ProjectID, Created: result.Created, ProjectCreated: result.ProjectCreated,
		KeyCount: result.KeyCount, FetchedAt: &now, Status: studioKeyOK,
	}
	switch {
	case result.Created:
		record.CreatedAt = &now
	case previous.Key == result.Key:
		// 同一个 Key：保留当初创建的时间与标记；这次没读到的项目信息沿用记录里的（同一条 Key 不会换项目，不清空）
		record.CreatedAt, record.Created, record.ProjectCreated = previous.CreatedAt, previous.Created, previous.ProjectCreated
		keepStudioKeyProject(&record, previous)
	}
	save(record)
	action := "读取"
	switch {
	case fill:
		action = "只读"
	case result.ProjectCreated:
		action = "新建项目并新建 Key"
	case result.Created:
		action = "新建"
	}
	message := fmt.Sprintf(
		"%s | %s | 末四位=%s | 项目=%s | 项目编号=%s | 显示名=%s | 项目 ID=%s | 耗时=%s",
		done, action, camoufoxnative.StudioKeyLast4(result.Key), orDash(record.ProjectName), orDash(record.ProjectNumber),
		orDash(record.ProjectDisplay), orDash(record.ProjectID), elapsed,
	)
	if len(result.Steps) > 0 {
		message += " | 步骤=" + studioKeyStepsText(result.Steps)
	}
	admin.requests.log(label, "INFO", message)
	return true
}

// keepStudioKeyProject 同一条 Key 这次没读到的项目字段沿用上一次的记录：编号缺时整组沿用（项目名、编号），显示名与项目 ID 各自缺时沿用；
// 这次读到的编号与上次不同时不混用上次的显示名与项目 ID
func keepStudioKeyProject(record *studioKeyFile, previous studioKeyFile) {
	if record.ProjectNumber == "" && previous.ProjectNumber != "" {
		record.ProjectName, record.ProjectNumber = previous.ProjectName, previous.ProjectNumber
	}
	if record.ProjectNumber != "" && previous.ProjectNumber != "" && record.ProjectNumber != previous.ProjectNumber {
		return
	}
	if record.ProjectDisplay == "" {
		record.ProjectDisplay = previous.ProjectDisplay
	}
	if record.ProjectID == "" {
		record.ProjectID = previous.ProjectID
	}
}

// studioKeyStepsText 为写进运行日志的创建步骤（打码、单行、截短）
func studioKeyStepsText(steps []string) string {
	text := camoufoxnative.RedactStudioKeys(strings.Join(steps, "→"))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 300 {
		text = string(runes[:299]) + "…"
	}
	return text
}

// fetchStudioKeyWithLease 在账户独占租约内获取 Key（与 verifyLogin 一样：等进行中的请求结束，期间账户不接新请求），
// 浏览器同样占一个 IP 池出口
func (admin *runtimeAdmin) fetchStudioKeyWithLease(
	ctx context.Context, accountID string, driver aistudio.StudioKeyDriver, options camoufoxnative.StudioKeyOptions,
) (camoufoxnative.StudioKeyResult, error) {
	lease, err := admin.acquireAccountForOperation(ctx, accountID)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	defer func() { _ = lease.Release() }()
	account := lease.Account()
	proxy, poolLease := admin.browserProxy(account.ID, account.ID, proxyPoolKindVerify, account.Config.Label, account.Config.Proxy)
	defer poolLease.release()
	request := admin.loginRequest(account, account.Directory, proxy)
	request.OnboardingCompleted = admin.onboardingLogger(account.Config.Label, "获取 API Key")
	return driver.FetchStudioKey(ctx, request, account.StorageState, options)
}

// studioKeyFailureReason 返回写进记录与日志的失败原因（打码、单行、截短；创建流程的失败带步骤与对话框摘要，上限放宽到 600 字）
func studioKeyFailureReason(err error) string {
	var keyErr *camoufoxnative.StudioKeyError
	text := err.Error()
	if errors.As(err, &keyErr) {
		text = keyErr.Error()
	}
	text = camoufoxnative.StripPageSnapshots(camoufoxnative.RedactStudioKeys(text))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 600 {
		text = string(runes[:599]) + "…"
	}
	return text
}

// FetchStudioKey 使用按需准备的 Camoufox 获取 API Key（options.ReadOnly 时只读）
func (lazy *lazyLoginDriver) FetchStudioKey(
	ctx context.Context, request aistudio.IsolatedLoginRequest, state aistudio.StorageState, options camoufoxnative.StudioKeyOptions,
) (camoufoxnative.StudioKeyResult, error) {
	driver, err := lazy.resolve(ctx)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	return driver.FetchStudioKey(ctx, request, state, options)
}

var _ api.StudioKeyService = (*runtimeManager)(nil)
