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
// 不删除这个账户的 Key；唯一的例外是作废的 Key（见下），它不是这个账户的 Key，连同记录一起删掉。正在处理的账户不动。
// 清除与写记录互斥（studioKeyJob.files），删之前在锁内重新读一次文件再决定。
//
// 作废的 Key：旧版本按文本在页面 HTML 里扫到了页面自己的公共 Key，给每个账户都记成了“成功”。下面两种 Key 都判为作废（见 invalidFor）：
//   - 确定不是这个账户的（voided）：同一条 Key 保存在两个以上账户里，记录上标着 invalid（曾经如此），或是作废过的 Key（指纹对上，见下；
//     浏览器每次运行认出的页面公共 Key 也记成作废指纹，见 camoufoxnative.StudioKeyResult.ClientFingerprints）；
//   - 旧版本保存、没有核对过的：记录的格式版本低于 studioKeyVersion（旧版本不核对页面的 Key 列表）。它也可能确实是这个账户的，所以只是
//     不再使用，不留指纹：重新获取时核对得上照样保存。
//
// 作废的 Key 在列表里不算有 Key（状态 invalid，进“失败”标签），reveal 报错，导出与“复制全部 Key”不含它，“全部获取”（只补没有 Key 的）
// 把它当作没有 Key、重新获取，补项目信息跳过它；重新获取时不把它当作上次的 Key 保留，也不交给驱动当已保存的 Key。获取时其他账户保存着的
// Key 与确定作废的 Key 都交给驱动排除（StudioKeyOptions.Exclude、ExcludeFingerprints）：页面上有别的行时选别的行，都被排除时失败，
// 不会再读回同一条作废的 Key。
//
// 作废要记得住：列表（以及导出、reveal、排队、获取、清除）每次读全部记录时，发现同一条 Key 有两个以上账户保存着、其中有还没标 invalid 的，
// 就给这些记录都写上 invalid 标记（之后删掉其中的账户、只剩一个时也仍作废）。作废的 Key 从记录里去掉（重新获取时、驱动认出是公共 Key 时）
// 或连同记录清除时，在这个账户的记录里留下它的 SHA-256 指纹（void_keys；清除时记录只剩指纹，列表里不显示）；进程里另记一份全部作废过的
// 指纹（studioKeyJob.voided）。获取到的 Key 与别的账户保存的相同、或是作废过的（指纹对上）时不保存，记为失败，并给仍保存着它的账户的记录
// 写上 invalid 标记。浏览器认出的页面公共 Key 的指纹记进这个账户的记录（void_keys）与进程里，保存着它的记录写上 invalid 标记。
// 指纹只用来比对，不写日志。作废是单向的：没有撤销的操作（两个账户确实共用同一条 Key 时也会作废，见 README）。

const studioKeyFileName = "studio-key.json"

// studioKeyVersion 为记录的格式版本：2 起保存的 Key 都与页面的 Key 列表核对过（见 camoufoxnative 的 studio_key_confirm.go）；
// 更早的版本（1）不核对，保存的 Key 按作废处理（studioKeyUnconfirmed）。读文件时没有版本号的按 1；内存里新建的记录版本为 0，
// 写文件时写成 studioKeyVersion
const studioKeyVersion = 2

// studioKey 记录的结论
const (
	studioKeyOK      = "ok"
	studioKeyNone    = "none"
	studioKeyFailed  = "failed"
	studioKeySkipped = "skipped"
	// studioKeyInvalid 只用于列表：记录里的 Key 已作废（见文件开头），不落盘
	studioKeyInvalid = "invalid"
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
	// Invalid 表示这条 Key 已知是多个账户共有的（多半是页面自己的公共 Key），已作废；重新获取到别的 Key 时随新记录去掉
	Invalid bool `json:"invalid,omitempty"`
	// VoidKeys 为从这个账户的记录里去掉（或连同记录清除）的作废 Key 的 SHA-256 指纹（十六进制，最多 studioKeyVoidLimit 条，新的在后）：
	// 之后哪个账户再读到这条 Key 都不保存。只有指纹、没有 Key 与状态的记录是清除留下的（tombstone），列表里不显示
	VoidKeys []string `json:"void_keys,omitempty"`
}

// studioKeyVoidLimit 为一条记录里最多留下的作废指纹
const studioKeyVoidLimit = 16

