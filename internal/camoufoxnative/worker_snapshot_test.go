package camoufoxnative

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeSnapshot 为假页面对快照表达式的回答：地址带多余参数、标题带“|”、正文带邮箱，检查 Go 侧整理
const fakeSnapshot = `{"href":"https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest&authuser=1","title":"Google AI Studio | Beta",` +
	`"ready":"complete","vis":"visible","age":118,"box":[1,0],"input":[1,0],"textareas":0,"run":[1,0,0],"waa":false,` +
	`"overlays":["cdk-overlay: Something went wrong Reload"],"ms":["ms-app"],"body":"Signed in as lisa@gmail.com"}`

// fakePageURL 为假页面的 location.href
const fakePageURL = "https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest&authuser=1"

// fakeEvaluate 为假页面对一条 script.evaluate 的回答：value 为页面返回值，exception 为页面抛错的文字；
// closeConnection 为真时假浏览器直接断开连接（模拟浏览器崩溃），noReply 为真时不回答这条命令（模拟浏览器卡住）；
// received 不为空时假浏览器收到这条命令后先调用它（在假浏览器的协程里）
type fakeEvaluate struct {
	value           any
	exception       string
	closeConnection bool
	noReply         bool
	received        func()
}

// fakeBiDiPage 为测试用的假 Camoufox：WebSocket 上按 BiDi 协议回答命令，script.evaluate 交给 evaluate 回调；
// 快照表达式回答构造时给定的 snapshot（newFakeBiDiPage 为 fakeSnapshot）并计数
type fakeBiDiPage struct {
	client    *bidiClient
	snapshots atomic.Int32
}

func newFakeBiDiPage(t *testing.T, evaluate func(expression string) fakeEvaluate) *fakeBiDiPage {
	t.Helper()
	return newFakeBiDiPageWithSnapshot(t, evaluate, fakeEvaluate{value: fakeSnapshot})
}

// newFakeBiDiPageWithSnapshot 同 newFakeBiDiPage，快照表达式改为回答 snapshot
func newFakeBiDiPageWithSnapshot(t *testing.T, evaluate func(expression string) fakeEvaluate, snapshot fakeEvaluate) *fakeBiDiPage {
	t.Helper()
	page := &fakeBiDiPage{}
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var message struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := connection.ReadJSON(&message); err != nil {
				return
			}
			result := map[string]any{}
			switch message.Method {
			case "browsingContext.getTree":
				result = map[string]any{"contexts": []any{map[string]any{"context": "ctx-1"}}}
			case "network.addIntercept":
				result = map[string]any{"intercept": "intercept-1"}
			case "script.evaluate":
				expression, _ := message.Params["expression"].(string)
				answer := fakeEvaluate{value: true}
				if expression == pageSnapshotExpression {
					page.snapshots.Add(1)
					answer = snapshot
				} else if evaluate != nil {
					answer = evaluate(expression)
				}
				if answer.received != nil {
					answer.received()
				}
				if answer.closeConnection {
					return
				}
				if answer.noReply {
					continue
				}
				result = evaluateResult(answer)
			}
			if err := connection.WriteJSON(map[string]any{"id": message.ID, "type": "success", "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("连接假 BiDi: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	page.client = newBiDiClient(connection)
	return page
}

// evaluateResult 按 BiDi script.evaluate 的结果格式编码页面返回值或异常
func evaluateResult(answer fakeEvaluate) map[string]any {
	if answer.exception != "" {
		return map[string]any{"type": "exception", "exceptionDetails": map[string]any{"text": "Error: " + answer.exception}}
	}
	switch value := answer.value.(type) {
	case string:
		return map[string]any{"type": "success", "result": map[string]any{"type": "string", "value": value}}
	case bool:
		return map[string]any{"type": "success", "result": map[string]any{"type": "boolean", "value": value}}
	default:
		return map[string]any{"type": "success", "result": map[string]any{"type": "number", "value": value}}
	}
}

// checkFakeSnapshot 确认错误末尾带着整理后的假页面快照
func checkFakeSnapshot(t *testing.T, message string) {
	t.Helper()
	decoded := snapshotJSON(t, message)
	if decoded["url"] != "aistudio.google.com/prompts/new_chat?model=gemini-flash-latest" || decoded["title"] != "Google AI Studio ¦ Beta" ||
		decoded["body"] != "Signed in as li***@***" {
		t.Fatalf("快照整理不对: %v", decoded)
	}
	// url= 字段照旧是完整地址，只检查快照部分
	snapshot := message[strings.LastIndex(message, " | 页面="):]
	if strings.Contains(snapshot, "lisa@") || strings.Contains(snapshot, "authuser") {
		t.Fatalf("快照里不应有邮箱与多余的地址参数: %s", snapshot)
	}
}

// TestBootstrapFailuresCarryPageSnapshot bootstrap 各页面类失败在原错误末尾带上页面快照，原错误前缀逐字不变；
// 初始化 ctx 已过期时快照与 url 仍能取到（独立的短上下文）；快照带导航 AI Studio 至今的秒数 since（与页面的 age 对照）
func TestBootstrapFailuresCarryPageSnapshot(t *testing.T) {
	const prompt = "AIStudio2API bootstrap test"
	cases := []struct {
		name string
		// fail 返回非空回答时替换该表达式的正常回答
		fail   func(expression string) (fakeEvaluate, bool)
		prefix string
	}{
		{
			name:   "输入框未就绪",
			fail:   whenExpression(workerPageReadyExpression, fakeEvaluate{value: false}),
			prefix: "AI Studio 输入框未就绪 url=" + fakePageURL + ": ",
		},
		{
			name:   "填写提示词",
			fail:   whenExpression(fillPromptExpression(prompt), fakeEvaluate{exception: "提示词输入框不存在"}),
			prefix: `填写 bootstrap 提示词失败 value="" err=页面表达式异常: `,
		},
		{
			name:   "Run 按钮未启用",
			fail:   whenExpression(runButtonEnabledExpression, fakeEvaluate{value: false}),
			prefix: "等待官网 Run 按钮启用: ",
		},
		{
			name:   "提交提示词",
			fail:   whenExpression(submitPromptExpression, fakeEvaluate{exception: "官网 Run 按钮不存在"}),
			prefix: "提交官网提示词: 页面表达式异常: ",
		},
		{
			name:   "WAA service 未暴露",
			fail:   whenExpression("Boolean(window.__aistudioWaaService)", fakeEvaluate{value: false}),
			prefix: "官网 WAA service 未暴露: ",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			page := newFakeBiDiPage(t, func(expression string) fakeEvaluate {
				if answer, ok := item.fail(expression); ok {
					return answer
				}
				return healthyBootstrapPage(expression, prompt)
			})
			worker := &Worker{client: page.client}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storageState{
				Cookies: []storageCookie{{Name: "SID", Value: "x", Domain: ".google.com", Path: "/", Secure: true}},
			})
			if err == nil {
				t.Fatal("bootstrap 应失败")
			}
			if !strings.HasPrefix(err.Error(), item.prefix) {
				t.Fatalf("原错误前缀应逐字不变\n得到 %s\n期望前缀 %s", err.Error(), item.prefix)
			}
			checkFakeSnapshot(t, err.Error())
			if since, ok := snapshotJSON(t, err.Error())["since"].(float64); !ok || since < 0 || since > 5 {
				t.Fatalf("快照应带导航至今的秒数 since: %s", err.Error())
			}
			if page.snapshots.Load() != 1 {
				t.Fatalf("失败时应采集 1 次快照，得到 %d", page.snapshots.Load())
			}
		})
	}
}

