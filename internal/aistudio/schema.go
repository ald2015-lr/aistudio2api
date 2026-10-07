package aistudio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var schemaTypeCodes = map[string]int64{
	"string":  1,
	"number":  2,
	"integer": 3,
	"boolean": 4,
	"array":   5,
	"object":  6,
}

// maxSchemaDepth 为 JSON Schema 的最大嵌套层数；实际工具 schema 很少超过十几层
const maxSchemaDepth = 64

// maxSchemaBytes 为单个 JSON Schema 的大小上限
const maxSchemaBytes = 1 << 20

// encodeJSONSchema 把 JSON Schema 转换为 AI Studio 的 Schema 数组
func encodeJSONSchema(raw json.RawMessage) ([]any, error) {
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("schema 超过 %d 字节上限", maxSchemaBytes)
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		raw = json.RawMessage(emptyFunctionParameters)
	}
	return encodeSchemaNode(raw, 0)
}

// encodeSchemaNode 转换一个 schema 节点；depth 为当前嵌套层数
func encodeSchemaNode(raw json.RawMessage, depth int) ([]any, error) {
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("schema 嵌套超过 %d 层", maxSchemaDepth)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, fmt.Errorf("schema 必须是 JSON object")
	}
	cleanNullFields(schema)
	if err := normalizeNot(schema); err != nil {
		return nil, err
	}
	normalizeBooleanItems(schema)
	if err := normalizeConstAndMetadata(schema); err != nil {
		return nil, err
	}
	if err := normalizeNullableVariants(schema); err != nil {
		return nil, err
	}
	if err := normalizeImplicitType(schema); err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		"type": true, "format": true, "description": true, "nullable": true,
		"enum": true, "items": true, "properties": true, "required": true,
		"minItems": true, "maxItems": true, "minProperties": true, "maxProperties": true,
		"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
		"pattern": true, "example": true, "oneOf": true, "anyOf": true,
		"allOf": true, "not": true, "propertyOrdering": true,
		"$schema": true, "additionalProperties": true, "default": true, "exclusiveMinimum": true,
		"propertyNames": true, "prefixItems": true,
	}
	for name := range schema {
		if !allowed[name] {
			return nil, &UnverifiedProtocolError{Feature: "JSON schema 字段 " + name}
		}
	}
	typeName, err := schemaType(schema)
	if err != nil {
		return nil, err
	}
	typeName = strings.ToLower(typeName)
	typeCode, ok := schemaTypeCodes[typeName]
	if !ok {
		return nil, fmt.Errorf("未知 schema.type %q", typeName)
	}
	wire := []any{typeCode}
	if value, ok := schema["format"]; ok {
		format, err := schemaString(value, "format")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 1, format)
	}
	if value, ok := schema["description"]; ok {
		description, err := schemaString(value, "description")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 2, description)
	}
	if value, ok := schema["nullable"]; ok {
		var nullable bool
		if err := json.Unmarshal(value, &nullable); err != nil {
			return nil, fmt.Errorf("schema.nullable 必须是布尔值")
		}
		wire = setWireField(wire, 3, nullable)
	}
	if value, ok := schema["enum"]; ok {
		values, err := schemaStrings(value, "enum")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 4, values)
	}
	if value, ok := schema["items"]; ok {
		items, err := encodeSchemaNode(value, depth+1)
		if err != nil {
			return nil, fmt.Errorf("schema.items: %w", err)
		}
		wire = setWireField(wire, 5, items)
	}
	for _, field := range []struct {
		name  string
		index int
	}{
		{name: "minItems", index: 21},
		{name: "maxItems", index: 20},
		{name: "minProperties", index: 8},
		{name: "maxProperties", index: 9},
		{name: "minLength", index: 12},
		{name: "maxLength", index: 13},
	} {
		if value, ok := schema[field.name]; ok {
			integer, err := schemaInteger(value, field.name)
			if err != nil {
				return nil, err
			}
			wire = setWireField(wire, field.index, integer)
		}
	}
	if value, ok := schema["properties"]; ok {
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(value, &properties); err != nil || properties == nil {
			return nil, fmt.Errorf("schema.properties 必须是 JSON object")
		}
		if removed := normalizeBooleanProperties(properties); len(removed) > 0 {
			if err := dropRequired(schema, removed); err != nil {
				return nil, err
			}
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		entries := make([]any, 0, len(names))
		for _, name := range names {
			property, err := encodeSchemaNode(properties[name], depth+1)
			if err != nil {
				return nil, fmt.Errorf("schema.properties.%s: %w", name, err)
			}
			entries = append(entries, []any{name, property})
		}
		if len(entries) > 0 {
			wire = setWireField(wire, 6, entries)
		}
	}
	if value, ok := schema["required"]; ok {
		required, err := schemaStrings(value, "required")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 7, required)
	}
	for _, field := range []struct {
		name  string
		index int
	}{
		{name: "minimum", index: 10},
		{name: "maximum", index: 11},
	} {
		if value, ok := schema[field.name]; ok {
			number, err := schemaNumber(value, field.name)
			if err != nil {
				return nil, err
			}
			wire = setWireField(wire, field.index, number)
		}
	}
	if value, ok := schema["pattern"]; ok {
		pattern, err := schemaString(value, "pattern")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 14, pattern)
	}
	if value, ok := schema["example"]; ok {
		var example any
		if err := json.Unmarshal(value, &example); err != nil {
			return nil, fmt.Errorf("schema.example 必须是 JSON value")
		}
		wire = setWireField(wire, 15, encodeWireValue(example))
	}
	for _, field := range []struct {
		name  string
		index int
	}{
		{name: "oneOf", index: 16},
		{name: "anyOf", index: 17},
		{name: "allOf", index: 18},
	} {
		if value, ok := schema[field.name]; ok {
			variants, err := encodeSchemaVariants(value, field.name, depth+1)
			if err != nil {
				return nil, err
			}
			if len(variants) > 0 {
				wire = setWireField(wire, field.index, variants)
			}
		}
	}
	if value, ok := schema["not"]; ok {
		notSchema, err := encodeSchemaNode(value, depth+1)
		if err != nil {
			return nil, fmt.Errorf("schema.not: %w", err)
		}
		wire = setWireField(wire, 19, notSchema)
	}
	if value, ok := schema["propertyOrdering"]; ok {
		ordering, err := schemaStrings(value, "propertyOrdering")
		if err != nil {
			return nil, err
		}
		wire = setWireField(wire, 22, ordering)
	}
	return wire, nil
}