// studioKeyFingerprint 返回 Key 的 SHA-256 指纹（十六进制，与驱动的 camoufoxnative.StudioKeyFingerprint 相同）：只用来记住作废过的 Key，
// 不写日志
func studioKeyFingerprint(key string) string {
	return camoufoxnative.StudioKeyFingerprint(key)
}

// withVoidKey 在记录的作废指纹里加上 key 的指纹（已有时不重复；超过上限时去掉最早的）
func withVoidKey(record studioKeyFile, key string) studioKeyFile {
	if key == "" {
		return record
	}
	return withVoidFingerprints(record, []string{studioKeyFingerprint(key)})
}

// withVoidFingerprints 在记录的作废指纹里加上这些指纹（已有的不重复；超过上限时去掉最早的）
func withVoidFingerprints(record studioKeyFile, fingerprints []string) studioKeyFile {
	for _, fingerprint := range fingerprints {
		if fingerprint == "" || slices.Contains(record.VoidKeys, fingerprint) {
			continue
		}
		record.VoidKeys = append(slices.Clone(record.VoidKeys), fingerprint)
	}
	if len(record.VoidKeys) > studioKeyVoidLimit {
		record.VoidKeys = record.VoidKeys[len(record.VoidKeys)-studioKeyVoidLimit:]
	}
	return record
}

// studioKeyUnconfirmed 表示记录里的 Key 是旧版本保存的、没有与页面的 Key 列表核对过（格式版本低于 studioKeyVersion；内存里新建、
// 还没写过文件的记录版本为 0，不算）
func studioKeyUnconfirmed(record studioKeyFile) bool {
	return record.Key != "" && record.Version > 0 && record.Version < studioKeyVersion
}

// studioKeyTombstone 表示记录只剩作废指纹（清除作废的 Key 时留下）：列表里不显示，也不算有记录
func studioKeyTombstone(record studioKeyFile) bool {
	return record.Key == "" && record.Status == "" && len(record.VoidKeys) > 0
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
	if record.Version <= 0 {
		// 没有版本号的按最早的版本（不核对 Key）
		record.Version = 1
	}
	return record, true, nil
}

// writeStudioKeyFile 以 0600 原子写入记录：内存里新建的（版本 0）与没有 Key 的记录写成 studioKeyVersion；读出来的带 Key 的记录保留原来的
// 版本（旧版本没有核对过的 Key 不会因为改写记录就算核对过）
func writeStudioKeyFile(directory string, record studioKeyFile) error {
	if record.Version <= 0 || record.Key == "" {
		record.Version = studioKeyVersion
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(directory, studioKeyFileName), append(data, '\n'), 0o600)
}

// studioKeyView 把记录转成列表项（不含完整 Key）；verdict 为记录里的 Key 是否作废（见 studioKeyJob.invalidFor）。作废的 Key 列为
// invalid：不算有 Key，不给末四位与项目，原因写明为什么作废。原因截短（见 studioKeyListReason）
func studioKeyView(accountID string, label string, record studioKeyFile, verdict studioKeyVerdict) api.StudioKeyRecord {
	if record.Key != "" && verdict.invalid {
		return api.StudioKeyRecord{
			AccountID: accountID, Label: label, Status: studioKeyInvalid, Reason: studioKeyInvalidReason(verdict),
			FetchedAt: record.FetchedAt, AttemptAt: record.AttemptAt,
		}
	}
	view := api.StudioKeyRecord{
		AccountID: accountID, Label: label, Status: record.Status, Reason: studioKeyListReason(record.Reason),
		HasKey: record.Key != "", ProjectName: record.ProjectName, ProjectNumber: record.ProjectNumber,
		ProjectDisplay: record.ProjectDisplay, ProjectID: record.ProjectID, Created: record.Created, ProjectCreated: record.ProjectCreated,
		KeyCount: record.KeyCount, FetchedAt: record.FetchedAt, CreatedAt: record.CreatedAt, AttemptAt: record.AttemptAt,
	}
	if record.Key != "" {
		view.Last4 = camoufoxnative.StudioKeyLast4(record.Key)
	}
	return view
}

// studioKeyListReasonLimit 为列表里失败原因的上限（字符）：记录里的原因带诊断，最长 1200 字，管理页每几秒读一次全部记录，列表只给开头，
// 完整的原因（含诊断）写在运行日志里
const studioKeyListReasonLimit = 300

