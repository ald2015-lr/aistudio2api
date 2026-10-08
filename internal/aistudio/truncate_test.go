package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type countingTransport struct {
	counts []int64
	calls  int
}

func (t *countingTransport) Do(_ context.Context, _ RPCRequest) (*RPCResponse, error) {
	count := t.counts[min(t.calls, len(t.counts)-1)]
	t.calls++
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader("[" + strconv.FormatInt(count, 10) + "]")),
	}, nil
}

func truncateClient(t *testing.T, counts ...int64) (*Client, *countingTransport) {
	t.Helper()
	transport := &countingTransport{counts: counts}
	client, err := NewClient(ClientOptions{
		Transport: transport,
		Protected: ProtectedTransportFunc(func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
			return nil, errors.New("unused")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, transport
}

// 早期轮次主要是本地估算为 0 的内联媒体时，删到只剩最新一轮应交给权威计数复核，而不是直接 400
func TestTruncateRequestRecountsAfterDroppingMediaTurns(t *testing.T) {
	client, transport := truncateClient(t, 5000, 40)
	latest := strings.Repeat("hello world ", 30)
	request := GenerateRequest{
		Model: "models/test",
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("png")}}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "ok"}}},
			{Role: RoleUser, Parts: []Part{{Text: latest}}},
		},
	}
	limit := EstimatedInputTokens(request)
	truncated, err := client.truncateRequest(context.Background(), request, limit)
	if err != nil {
		t.Fatalf("截断失败: %v", err)
	}
	if len(truncated.Contents) != 1 || truncated.Contents[0].Parts[0].Text != latest {
		t.Fatalf("应只保留最新一轮: %+v", truncated.Contents)
	}
	if transport.calls != 2 {
		t.Fatalf("删完早期轮次后应重新计数，实际计数 %d 次", transport.calls)
	}
}

func TestTruncateRequestRejectsOversizedLatestTurn(t *testing.T) {
	client, _ := truncateClient(t, 5000)
	request := GenerateRequest{
		Model:    "models/test",
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: strings.Repeat("hello world ", 30)}}}},
	}
	_, err := client.truncateRequest(context.Background(), request, EstimatedInputTokens(request))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("最新一轮本身超限应返回 ErrInvalidArgument，实际 %v", err)
	}
}