// normalizeNullableVariants 将 JSON Schema null 联合映射为 AI Studio nullable
func normalizeNullableVariants(schema map[string]json.RawMessage) error {
	if raw, ok := schema["type"]; ok {
		var typeName string
		if err := json.Unmarshal(raw, &typeName); err != nil {
			var typeNames []string
			if arrayErr := json.Unmarshal(raw, &typeNames); arrayErr != nil || len(typeNames) == 0 {
				return fmt.Errorf("schema.type 必须是字符串或字符串数组")
			}
			nonNull := make([]string, 0, len(typeNames))
			nullable := false
			for _, name := range typeNames {
				if strings.EqualFold(name, "null") {
					nullable = true
					continue
				}
				nonNull = append(nonNull, name)
			}
			if len(nonNull) == 0 {
				return fmt.Errorf("schema.type 必须包含非 null 类型")
			}
			encodedType, marshalErr := json.Marshal(nonNull[0])
			if marshalErr != nil {
				return marshalErr
			}
			schema["type"] = encodedType
			if len(nonNull) > 1 {
				variants := make([]map[string]string, 0, len(nonNull))
				for _, name := range nonNull {
					variants = append(variants, map[string]string{"type": name})
				}
				encodedVariants, marshalErr := json.Marshal(variants)
				if marshalErr != nil {
					return marshalErr
				}
				schema["anyOf"] = encodedVariants
			}
			if nullable {
				schema["nullable"] = json.RawMessage("true")
			}
		}
	}
	for _, name := range []string{"anyOf", "oneOf"} {
		raw, ok := schema[name]
		if !ok {
			continue
		}
		var variants []json.RawMessage
		if err := json.Unmarshal(raw, &variants); err != nil {
			return fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
		}
		filtered := variants[:0]
		nullable := false
		for _, variant := range variants {
			var value map[string]json.RawMessage
			if err := json.Unmarshal(variant, &value); err != nil || value == nil {
				return fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
			}
			typeValue, exists := value["type"]
			if exists {
				typeName, err := schemaString(typeValue, "type")
				if err != nil {
					return err
				}
				if strings.EqualFold(typeName, "null") {
					nullable = true
					continue
				}
			}
			filtered = append(filtered, variant)
		}
		if !nullable {
			continue
		}
		if len(filtered) == 0 {
			return fmt.Errorf("schema.%s 必须包含非 null 类型", name)
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return err
		}
		schema[name] = encoded
		schema["nullable"] = json.RawMessage("true")
	}
	return nil
}