// whenExpression 在页面执行 target 表达式时返回 answer
func whenExpression(target string, answer fakeEvaluate) func(string) (fakeEvaluate, bool) {
	return func(expression string) (fakeEvaluate, bool) {
		return answer, expression == target
	}
}

// healthyBootstrapPage 为一切正常的官网页面对 bootstrap 各步表达式的回答
func healthyBootstrapPage(expression string, prompt string) fakeEvaluate {
	switch expression {
	case "location.href":
		return fakeEvaluate{value: fakePageURL}
	case dismissOverlaysExpression:
		return fakeEvaluate{value: 0}
	case snapshotHookExpression():
		return fakeEvaluate{value: "snapshotKey"}
	case fillPromptExpression(prompt):
		return fakeEvaluate{value: prompt}
	}
	return fakeEvaluate{value: true}
}

// TestProofFailureCarriesPageSnapshot 运行中 Worker 同步官网 prompt 失败时带上页面快照；成功与请求已取消时不多做 evaluate
func TestProofFailureCarriesPageSnapshot(t *testing.T) {
	const prompt = "hello"
	t.Run("同步失败", func(t *testing.T) {
		page := newFakeBiDiPage(t, func(expression string) fakeEvaluate {
			if expression == fillPromptExpression(prompt) {
				return fakeEvaluate{exception: "提示词输入框不存在"}
			}
			return fakeEvaluate{value: true}
		})
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		// Worker 一小时前导航 AI Studio：since 约 3600，与页面的 age（118）对照即可看出页面中途被替换过
		page.client.navigatedAt.Store(time.Now().Add(-time.Hour).UnixNano())
		_, err := worker.Proof(context.Background(), "digest", prompt)
		if err == nil || !strings.HasPrefix(err.Error(), "同步官网 prompt: 页面表达式异常: ") {
			t.Fatalf("原错误前缀应逐字不变: %v", err)
		}
		checkFakeSnapshot(t, err.Error())
		decoded := snapshotJSON(t, err.Error())
		if since, _ := decoded["since"].(float64); since < 3599 || since > 3602 || decoded["age"] != float64(118) {
			t.Fatalf("运行中的快照应同时带 age 与 since: %v", decoded)
		}
		if page.snapshots.Load() != 1 {
			t.Fatalf("失败时应采集 1 次快照，得到 %d", page.snapshots.Load())
		}
	})
	t.Run("成功", func(t *testing.T) {
		page := newFakeBiDiPage(t, func(expression string) fakeEvaluate {
			if expression == fillPromptExpression(prompt) {
				return fakeEvaluate{value: prompt}
			}
			return fakeEvaluate{value: "!proof"}
		})
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		proof, err := worker.Proof(context.Background(), "digest", prompt)
		if err != nil || proof != "!proof" {
			t.Fatalf("Proof = %q, %v", proof, err)
		}
		if page.snapshots.Load() != 0 {
			t.Fatalf("成功时不应采集快照，得到 %d 次", page.snapshots.Load())
		}
	})
	t.Run("请求已取消", func(t *testing.T) {
		page := newFakeBiDiPage(t, nil)
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := worker.Proof(ctx, "digest", prompt)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "页面=") {
			t.Fatalf("请求已取消时应原样返回、不带快照: %v", err)
		}
		if page.snapshots.Load() != 0 {
			t.Fatalf("请求已取消时不应采集快照，得到 %d 次", page.snapshots.Load())
		}
	})
	t.Run("浏览器已断开", func(t *testing.T) {
		page := newFakeBiDiPage(t, func(string) fakeEvaluate { return fakeEvaluate{closeConnection: true} })
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		_, err := worker.Proof(context.Background(), "digest", prompt)
		if err == nil || !strings.HasPrefix(err.Error(), "同步官网 prompt: BiDi 连接已断开: ") ||
			!strings.Contains(err.Error(), " | 页面=快照失败: BiDi 连接已断开: ") {
			t.Fatalf("快照失败时应保留原错误并追加简短原因: %v", err)
		}
	})
}

