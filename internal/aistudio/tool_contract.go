package aistudio

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dlclark/regexp2/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolContractViolation 为工具约束违约的种类
type ToolContractViolation string

const (
	// ToolContractMissingCall 为要求调用工具（tool_choice 为 required/any 或指定了函数），上游到结束都没有调用
	ToolContractMissingCall ToolContractViolation = "missing_call"
	// ToolContractUnselectedFunction 为上游调用了没有选择的函数
	ToolContractUnselectedFunction ToolContractViolation = "unselected_function"
	// ToolContractSchemaMismatch 为 strict 函数的参数不符合 Schema
	ToolContractSchemaMismatch ToolContractViolation = "schema_mismatch"
)

// ToolContractError 表示上游回复违反了客户端显式要求的工具约束（必须调用、指定函数、strict 参数）
type ToolContractError struct {
	// Detail 为写进日志的中文原因
	Detail string
	// Kind 为违约的种类，为空时对外按笼统的说明返回
	Kind ToolContractViolation
	// Function 为参数不符合 strict Schema 的函数（客户端声明的函数名）
	Function string
	// Required 为要求调用的工具（被选中的函数，声明了内置工具时含内置工具名）；SingleFunction 为只要求这一个函数
	Required       []string
	SingleFunction bool
	// Attempts 为这次 Generate 内共生成的次数（含同账号重新生成），写进日志与对外说明
	Attempts int
	// Blocked 为要求调用却没有调用、上游以内容拦截类原因结束时的结束原因（safety、recitation 等）：不重新生成，对外说明写出它
	Blocked string
}

// Error 返回违约原因
func (e *ToolContractError) Error() string {
	message := "上游回复不满足工具约束: " + e.Detail
	if e.Attempts > 1 {
		message += fmt.Sprintf("（共生成 %d 次）", e.Attempts)
	}
	return message
}

// ToolContractPublicMessage 为没有具体违约种类时对外的说明（没有官方原文，措辞与官方风格一致）
const ToolContractPublicMessage = "The model response did not satisfy the requested tool constraints (tool_choice, parallel tool calls or strict schema). Please retry."

// PublicMessage 返回对客户端的英文说明：写明是哪种违约与共生成了几次，不含会让中转自动禁用渠道的字样。
// 未选择的函数名来自上游输出，不写进说明；客户端声明的函数名含这类单词时同样不写（见 publicToolName），日志照常写原名
func (e *ToolContractError) PublicMessage() string {
	tried := ""
	if e.Attempts > 1 {
		tried = fmt.Sprintf(" (tried %d times)", e.Attempts)
	}
	switch e.Kind {
	case ToolContractMissingCall:
		required := publicToolNames(e.Required)
		if e.Blocked != "" {
			// 内容拦截：重试同一内容多半同样被拦截，不写 Please retry
			stopped := "The model stopped (" + e.Blocked + ") without calling "
			switch {
			case len(required) == 1 && e.SingleFunction:
				return stopped + "the required function " + required[0] + tried + "."
			case len(required) > 0:
				return stopped + "any of the required tools (" + strings.Join(required, ", ") + ")" + tried + "."
			case len(e.Required) == 1 && e.SingleFunction:
				return stopped + "the required function" + tried + "."
			case len(e.Required) > 0:
				return stopped + "any of the required tools" + tried + "."
			}
			return stopped + "a required tool" + tried + "."
		}
		switch {
		case len(required) == 1 && e.SingleFunction:
			return "The model did not call the required function " + required[0] + tried + ". Please retry."
		case len(required) > 0:
			return "The model did not call any of the required tools (" + strings.Join(required, ", ") + ")" + tried + ". Please retry."
		case len(e.Required) == 1 && e.SingleFunction:
			return "The model did not call the required function" + tried + ". Please retry."
		case len(e.Required) > 0:
			return "The model did not call any of the required tools" + tried + ". Please retry."
		}
		return "The model did not call a required tool" + tried + ". Please retry."
	case ToolContractUnselectedFunction:
		return "The model called a function that was not selected" + tried + ". Please retry."
	case ToolContractSchemaMismatch:
		if name := publicToolName(e.Function); name != "" {
			return "The model's arguments for " + name + " did not match the strict schema" + tried + ". Please retry."
		}
		return "The model's function arguments did not match the strict schema" + tried + ". Please retry."
	}
	return ToolContractPublicMessage
}

