package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// fileVersion 为两份文件当前的格式版本；以后任何格式变化都要升版本
const fileVersion = 1

// 文件错误的原因类别（FileError.Reason）
const (
	problemRead              = "read"
	problemJSON              = "json"
	problemVersion           = "version"
	problemInvalid           = "invalid"
	problemMissingWithBackup = "missing_with_backup"
)

// loadProblem 为文件读不出或内容不对的原因：text 是固定的中文说明（日志的“原因=”与页面共用），从不含文件内容与 err.Error() 原文
type loadProblem struct {
	reason string
	text   string
}

// checkJSON 逐 token 检查同一对象里有没有重复的键（不区分大小写，与 encoding/json 认字段的方式一致）：
// encoding/json 解到结构体时重复键只留最后一个，人工编辑出的重复键会在下次写盘时悄悄丢掉一份。语法错误交给 json.Unmarshal 报
func checkJSON(data []byte) *loadProblem {
	decoder := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]struct{}
	}
	var stack []*frame
	afterValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				stack = append(stack, &frame{object: true, expectKey: true, keys: map[string]struct{}{}})
			case '[':
				stack = append(stack, &frame{})
			default:
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				afterValue()
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
				top := stack[len(stack)-1]
				key := strings.ToLower(value)
				if _, exists := top.keys[key]; exists {
					return &loadProblem{reason: problemJSON, text: fmt.Sprintf("有重复的键（第 %d 字节附近）", decoder.InputOffset())}
				}
				top.keys[key] = struct{}{}
				top.expectKey = false
				continue
			}
			afterValue()
		default:
			afterValue()
		}
	}
}

// jsonProblem 只取错误的位置：语法错误的原文会带出一个字符，类型错误带字段路径，都不写进说明
func jsonProblem(err error) *loadProblem {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return &loadProblem{reason: problemJSON, text: fmt.Sprintf("JSON 格式错误（第 %d 字节附近）", syntax.Offset)}
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &loadProblem{reason: problemJSON, text: fmt.Sprintf("JSON 格式错误（第 %d 字节附近的值类型不对）", typeErr.Offset)}
	}
	return &loadProblem{reason: problemJSON, text: "JSON 格式错误（文件不完整或不是 JSON）"}
}

func versionProblem(version int) *loadProblem {
	if version == 0 {
		return &loadProblem{reason: problemVersion, text: "文件没有版本号（version）"}
	}
	return &loadProblem{reason: problemVersion, text: fmt.Sprintf("版本 %d 不认识（可能由更新的版本写入）", version)}
}

func invalidProblem(index int, rule string) *loadProblem {
	return &loadProblem{reason: problemInvalid, text: fmt.Sprintf("第 %d 条记录不合法（%s）", index+1, rule)}
}
