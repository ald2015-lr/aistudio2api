package aistudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func encodeRequestedTools(tools Tools) ([]any, bool, error) {
	switch tools.ToolConfig.Mode {
	case "none":
		return nil, true, nil
	case "", "auto", "required", "validated":
	default:
		return nil, false, fmt.Errorf("未知 tool choice %q", tools.ToolConfig.Mode)
	}
	if len(tools.Functions) == 0 && len(tools.Google) == 0 && tools.GoogleSearch == nil {
		return nil, false, nil
	}
	wire := make([]any, 0, len(tools.Google)+2)
	if len(tools.Functions) > 0 {
		declarations := make([]any, 0, len(tools.Functions))
		for index, declaration := range tools.Functions {
			encoded, err := encodeFunctionDeclaration(declaration)
			if err != nil {
				return nil, false, fmt.Errorf("编码 function declaration %d: %w", index, err)
			}
			declarations = append(declarations, encoded)
		}
		wire = append(wire, []any{nil, declarations})
	}
	search := GoogleSearchOptions{}
	searchRequested := tools.GoogleSearch != nil
	if tools.GoogleSearch != nil {
		search = *tools.GoogleSearch
	}
	for _, name := range tools.Google {
		search.WebSearch = search.WebSearch || name == "google_search"
		search.ImageSearch = search.ImageSearch || name == "image_search"
		searchRequested = searchRequested || name == "google_search" || name == "image_search"
	}
	if searchRequested && !search.WebSearch && !search.ImageSearch {
		search.WebSearch = true
	}
	searchEncoded := false
	for _, name := range tools.Google {
		switch name {
		case "code_execution":
			wire = append(wire, []any{[]any{}})
		case "google_search", "image_search":
			if !searchEncoded {
				wire = append(wire, encodeSearchTool(search))
				searchEncoded = true
			}
		case "url_context":
			tool := make([]any, 8)
			tool[7] = []any{}
			wire = append(wire, tool)
		case "google_maps":
			tool := make([]any, 11)
			tool[10] = []any{}
			wire = append(wire, tool)
		default:
			return nil, false, fmt.Errorf("未知 Google tool %q", name)
		}
	}
	if searchRequested && !searchEncoded {
		wire = append(wire, encodeSearchTool(search))
	}
	return wire, true, nil
}

func encodeSearchTool(options GoogleSearchOptions) []any {
	length := 1
	if options.ImageSearch {
		length = 2
	}
	searchTypes := make([]any, length)
	if options.WebSearch {
		searchTypes[0] = []any{}
	}
	if options.ImageSearch {
		searchTypes[1] = []any{}
	}
	searchConfig := []any{nil, searchTypes}
	if options.TimeRange != nil {
		searchConfig[0] = encodeGoogleSearchTimeRange(*options.TimeRange)
	}
	return []any{nil, nil, nil, searchConfig}
}

func encodeGoogleSearchTimeRange(value GoogleSearchTimeRange) []any {
	wire := make([]any, 2)
	if !value.StartTime.IsZero() {
		wire[0] = encodeGoogleTimestamp(value.StartTime)
	}
	if !value.EndTime.IsZero() {
		wire[1] = encodeGoogleTimestamp(value.EndTime)
	}
	return wire
}

func encodeGoogleTimestamp(value time.Time) []any {
	return []any{strconv.FormatInt(value.Unix(), 10)}
}

// encodeFunctionDeclaration 编码 Playground 函数声明。
//
// 参数 Schema 含 Playground 无法编码的写法（$ref/$defs、uniqueItems、examples、数值或 null 常量、非字符串 enum 等）时，
// 不让整个请求失败：改为只编码层级与类型（toolSchemaShape），并把完整 Schema 附在函数说明里交给模型遵守。
// strict 函数同样附上完整 Schema。超过大小或嵌套上限的 Schema 仍直接报错
func encodeFunctionDeclaration(declaration FunctionDeclaration) ([]any, error) {
	wire, _, err := encodeFunctionDeclarationHint(declaration)
	return wire, err
}