// relayDisableWords 为 new-api、one-api 等中转据以自动禁用渠道的单词（它们按错误消息做子串匹配，不看状态码；one-api 一类单独匹配
// credit、balance 等单词）。比较时去掉空格、下划线、连字符等分隔符，按小写匹配
var relayDisableWords = []string{
	"permission", "denied", "quota", "insufficient", "credit", "balance", "billing", "unauthorized", "authorized", "forbidden",
	"disabled", "deactivated", "suspended", "terminated", "expired", "violation", "restricted", "organization", "apikey",
	"notallowed", "欠费",
}

// publicToolName 返回可以写进对外说明的工具名：客户端声明的函数名可以是任意单词（check_quota、get_balance、insufficient_stock_alert 等），
// 原样写进 502 的说明会让中转把整个渠道禁掉；名字里含 relayDisableWords 中的单词时返回空串，调用方改用不带名字的写法
func publicToolName(name string) string {
	var compact strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			compact.WriteRune(r)
		}
	}
	folded := compact.String()
	for _, word := range relayDisableWords {
		if strings.Contains(folded, word) {
			return ""
		}
	}
	return name
}

// publicToolNames 返回 names 都能写进对外说明时的原名单；其中任何一个不能写时为 nil（只列一部分会让人误以为只要求这几个）
func publicToolNames(names []string) []string {
	for _, name := range names {
		if publicToolName(name) == "" {
			return nil
		}
	}
	return names
}

// toolContract 保存客户端显式要求的调用模式与参数契约
type toolContract struct {
	required   bool
	single     bool
	names      map[string]bool
	schemas    map[string]*jsonschema.Schema
	serverSide bool
	// searchHints 为按搜索偏好追加的要求，toolNames 为必须调用时写进要求的工具名（被选中的函数，必须调用且声明了内置工具时含内置工具）。
	// 同账号重新生成时据此重写系统指令里的工具要求（见 tool_guard.go 的 toolHintsFor）
	searchHints []string
	toolNames   []string
	// functionNames 为被选中的函数（按声明顺序）
	functionNames []string
}

