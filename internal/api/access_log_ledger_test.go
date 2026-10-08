package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// capturingAdmin 记录请求日志收到的完成记录
type capturingAdmin struct {
	AdminService
	mu      sync.Mutex
	entries []AccessLog
}

func (admin *capturingAdmin) RecordAccessStart(AccessLog) {}

func (admin *capturingAdmin) RecordAccessLog(entry AccessLog) {
	admin.mu.Lock()
	admin.entries = append(admin.entries, entry)
	admin.mu.Unlock()
}

func (admin *capturingAdmin) finished() []AccessLog {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return append([]AccessLog(nil), admin.entries...)
}

// TestAccessLogKeepsLocalFieldsWithLedgerFields 合并账本字段（排队、proof、上游尝试、密钥校验）后，
// 本地的回复指纹、实际服务模型、降级判定、seed、top_k 与失败状态码仍原样写入请求日志
func TestAccessLogKeepsLocalFieldsWithLedgerFields(t *testing.T) {
	admin := &capturingAdmin{}
	topK := 40
	seed := int64(7)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		time.Sleep(5 * time.Millisecond)
		SetAccessLogGenerationConfig(ctx, aistudio.GenerationConfig{TopK: &topK, Seed: &seed})
		SetAccessLogTarget(ctx, "gemini-test", "a@example.com")
		AddAccessLogAttempt(ctx, RequestAttempt{Account: "b@example.com", Channel: "build", Error: "429", DurationMS: 3})
		MarkAccessLogScheduled(ctx)
		AddAccessLogProof(ctx, 20*time.Millisecond)
		AddAccessLogProof(ctx, 5*time.Millisecond)
		SetAccessLogReplyHash(ctx, "abcdef123456")
		SetAccessLogServedModel(ctx, "other-model")
		SetAccessLogDowngrade(ctx, aistudio.DowngradeDecision{Verdict: "rejected", Reason: "speed"})
		SetAccessLogError(ctx, aistudio.ErrInvalidArgument)
		w.WriteHeader(http.StatusOK)
	})
	key := func() string { return "sk-test" }
	handler := requestLoggingMiddleware(admin, authMiddleware(key, inner))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer sk-test")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	entries := admin.finished()
	if len(entries) != 1 {
		t.Fatalf("请求日志条数=%d，期望 1", len(entries))
	}
	entry := entries[0]
	if entry.ReplyHash != "abcdef123456" || entry.ServedModel != "other-model" || entry.Downgrade == nil || entry.Downgrade.Verdict != "rejected" {
		t.Fatalf("本地字段丢失: reply=%q served=%q downgrade=%+v", entry.ReplyHash, entry.ServedModel, entry.Downgrade)
	}
	if entry.Seed != "7" || entry.TopK != "40" {
		t.Fatalf("seed 或 top_k 丢失: seed=%q topK=%q", entry.Seed, entry.TopK)
	}
	if entry.Status != http.StatusBadRequest {
		t.Fatalf("失败状态码没有覆盖 200: %d", entry.Status)
	}
	if !entry.Authorized || entry.QueueWait < 5*time.Millisecond || entry.Proof != 25*time.Millisecond {
		t.Fatalf("账本字段不对: authorized=%v queue=%s proof=%s", entry.Authorized, entry.QueueWait, entry.Proof)
	}
	if len(entry.Attempts) != 1 || entry.Attempts[0].Channel != "build" || entry.Attempts[0].Error != "429" {
		t.Fatalf("上游尝试不对: %+v", entry.Attempts)
	}

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	unauthorized.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(httptest.NewRecorder(), unauthorized)
	if entries := admin.finished(); len(entries) != 2 || entries[1].Authorized {
		t.Fatal("未通过密钥校验的请求不应标记为已校验")
	}
}

// TestGenerationInputKeepsRequestIDWhenEmpty 生成请求没有 ID 时保留请求日志生成的 ID，账本不会出现空 ID
func TestGenerationInputKeepsRequestIDWhenEmpty(t *testing.T) {
	metadata := &accessLogMetadata{requestID: "req_1"}
	metadata.setGenerationInput(aistudio.GenerateRequest{})
	if metadata.id() != "req_1" {
		t.Fatalf("空 ID 覆盖了请求日志 ID: %q", metadata.id())
	}
	metadata.setGenerationInput(aistudio.GenerateRequest{ID: "chatcmpl_1"})
	if metadata.id() != "chatcmpl_1" {
		t.Fatalf("生成请求 ID 没有写入: %q", metadata.id())
	}
}

// TestAccessLogAttemptsSnapshotIsCopy 快照中的上游尝试是副本，之后的追加不影响已写出的记录
func TestAccessLogAttemptsSnapshotIsCopy(t *testing.T) {
	metadata := &accessLogMetadata{}
	metadata.attempts = append(make([]RequestAttempt, 0, 4), RequestAttempt{Error: errors.New("first").Error()})
	snapshot := metadata.snapshot()
	metadata.attempts = append(metadata.attempts, RequestAttempt{Error: "second"})
	metadata.attempts[0].Error = "changed"
	if len(snapshot.attempts) != 1 || snapshot.attempts[0].Error != "first" {
		t.Fatalf("快照被后续修改影响: %+v", snapshot.attempts)
	}
}