// TestProofSnapshotBoundedByRequest 运行中 Worker 的快照受请求 ctx 约束：浏览器不回答快照时快照在请求期限之前结束，
// 返回时请求还没到期（Prepare 照常判定 Worker 失败并重建）；剩余时间不足时不采集；快照期间请求被取消时立即返回原错误
func TestProofSnapshotBoundedByRequest(t *testing.T) {
	const prompt = "hello"
	failFill := func(expression string) fakeEvaluate {
		if expression == fillPromptExpression(prompt) {
			return fakeEvaluate{exception: "提示词输入框不存在"}
		}
		return fakeEvaluate{value: true}
	}
	t.Run("快照不回应", func(t *testing.T) {
		page := newFakeBiDiPageWithSnapshot(t, failFill, fakeEvaluate{noReply: true})
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_, err := worker.Proof(ctx, "digest", prompt)
		if ctx.Err() != nil {
			t.Fatalf("快照把请求拖过了期限: %v", err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "同步官网 prompt: 页面表达式异常: ") ||
			!strings.Contains(err.Error(), " | 页面=快照失败: ") {
			t.Fatalf("应保留原错误并追加快照失败的原因: %v", err)
		}
		if page.snapshots.Load() != 1 {
			t.Fatalf("应请求 1 次快照，得到 %d", page.snapshots.Load())
		}
	})
	t.Run("剩余时间不足", func(t *testing.T) {
		page := newFakeBiDiPage(t, failFill)
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), pageSnapshotRequestMargin+pageSnapshotMinimum/2)
		defer cancel()
		_, err := worker.Proof(ctx, "digest", prompt)
		if err == nil || strings.Contains(err.Error(), "页面=") {
			t.Fatalf("剩余时间不足时应原样返回、不带快照: %v", err)
		}
		if page.snapshots.Load() != 0 {
			t.Fatalf("剩余时间不足时不应采集快照，得到 %d 次", page.snapshots.Load())
		}
	})
	t.Run("快照期间请求取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		page := newFakeBiDiPageWithSnapshot(t, failFill, fakeEvaluate{noReply: true, received: cancel})
		worker := &Worker{client: page.client, contextID: "ctx-1"}
		started := time.Now()
		_, err := worker.Proof(ctx, "digest", prompt)
		if elapsed := time.Since(started); elapsed >= pageSnapshotTimeout {
			t.Fatalf("请求取消后快照应立即结束，用了 %s", elapsed)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "同步官网 prompt: 页面表达式异常: ") || strings.Contains(err.Error(), "页面=") {
			t.Fatalf("快照期间请求取消时应原样返回原错误: %v", err)
		}
		if page.snapshots.Load() != 1 {
			t.Fatalf("应请求 1 次快照，得到 %d", page.snapshots.Load())
		}
	})
}
