package camoufoxnative

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/waa/goja"
	"github.com/gorilla/websocket"
)

// fakeBiDi 是只回复命令的 WebDriver BiDi 服务端：记录收到的命令，按 reply 返回 success 结果
type fakeBiDi struct {
	mu       sync.Mutex
	commands []map[string]any
	reply    func(method string, params map[string]any) map[string]any
}

func (fake *fakeBiDi) recorded() []map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]map[string]any(nil), fake.commands...)
}

// newFakeBiDiClient 启动 fakeBiDi 并返回连到它的客户端
func newFakeBiDiClient(t *testing.T, reply func(method string, params map[string]any) map[string]any) (*bidiClient, *fakeBiDi) {
	t.Helper()
	fake := &fakeBiDi{reply: reply}
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var command map[string]any
			if err := connection.ReadJSON(&command); err != nil {
				return
			}
			fake.mu.Lock()
			fake.commands = append(fake.commands, command)
			fake.mu.Unlock()
			method, _ := command["method"].(string)
			params, _ := command["params"].(map[string]any)
			result := map[string]any{}
			if fake.reply != nil {
				result = fake.reply(method, params)
			}
			if err := connection.WriteJSON(map[string]any{"id": command["id"], "type": "success", "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return newBiDiClient(connection), fake
}

// runJS 用内置的 JS 引擎执行页面脚本，返回脚本经 emit 输出的内容（只用于核对页面脚本的语义，不启动浏览器）
func runJS(t *testing.T, script string) string {
	t.Helper()
	vm := goja.New()
	var output []string
	if err := vm.Set("emit", func(value string) { output = append(output, value) }); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("执行页面脚本失败: %v", err)
	}
	return strings.Join(output, "\n")
}

// fakePageStorage 为模拟页面 localStorage 与 location 的前置脚本
func fakePageStorage(origin string, initial map[string]string) string {
	encoded, _ := json.Marshal(initial)
	originJSON, _ := json.Marshal(origin)
	return `const store = new Map(Object.entries(` + string(encoded) + `));
globalThis.location = {origin: ` + string(originJSON) + `};
globalThis.localStorage = {
  getItem: name => store.has(name) ? store.get(name) : null,
  setItem: (name, value) => { store.set(String(name), String(value)); },
  key: index => [...store.keys()][index] ?? null,
  get length() { return store.size; },
};
`
}

var savedOrigins = []storageOrigin{
	{Origin: aiStudioOrigin, LocalStorage: []localStorageItem{
		{Name: "aiStudioUserPreference", Value: `{"format":"old"}`},
		{Name: "xap-tour", Value: "1"},
	}},
	{Origin: "https://accounts.google.com", LocalStorage: []localStorageItem{{Name: "other", Value: "x"}}},
}

// TestLocalStoragePreloadOnlyFillsMissing preload 只写入页面里还没有的键：第一次载入（新 profile，全部缺少）与原来一样全部写入；
// 网页自己升级过的值在刷新后保留；其他 origin 的页面不写
func TestLocalStoragePreloadOnlyFillsMissing(t *testing.T) {
	function, err := localStoragePreloadFunction(savedOrigins)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(function, "if (localStorage.getItem(name) === null) localStorage.setItem(name, value);") {
		t.Fatalf("preload 应先 getItem、为 null 才 setItem:\n%s", function)
	}
	if strings.Count(function, "setItem") != 1 {
		t.Fatalf("preload 只能有一处带条件的 setItem:\n%s", function)
	}
	run := func(origin string, initial map[string]string) map[string]string {
		output := runJS(t, fakePageStorage(origin, initial)+"("+function+")();\nemit(JSON.stringify(Object.fromEntries(store)));\n")
		var store map[string]string
		if err := json.Unmarshal([]byte(output), &store); err != nil {
			t.Fatalf("解析脚本输出 %q: %v", output, err)
		}
		return store
	}
	first := run(aiStudioOrigin, map[string]string{})
	if first["aiStudioUserPreference"] != `{"format":"old"}` || first["xap-tour"] != "1" || len(first) != 2 {
		t.Fatalf("第一次载入应写入全部保存的键，实际 %v", first)
	}
	upgraded := run(aiStudioOrigin, map[string]string{"aiStudioUserPreference": `{"format":"new"}`, "selectedProject": "p"})
	if upgraded["aiStudioUserPreference"] != `{"format":"new"}` || upgraded["selectedProject"] != "p" || upgraded["xap-tour"] != "1" {
		t.Fatalf("网页升级过的值应保留、缺少的键补上，实际 %v", upgraded)
	}
	other := run("https://example.com", map[string]string{})
	if len(other) != 0 {
		t.Fatalf("没有保存值的 origin 不应写入，实际 %v", other)
	}
}

// TestInstallLocalStorageSendsPreload installLocalStorage 经 script.addPreloadScript 安装只补缺的函数，限定在指定的 tab
func TestInstallLocalStorageSendsPreload(t *testing.T) {
	client, fake := newFakeBiDiClient(t, func(string, map[string]any) map[string]any {
		return map[string]any{"script": "preload-1"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.installLocalStorage(ctx, "tab-1", savedOrigins); err != nil {
		t.Fatal(err)
	}
	commands := fake.recorded()
	if len(commands) != 1 || commands[0]["method"] != "script.addPreloadScript" {
		t.Fatalf("命令 = %v，期望一条 script.addPreloadScript", commands)
	}
	params, _ := commands[0]["params"].(map[string]any)
	want, _ := localStoragePreloadFunction(savedOrigins)
	if params["functionDeclaration"] != want {
		t.Fatalf("preload 函数 = %v，期望 %s", params["functionDeclaration"], want)
	}
	contexts, _ := params["contexts"].([]any)
	if len(contexts) != 1 || contexts[0] != "tab-1" {
		t.Fatalf("preload contexts = %v", params["contexts"])
	}
}

// TestStorageLocalStorageExport Worker 就绪后用只读的 script.evaluate 导出当前 AI Studio 页面的 localStorage；
// 页面不在 AI Studio 时报错，错误不含值；runtime 已关闭时报错
func TestStorageLocalStorageExport(t *testing.T) {
	for _, forbidden := range []string{"setItem", "removeItem", "clear(", "location.href ="} {
		if strings.Contains(localStorageExportExpression, forbidden) {
			t.Fatalf("导出表达式应只读，不能含 %q", forbidden)
		}
	}
	// page 为页面返回的导出结果；fake 服务端在自己的协程里读取，经 socket 的先后关系竞态检测看不到，用原子值传递
	var page atomic.Value
	page.Store(`{"origin":"https://aistudio.google.com","localStorage":[{"name":"aiStudioUserPreference","value":"SECRET-PREF"},{"name":"selectedProject","value":"p"}]}`)
	client, fake := newFakeBiDiClient(t, func(method string, _ map[string]any) map[string]any {
		if method != "script.evaluate" {
			return map[string]any{}
		}
		return map[string]any{"type": "success", "realm": "r", "result": map[string]any{"type": "string", "value": page.Load()}}
	})
	worker := &Worker{client: client, contextID: "tab-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	encoded, err := worker.StorageLocalStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var origin storageOrigin
	if err := json.Unmarshal(encoded, &origin); err != nil {
		t.Fatal(err)
	}
	if origin.Origin != aiStudioOrigin || len(origin.LocalStorage) != 2 || origin.LocalStorage[0].Value != "SECRET-PREF" {
		t.Fatalf("导出结果 = %+v", origin)
	}
	commands := fake.recorded()
	params, _ := commands[0]["params"].(map[string]any)
	if commands[0]["method"] != "script.evaluate" || params["expression"] != localStorageExportExpression {
		t.Fatalf("导出应只执行一次只读的 script.evaluate，实际 %v", commands)
	}

	page.Store(`{"origin":"https://accounts.google.com","localStorage":[{"name":"k","value":"SECRET-PREF"}]}`)
	if _, err := worker.StorageLocalStorage(ctx); err == nil || strings.Contains(err.Error(), "SECRET-PREF") {
		t.Fatalf("页面不在 AI Studio 时应报错且不含值，实际 %v", err)
	}
	page.Store(`not json SECRET-PREF`)
	if _, err := worker.StorageLocalStorage(ctx); err == nil || strings.Contains(err.Error(), "SECRET-PREF") {
		t.Fatalf("结果无法解析时应报错且不含值，实际 %v", err)
	}
	worker.closed = true
	if _, err := worker.StorageLocalStorage(ctx); err == nil {
		t.Fatal("runtime 已关闭时应报错")
	}
}

// TestLocalStorageExportExpressionSemantics 导出表达式按键名排序列出全部项（含与 Storage 自带属性同名的键），不改动页面存储
func TestLocalStorageExportExpressionSemantics(t *testing.T) {
	output := runJS(t, fakePageStorage(aiStudioOrigin, map[string]string{"b": "2", "length": "L", "a": "1"})+
		"const before = JSON.stringify([...store]);\nconst result = "+localStorageExportExpression+";\n"+
		"if (JSON.stringify([...store]) !== before) throw new Error('导出改动了存储');\nemit(result);\n")
	var origin storageOrigin
	if err := json.Unmarshal([]byte(output), &origin); err != nil {
		t.Fatalf("解析脚本输出 %q: %v", output, err)
	}
	names := make([]string, 0, len(origin.LocalStorage))
	for _, item := range origin.LocalStorage {
		names = append(names, item.Name+"="+item.Value)
	}
	if origin.Origin != aiStudioOrigin || strings.Join(names, ",") != "a=1,b=2,length=L" {
		t.Fatalf("导出结果 = %+v", origin)
	}
}