// normalizeConstAndMetadata 将当前节点及其直接组合子节点（anyOf、oneOf、allOf）的字符串常量和说明字段
// 转换为可发送结构。
//
// 只处理一层：后续的 nullable 与隐式类型规范化只检查直接子节点的 type；更深的子节点在各自编码时再规范化。
// 原先这里递归规范化整棵子树，而每个子节点编码时又再递归一次，嵌套 anyOf 的耗时随层数按立方增长，
// 几 KB 的工具 schema 就要数十秒 CPU
func normalizeConstAndMetadata(schema map[string]json.RawMessage) error {
	if err := normalizeSchemaConst(schema); err != nil {
		return err
	}
	for _, name := range []string{"anyOf", "oneOf", "allOf"} {
		raw, ok := schema[name]
		if !ok {
			continue
		}
		var variants []json.RawMessage
		if err := json.Unmarshal(raw, &variants); err != nil {
			return fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
		}
		for index, variant := range variants {
			var subSchema map[string]json.RawMessage
			if err := json.Unmarshal(variant, &subSchema); err != nil || subSchema == nil {
				return fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
			}
			if err := normalizeSchemaConst(subSchema); err != nil {
				return fmt.Errorf("schema.%s[%d]: %w", name, index, err)
			}
			encoded, err := json.Marshal(subSchema)
			if err != nil {
				return err
			}
			variants[index] = encoded
		}
		encoded, err := json.Marshal(variants)
		if err != nil {
			return err
		}
		schema[name] = encoded
	}
	return nil
}

// normalizeSchemaConst 删除说明字段，并把字符串 const 转换为单值 enum
func normalizeSchemaConst(schema map[string]json.RawMessage) error {
	delete(schema, "title")
	delete(schema, "$id")
	delete(schema, "$comment")
	raw, ok := schema["const"]
	if !ok {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("schema.const 必须是字符串")
	}
	value, ok := decoded.(string)
	if !ok {
		return fmt.Errorf("schema.const 只支持字符串")
	}
	if rawType, exists := schema["type"]; exists {
		typeName, err := schemaString(rawType, "type")
		if err != nil || !strings.EqualFold(typeName, "string") {
			return fmt.Errorf("schema.const 只支持 string 类型")
		}
	} else {
		schema["type"] = json.RawMessage(`"string"`)
	}
	enum, err := json.Marshal([]string{value})
	if err != nil {
		return err
	}
	schema["enum"] = enum
	delete(schema, "const")
	return nil
}