// studioKeyListReason 返回列表里的失败原因：打码，超过 studioKeyListReasonLimit 时截短并注明完整原因在运行日志里
func studioKeyListReason(reason string) string {
	reason = camoufoxnative.RedactStudioKeys(reason)
	if runes := []rune(reason); len(runes) > studioKeyListReasonLimit {
		reason = string(runes[:studioKeyListReasonLimit]) + "…（完整原因与诊断见运行日志）"
	}
	return reason
}

// studioKeyInvalidReason 为作废的 Key 在列表里（与 reveal 的错误里）的原因
func studioKeyInvalidReason(verdict studioKeyVerdict) string {
	switch {
	case verdict.others > 0:
		return fmt.Sprintf("这条 Key 与另外 %d 个账户保存的相同（多半是页面自己的公共 Key，不是这个账户的），已作废；可用“全部获取”重新获取，或清除这条记录", verdict.others)
	case verdict.voided:
		return "这条 Key 已作废（曾与其他账户保存的相同，或浏览器认出它是页面自己的公共 Key），不是这个账户的；可用“全部获取”重新获取，或清除这条记录"
	}
	return "这条 Key 是旧版本保存的，没有与页面上的 Key 列表核对过（旧版本会把页面自己的公共 Key 当成账户的 Key），不再使用；可用“全部获取”重新获取，或清除这条记录"
}

// studioKeyDuplicateReason 为获取到的 Key 与别的账户相同、没有保存时的原因
func studioKeyDuplicateReason(others int) string {
	if others > 0 {
		return fmt.Sprintf("读到的 Key 与另外 %d 个账户相同（多半是页面自己的公共 Key，不是这个账户的），没有保存", others)
	}
	return "读到的 Key 是已作废的那条（与其他账户相同，多半是页面自己的公共 Key），没有保存"
}

// studioKeyHolders 为每条已保存的 Key 被哪些账户保存着（按账户列表顺序，读缓存；读不出的记录不算）
type studioKeyHolders map[string][]string

// others 返回除 accountID 之外还保存着 key 的账户数
func (holders studioKeyHolders) others(key string, accountID string) int {
	count := 0
	for _, id := range holders[key] {
		if id != accountID {
			count++
		}
	}
	return count
}

// studioKeyVerdict 为一条记录里的 Key 是否作废（见 studioKeyJob.invalidFor）
type studioKeyVerdict struct {
	// invalid 表示作废：不算有 Key、不导出、不能 reveal，“全部获取”重新获取，补项目信息跳过，重新获取时不沿用
	invalid bool
	// voided 表示确定不是这个账户的 Key：另有账户保存着同一条、记录上标着 invalid，或是作废过的 Key（指纹对上）。去掉或清除时留下指纹，
	// 获取时交给驱动排除。invalid 为真、voided 为假时是旧版本保存、没有核对过的 Key（studioKeyUnconfirmed）：不留指纹、不排除
	voided bool
	// others 为另外还保存着同一条 Key 的账户数
	others int
}

// invalidFor 判断账户的记录里的 Key 是否作废（见 studioKeyVerdict）
func (job *studioKeyJob) invalidFor(record studioKeyFile, accountID string, holders studioKeyHolders) studioKeyVerdict {
	if record.Key == "" {
		return studioKeyVerdict{}
	}
	others := holders.others(record.Key, accountID)
	voided := others > 0 || record.Invalid || job.voidedKey(record.Key)
	return studioKeyVerdict{invalid: voided || studioKeyUnconfirmed(record), voided: voided, others: others}
}

// stripStudioKey 去掉记录里的 Key 与随它的项目、创建信息（作废的 Key 不保留）；void 为真时（确定不是这个账户的）留下它的指纹（见 VoidKeys）。
// 状态、原因、尝试时间与已有的指纹不动
func stripStudioKey(record studioKeyFile, void bool) studioKeyFile {
	stripped := studioKeyFile{Status: record.Status, Reason: record.Reason, AttemptAt: record.AttemptAt, VoidKeys: record.VoidKeys}
	if !void {
		return stripped
	}
	return withVoidKey(stripped, record.Key)
}

// void 记下作废过的 Key（进程里，指纹）
func (job *studioKeyJob) void(key string) {
	if key == "" {
		return
	}
	job.voidFingerprints([]string{studioKeyFingerprint(key)})
}

