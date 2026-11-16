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

// TestExtractStudioProjectsOperation 建项目接口的长操作结果（Cloud Resource Manager 形态，合成数据）：编号、项目 ID 与显示名，
// 状态之类的大写枚举不当显示名
func TestExtractStudioProjectsOperation(t *testing.T) {
	body := `{"name":"operations/cp.7340000000000000000","done":true,"response":{"@type":"type.googleapis.com/google.cloudresourcemanager.v3.Project",` +
		`"name":"projects/123456789012","parent":"organizations/0","projectId":"gen-lang-client-0123456789","state":"ACTIVE",` +
		`"displayName":"Gemini Project","createTime":"2026-11-16T08:00:00.000Z"}}`
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 1 {
		t.Fatalf("projects = %+v", projects)
	}
	want := StudioProject{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"}
	if projects[0] != want {
		t.Fatalf("project = %+v，期望 %+v", projects[0], want)
	}
}

// TestExtractStudioProjectsNestedArrays 页面自己的项目列表（protobuf-JSON 嵌套数组、带 XSSI 前缀，合成数据）：每条记录取编号、
// 同一层的 gen-lang-client-<数字> 与显示名；大写的状态枚举不当显示名
func TestExtractStudioProjectsNestedArrays(t *testing.T) {
	body := ")]}'\n" + `[[["projects/123456789012","gen-lang-client-0123456789","ACTIVE","Gemini Project",null,[1763280000,0]],` +
		`["projects/210987654321","gen-lang-client-0987654321","Default Gemini Project"]]]`
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 2 || projects[0].Number != "123456789012" || projects[0].ID != "gen-lang-client-0123456789" ||
		projects[0].Display != "Gemini Project" || projects[1].Display != "Default Gemini Project" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestExtractStudioProjectsBatchExecute batchexecute 分块、项目在再包一层的 JSON 字符串里；Key 的资源名（projects/<数字>/…/keys/…）
// 也算出它所在的项目，与另一处的显示名合并
func TestExtractStudioProjectsBatchExecute(t *testing.T) {
	inner, _ := json.Marshal([]any{[]any{"projects/123456789012/locations/global/keys/abc", nil, []any{"projects/123456789012", "Gemini Project"}}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "CreateProject", string(inner), nil, nil, "generic"}})
	body := ")]}'\n\n" + "123\n" + string(outer) + "\n25\n[[\"e\",4,null,null,1]]\n"
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 1 || projects[0].Name != "projects/123456789012" || projects[0].Display != "Gemini Project" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestExtractStudioProjectsIgnoresNoise 没有项目、编号太短（页面脚本里偶然出现的短串）、前面紧接着字母的（subprojects/…）都不算；
// 不是 JSON 时按文本扫描
func TestExtractStudioProjectsIgnoresNoise(t *testing.T) {
	for _, body := range []string{`{"a":"b"}`, `["projects/12","x"]`, `var a="subprojects/1234567890";`} {
		if projects := ExtractStudioProjects([]byte(body)); len(projects) != 0 {
			t.Fatalf("%s: projects = %+v", body, projects)
		}
	}
	projects := ExtractStudioProjects([]byte(`<html>see projects/123456789012 and projects/123456789012/x</html>`))
	if len(projects) != 1 || projects[0].Number != "123456789012" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestMergeStudioProjects 同一编号只留一条，后来的结果补上 ID 与显示名
func TestMergeStudioProjects(t *testing.T) {
	merged := mergeStudioProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012"}},
		[]StudioProject{{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"},
			{Name: "projects/210987654321", Number: "210987654321"}})
	if len(merged) != 2 || merged[0].ID != "gen-lang-client-0123456789" || merged[0].Display != "Gemini Project" {
		t.Fatalf("merged = %+v", merged)
	}
}
