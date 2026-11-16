package camoufoxnative

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 解析 AI Studio API Keys 页面（aistudio.google.com/apikey）自己发出的请求的响应正文，从中找出 API Key 与所属项目。
//
// 假设（页面接口没有公开文档，形态随官网改版可能变化，因此不按固定字段位置解析）：
//   - 响应正文是 JSON 或“类 JSON”：可能带 XSSI 前缀（)]}'），可能是 batchexecute 那种按行分块、每块前有长度行的格式，
//     也可能是 protobuf-JSON 的多层嵌套数组；JSON 里的字符串还可能再包一层 JSON（batchexecute 的第三项）。
//   - Key 是完整字符串值，形如 AIza 加 35 位（经典 Google API Key），或 AQ. 加至少 20 位（新格式）。
//   - 同一条 Key 记录里有形如 projects/<数字> 的项目名；离 Key 最近的那一层数组或对象里的 projects/<数字> 即其所属项目。
//
// 解析先按 JSON 递归遍历（逐层尝试解开字符串里的 JSON），对每个 Key 从它所在的最内层容器往外找第一个含 projects/<数字> 的容器；
// 整段不是 JSON 时退回按文本扫描，项目取 Key 前后一段距离内最近的 projects/<数字>。
// 解析结果只在内存里交给调用方，任何错误文案都不带 Key 原文（见 RedactStudioKeys）。

var (
	studioKeyPattern       = regexp.MustCompile(`^(?:AIza[0-9A-Za-z_-]{35}|AQ\.[0-9A-Za-z_-]{20,})$`)
	studioKeyScanPattern   = regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}|AQ\.[0-9A-Za-z_-]{20,}`)
	studioProjectPattern   = regexp.MustCompile(`^projects/([0-9]+)$`)
	studioProjectScan      = regexp.MustCompile(`projects/([0-9]+)`)
	studioDisplayNameRegex = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()'-]{1,79}$`)
)

// studioKeyScanWindow 为按文本扫描时在 Key 前后找项目名的距离（字节）
const studioKeyScanWindow = 600

// StudioKeyCandidate 为在响应正文里找到的一条 Key 与它所属的项目。Key 是机密：不要写进日志或错误
type StudioKeyCandidate struct {
	Key string
	// ProjectName 为 projects/<数字>，ProjectNumber 为其中的数字；没有找到时都为空
	ProjectName   string
	ProjectNumber string
	// ProjectDisplay 为同一记录里看起来像项目显示名的字符串（尽力而为，可为空）
	ProjectDisplay string
}

// IsStudioKey 判断字符串是否是完整的 API Key
func IsStudioKey(value string) bool {
	return studioKeyPattern.MatchString(value)
}

// RedactStudioKeys 把文本里所有像 API Key 的片段换成“<key:…末四位>”，用于错误、日志与诊断文本
func RedactStudioKeys(text string) string {
	return studioKeyScanPattern.ReplaceAllStringFunc(text, func(key string) string {
		return "<key:…" + StudioKeyLast4(key) + ">"
	})
}

// StudioKeyLast4 返回 Key 的末四位
func StudioKeyLast4(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}

// ExtractStudioKeys 从一段响应正文里找出全部 Key（按出现顺序去重）
func ExtractStudioKeys(body []byte) []StudioKeyCandidate {
	text := string(bytes.TrimSpace(body))
	if !studioKeyScanPattern.MatchString(text) {
		return nil
	}
	var collector studioKeyCollector
	parsedAny := false
	for _, chunk := range studioJSONChunks(text) {
		var value any
		decoder := json.NewDecoder(strings.NewReader(chunk))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		parsedAny = true
		collector.walk(value, nil, 0)
	}
	if !parsedAny || len(collector.found) == 0 {
		return scanStudioKeys(text)
	}
	return collector.found
}

// studioJSONChunks 把正文拆成可能的 JSON 段：去掉 XSSI 前缀；batchexecute 按行分块时每行单独尝试；也尝试整段
func studioJSONChunks(text string) []string {
	if strings.HasPrefix(text, ")]}'") {
		text = strings.TrimSpace(text[4:])
	}
	chunks := []string{text}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == text {
			continue
		}
		if line[0] == '[' || line[0] == '{' {
			chunks = append(chunks, line)
		}
	}
	return chunks
}

type studioKeyCollector struct {
	found []StudioKeyCandidate
}