// ValidateToolChoice 检查工具选择本身是否成立（函数重名、指定了未声明的函数、要求调用却没有工具）。
// 不需要账号，可以在选号前调用：原先只在拿到账号之后检查，账号全部冷却或未就绪时，
// 参数错误会被报成 429/503 而不是 400
func ValidateToolChoice(tools Tools) error {
	config := tools.ToolConfig
	if config.Mode == "none" {
		return nil
	}
	names := make(map[string]bool, len(tools.Functions))
	for _, declaration := range tools.Functions {
		if len(config.AllowedFunctionNames) > 0 && !slices.Contains(config.AllowedFunctionNames, declaration.Name) {
			continue
		}
		if names[declaration.Name] {
			return fmt.Errorf("function %q 重复", declaration.Name)
		}
		names[declaration.Name] = true
	}
	for _, name := range config.AllowedFunctionNames {
		if !names[name] {
			return fmt.Errorf("tool choice 引用了未声明函数 %q", name)
		}
	}
	serverSide := len(config.AllowedFunctionNames) == 0 && (len(tools.Google) > 0 || tools.GoogleSearch != nil)
	if config.Mode == "required" && len(names) == 0 && !serverSide {
		return fmt.Errorf("required tool choice 需要至少一个工具")
	}
	return nil
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
	if err := ValidateToolChoice(request.Tools); err != nil {
		return request, nil, err
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
		if len(search.BlockedDomains) > 0 {
			hints = append(hints, "Exclude these domains from web searches using -site: queries and do not cite them: "+strings.Join(search.BlockedDomains, ", "))
		}
	}
	contract := &toolContract{
		required: config.Mode == "required",
		single:   config.ParallelCalls != nil && !*config.ParallelCalls && len(request.Tools.Functions) > 0,
		names:    map[string]bool{}, schemas: map[string]*jsonschema.Schema{},
		searchHints: slices.Clone(hints),
	}
	functions := make([]FunctionDeclaration, 0, len(request.Tools.Functions))
	for _, declaration := range request.Tools.Functions {
		if len(config.AllowedFunctionNames) > 0 && !slices.Contains(config.AllowedFunctionNames, declaration.Name) {
			continue
		}
		if config.Mode == "validated" {
			declaration.Strict = true
		}
		contract.names[declaration.Name] = true
		contract.functionNames = append(contract.functionNames, declaration.Name)
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
	contract.serverSide = len(config.AllowedFunctionNames) == 0 && (len(request.Tools.Google) > 0 || request.Tools.GoogleSearch != nil)
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
		contract.toolNames = names
		// 第一次发送就用明确的写法（第 1 级：必须调用、参数符合 Schema、不要只回复文字，见 toolHintsFor）：原先第一次发送写
		// “Use one of these tools in this response: …. Use the tool before answering.”，gemini-4 等走 Playground 时常不照做，
		// 几乎每次都要在同账号多跑一整次生成。Build 通道另有 functionCallingConfig 原生强制，多这句要求无害
		hints, _ = contract.toolHintsFor(contract.firstLevel())
	} else if contract.single {
		hints = append(hints, toolSingleHint)
	}
	if len(hints) > 0 {
		// 记下客户端的系统提示与追加的要求：发送前补做视频模型忽略系统提示时只去掉客户端的部分（见 ignoreOmniSystemPromptBeforeSend）
		request.clientSystem, request.toolHints = request.System, strings.Join(hints, "\n")
		request.System = joinToolHints(request.System, request.toolHints)
	}
	if !contract.required && len(config.AllowedFunctionNames) == 0 && len(contract.schemas) == 0 && !contract.single {
		contract = nil
	}
	// 契约随请求交给 Client.Generate：Playground 通道在解码循环里核对（必须调用时扣住事件，违约且还没交出事件时同账号重新生成，
	// 见 tool_guard.go），Build 与 Interaction 通道照旧经 forward 核对
	request.toolContract = contract
	return request, contract, nil
}

// toolSingleHint 为要求单次调用时追加的要求
const toolSingleHint = "Call at most one function in this response."

// firstLevel 返回第一次发送的工具要求级别（见 toolHintsFor）：必须调用时为 1（第一次发送就写明必须调用），其余为 0
// （auto 模式的 strict 或单次调用照旧只写搜索偏好与单次调用，与原先逐字节相同）
func (contract *toolContract) firstLevel() int {
	if contract != nil && contract.required {
		return 1
	}
	return 0
}

// joinToolHints 把工具要求（以换行连接）追加在系统提示之后
func joinToolHints(system string, hints string) string {
	return strings.TrimSpace(system + "\n" + hints)
}