// encodeFunctionDeclarationHint 同 encodeFunctionDeclaration，另返回附在说明里的完整 Schema 提示字数（没有附时为 0）
func encodeFunctionDeclarationHint(declaration FunctionDeclaration) ([]any, int64, error) {
	if declaration.Name == "" {
		return nil, 0, fmt.Errorf("function declaration 缺少名称")
	}
	// required 列出的未定义属性补上 {}（超过大小或嵌套上限的 Schema 不补，由 encodeJSONSchema 照常拒绝）
	raw := defineRequiredProperties(normalizeFunctionParameters(declaration.Parameters))
	parameters, err := encodeJSONSchema(raw)
	degraded := false
	if err != nil {
		if isSchemaLimitError(err) {
			return nil, 0, fmt.Errorf("parameters: %w", err)
		}
		var schema any
		if json.Unmarshal(raw, &schema) != nil {
			return nil, 0, fmt.Errorf("parameters: %w", err)
		}
		shape, marshalErr := json.Marshal(toolSchemaShape(schema, 0))
		if marshalErr != nil {
			return nil, 0, marshalErr
		}
		parameters, err = encodeJSONSchema(shape)
		if err != nil {
			return nil, 0, fmt.Errorf("parameters: %w", err)
		}
		degraded = true
	}
	description := declaration.Description
	// enum 里有空串时编码去掉了空串（官网拒收），同样附上完整 Schema
	if degraded || declaration.Strict || schemaHasEmptyEnum(raw) {
		description = appendSchemaHint(description, "Arguments must follow this JSON Schema: ", raw)
	}
	var wireDescription any
	if description != "" {
		wireDescription = description
	}
	return []any{declaration.Name, wireDescription, parameters}, schemaHintChars(declaration.Description, description), nil
}

// schemaHintChars 返回说明因附上完整 Schema 增加的字数
func schemaHintChars(before string, after string) int64 {
	return max(int64(utf8.RuneCountInString(after)-utf8.RuneCountInString(before)), 0)
}

// maxSchemaHintBytes 为附在函数说明里的完整 Schema 的大小上限，超过时不附
const maxSchemaHintBytes = 16 << 10

// appendSchemaHint 在说明末尾附上完整 Schema
func appendSchemaHint(description string, prefix string, raw json.RawMessage) string {
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil || compact.Len() > maxSchemaHintBytes {
		return description
	}
	return strings.TrimSpace(description + "\n" + prefix + compact.String())
}

// encodeResponseSchema 编码 Playground 结构化输出 Schema。含 Playground 无法编码的写法时与函数参数一样降级：
// 只编码层级与类型，把完整 Schema 附在根节点说明里交给模型遵守（原先整个请求返回 400）。
// 超过大小或嵌套上限的 Schema 仍直接报错；Build 通道原样发送 responseJsonSchema，不经过这里
func encodeResponseSchema(raw json.RawMessage) ([]any, error) {
	wire, _, err := encodeResponseSchemaHint(raw)
	return wire, err
}

// encodeResponseSchemaHint 同 encodeResponseSchema，另返回降级时附在根说明里的完整 Schema 提示字数（没有附时为 0）
func encodeResponseSchemaHint(raw json.RawMessage) ([]any, int64, error) {
	wire, err := encodeJSONSchema(raw)
	if err == nil && schemaHasEmptyEnum(raw) {
		// enum 里的空串编码时去掉了（官网拒收）：完整 Schema 附在根节点说明里
		description, _ := wireDescription(wire)
		hinted := appendSchemaHint(description, "The response must follow this JSON Schema: ", raw)
		if hinted == description {
			return wire, 0, nil
		}
		return setWireField(wire, 2, hinted), schemaHintChars(description, hinted), nil
	}
	if err == nil || isSchemaLimitError(err) {
		return wire, 0, err
	}
	var schema any
	if json.Unmarshal(raw, &schema) != nil {
		return nil, 0, err
	}
	shape := toolSchemaShape(schema, 0)
	description, _ := shape["description"].(string)
	hinted := appendSchemaHint(description, "The response must follow this JSON Schema: ", raw)
	shape["description"] = hinted
	encoded, marshalErr := json.Marshal(shape)
	if marshalErr != nil {
		return nil, 0, marshalErr
	}
	wire, shapeErr := encodeJSONSchema(encoded)
	if shapeErr != nil {
		return nil, 0, err
	}
	return wire, schemaHintChars(description, hinted), nil
}

// wireDescription 返回已编码 Schema 数组的说明（第 3 位）
func wireDescription(wire []any) (string, bool) {
	if len(wire) <= 2 {
		return "", false
	}
	description, ok := wire[2].(string)
	return description, ok
}

