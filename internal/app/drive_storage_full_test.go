package app

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// Drive 存储空间已满（上游 Mag1cFall/AIStudio2API efda79b、e6f067a）的 app 层：附件改为内联时日志写明“Drive 空间已满”，
// 不写“Drive 未授权”；Drive 的 403 不算 403 无权限；空间已满的记录写盘与恢复

// driveFullUpstream 模拟 aistudio.PooledService 在 Drive 存储空间已满时的内联附件处理：原样返回 inline data，副本集合报告空间已满
type driveFullUpstream struct {
	*bareRateUpstream
}

func (upstream *driveFullUpstream) UploadInlineMediaToLease(
	_ context.Context, _ *aistudio.AccountLease, contents []aistudio.Content, _ *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	return contents, aistudio.NewDriveStorageFullCopiesForTest(), nil
}

// TestDriveStorageFullInlineLog Drive 存储空间已满、附件改为内联发送：请求在同一账号上完成（不换号、不冷却），写
// “Drive 空间已满，附件改为内联”（同账号 10 分钟最多一行），不写“Drive 未授权”，也不写“内联附件处理完成”
func TestDriveStorageFullInlineLog(t *testing.T) {
	const account = "drive-full@example.com"
	upstream := &driveFullUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}}
	service, pool := firstEventTestService(t, upstream, 0, account)
	registry, _ := newConsoleTestRegistry(0, 8)
	service.requests = registry
	for index, requestID := range []string{"req-full-1", "req-full-2"} {
		text, err := runInlineImageRequest(t, service, requestID, account)
		if err != nil || text != "hello" {
			t.Fatalf("第 %d 次：text=%q err=%v，期望在同一账号上内联发送完成", index+1, text, err)
		}
	}
	if got := countSourceLogs(service.requests, account, "Drive 空间已满，附件改为内联 | 附件=1 | 原始=3B | 耗时="); got != 1 {
		t.Fatalf("“Drive 空间已满，附件改为内联”%d 条，期望同账号 10 分钟内只写 1 条；日志 = %v", got, serviceLogMessages(service.requests))
	}
	for _, unwanted := range []string{"Drive 未授权", "内联附件处理完成", "账号切换", "账号冷却"} {
		if got := countSourceLogs(service.requests, account, unwanted); got != 0 {
			t.Fatalf("空间已满改为内联时不应写“%s”：日志 = %v", unwanted, serviceLogMessages(service.requests))
		}
	}
	if status, _ := pool.StatusOf(account); len(status.Cooldowns) != 0 {
		t.Fatalf("空间已满改为内联时不应冷却账号：%+v", status.Cooldowns)
	}
}

// TestPermissionDeniedIgnoresDrive Drive 的 REST 请求（上传、续传、分块、下载、删除）返回的 403 不算 403 无权限，不计入 403 自动暂停、
// 单模型 403 冷却与换号；生成等 MakerSuite RPC 的 403 照旧算
func TestPermissionDeniedIgnoresDrive(t *testing.T) {
	body := `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"storageQuotaExceeded"}]}}`
	for _, method := range []string{"DriveUpload", "DriveUploadInit", "DriveUploadChunk", "DriveDownload", "DriveDelete"} {
		if isPermissionDenied(&aistudio.RPCError{Method: method, StatusCode: http.StatusForbidden, Message: body}) {
			t.Fatalf("%s 的 403 不应算 403 无权限", method)
		}
	}
	if !isPermissionDenied(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}) {
		t.Fatal("生成的 403 / 协议错误码 7 仍应算 403 无权限")
	}
}

// TestDriveStateMemoryStorageFull 空间已满的记录写成 f，重新打开后按 DriveStateStorageFull 交给生成服务恢复；之后改记可用时照常覆盖
func TestDriveStateMemoryStorageFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), driveStateFileName)
	now := time.Now()
	memory := openDriveStateMemory(path, nil)
	memory.observe(aistudio.DriveRecord{AccountID: "full@example.com", State: aistudio.DriveStateStorageFull, CheckedAt: now, Identity: "id-full"})
	if err := memory.close(); err != nil {
		t.Fatal(err)
	}
	reopened := openDriveStateMemory(path, nil)
	if entry := reopened.entries["full@example.com"]; entry.State != "f" {
		t.Fatalf("空间已满应写成 f，实际 %+v", entry)
	}
	store := &driveRecordRecorder{}
	reopened.attach(store)
	if len(store.restored) != 1 || store.restored[0].State != aistudio.DriveStateStorageFull || store.restored[0].Identity != "id-full" {
		t.Fatalf("恢复 = %+v，期望一条空间已满的记录", store.restored)
	}
	store.observer(aistudio.DriveRecord{AccountID: "full@example.com", State: aistudio.DriveStateAvailable, CheckedAt: now, Identity: "id-full"})
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}
	if entry := openDriveStateMemory(path, nil).entries["full@example.com"]; entry.State != "a" {
		t.Fatalf("之后改记可用应覆盖，实际 %+v", entry)
	}
}