// compileToolSchema 编译 strict 函数的参数 Schema，用于核对上游返回的参数
func compileToolSchema(parameters []byte) (*jsonschema.Schema, error) {
	raw := normalizeFunctionParameters(parameters)
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("schema 超过 %d 字节上限", maxSchemaBytes)
	}
	// 与发给上游的 Schema 一致：required 列出的未定义属性补上 {}，否则 additionalProperties:false 时这些必填属性永远无法满足
	raw = defineRequiredProperties(raw)
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
		// 核对时 panic：以错误事件结束本请求并读完上游，不让进程退出
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			err := decodePanicError("核对工具调用契约", value)
			select {
			case destination <- Event{Kind: EventError, Err: err}:
			case <-ctx.Done():
			}
			go func() {
				for range source {
				}
			}()
		}()
		for event := range source {
			if failed {
				continue
			}
			if event.Kind == EventGrounding || event.Kind == EventExecutableCode || event.Kind == EventCodeExecutionResult {
				serverSideUsed = true
			}
			var violation *ToolContractError
			if event.Kind == EventToolCall && event.ToolCall != nil {
				calls++
				if contract.single && calls > 1 {
					// 要求单次调用而上游返回了多个：只保留第一个，其余丢弃
					continue
				}
				violation = contract.check(*event.ToolCall)
			}
			if event.Kind == EventFinish && contract.required && calls == 0 && !(contract.serverSide && serverSideUsed) {
				violation = contract.missingCall("")
			}
			if violation != nil {
				event = Event{Kind: EventError, Err: violation}
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

// check 核对一个函数调用：调用了未选择的函数、strict 参数不符时返回违约
func (contract *toolContract) check(call FunctionCall) *ToolContractError {
	if !contract.names[call.Name] {
		return &ToolContractError{Kind: ToolContractUnselectedFunction, Detail: fmt.Sprintf("上游调用了未选择的函数 %q", call.Name)}
	}
	if schema := contract.schemas[call.Name]; schema != nil {
		if err := validateToolArguments(schema, call.Arguments); err != nil {
			reason := err.Error()
			if runes := []rune(reason); len(runes) > regenerationNoteLimit {
				reason = string(runes[:regenerationNoteLimit]) + "…"
			}
			return &ToolContractError{
				Kind: ToolContractSchemaMismatch, Function: call.Name,
				Detail: fmt.Sprintf("函数 %q 的参数不符合 strict Schema: %s", call.Name, reason),
			}
		}
	}
	return nil
}

// missingCall 返回要求调用却没有调用的违约；ending 为结束的情形（例如“结束原因=stop”），写进日志
func (contract *toolContract) missingCall(ending string) *ToolContractError {
	detail := "要求调用工具，上游没有返回工具调用"
	if len(contract.toolNames) > 0 {
		detail = "要求调用工具 " + strings.Join(contract.toolNames, ", ") + "，上游没有返回工具调用"
	}
	if ending != "" {
		detail += "（" + ending + "）"
	}
	return &ToolContractError{
		Kind: ToolContractMissingCall, Detail: detail, Required: slices.Clone(contract.toolNames), SingleFunction: contract.singleFunction(),
	}
}

// singleFunction 判断必须调用的只有一个工具，且它就是唯一被选中的函数（不是内置工具）
func (contract *toolContract) singleFunction() bool {
	return len(contract.toolNames) == 1 && len(contract.functionNames) == 1 && contract.toolNames[0] == contract.functionNames[0]
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

// localCallIDPrefix 为本地补发的函数调用 ID 前缀。上游的调用 ID 是可选的，不带 ID 时客户端拿到空 id，
// 并行调用的结果无法对应回去（Responses 流还会复用同一个输出序号）；本地补一个 ID 给客户端，
// 发回上游时去掉，上游看到的与它原先返回的一致
const localCallIDPrefix = "call_local_"

var localCallIDSequence atomic.Uint64

// assignLocalCallID 为没有 ID 的函数调用事件补一个本地 ID
func assignLocalCallID(event Event) Event {
	if event.Kind != EventToolCall || event.ToolCall == nil || event.ToolCall.ID != "" {
		return event
	}
	call := *event.ToolCall
	call.ID = fmt.Sprintf("%s%d_%d", localCallIDPrefix, time.Now().UnixNano(), localCallIDSequence.Add(1))
	event.ToolCall = &call
	return event
}

// AssignLocalCallID 供公开协议层兜底：生成服务之外来源的事件同样按本地规则补 ID，发回上游时照常去掉
func AssignLocalCallID(event Event) Event {
	return assignLocalCallID(event)
}

// LocalCallID 判断调用 ID 是不是本地补发的（call_local_*，进程内唯一）
func LocalCallID(id string) bool {
	return strings.HasPrefix(id, localCallIDPrefix)
}

// upstreamCallID 返回发回上游时使用的调用 ID：本地补发的 ID 不发给上游
func upstreamCallID(id string) string {
	if strings.HasPrefix(id, localCallIDPrefix) {
		return ""
	}
	return id
}