// toolSchemaShape 提取工具参数的层级与类型，供 Playground 编码；depth 防止病态嵌套
func toolSchemaShape(value any, depth int) map[string]any {
	result := map[string]any{}
	object, ok := value.(map[string]any)
	if !ok || depth > maxSchemaDepth {
		return result
	}
	for _, key := range []string{
		"type", "format", "nullable", "description", "required", "minimum", "maximum", "minLength", "maxLength",
		"pattern", "minItems", "maxItems", "minProperties", "maxProperties", "propertyOrdering",
	} {
		if item, exists := object[key]; exists && item != nil {
			result[key] = item
		}
	}
	if description, ok := result["description"]; ok {
		if _, isString := description.(string); !isString {
			delete(result, "description")
		}
	}
	if constant, exists := object["const"]; exists {
		if constant == nil {
			result["type"], result["nullable"] = "string", true
			result["description"] = "The value must be JSON null"
		} else {
			encoded, _ := json.Marshal(constant)
			description, _ := result["description"].(string)
			result["description"] = strings.TrimSpace(description + "\nThe value must be: " + string(encoded))
			switch value := constant.(type) {
			case float64:
				result["type"] = "number"
				if math.Trunc(value) == value {
					result["type"] = "integer"
				}
				result["minimum"], result["maximum"] = value, value
			case bool:
				result["type"] = "boolean"
			case string:
				result["type"], result["enum"] = "string", []any{value}
			case []any:
				result["type"] = "array"
			case map[string]any:
				result["type"] = "object"
			}
		}
	}
	if values, ok := object["enum"].([]any); ok && len(values) > 0 {
		stringsOnly := true
		for _, value := range values {
			_, isString := value.(string)
			stringsOnly = stringsOnly && isString
		}
		if stringsOnly {
			result["enum"] = values
		} else {
			encoded, _ := json.Marshal(values)
			description, _ := result["description"].(string)
			result["description"] = strings.TrimSpace(description + "\nAllowed values: " + string(encoded))
		}
	}
	if nullOnlyType(result["type"]) {
		result["type"], result["nullable"] = "string", true
		result["description"] = "The value must be JSON null"
	}
	if properties, ok := object["properties"].(map[string]any); ok {
		children := make(map[string]any, len(properties))
		for key, child := range properties {
			children[key] = toolSchemaShape(child, depth+1)
		}
		result["properties"] = children
	}
	if child, exists := object["items"]; exists {
		result["items"] = toolSchemaShape(child, depth+1)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if branches, ok := object[key].([]any); ok {
			children := make([]any, len(branches))
			for index, branch := range branches {
				children[index] = toolSchemaShape(branch, depth+1)
			}
			result[key] = children
		}
	}
	return result
}

// nullOnlyType 判断 type 是否只有 null
func nullOnlyType(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.EqualFold(typed, "null")
	case []any:
		if len(typed) == 0 {
			return false
		}
		for _, name := range typed {
			if name, ok := name.(string); !ok || !strings.EqualFold(name, "null") {
				return false
			}
		}
		return true
	}
	return false
}

func hasMethod(model Model, method string) bool {
	for _, candidate := range model.Methods {
		if candidate == method {
			return true
		}
	}
	return false
}

func validateFunctionCall(call *FunctionCall) error {
	if call == nil || call.Name == "" {
		return fmt.Errorf("function call 缺少名称")
	}
	return nil
}

// ErrFunctionCallMissingName 表示上游返回的函数调用没有函数名：客户端无法执行这个调用，整次回复按错误结束（对外为 502，说明见 api 包）
var ErrFunctionCallMissingName = errors.New("上游返回的函数调用缺少函数名，无法交给客户端执行")