// omniStorageFullUpstream 在 omniDriveUpstream 上让 full 里的账号像 Drive 存储空间已满一样处理内联附件：原样返回 inline data，
// 副本集合报告空间已满，记为传不上 Drive（DriveCandidates 归入 Unavailable，与 aistudio.PooledService 相同）
type omniStorageFullUpstream struct {
	*omniDriveUpstream
	full map[string]bool
}

func (upstream *omniStorageFullUpstream) UploadInlineMediaToLease(
	ctx context.Context, lease *aistudio.AccountLease, contents []aistudio.Content, temporary *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	accountID := lease.Account().ID
	if !upstream.full[accountID] {
		return upstream.omniDriveUpstream.UploadInlineMediaToLease(ctx, lease, contents, temporary)
	}
	upstream.mu.Lock()
	upstream.uploads = append(upstream.uploads, accountID)
	upstream.states[accountID] = aistudio.DriveStateUnavailable
	upstream.mu.Unlock()
	return contents, aistudio.NewDriveStorageFullCopiesForTest(), nil
}

// TestOmniDriveStorageFullSwitchesAccount Interaction 模型的视频选到 Drive 存储空间已满的账号：附件仍是 inline data，不调用 Generate，
// 照“没有 Drive 授权”的规则换有 Drive 的账号（不冷却），请求进度写明“账号 Drive 空间已满”，不写“账号没有 Drive 授权”
func TestOmniDriveStorageFullSwitchesAccount(t *testing.T) {
	accounts := []string{"a-full@example.com", "z-drive@example.com"}
	upstream := &omniStorageFullUpstream{omniDriveUpstream: newOmniDriveUpstream(accounts, "z-drive@example.com"), full: map[string]bool{"a-full@example.com": true}}
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
	service.workers.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: omniDriveModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	text, err, _ := runOmniDriveRequest(t, service, newAPIVideoRequest("req-storage-full", omniDriveModel))
	if err != nil || text != "ok" {
		t.Fatalf("应换到有 Drive 的账号成功：text=%q err=%v", text, err)
	}
	if calls, uploads, _ := upstream.snapshot(); !slices.Equal(calls, []string{"z-drive@example.com"}) ||
		!slices.Equal(uploads, []string{"a-full@example.com", "z-drive@example.com"}) {
		t.Fatalf("Generate=%v 上传=%v，期望空间已满的账号不调用 Generate、换到有 Drive 的账号", calls, uploads)
	}
	if !progressLogged(service, "账号 Drive 空间已满，Omni 的视频（及不能内联的图片）无法发送，换有 Drive 授权的账号") {
		t.Fatal("应在请求进度里写明 Drive 空间已满")
	}
	if progressLogged(service, "账号没有 Drive 授权") {
		t.Fatal("空间已满的账号不应写成没有 Drive 授权")
	}
	assertNoAccountCooldown(t, pool)
}

// driveStatesService 为报告候选 Drive 状态的假生成服务（只实现 DriveCandidates）
type driveStatesService struct {
	aistudio.Service
	unavailable []string
}

func (service *driveStatesService) DriveCandidates(context.Context, aistudio.AccountSelection) aistudio.DriveCandidates {
	return aistudio.DriveCandidates{Unavailable: service.unavailable}
}

// TestTranscriptionCandidatesSkipDriveUnavailable 转写的候选去掉记着 Drive 不可用或存储空间已满的账户：转写先把音频上传到账户的 Drive，
// 选到它们只会白传一次再换号（原先候选不看 Drive 记忆）；候选全都记着不可用时照旧全部保留（记录可能已过时）
func TestTranscriptionCandidatesSkipDriveUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, "alice@example.com", "bob@example.com")
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, 1, 1, 1, false)
	defer workers.Close()
	drive := &driveStatesService{unavailable: []string{accounts[0].ID}}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: workers, service: drive}
	candidates, err := service.transcriptionCandidates(ctx, testRuntimeModel)
	if err != nil || !slices.Equal(candidates, []string{accounts[1].ID}) {
		t.Fatalf("候选 = %v（err=%v），期望去掉 Drive 不可用的 %s", candidates, err, accounts[0].ID)
	}
	drive.unavailable = []string{accounts[0].ID, accounts[1].ID}
	candidates, err = service.transcriptionCandidates(ctx, testRuntimeModel)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("候选 = %v（err=%v），全都记着不可用时应全部保留", candidates, err)
	}
}
