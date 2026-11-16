package camoufoxnative

import (
	"encoding/json"
	"strings"
	"testing"
)

// 测试用的假 Key（不是真实 Key）
var (
	fakeNewKey     = "AQ.TESTKEY" + strings.Repeat("x", 24)
	fakeClassicKey = "AIza" + "TESTKEY" + strings.Repeat("0", 28)
)

// TestExtractStudioKeysNestedArrays 带 XSSI 前缀的 protobuf-JSON 嵌套数组：Key 与同一条记录里的 projects/<数字> 配对，
// 显示名取同一记录里的普通文字；另一条记录的项目不会串到这条 Key 上
func TestExtractStudioKeysNestedArrays(t *testing.T) {
	body := `)]}'
[[["projects/111/locations/global/keys/abc",null,"` + fakeNewKey + `",["projects/111","Default Gemini Project"],[1730000000,0]],
["projects/222/locations/global/keys/def",null,"` + fakeClassicKey + `",["projects/222","Other project"]]],null]`
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 2 {
		t.Fatalf("keys = %d，期望 2", len(keys))
	}
	if keys[0].Key != fakeNewKey || keys[0].ProjectName != "projects/111" || keys[0].ProjectNumber != "111" {
		t.Fatalf("第一条 = %+v", redactCandidate(keys[0]))
	}
	if keys[0].ProjectDisplay != "Default Gemini Project" {
		t.Fatalf("显示名 = %q", keys[0].ProjectDisplay)
	}
	if keys[1].Key != fakeClassicKey || keys[1].ProjectNumber != "222" {
		t.Fatalf("第二条 = %+v", redactCandidate(keys[1]))
	}
}

// TestExtractStudioKeysBatchExecute batchexecute 分块格式：每块前有长度行，Key 在再包一层的 JSON 字符串里
func TestExtractStudioKeysBatchExecute(t *testing.T) {
	inner, _ := json.Marshal([]any{[]any{[]any{"projects/333", fakeNewKey, "My Project"}}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "ListKeys", string(inner), nil, nil, "generic"}})
	body := ")]}'\n\n" + "123\n" + string(outer) + "\n25\n[[\"e\",4,null,null,1]]\n"
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 1 || keys[0].Key != fakeNewKey || keys[0].ProjectName != "projects/333" {
		t.Fatalf("keys = %+v", redactCandidates(keys))
	}
}

// TestExtractStudioKeysPlainTextFallback 不是 JSON 时按文本扫描，项目取最近的 projects/<数字>
func TestExtractStudioKeysPlainTextFallback(t *testing.T) {
	body := `garbage projects/9 ........ key=` + fakeClassicKey + ` projects/444 tail`
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 1 || keys[0].ProjectNumber != "444" {
		t.Fatalf("keys = %+v", redactCandidates(keys))
	}
	// 更长串里截出来的片段不算 Key
	if got := ExtractStudioKeys([]byte("x" + fakeClassicKey + "TOOLONG")); len(got) != 0 {
		t.Fatalf("截断片段被当成 Key: %d", len(got))
	}
}

// TestExtractStudioKeysNone 没有 Key 的响应返回空
func TestExtractStudioKeysNone(t *testing.T) {
	if keys := ExtractStudioKeys([]byte(`)]}'` + "\n" + `[[["projects/1","name"]]]`)); len(keys) != 0 {
		t.Fatalf("keys = %d", len(keys))
	}
}

// TestRedactStudioKeys 错误与日志文本里的 Key 只留末四位
func TestRedactStudioKeys(t *testing.T) {
	text := RedactStudioKeys("失败: " + fakeNewKey + " / " + fakeClassicKey)
	if strings.Contains(text, fakeNewKey) || strings.Contains(text, fakeClassicKey) || strings.Contains(text, "TESTKEY") {
		t.Fatalf("没有打码: %s", text)
	}
	if !strings.Contains(text, "<key:…xxxx>") {
		t.Fatalf("缺少末四位: %s", text)
	}
	keyErr := &StudioKeyError{Reason: "r", Cause: errorString("页面 " + fakeNewKey)}
	if strings.Contains(keyErr.Error(), fakeNewKey) {
		t.Fatalf("StudioKeyError 泄露 Key: %s", keyErr.Error())
	}
}

// TestChooseStudioKeyAfterCreate 创建之后只认创建前没有的新 Key
func TestChooseStudioKeyAfterCreate(t *testing.T) {
	keys := []StudioKeyCandidate{{Key: fakeClassicKey}, {Key: fakeNewKey}}
	chosen, ok := chooseStudioKey(keys, map[string]bool{fakeClassicKey: true}, true)
	if !ok || chosen.Key != fakeNewKey {
		t.Fatalf("chosen = %+v", redactCandidate(chosen))
	}
	if _, ok := chooseStudioKey(keys[:1], map[string]bool{fakeClassicKey: true}, true); ok {
		t.Fatal("只有旧 Key 时不应选中")
	}
}

type errorString string

func (err errorString) Error() string { return string(err) }

func redactCandidate(candidate StudioKeyCandidate) StudioKeyCandidate {
	candidate.Key = RedactStudioKeys(candidate.Key)
	return candidate
}

func redactCandidates(keys []StudioKeyCandidate) []StudioKeyCandidate {
	out := make([]StudioKeyCandidate, len(keys))
	for index, key := range keys {
		out[index] = redactCandidate(key)
	}
	return out
}