// decodeFunctionCall 解码 Part field 11 的函数调用 [name, Struct args, id?]。函数名缺失、为 null、不是字符串或为空串时无法调用，按带
// ErrFunctionCallMissingName 的协议证据错误返回；参数按 decodeWireStruct 尽量解码（单个参数异常时该参数按 null），整个参数读不出时按 {}；
// 调用 ID 不是字符串时忽略（之后补发本地 ID）。都写容错日志，不丢掉整次调用
func decodeFunctionCall(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance) (FunctionCall, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return FunctionCall{}, withMethod(err, gcMethod)
	}
	var name string
	if json.Unmarshal(rawAt(values, 0), &name) != nil || strings.TrimSpace(name) == "" {
		return FunctionCall{}, &ProtocolEvidenceError{
			Method: gcMethod, Path: path + "[0]", Detail: ErrFunctionCallMissingName.Error(), Err: ErrFunctionCallMissingName, Raw: raw,
		}
	}
	arguments := json.RawMessage(`{}`)
	if argumentsRaw := rawAt(values, 1); !isJSONNull(argumentsRaw) {
		arguments = decodeWireStruct(argumentsRaw, path+"[1]", tolerance)
	}
	id := ""
	if idRaw := rawAt(values, 2); !isJSONNull(idRaw) && json.Unmarshal(idRaw, &id) != nil {
		id = ""
		tolerance.note(gcMethod, path+"[2]", "函数调用 ID 不是字符串，已忽略（之后补发本地 ID）", idRaw)
	}
	return FunctionCall{ID: id, Name: name, Arguments: arguments}, nil
}

func encodeWireStructJSON(raw json.RawMessage) ([]any, error) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("必须是 JSON object")
	}
	return encodeWireStruct(object), nil
}

func encodeWireStruct(object map[string]any) []any {
	if len(object) == 0 {
		return []any{}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]any, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, []any{key, encodeWireValue(object[key])})
	}
	return []any{entries}
}

func encodeWireValue(value any) []any {
	switch typed := value.(type) {
	case nil:
		return []any{int64(0)}
	case float64:
		return []any{nil, typed}
	case string:
		return []any{nil, nil, typed}
	case bool:
		return []any{nil, nil, nil, typed}
	case map[string]any:
		return []any{nil, nil, nil, nil, encodeWireStruct(typed)}
	case []any:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, encodeWireValue(item))
		}
		return []any{nil, nil, nil, nil, nil, []any{values}}
	default:
		panic(fmt.Sprintf("json.Unmarshal 返回未识别类型 %T", value))
	}
}

// decodeWireStruct 把 google.protobuf.Struct 的线格式解码为 JSON object。尽量按最合理的值解码，不返回错误：Struct 读不出时为 {}，
// 单个参数异常时该参数按 null（见 decodeWireStructObject 与 decodeWireValue），都写容错日志
func decodeWireStruct(raw json.RawMessage, path string, tolerance *decodeTolerance) json.RawMessage {
	encoded, err := json.Marshal(decodeWireStructObject(raw, path, tolerance))
	if err != nil {
		// 解码出的值只有 JSON 能表示的类型（NaN 与无穷已按 null），这里只是兜底
		tolerance.note(gcMethod, path, "函数参数无法编码为 JSON，按 {}", toleranceText("%v", err))
		return json.RawMessage(`{}`)
	}
	return encoded
}

// decodeWireStructObject 解码 Struct [[[key, Value]...]]：不是数组时为空 object；map entry 不是数组、缺少键或键不是字符串时跳过该项；
// 只有键没有值时该参数为 null，多于两项时只取前两项
func decodeWireStructObject(raw json.RawMessage, path string, tolerance *decodeTolerance) map[string]any {
	object := make(map[string]any)
	fields, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "函数参数 Struct 不是数组，按 {}", raw)
		return object
	}
	entriesRaw := rawAt(fields, 0)
	if isJSONNull(entriesRaw) {
		return object
	}
	entries, err := rawArray(entriesRaw, path+"[0]", raw)
	if err != nil {
		tolerance.note(gcMethod, path+"[0]", "函数参数 Struct 的字段列表不是数组，按 {}", entriesRaw)
		return object
	}
	for index, entryRaw := range entries {
		entryPath := fmt.Sprintf("%s[0][%d]", path, index)
		entry, err := rawArray(entryRaw, entryPath, raw)
		if err != nil || len(entry) == 0 {
			tolerance.note(gcMethod, entryPath, "函数参数的 map entry 不是数组或为空，已跳过", entryRaw)
			continue
		}
		var key string
		if json.Unmarshal(entry[0], &key) != nil || isJSONNull(entry[0]) {
			tolerance.note(gcMethod, entryPath+"[0]", "函数参数的键不是字符串，已跳过该参数", entry[0])
			continue
		}
		switch {
		case len(entry) == 1:
			tolerance.note(gcMethod, entryPath, "函数参数只有键没有值，按 null", entryRaw)
			object[key] = nil
			continue
		case len(entry) > 2:
			tolerance.note(gcMethod, entryPath, "函数参数的 map entry 多于两项，只取键与值", entryRaw)
		}
		object[key] = decodeWireValue(entry[1], entryPath+"[1]", tolerance)
	}
	return object
}

