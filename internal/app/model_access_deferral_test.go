package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// diskModelAccess 读出账户运行态文件里模型的资格
func diskModelAccess(t *testing.T, path string, model string) aistudio.ModelAccess {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		ModelAccess map[string]aistudio.ModelAccess `json:"model_access"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.ModelAccess[model]
}

// TestModelAccessDeferralFlushedOnStop 生成服务启动后（startDeferredWrites），已验证模型的连续成功只推进 CheckedAt、不写运行态文件；
// 生成服务停止（stopDeferredCookieWrites）时全部写出，之后改回每次立即写
func TestModelAccessDeferralFlushedOnStop(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "hello"}, {Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	path := account.RuntimePath
	runScriptedRequest(t, service, abnormalTestRequest("req-deferral-first"))
	pool.WaitBackground()
	if access := diskModelAccess(t, path, testRuntimeModel); access.State != aistudio.ModelAccessVerified {
		t.Fatalf("第一次成功应立即写成已验证: %+v", access)
	}
	// 生成服务启动时打开推迟（startDeferredWrites）
	service.startDeferredWrites()
	before, _ := os.ReadFile(path)
	for range 5 {
		time.Sleep(2 * time.Millisecond)
		runScriptedRequest(t, service, abnormalTestRequest("req-deferral"))
	}
	pool.WaitBackground()
	lastLease := time.Now()
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("只推进 CheckedAt 的成功不应写运行态文件")
	}
	service.stopDeferredCookieWrites()
	access := diskModelAccess(t, path, testRuntimeModel)
	if after, _ := os.ReadFile(path); bytes.Equal(before, after) || access.State != aistudio.ModelAccessVerified ||
		!access.CheckedAt.After(diskCheckedAt(t, before, testRuntimeModel)) || access.CheckedAt.After(lastLease) {
		t.Fatalf("停止时应写出推迟的时间: %+v", access)
	}
	// 停止后改回每次立即写
	written, _ := os.ReadFile(path)
	time.Sleep(2 * time.Millisecond)
	runScriptedRequest(t, service, abnormalTestRequest("req-deferral-after"))
	pool.WaitBackground()
	if after, _ := os.ReadFile(path); bytes.Equal(written, after) {
		t.Fatal("停止后应立即写")
	}
}

func diskCheckedAt(t *testing.T, data []byte, model string) time.Time {
	t.Helper()
	var state struct {
		ModelAccess map[string]aistudio.ModelAccess `json:"model_access"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.ModelAccess[model].CheckedAt
}

// TestRuntimeWiringAbnormalFinishAndDeferral 经 newRuntime 装配（生产装配）：启动时按配置读入 ABNORMAL_FINISH_ERROR；
// 进程关闭（closeRuntime）时写出推迟的模型资格成功时间
func TestRuntimeWiringAbnormalFinishAndDeferral(t *testing.T) {
	isolateDisableNotes(t)
	root := filepath.Join(t.TempDir(), "auth")
	const accountID = "deferral-wiring@example.com"
	writeEnableTestAccount(t, root, accountID, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	cfg.AbnormalFinishError = true
	lifecycle, stop := context.WithCancel(ctx)
	useTempCatalogCacheDir(t)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	if !admin.service.abnormalFinishError.Load() {
		t.Fatal("启动时应按配置打开 ABNORMAL_FINISH_ERROR")
	}
	if err := admin.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	account, err := admin.pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	generation := admin.pool.ModelAccessGeneration(accountID)
	base := time.Now().Add(-time.Hour).UTC()
	if _, err := admin.pool.MarkModelAccessVerifiedIfGeneration(accountID, testRuntimeModel, generation, base); err != nil {
		t.Fatal(err)
	}
	if err := admin.pool.SetModelAccessDeferral(true); err != nil {
		t.Fatal(err)
	}
	later := base.Add(time.Second)
	if _, err := admin.pool.MarkModelAccessVerifiedIfGeneration(accountID, testRuntimeModel, generation, later); err != nil {
		t.Fatal(err)
	}
	if access := diskModelAccess(t, account.RuntimePath, testRuntimeModel); !access.CheckedAt.Equal(base) {
		t.Fatalf("推迟期间不应写盘: %+v", access)
	}
	stop()
	if err := closeRuntime(); err != nil {
		t.Fatal(err)
	}
	if access := diskModelAccess(t, account.RuntimePath, testRuntimeModel); !access.CheckedAt.Equal(later) {
		t.Fatalf("进程关闭时应写出推迟的时间: %+v", access)
	}
}