// normalizeImplicitType 为未声明类型的节点和缺少元素定义的数组补齐 AI Studio 必需的字段
func normalizeImplicitType(schema map[string]json.RawMessage) error {
	if _, ok := schema["type"]; !ok {
		var typed map[string]json.RawMessage
		for _, name := range []string{"anyOf", "oneOf", "allOf"} {
			var variants []map[string]json.RawMessage
			if raw, ok := schema[name]; ok && json.Unmarshal(raw, &variants) == nil {
				for _, variant := range variants {
					if _, ok := variant["type"]; ok && typed == nil {
						typed = variant
					}
				}
			}
		}
		switch {
		case schema["properties"] != nil:
			schema["type"] = json.RawMessage(`"object"`)
		case schema["items"] != nil || schema["prefixItems"] != nil:
			schema["type"] = json.RawMessage(`"array"`)
		default:
			schema["type"] = json.RawMessage(`"string"`)
		}
		if typed != nil {
			schema["type"] = typed["type"]
			if items, ok := typed["items"]; ok && schema["items"] == nil {
				schema["items"] = items
			}
		}
	}
	var typeName string
	if json.Unmarshal(schema["type"], &typeName) != nil || !strings.EqualFold(typeName, "array") || schema["items"] != nil {
		return nil
	}
	schema["items"] = json.RawMessage(`{"type":"string"}`)
	var prefix []map[string]json.RawMessage
	if raw, ok := schema["prefixItems"]; ok && json.Unmarshal(raw, &prefix) == nil {
		typed := make([]map[string]json.RawMessage, 0, len(prefix))
		for _, item := range prefix {
			if _, ok := item["type"]; ok {
				typed = append(typed, item)
			}
		}
		if len(typed) > 0 {
			encoded, err := json.Marshal(map[string]any{"anyOf": typed})
			if err != nil {
				return err
			}
			schema["items"] = encoded
		}
	}
	return nil
}

func schemaType(schema map[string]json.RawMessage) (string, error) {
	if value, ok := schema["type"]; ok {
		typeName, err := schemaString(value, "type")
		if err != nil || typeName == "" {
			return "", fmt.Errorf("schema.type 必须是字符串")
		}
		return typeName, nil
	}
	for _, name := range []string{"anyOf", "oneOf", "allOf"} {
		value, ok := schema[name]
		if !ok {
			continue
		}
		var variants []map[string]json.RawMessage
		if err := json.Unmarshal(value, &variants); err != nil {
			return "", fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
		}
		for _, variant := range variants {
			if typeValue, exists := variant["type"]; exists {
				return schemaString(typeValue, "type")
			}
		}
	}
	return "", fmt.Errorf("schema.type 必须是字符串")
}

func schemaInteger(raw json.RawMessage, name string) (int64, error) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("schema.%s 必须是非负整数", name)
	}
	return value, nil
}

func schemaNumber(raw json.RawMessage, name string) (float64, error) {
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("schema.%s 必须是数字", name)
	}
	return value, nil
}

func encodeSchemaVariants(raw json.RawMessage, name string, depth int) ([]any, error) {
	var variants []json.RawMessage
	if err := json.Unmarshal(raw, &variants); err != nil {
		return nil, fmt.Errorf("schema.%s 必须是 JSON object 数组", name)
	}
	encoded := make([]any, 0, len(variants))
	for index, variant := range variants {
		wire, err := encodeSchemaNode(variant, depth)
		if err != nil {
			return nil, fmt.Errorf("schema.%s[%d]: %w", name, index, err)
		}
		encoded = append(encoded, wire)
	}
	return encoded, nil
}

func schemaString(raw json.RawMessage, name string) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("schema.%s 必须是字符串", name)
	}
	return value, nil
}

func schemaStrings(raw json.RawMessage, name string) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("schema.%s 必须是字符串数组", name)
	}
	return values, nil
}

func setWireField(wire []any, index int, value any) []any {
	for len(wire) <= index {
		wire = append(wire, nil)
	}
	wire[index] = value
	return wire
}

// emptyFunctionParameters 为"没有参数"的函数参数 schema
const emptyFunctionParameters = `{"type":"object","properties":{}}`

// normalizeFunctionParameters 统一函数参数 schema 的各种写法：无参函数的 parameters 常被写成 null、{}、true，
// 原样转发时 Playground 编码报"schema 必须是 JSON object"，Build 通道被上游以 400 拒绝
// （schema at top-level must be a boolean or an object）。这些写法都按"没有参数的对象"处理；
// 被编码成 JSON 字符串的 schema 先解开
func normalizeFunctionParameters(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if json.Unmarshal([]byte(trimmed), &inner) == nil {
			trimmed = strings.TrimSpace(inner)
		}
	}
	switch trimmed {
	case "", "null", "{}", "true", "false", `""`:
		return json.RawMessage(emptyFunctionParameters)
	}
	return json.RawMessage(trimmed)
}