// decodeWireValue 解码 google.protobuf.Value 的 oneof [null_value, number, string, bool, Struct, ListValue]，按最合理的值解码，不返回错误：
//   - 一个字段都没有设置（[]、[null]）时按 null；设置了多个时取第一个
//   - null_value 不论写成 0 还是别的值都是 null
//   - number 写成字符串时按数字解析，"NaN"、"Infinity" 等 JSON 表示不了的值按 null
//   - string 写成数字或布尔值时按原文转成字符串，bool 按“非 0、非 false 即为真”
//   - 读不出时该值按 null
//
// 除原本的写法（null 写成 [0]，其余各自的类型）外都写容错日志
func decodeWireValue(raw json.RawMessage, path string, tolerance *decodeTolerance) any {
	fields, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "函数参数 Value 不是数组，按 null", raw)
		return nil
	}
	variant, count := -1, 0
	for index := 0; index < 6 && index < len(fields); index++ {
		if isJSONNull(fields[index]) {
			continue
		}
		if variant < 0 {
			variant = index
		}
		count++
	}
	if variant < 0 {
		tolerance.note(gcMethod, path, "函数参数 Value 没有设置任何字段，按 null", raw)
		return nil
	}
	if count > 1 {
		tolerance.note(gcMethod, path, "函数参数 Value 同时设置多个字段，取第一个", raw)
	}
	value := fields[variant]
	valuePath := fmt.Sprintf("%s[%d]", path, variant)
	switch variant {
	case 0:
		if code, err := rawInt64(value, valuePath, raw); err != nil || code != 0 {
			tolerance.note(gcMethod, valuePath, "函数参数的 null_value 写法不常见，按 null", value)
		}
		return nil
	case 1:
		var number float64
		if json.Unmarshal(value, &number) == nil {
			return number
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
				tolerance.note(gcMethod, valuePath, "函数参数的数字写成字符串，按数字解析", value)
				return parsed
			}
		}
		tolerance.note(gcMethod, valuePath, "函数参数的数字无法用 JSON 表示或读不出（如 NaN、Infinity），按 null", value)
		return nil
	case 2:
		text, converted, ok := lenientText(value)
		if !ok {
			tolerance.note(gcMethod, valuePath, "函数参数的字符串读不出，按 null", value)
			return nil
		}
		if converted {
			tolerance.note(gcMethod, valuePath, "函数参数的字符串写成其他类型，按原文转成字符串", value)
		}
		return text
	case 3:
		if flag, err := rawBool(value, valuePath, raw); err == nil {
			return flag
		}
		if flag, ok := lenientBool(value); ok {
			tolerance.note(gcMethod, valuePath, "函数参数的布尔值写法不常见，按非 0 即真处理", value)
			return flag
		}
		tolerance.note(gcMethod, valuePath, "函数参数的布尔值读不出，按 null", value)
		return nil
	case 4:
		return decodeWireStructObject(value, valuePath, tolerance)
	default:
		return decodeWireList(value, valuePath, tolerance)
	}
}

// decodeWireList 解码 ListValue [[Value...]]：不是数组时为空列表，各元素按 decodeWireValue 解码
func decodeWireList(raw json.RawMessage, path string, tolerance *decodeTolerance) []any {
	fields, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "函数参数的 ListValue 不是数组，按 []", raw)
		return []any{}
	}
	itemsRaw := rawAt(fields, 0)
	if isJSONNull(itemsRaw) {
		return []any{}
	}
	items, err := rawArray(itemsRaw, path+"[0]", raw)
	if err != nil {
		tolerance.note(gcMethod, path+"[0]", "函数参数的 ListValue 元素列表不是数组，按 []", itemsRaw)
		return []any{}
	}
	values := make([]any, 0, len(items))
	for index, item := range items {
		values = append(values, decodeWireValue(item, fmt.Sprintf("%s[0][%d]", path, index), tolerance))
	}
	return values
}
