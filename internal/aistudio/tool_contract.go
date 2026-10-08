package aistudio

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolContractError 表示上游回复违反了客户端显式要求的工具约束（必须调用、指定函数、strict 参数）
type ToolContractError struct {
	Detail string
}

// Error 返回违约原因
func (e *ToolContractError) Error() string {
	return "上游回复不满足工具约束: " + e.Detail
}

// toolContract 保存客户端显式要求的调用模式与参数契约
type toolContract struct {
	required   bool
	single     bool
	names      map[string]bool
	schemas    map[string]*jsonschema.Schema
	serverSide bool
}

// prepareToolRequest 把工具选择与约束转换为共同生成请求，返回需要在事件流上核对的契约（不需要时为 nil）。
//
// Build 通道按 functionCallingConfig 原生执行必须调用与指定函数；Playground 没有对应字段，
// 改为在系统指令里写明要求，再由 forward 核对上游实际返回的调用
func prepareToolRequest(request GenerateRequest) (GenerateRequest, *toolContract, error) {
	config := request.Tools.ToolConfig
	if config.Mode == "none" {
		return request, nil, nil
	}
	var hints []string
	if search := request.Tools.GoogleSearch; search != nil {
		if search.ContextSize != "" {
			hints = append(hints, "Use "+search.ContextSize+" search context depth.")
		}
		if len(search.UserLocation) > 0 {
			hints = append(hints, "Approximate search user location: "+string(search.UserLocation))
		}
		if len(search.AllowedDomains) > 0 {
			hints = append(hints, "Restrict web searches to these domains using site: queries: "+strings.Join(search.AllowedDomains, ", "))
		}
	}
	contract := &toolContract{
		required: config.Mode == "required",
		single:   config.ParallelCalls != nil && !*config.ParallelCalls && len(request.Tools.Functions) > 0,
		names:    map[string]bool{}, schemas: map[string]*jsonschema.Schema{},
	}
	functions := make([]FunctionDeclaration, 0, len(request.Tools.Functions))
	for _, declaration := range request.Tools.Functions {
		if len(config.AllowedFunctionNames) > 0 && !slices.Contains(config.AllowedFunctionNames, declaration.Name) {
			continue
		}
		if contract.names[declaration.Name] {
			return request, nil, fmt.Errorf("function %q 重复", declaration.Name)
		}
		if config.Mode == "validated" {
			declaration.Strict = true
		}
		contract.names[declaration.Name] = true
		functions = append(functions, declaration)
		if !declaration.Strict {
			continue
		}
		schema, err := compileToolSchema(declaration.Parameters)
		if err != nil {
			// 客户端的 Schema 本身无法编译（如引用了未提供的定义）：不阻止请求，只是不再核对这个函数的参数
			slog.Warn("strict 工具 Schema 无法编译，跳过参数校验", "function", declaration.Name, "error", err)
			continue
		}
		contract.schemas[declaration.Name] = schema
	}
	for _, name := range config.AllowedFunctionNames {
		if !contract.names[name] {
			return request, nil, fmt.Errorf("tool choice 引用了未声明函数 %q", name)
		}
	}
	contract.serverSide = len(config.AllowedFunctionNames) == 0 && (len(request.Tools.Google) > 0 || request.Tools.GoogleSearch != nil)
	if contract.required && len(functions) == 0 && !contract.serverSide {
		return request, nil, fmt.Errorf("required tool choice 需要至少一个工具")
	}
	if len(config.AllowedFunctionNames) > 0 || config.Mode == "validated" {
		// 指定函数时只发送被选中的函数，上游只能调用它们
		request.Tools.Functions = functions
	}
	if contract.required {
		names := make([]string, 0, len(functions)+len(request.Tools.Google)+1)
		for _, declaration := range functions {
			names = append(names, declaration.Name)
		}
		if contract.serverSide {
			names = append(names, request.Tools.Google...)
			if request.Tools.GoogleSearch != nil && !slices.Contains(names, "google_search") {
				names = append(names, "google_search")
			}
		}
		hints = append(hints, "Use one of these tools in this response: "+strings.Join(names, ", ")+". Use the tool before answering.")
	}
	if contract.single {
		hints = append(hints, "Call at most one function in this response.")
	}
	if len(hints) > 0 {
		request.System = strings.TrimSpace(strings.Join(append([]string{request.System}, hints...), "\n"))
	}
	if !contract.required && len(config.AllowedFunctionNames) == 0 && len(contract.schemas) == 0 && !contract.single {
		contract = nil
	}
	return request, contract, nil
}

