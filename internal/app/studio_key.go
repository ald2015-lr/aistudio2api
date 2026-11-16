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
// （见 camoufoxnative.FetchStudioKey）。账户逐个处理，打开过浏览器的账户之后等 STUDIO_KEY_DELAY 秒（每次等待前重新读取配置，
// 保存后立即生效）；未登录、停用、组织停用、正在启停收尾的账户直接跳过、不等待。
//
// Key 保存在账户目录的 studioKeyFileName（0600，账户删除时随目录删除，重启后仍在）。Key 是机密：不写运行日志、错误与列表接口，
// 列表只给末四位，完整 Key 只经 reveal 接口返回（管理页的复制按钮）。

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
	Created        bool       `json:"created,omitempty"`
	KeyCount       int        `json:"key_count,omitempty"`
	FetchedAt      *time.Time `json:"fetched_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
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
		ProjectDisplay: record.ProjectDisplay, Created: record.Created, KeyCount: record.KeyCount,
		FetchedAt: record.FetchedAt, CreatedAt: record.CreatedAt, AttemptAt: record.AttemptAt,
	}
	if record.Key != "" {
		view.Last4 = camoufoxnative.StudioKeyLast4(record.Key)
	}
	return view
}

// studioKeyJob 为获取 Key 的队列（进程级，挂在 runtimeManager 上，跨生成服务重建保留）
type studioKeyJob struct {
	mu      sync.Mutex
	queue   []string
	current string
	running bool
	nextAt  time.Time
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

// load 读取账户的记录（先看缓存）；读取出错不缓存
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
	job.remember(accountID, directory, record, ok)
	return record, ok, nil
}

func (job *studioKeyJob) remember(accountID string, directory string, record studioKeyFile, ok bool) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.cache == nil {
		job.cache = make(map[string]studioKeyCached)
	}
	job.cache[accountID] = studioKeyCached{directory: directory, stamp: statStudioKeyFile(directory), record: record, ok: ok}
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

// snapshot 返回队列状态
func (job *studioKeyJob) snapshot() (running bool, current string, queue []string, nextAt *time.Time) {
	job.mu.Lock()
	defer job.mu.Unlock()
	queue = slices.Clone(job.queue)
	if queue == nil {
		queue = []string{}
	}
	if !job.nextAt.IsZero() {
		at := job.nextAt
		nextAt = &at
	}
	return job.running, job.current, queue, nextAt
}

// enqueue 把账户加入队列（已在队列里或正在处理的不重复加入），返回是否需要启动处理协程
func (job *studioKeyJob) enqueue(accountIDs []string) bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" || id == job.current || slices.Contains(job.queue, id) {
			continue
		}
		job.queue = append(job.queue, id)
	}
	if job.running || len(job.queue) == 0 {
		return false
	}
	job.running = true
	return true
}

// next 取出下一个账户；队列空时结束处理
func (job *studioKeyJob) next() (string, bool) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.nextAt = time.Time{}
	if len(job.queue) == 0 {
		job.running = false
		job.current = ""
		return "", false
	}
	id := job.queue[0]
	job.queue = job.queue[1:]
	job.current = id
	return id, true
}

func (job *studioKeyJob) waiting(at time.Time) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.current = ""
	job.nextAt = at
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
	running, current, queue, nextAt := manager.studioKeys.snapshot()
	status := api.StudioKeyStatus{
		DelaySeconds: int(manager.studioKeyDelay() / time.Second),
		Running:      running, Current: current, Queue: queue, NextAt: nextAt,
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

// FetchStudioKeys 把账户加入获取队列（逐个处理），返回最新状态
func (manager *runtimeManager) FetchStudioKeys(ctx context.Context, input api.StudioKeyFetchInput) (api.StudioKeyStatus, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyStatus{}, err
	}
	var accepted []string
	for _, id := range input.AccountIDs {
		if _, ok := admin.pool.StatusOf(id); ok {
			accepted = append(accepted, id)
		}
	}
	if len(accepted) == 0 {
		return api.StudioKeyStatus{}, accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, strings.Join(input.AccountIDs, ",")))
	}
	if manager.studioKeys.enqueue(accepted) {
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
	return api.StudioKeySecret{
		AccountID: accountID, Key: record.Key, ProjectName: record.ProjectName, ProjectNumber: record.ProjectNumber,
	}, nil
}

// runStudioKeys 逐个处理队列；打开过浏览器的账户之后等 STUDIO_KEY_DELAY
func (manager *runtimeManager) runStudioKeys(ctx context.Context) {
	for {
		id, ok := manager.studioKeys.next()
		if !ok {
			return
		}
		opened := manager.fetchStudioKey(ctx, id)
		if ctx.Err() != nil {
			// 进程退出：清空队列
			for {
				if _, more := manager.studioKeys.next(); !more {
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
		manager.studioKeys.waiting(time.Now().Add(delay))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// studioKeySkipReason 返回不处理这个账户的原因（未登录、停用、组织停用、不可用、需要验证账号而整户暂停、正在启停收尾）；可以处理时为空
func studioKeySkipReason(status aistudio.AccountStatus, verifyPaused bool) string {
	switch {
	case !status.Enabled || status.State == aistudio.AccountDisabled:
		return "账户已停用"
	case status.State == aistudio.AccountAuthRequired:
		return "账户需要登录"
	case status.OrgDisabled || status.State == aistudio.AccountOrgDisabled:
		return "账户所属组织停用了 AI Studio"
	case status.State == aistudio.AccountUnavailable:
		return "账户不可用"
	case verifyPaused:
		return "账户需要验证账号，整户暂停中"
	case status.Transition != "":
		return "账户正在启停收尾（暂停中）"
	}
	return ""
}

// fetchStudioKey 处理一个账户并写回记录；返回是否打开过浏览器（决定之后是否等待间隔）
func (manager *runtimeManager) fetchStudioKey(ctx context.Context, accountID string) bool {
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
	save := func(record studioKeyFile) {
		record.AttemptAt = &now
		err := writeStudioKeyFile(directory, record)
		if err == nil {
			manager.studioKeys.remember(accountID, directory, record, true)
		} else {
			admin.requests.log(label, "WARN", "获取 API Key | 保存记录失败 | 错误="+camoufoxnative.RedactStudioKeys(err.Error()))
		}
	}
	if reason := studioKeySkipReason(status, admin.pool.VerifyRequiredPaused(accountID)); reason != "" {
		previous.Status, previous.Reason = studioKeySkipped, reason
		save(previous)
		admin.requests.log(label, "INFO", "获取 API Key | 跳过 | 原因="+reason)
		return false
	}
	driver, ok := admin.login.(aistudio.StudioKeyDriver)
	if !ok {
		previous.Status, previous.Reason = studioKeyFailed, "当前登录驱动不支持获取 API Key"
		save(previous)
		return false
	}
	startedAt := time.Now()
	admin.requests.log(label, "INFO", "获取 API Key | 打开 API Keys 页面")
	result, err := admin.fetchStudioKeyWithLease(ctx, accountID, driver)
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	if err != nil {
		reason := studioKeyFailureReason(err)
		status := studioKeyFailed
		if camoufoxnative.ErrStudioKeyNone(err) {
			status = studioKeyNone
		}
		previous.Status, previous.Reason = status, reason
		save(previous)
		admin.requests.log(label, "WARN", fmt.Sprintf("获取 API Key 失败 | 耗时=%s | 原因=%s", elapsed, reason))
		return true
	}
	record := studioKeyFile{
		Key: result.Key, ProjectName: result.ProjectName, ProjectNumber: result.ProjectNumber,
		ProjectDisplay: result.ProjectDisplay, Created: result.Created, KeyCount: result.KeyCount,
		FetchedAt: &now, Status: studioKeyOK,
	}
	switch {
	case result.Created:
		record.CreatedAt = &now
	case previous.Key == result.Key:
		// 同一个 Key：保留当初创建的时间与标记
		record.CreatedAt, record.Created = previous.CreatedAt, previous.Created
	}
	save(record)
	action := "读取"
	if result.Created {
		action = "新建"
	}
	admin.requests.log(label, "INFO", fmt.Sprintf(
		"获取 API Key 完成 | %s | 末四位=%s | 项目=%s | 项目编号=%s | 耗时=%s",
		action, camoufoxnative.StudioKeyLast4(result.Key), orDash(result.ProjectName), orDash(result.ProjectNumber), elapsed,
	))
	return true
}

// fetchStudioKeyWithLease 在账户独占租约内获取 Key（与 verifyLogin 一样：等进行中的请求结束，期间账户不接新请求），
// 浏览器同样占一个 IP 池出口
func (admin *runtimeAdmin) fetchStudioKeyWithLease(
	ctx context.Context, accountID string, driver aistudio.StudioKeyDriver,
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
	return driver.FetchStudioKey(ctx, request, account.StorageState)
}

// studioKeyFailureReason 返回写进记录与日志的失败原因（打码、单行、截短）
func studioKeyFailureReason(err error) string {
	var keyErr *camoufoxnative.StudioKeyError
	text := err.Error()
	if errors.As(err, &keyErr) {
		text = keyErr.Error()
	}
	text = camoufoxnative.StripPageSnapshots(camoufoxnative.RedactStudioKeys(text))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 300 {
		text = string(runes[:299]) + "…"
	}
	return text
}

// FetchStudioKey 使用按需准备的 Camoufox 获取 API Key
func (lazy *lazyLoginDriver) FetchStudioKey(
	ctx context.Context, request aistudio.IsolatedLoginRequest, state aistudio.StorageState,
) (camoufoxnative.StudioKeyResult, error) {
	driver, err := lazy.resolve(ctx)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	return driver.FetchStudioKey(ctx, request, state)
}

var _ api.StudioKeyService = (*runtimeManager)(nil)