// walk 递归遍历 JSON 值；path 为从根到当前值的容器链（最内层在最后）
func (collector *studioKeyCollector) walk(value any, path []any, depth int) {
	if depth > 64 {
		return
	}
	switch item := value.(type) {
	case []any:
		next := append(path, item)
		for _, child := range item {
			collector.walk(child, next, depth+1)
		}
	case map[string]any:
		next := append(path, item)
		for _, child := range item {
			collector.walk(child, next, depth+1)
		}
	case string:
		trimmed := strings.TrimSpace(item)
		if IsStudioKey(trimmed) {
			collector.add(trimmed, path)
			return
		}
		// 字符串里再包一层 JSON（batchexecute）
		if len(trimmed) > 1 && (trimmed[0] == '[' || trimmed[0] == '{') && studioKeyScanPattern.MatchString(trimmed) {
			var nested any
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			if decoder.Decode(&nested) == nil {
				collector.walk(nested, path, depth+1)
			}
		}
	}
}

func (collector *studioKeyCollector) add(key string, path []any) {
	for _, existing := range collector.found {
		if existing.Key == key {
			return
		}
	}
	candidate := StudioKeyCandidate{Key: key}
	for index := len(path) - 1; index >= 0; index-- {
		values := shallowStrings(path[index], 3)
		project := ""
		for _, value := range values {
			if match := studioProjectPattern.FindStringSubmatch(value); match != nil {
				project = match[0]
				candidate.ProjectNumber = match[1]
				break
			}
		}
		if project == "" {
			continue
		}
		candidate.ProjectName = project
		for _, value := range values {
			if value != key && !IsStudioKey(value) && !studioProjectScan.MatchString(value) && looksLikeDisplayName(value) {
				candidate.ProjectDisplay = value
				break
			}
		}
		break
	}
	collector.found = append(collector.found, candidate)
}

// shallowStrings 收集容器在 limit 层以内的字符串（按出现顺序；对象按键排序，结果稳定）
func shallowStrings(container any, limit int) []string {
	var out []string
	var visit func(value any, depth int)
	visit = func(value any, depth int) {
		if depth > limit {
			return
		}
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				visit(child, depth+1)
			}
		case map[string]any:
			keys := make([]string, 0, len(item))
			for name := range item {
				keys = append(keys, name)
			}
			sortStrings(keys)
			for _, name := range keys {
				visit(item[name], depth+1)
			}
		case string:
			out = append(out, strings.TrimSpace(item))
		}
	}
	visit(container, 0)
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// looksLikeDisplayName 判断字符串像不像项目显示名：不是地址、不是纯数字、不是长串编码
func looksLikeDisplayName(value string) bool {
	if value == "" || strings.Contains(value, "/") || strings.Contains(value, "@") || strings.Contains(value, "://") {
		return false
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return false
	}
	return studioDisplayNameRegex.MatchString(value)
}

// scanStudioKeys 按文本扫描：正文不是 JSON 时的退路
func scanStudioKeys(text string) []StudioKeyCandidate {
	var found []StudioKeyCandidate
	seen := make(map[string]bool)
	projects := studioProjectScan.FindAllStringSubmatchIndex(text, -1)
	for _, location := range studioKeyScanPattern.FindAllStringIndex(text, -1) {
		key := text[location[0]:location[1]]
		// 前后不能紧接着 Key 字符（避免把更长的串截出一段）
		if location[0] > 0 && isKeyByte(text[location[0]-1]) || location[1] < len(text) && isKeyByte(text[location[1]]) {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		candidate := StudioKeyCandidate{Key: key}
		best := -1
		for _, project := range projects {
			distance := project[0] - location[1]
			if project[1] <= location[0] {
				distance = location[0] - project[1]
			}
			if distance < 0 {
				distance = -distance
			}
			if distance <= studioKeyScanWindow && (best < 0 || distance < best) {
				best = distance
				candidate.ProjectName = text[project[0]:project[1]]
				candidate.ProjectNumber = text[project[2]:project[3]]
			}
		}
		found = append(found, candidate)
	}
	return found
}

func isKeyByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value == '_' || value == '-'
}

// mergeStudioKeys 合并多段响应里找到的 Key：同一 Key 只留一条，后来的结果补上缺少的项目信息
func mergeStudioKeys(existing []StudioKeyCandidate, more []StudioKeyCandidate) []StudioKeyCandidate {
	for _, candidate := range more {
		merged := false
		for index := range existing {
			if existing[index].Key != candidate.Key {
				continue
			}
			merged = true
			if existing[index].ProjectName == "" && candidate.ProjectName != "" {
				existing[index].ProjectName = candidate.ProjectName
				existing[index].ProjectNumber = candidate.ProjectNumber
			}
			if existing[index].ProjectDisplay == "" {
				existing[index].ProjectDisplay = candidate.ProjectDisplay
			}
		}
		if !merged {
			existing = append(existing, candidate)
		}
	}
	return existing
}