// compileToolSchema 编译 strict 函数的参数 Schema，用于核对上游返回的参数
func compileToolSchema(parameters []byte) (*jsonschema.Schema, error) {
	raw := normalizeFunctionParameters(parameters)
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("schema 超过 %d 字节上限", maxSchemaBytes)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(toolSchemaLoader{})
	compiler.UseRegexpEngine(compileToolPattern)
	const resource = "https://tools.invalid/parameters.json"
	if err := compiler.AddResource(resource, validationSchema(document, 0)); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

// toolPattern 使用 ECMA 正则核对 JSON Schema 字符串
type toolPattern struct {
	*regexp2.Regexp
}

// MatchString 返回字符串是否满足参数模式
func (pattern toolPattern) MatchString(value string) bool {
	matched, err := pattern.Regexp.MatchString(value)
	return err == nil && matched
}

// compileToolPattern 编译 JSON Schema 的 ECMA 模式；匹配超时限制回溯过多的模式
func compileToolPattern(value string) (jsonschema.Regexp, error) {
	pattern, err := regexp2.Compile(value, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	pattern.MatchTimeout = time.Second
	return toolPattern{pattern}, nil
}

// toolSchemaLoader 不加载外部 Schema：引用只能指向请求自带的定义
type toolSchemaLoader struct{}

// Load 返回外部 Schema 引用错误
func (toolSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema 引用 %q 未包含在工具声明中", url)
}

// validationSchema 把 Gemini 写法（大写类型名、nullable）转换为标准 JSON Schema 校验结构
func validationSchema(value any, depth int) any {
	schema, ok := value.(map[string]any)
	if !ok || depth > maxSchemaDepth {
		return value
	}
	if name, ok := schema["type"].(string); ok {
		schema["type"] = strings.ToLower(name)
	}
	if names, ok := schema["type"].([]any); ok {
		for index, name := range names {
			if name, ok := name.(string); ok {
				names[index] = strings.ToLower(name)
			}
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if entries, ok := schema[key].(map[string]any); ok {
			for name, child := range entries {
				entries[name] = validationSchema(child, depth+1)
			}
		}
	}
	for _, key := range []string{"items", "not", "additionalProperties", "contains", "if", "then", "else", "unevaluatedProperties", "unevaluatedItems"} {
		if child, ok := schema[key]; ok {
			schema[key] = validationSchema(child, depth+1)
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if entries, ok := schema[key].([]any); ok {
			for index, child := range entries {
				entries[index] = validationSchema(child, depth+1)
			}
		}
	}
	if schema["nullable"] == true {
		delete(schema, "nullable")
		return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
	}
	return schema
}

// forward 在工具调用交给客户端前核对契约：调用了未选择的函数、strict 参数不符、必须调用却没有调用时
// 以 ToolContractError 结束事件流；要求单次调用时只保留第一个函数调用
func (contract *toolContract) forward(ctx context.Context, source <-chan Event) <-chan Event {
	if contract == nil {
		return source
	}
	destination := make(chan Event, 8)
	go func() {
		defer close(destination)
		calls := 0
		serverSideUsed := false
		failed := false
		for event := range source {
			if failed {
				continue
			}
			if event.Kind == EventGrounding || event.Kind == EventExecutableCode || event.Kind == EventCodeExecutionResult {
				serverSideUsed = true
			}
			var violation string
			if event.Kind == EventToolCall && event.ToolCall != nil {
				calls++
				call := event.ToolCall
				if contract.single && calls > 1 {
					// 要求单次调用而上游返回了多个：只保留第一个，其余丢弃
					continue
				}
				if !contract.names[call.Name] {
					violation = fmt.Sprintf("上游调用了未选择的函数 %q", call.Name)
				} else if schema := contract.schemas[call.Name]; schema != nil {
					if err := validateToolArguments(schema, call.Arguments); err != nil {
						violation = fmt.Sprintf("函数 %q 的参数不符合 strict Schema: %v", call.Name, err)
					}
				}
			}
			if event.Kind == EventFinish && contract.required && calls == 0 && !(contract.serverSide && serverSideUsed) {
				violation = "要求调用工具，上游没有返回工具调用"
			}
			if violation != "" {
				event = Event{Kind: EventError, Err: &ToolContractError{Detail: violation}}
				failed = true
			}
			select {
			case destination <- event:
			case <-ctx.Done():
				// 客户端已断开：继续读完上游，让生产方正常结束
				for range source {
				}
				return
			}
		}
	}()
	return destination
}

func validateToolArguments(schema *jsonschema.Schema, arguments []byte) error {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = []byte(`{}`)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return err
	}
	return schema.Validate(value)
}