// cleanNullFields 删除值为显式 null 的字段（const 除外）。SDK 生成的 schema 常把未设置的可选字段写成 null，
// 例如 "description": null、"items": null、"minItems": null，按缺省处理
func cleanNullFields(schema map[string]json.RawMessage) {
	for name, value := range schema {
		if name != "const" && isJSONLiteral(value, "null") {
			delete(schema, name)
		}
	}
}

// normalizeNot 把 not 约束规范为可编码的形式。只处理当前节点，子 schema 在各自编码时再规范化。
//
// not 为 null、false、"" 或清理后为空的对象时不限制任何值，直接删除；not 为 true 时本应禁止所有值，
// 这样的 schema 无法满足，按宽松处理删除，避免整个请求失败；not:{"type":"null"} 等价于不可为 null；
// 字符串、数字等标量或数组写法按排除这些值处理，转换为 not+enum
func normalizeNot(schema map[string]json.RawMessage) error {
	raw, ok := schema["not"]
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONLiteral(trimmed, "null") || isJSONLiteral(trimmed, "false") ||
		isJSONLiteral(trimmed, "true") || bytes.Equal(trimmed, []byte(`""`)) {
		delete(schema, "not")
		return nil
	}
	switch trimmed[0] {
	case '{':
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &sub); err != nil || sub == nil {
			return fmt.Errorf("schema.not 必须是 JSON object")
		}
		cleanNullFields(sub)
		if len(sub) == 0 {
			delete(schema, "not")
			return nil
		}
		if len(sub) == 1 {
			if typeName, err := schemaString(sub["type"], "type"); err == nil && strings.EqualFold(typeName, "null") {
				delete(schema, "not")
				schema["nullable"] = json.RawMessage("false")
			}
		}
		return nil
	case '[':
		var values []any
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return fmt.Errorf("schema.not 必须是 JSON object")
		}
		return setNotEnum(schema, values)
	default:
		var value any
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return fmt.Errorf("schema.not 必须是 JSON object")
		}
		return setNotEnum(schema, []any{value})
	}
}

func setNotEnum(schema map[string]json.RawMessage, values []any) error {
	encoded, err := json.Marshal(map[string]any{"enum": values})
	if err != nil {
		return err
	}
	schema["not"] = encoded
	return nil
}

// normalizeBooleanItems 处理布尔形式的 items：true 不限制元素，按空 schema 处理；
// false 不允许任何元素，删除 items 并设 maxItems=0，元素定义交给 normalizeImplicitType 补齐
// （AI Studio 要求数组带 items）
func normalizeBooleanItems(schema map[string]json.RawMessage) {
	value, ok := schema["items"]
	if !ok {
		return
	}
	switch {
	case isJSONLiteral(value, "true"):
		schema["items"] = json.RawMessage(`{}`)
	case isJSONLiteral(value, "false"):
		delete(schema, "items")
		schema["maxItems"] = json.RawMessage("0")
	}
}

// normalizeBooleanProperties 处理布尔或 null 形式的属性 schema，返回被删除的属性名：
// true 与 null 不限制取值，按空 schema 处理；false 表示不允许出现该属性，直接删除
func normalizeBooleanProperties(properties map[string]json.RawMessage) []string {
	var removed []string
	for name, value := range properties {
		switch {
		case isJSONLiteral(value, "true"), isJSONLiteral(value, "null"):
			properties[name] = json.RawMessage(`{}`)
		case isJSONLiteral(value, "false"):
			delete(properties, name)
			removed = append(removed, name)
		}
	}
	return removed
}

// dropRequired 从 required 中去掉已删除的属性，避免 required 引用不存在的属性
func dropRequired(schema map[string]json.RawMessage, removed []string) error {
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	required, err := schemaStrings(raw, "required")
	if err != nil {
		return err
	}
	kept := required[:0]
	for _, name := range required {
		drop := false
		for _, gone := range removed {
			if name == gone {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, name)
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	schema["required"] = encoded
	return nil
}

func isJSONLiteral(raw json.RawMessage, literal string) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte(literal))
}