// voidFingerprints 记下记录里留下的作废指纹（进程里）
func (job *studioKeyJob) voidFingerprints(fingerprints []string) {
	if len(fingerprints) == 0 {
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.voided == nil {
		job.voided = make(map[string]bool)
	}
	for _, fingerprint := range fingerprints {
		job.voided[fingerprint] = true
	}
}

// voidedKey 判断 Key 作废过（指纹在进程里记下的、或某条记录留下的指纹里；记录里的指纹在读全部记录时并入）
func (job *studioKeyJob) voidedKey(key string) bool {
	if key == "" {
		return false
	}
	fingerprint := studioKeyFingerprint(key)
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.voided[fingerprint]
}

// voidedFingerprints 返回进程里记下的全部作废指纹（交给驱动排除）
func (job *studioKeyJob) voidedFingerprints() []string {
	job.mu.Lock()
	defer job.mu.Unlock()
	fingerprints := make([]string, 0, len(job.voided))
	for fingerprint := range job.voided {
		fingerprints = append(fingerprints, fingerprint)
	}
	slices.Sort(fingerprints)
	return fingerprints
}

// studioKeyEntry 为读到的一条记录（按账户列表顺序）
type studioKeyEntry struct {
	id     string
	label  string
	record studioKeyFile
	ok     bool
	err    error
}

// studioKeyEntries 读全部账户的记录（读缓存：每个账户一次 stat），并统计每条 Key 被哪些账户保存着
func (manager *runtimeManager) studioKeyEntries(ctx context.Context, admin *runtimeAdmin) ([]studioKeyEntry, studioKeyHolders, error) {
	var entries []studioKeyEntry
	holders := studioKeyHolders{}
	for _, id := range admin.pool.AccountIDs() {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		account, err := admin.pool.Account(id)
		if err != nil || account == nil {
			continue
		}
		record, ok, err := manager.studioKeys.load(id, account.Directory)
		if err == nil && ok {
			manager.studioKeys.voidFingerprints(record.VoidKeys)
			if studioKeyTombstone(record) {
				// 清除留下的只有指纹：不算有记录
				ok = false
			}
		}
		entries = append(entries, studioKeyEntry{id: id, label: account.Config.Label, record: record, ok: ok, err: err})
		if err == nil && ok && record.Key != "" {
			holders[record.Key] = append(holders[record.Key], id)
		}
	}
	return entries, holders, nil
}

// studioKeyIndex 读全部账户的记录（见 studioKeyEntries），并把同一条 Key 有两个以上账户保存着、其中有还没标 invalid 的记录都写上
// invalid 标记（见 markStudioKeyShared；之后删掉其中的账户、只剩一个时也仍作废）。locked 表示调用方已持有 files 锁。返回的记录是
// 写标记之前读到的（作废与否照样按持有数算出）
func (manager *runtimeManager) studioKeyIndex(ctx context.Context, admin *runtimeAdmin, locked bool) ([]studioKeyEntry, studioKeyHolders, error) {
	entries, holders, err := manager.studioKeyEntries(ctx, admin)
	if err != nil {
		return nil, nil, err
	}
	var unmarked []string
	for _, entry := range entries {
		if entry.err == nil && entry.ok && entry.record.Key != "" && !entry.record.Invalid && len(holders[entry.record.Key]) > 1 &&
			!slices.Contains(unmarked, entry.record.Key) {
			unmarked = append(unmarked, entry.record.Key)
		}
	}
	for _, key := range unmarked {
		if marked := manager.markStudioKeyShared(admin, key, nil, locked); marked > 0 {
			admin.requests.log("auth", "INFO", fmt.Sprintf("AI Studio API Key | 同一条 Key 保存在 %d 个账户里，记录标为作废 | 末四位=%s | 标记=%d",
				len(holders[key]), camoufoxnative.StudioKeyLast4(key), marked))
		}
	}
	return entries, holders, nil
}

// markStudioKeyShared 记下这条 Key 作废过（进程里的指纹），并给其余保存着它的账户（skip 里的除外）的记录写上 invalid 标记（只写还没标的）：
// 这条 Key 已知是多个账户共有的，之后哪怕只剩一个账户保存着它也不再当作有效。每次都扫一遍全部账户（读缓存，每个账户一次 stat：清除或
// 去掉之后又有账户存进这条 Key 时也标得上）。locked 表示调用方已持有 files 锁；返回写上标记的记录数
func (manager *runtimeManager) markStudioKeyShared(admin *runtimeAdmin, key string, skip map[string]bool, locked bool) int {
	if key == "" {
		return 0
	}
	manager.studioKeys.void(key)
	return manager.markStudioKeyRecords(admin, map[string]bool{studioKeyFingerprint(key): true}, skip, locked)
}

// voidStudioKeyFingerprints 记下这些作废指纹（进程里，如浏览器认出的页面公共 Key），并给保存着指纹对得上的 Key 的账户（skip 里的除外）
// 的记录写上 invalid 标记；返回写上标记的记录数
func (manager *runtimeManager) voidStudioKeyFingerprints(admin *runtimeAdmin, fingerprints []string, skip map[string]bool) int {
	if len(fingerprints) == 0 {
		return 0
	}
	manager.studioKeys.voidFingerprints(fingerprints)
	set := map[string]bool{}
	for _, fingerprint := range fingerprints {
		set[fingerprint] = true
	}
	return manager.markStudioKeyRecords(admin, set, skip, false)
}

// markStudioKeyRecords 给保存着指纹在 fingerprints 里的 Key 的账户（skip 里的除外）的记录写上 invalid 标记（只写还没标的；见
// markStudioKeyShared）。locked 表示调用方已持有 files 锁；返回写上标记的记录数
func (manager *runtimeManager) markStudioKeyRecords(admin *runtimeAdmin, fingerprints map[string]bool, skip map[string]bool, locked bool) int {
	job := &manager.studioKeys
	if !locked {
		job.files.Lock()
		defer job.files.Unlock()
	}
	marked := 0
	for _, id := range admin.pool.AccountIDs() {
		if skip[id] {
			continue
		}
		account, err := admin.pool.Account(id)
		if err != nil || account == nil {
			continue
		}
		// files 锁内写记录的路径都停着：缓存（按 stat 校验）与磁盘一致
		record, ok, err := job.load(id, account.Directory)
		if err != nil || !ok || record.Key == "" || record.Invalid || !fingerprints[studioKeyFingerprint(record.Key)] {
			continue
		}
		record.Invalid = true
		if err := writeStudioKeyFile(account.Directory, record); err != nil {
			job.forget(id)
			continue
		}
		job.remember(id, account.Directory, statStudioKeyFile(account.Directory), record, true)
		marked++
	}
	return marked
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
	// voided 为作废过的 Key 的指纹（进程里记下的与记录里留下的，见 VoidKeys；由 mu 保护，只增不减）
	voided map[string]bool
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

// write 写入账户的记录并更新缓存（与清除互斥）。磁盘上同一条 Key 已标着 invalid（获取期间被别处标上）时保留标记
func (job *studioKeyJob) write(accountID string, directory string, record studioKeyFile) error {
	job.files.Lock()
	defer job.files.Unlock()
	if record.Key != "" && !record.Invalid {
		if current, ok, err := readStudioKeyFile(directory); err == nil && ok && current.Invalid && current.Key == record.Key {
			record.Invalid = true
		}
	}
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
	entries, holders, err := manager.studioKeyIndex(ctx, admin, false)
	if err != nil {
		return api.StudioKeyStatus{}, err
	}
	for _, entry := range entries {
		if entry.err != nil {
			status.Records = append(status.Records, api.StudioKeyRecord{
				AccountID: entry.id, Label: entry.label, Status: studioKeyFailed, Reason: entry.err.Error(),
			})
			continue
		}
		if entry.ok {
			status.Records = append(status.Records, studioKeyView(entry.id, entry.label, entry.record,
				manager.studioKeys.invalidFor(entry.record, entry.id, holders)))
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
	var holders studioKeyHolders
	if input.MissingOnly || fill {
		if _, holders, err = manager.studioKeyIndex(ctx, admin, false); err != nil {
			return api.StudioKeyStatus{}, err
		}
	}
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
			// 作废的 Key 不算有 Key：“全部获取”重新获取它，补项目信息跳过它
			hasKey := hasRecord && record.Key != "" && !manager.studioKeys.invalidFor(record, id, holders).invalid
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
func (manager *runtimeManager) RevealStudioKey(ctx context.Context, accountID string) (api.StudioKeySecret, error) {
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
	_, holders, err := manager.studioKeyIndex(ctx, admin, false)
	if err != nil {
		return api.StudioKeySecret{}, err
	}
	if verdict := manager.studioKeys.invalidFor(record, accountID, holders); verdict.invalid {
		return api.StudioKeySecret{}, &adminOperationError{status: http.StatusConflict, code: "studio_key_invalid", message: studioKeyInvalidReason(verdict)}
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

// ExportStudioKeys 返回全部保存着的完整 Key（按账户列表顺序，只给“复制全部 Key”用）；作废的 Key 不在里面
func (manager *runtimeManager) ExportStudioKeys(ctx context.Context) (api.StudioKeyExport, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyExport{}, err
	}
	export := api.StudioKeyExport{Keys: []api.StudioKeySecret{}}
	entries, holders, err := manager.studioKeyIndex(ctx, admin, false)
	if err != nil {
		return api.StudioKeyExport{}, err
	}
	for _, entry := range entries {
		if entry.err != nil || !entry.ok || entry.record.Key == "" {
			continue
		}
		if manager.studioKeys.invalidFor(entry.record, entry.id, holders).invalid {
			continue
		}
		export.Keys = append(export.Keys, studioKeySecret(entry.id, entry.record))
	}
	return export, nil
}

// ClearStudioKeys 清除账户的记录：没有 Key 的删掉记录文件；保存着 Key 的只清掉失败或跳过的信息（状态回到成功、原因去掉、最近尝试
// 时间回到读到 Key 的时间），Key、项目、读取与创建时间都不动；例外：作废的 Key 连同记录一起清除——确定不是这个账户的（另有账户保存着
// 同一条、标着 invalid，或是作废过的）留下指纹，其余仍保存着它的账户的记录写上 invalid 标记；旧版本保存、没有核对过的只删掉，不留指纹。
// 记录里有作废指纹（见 VoidKeys）时不删文件，只留下指纹（tombstone，列表里不显示），之后再读到这条 Key 也不保存。正在处理的账户不动（排队中的照清）；不认识的账户只计数。
// 与写记录互斥，每个文件在锁内重新读一次再决定；作废与否按清除开始时（锁内）全部记录算一次，同一次清除里先删掉的不影响后面的。
// 运行日志只记计数
func (manager *runtimeManager) ClearStudioKeys(ctx context.Context, input api.StudioKeyClearInput) (api.StudioKeyClearResult, error) {
	admin, err := manager.studioKeyAdmin()
	if err != nil {
		return api.StudioKeyClearResult{}, err
	}
	var counts api.StudioKeyClearCounts
	job := &manager.studioKeys
	job.files.Lock()
	// 锁内写记录的路径都停着，缓存（按 stat 校验）与磁盘一致
	_, holders, err := manager.studioKeyIndex(ctx, admin, true)
	if err != nil {
		job.files.Unlock()
		return api.StudioKeyClearResult{}, err
	}
	// remove 清除记录：有作废指纹时只留下指纹（tombstone），否则删掉文件
	remove := func(id string, directory string, voids []string) error {
		if len(voids) > 0 {
			tombstone := studioKeyFile{VoidKeys: voids}
			if err := writeStudioKeyFile(directory, tombstone); err != nil {
				return err
			}
			job.remember(id, directory, statStudioKeyFile(directory), tombstone, true)
			return nil
		}
		if err := os.Remove(filepath.Join(directory, studioKeyFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		job.remember(id, directory, statStudioKeyFile(directory), studioKeyFile{}, false)
		return nil
	}
	var invalidKeys []string
	cleared := map[string]bool{}
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
		case studioKeyTombstone(record):
			// 只剩作废指纹（以前清除留下的）：列表里本来就没有，留着
			job.remember(id, directory, stamp, record, true)
			counts.Unchanged++
		case job.invalidFor(record, id, holders).invalid:
			// 作废的 Key 不当作这个账户的：连同记录清除。确定不是这个账户的（与其他账户相同、作废过）留下它的指纹，其余仍保存着它的账户
			// 标为作废；旧版本保存、没有核对过的只删掉（不留指纹：它也可能确实是这个账户的，重新获取时核对得上照样保存）
			voided := job.invalidFor(record, id, holders).voided
			voids := record.VoidKeys
			if voided {
				voids = withVoidKey(record, record.Key).VoidKeys
			}
			if err := remove(id, directory, voids); err != nil {
				job.forget(id)
				counts.Errors++
				continue
			}
			if voided {
				job.void(record.Key)
				cleared[id] = true
				if !slices.Contains(invalidKeys, record.Key) {
					invalidKeys = append(invalidKeys, record.Key)
				}
			}
			counts.Invalid++
		case record.Key == "":
			if err := remove(id, directory, record.VoidKeys); err != nil {
				job.forget(id)
				counts.Errors++
				continue
			}
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
	marked := 0
	for _, key := range invalidKeys {
		marked += manager.markStudioKeyShared(admin, key, cleared, true)
	}
	job.files.Unlock()
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"AI Studio API Key | 清除记录 | 删除=%d | 删除作废的 Key=%d | 清掉失败信息=%d | 正在处理没动=%d | 未知账户=%d | 出错=%d | 其余同一 Key 标为作废=%d",
		counts.Deleted, counts.Invalid, counts.Reset, counts.Busy, counts.Unknown, counts.Errors, marked))
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
	_, holders, _ := manager.studioKeyIndex(ctx, admin, false)
	prevVerdict := manager.studioKeys.invalidFor(previous, accountID, holders)
	if prevVerdict.invalid && fill {
		// 作废的 Key 不当作这个账户的，没有可补的项目信息：不打开浏览器、不写记录（列表里仍是作废）
		admin.requests.log(label, "INFO", title+" | 跳过 | 原因=已保存的 Key 已作废（与其他账户相同，或旧版本保存、没有核对过）")
		return false
	}
	// exclude 为交给驱动排除的 Key：其他账户保存着的，以及这个账户确定作废的（见 StudioKeyOptions.Exclude）
	var exclude []string
	for key, ids := range holders {
		if slices.ContainsFunc(ids, func(id string) bool { return id != accountID }) {
			exclude = append(exclude, key)
		}
	}
	switch {
	case prevVerdict.voided:
		// 确定作废的 Key 不当作上次的 Key 保留（记录里只留下它的指纹）、不交给驱动当已保存的 Key，交给驱动排除；记下它作废过（这次再读到
		// 它也不保存），其余保存着它的账户写上 invalid 标记（只剩一个时也不再当有效）
		marked := manager.markStudioKeyShared(admin, previous.Key, map[string]bool{accountID: true}, false)
		why := "曾与其他账户相同，或是页面自己的公共 Key"
		if prevVerdict.others > 0 {
			why = fmt.Sprintf("与另外 %d 个账户相同", prevVerdict.others)
		}
		admin.requests.log(label, "INFO", fmt.Sprintf("%s | 已保存的 Key 已作废（%s），这次不沿用 | 末四位=%s | 其余标为作废=%d",
			title, why, camoufoxnative.StudioKeyLast4(previous.Key), marked))
		if !slices.Contains(exclude, previous.Key) {
			exclude = append(exclude, previous.Key)
		}
		previous = stripStudioKey(previous, true)
	case prevVerdict.invalid:
		// 旧版本保存、没有核对过的 Key：不沿用、不交给驱动当已保存的 Key，也不排除、不留指纹（核对得上照样保存）
		admin.requests.log(label, "INFO", fmt.Sprintf("%s | 已保存的 Key 是旧版本保存的、没有核对过，这次不沿用 | 末四位=%s",
			title, camoufoxnative.StudioKeyLast4(previous.Key)))
		previous = stripStudioKey(previous, false)
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
	options := camoufoxnative.StudioKeyOptions{ReadOnly: fill, SavedKey: previous.Key, Exclude: exclude,
		ExcludeFingerprints: manager.studioKeys.voidedFingerprints()}
	startedAt := time.Now()
	admin.requests.log(label, "INFO", title+" | 打开 API Keys 页面")
	result, err := admin.fetchStudioKeyWithLease(ctx, accountID, driver, options)
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	// 浏览器认出的页面公共 Key（成功、失败都有）：记成作废指纹（进程里与这个账户的记录里），保存着它的账户的记录写上 invalid 标记
	// （旧版本只给一个账户记下它时，按个数认不出，靠这里发现）
	clientFingerprints := result.ClientFingerprints
	var keyErr *camoufoxnative.StudioKeyError
	if err != nil && errors.As(err, &keyErr) {
		clientFingerprints = keyErr.ClientFingerprints
	}
	if marked := manager.voidStudioKeyFingerprints(admin, clientFingerprints, map[string]bool{accountID: true}); marked > 0 {
		admin.requests.log(label, "INFO", fmt.Sprintf("%s | 浏览器认出页面自己的公共 Key，保存着它的记录标为作废 | 标记=%d", title, marked))
	}
	savedIsClient := previous.Key != "" && slices.Contains(clientFingerprints, studioKeyFingerprint(previous.Key))
	previous = withVoidFingerprints(previous, clientFingerprints)
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
		if previous.Key != "" && (savedIsClient || keyErr != nil && keyErr.SavedIsClient) {
			// 驱动认出已保存的 Key 就是页面自己的公共 Key：它不是这个账户的，不再保留
			marked := manager.markStudioKeyShared(admin, previous.Key, map[string]bool{accountID: true}, false)
			admin.requests.log(label, "INFO", fmt.Sprintf("%s | 已保存的 Key 是页面自己的公共 Key，已去掉 | 末四位=%s | 其余标为作废=%d",
				title, camoufoxnative.StudioKeyLast4(previous.Key), marked))
			previous = stripStudioKey(previous, true)
		}
		previous.Status, previous.Reason = status, reason
		save(previous)
		admin.requests.log(label, "WARN", fmt.Sprintf("%s | 耗时=%s | 原因=%s", failed, elapsed, reason))
		return true
	}
	// 读到的 Key 与别的账户保存的相同，或是作废过的（指纹对上，含刚作废的那条）：多半是页面自己的公共 Key，不保存（上次的有效 Key
	// 照旧保留，记录里留下这条的指纹），并给仍保存着它的账户的记录写上 invalid 标记
	if _, latest, _ := manager.studioKeyEntries(ctx, admin); latest != nil {
		holders = latest
	}
	if others := holders.others(result.Key, accountID); others > 0 || manager.studioKeys.voidedKey(result.Key) {
		reason := studioKeyDuplicateReason(others)
		marked := manager.markStudioKeyShared(admin, result.Key, map[string]bool{accountID: true}, false)
		if previous.Key == result.Key {
			// 获取期间别的账户的记录里也出现了这条（进程外的改动）：上次的这条同样作废
			previous = stripStudioKey(previous, true)
		}
		previous = withVoidKey(previous, result.Key)
		previous.Status, previous.Reason = studioKeyFailed, reason
		save(previous)
		admin.requests.log(label, "WARN", fmt.Sprintf("%s | 耗时=%s | 末四位=%s | 其余标为作废=%d | 原因=%s", failed, elapsed,
			camoufoxnative.StudioKeyLast4(result.Key), marked, reason))
		return true
	}
	record := studioKeyFile{
		Key: result.Key, ProjectName: result.ProjectName, ProjectNumber: result.ProjectNumber,
		ProjectDisplay: result.ProjectDisplay, ProjectID: result.ProjectID, Created: result.Created, ProjectCreated: result.ProjectCreated,
		KeyCount: result.KeyCount, FetchedAt: &now, Status: studioKeyOK, VoidKeys: previous.VoidKeys,
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

// studioKeyStepsText 为写进运行日志的创建步骤与诊断（打码、单行、截短）
func studioKeyStepsText(steps []string) string {
	text := camoufoxnative.RedactStudioKeys(strings.Join(steps, "→"))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 900 {
		text = string(runes[:899]) + "…"
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
	proxy, poolLease, err := admin.browserProxy(account.ID, account.ID, proxyPoolKindVerify, account.Config.Label, account.Config.Proxy)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	defer poolLease.release()
	request := admin.loginRequest(account, account.Directory, proxy)
	request.OnboardingCompleted = admin.onboardingLogger(account.Config.Label, "获取 API Key")
	return driver.FetchStudioKey(ctx, request, account.StorageState, options)
}

// studioKeyFailureReason 返回写进记录与日志的失败原因（打码、单行、截短；创建流程的失败带步骤、对话框摘要与诊断，上限放宽到 1200 字）
func studioKeyFailureReason(err error) string {
	var keyErr *camoufoxnative.StudioKeyError
	text := err.Error()
	if errors.As(err, &keyErr) {
		text = keyErr.Error()
	}
	text = camoufoxnative.StripPageSnapshots(camoufoxnative.RedactStudioKeys(text))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 1200 {
		text = string(runes[:1199]) + "…"
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
