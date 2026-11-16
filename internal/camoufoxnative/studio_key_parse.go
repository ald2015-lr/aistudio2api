package camoufoxnative

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
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
// Key 自己那条记录（往外到出现别的 Key 为止）里的资源名 projects/<数字>/…（如 …/keys/…）也算它的项目，并且优先于多条 Key 共用的
// 容器里的 projects/<数字>；自己那条记录里的项目 ID（gen-lang-client-<数字>）与像显示名的文字另记下来，记录里没有项目编号时
// 与响应里的项目对照（见 correlateStudioProject）。整段不是 JSON 时退回按文本扫描，项目取 Key 前后一段距离内最近的 projects/<数字>。
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
	// ProjectDisplay 为与 projects/<数字> 同一层、像项目显示名的文字（只在那一层是项目自己的记录时取，见 studioProjectRecordDisplay；
	// 尽力而为，可为空）
	ProjectDisplay string
	// ProjectID 为 Key 自己那条记录里的项目 ID（gen-lang-client-<数字>，尽力而为，可为空）
	ProjectID string
	// projectHints 为 Key 自己那条记录里像显示名的文字（可能是项目显示名，也可能是 Key 自己的名字），只用来与响应里的项目
	// 显示名逐字对照（见 correlateStudioProject），不直接当项目名
	projectHints []string
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
	ownFrom, resource := noteOwnRecord(&candidate, path)
	for index := len(path) - 1; index >= 0; index-- {
		values := shallowStrings(path[index], 3)
		project := ""
		number := ""
		for _, value := range values {
			if match := studioProjectPattern.FindStringSubmatch(value); match != nil {
				project, number = match[0], match[1]
				break
			}
		}
		if project == "" {
			continue
		}
		if index < ownFrom && resource != "" {
			// 整段的 projects/<数字> 在多条 Key 共用的容器里，Key 自己的资源名却写着它的项目：按资源名
			break
		}
		candidate.ProjectName, candidate.ProjectNumber = project, number
		// 显示名只取与 projects/<数字> 同一层的文字，而且那一层是项目自己的记录（不是 Key 的记录，见 studioProjectRecordDisplay）
		if parent, field, ok := studioStringParent(path[index], project, 3); ok && field != "parent" {
			candidate.ProjectDisplay = studioProjectRecordDisplay(parent)
		}
		break
	}
	if candidate.ProjectNumber == "" && resource != "" {
		candidate.ProjectName, candidate.ProjectNumber = "projects/"+resource, resource
	}
	collector.found = append(collector.found, candidate)
}

// noteOwnRecord 在 Key 自己那条记录里（从最内层容器往外，遇到含有别的 Key 的容器就停：那已是多条 Key 的列表）记下项目 ID
// （gen-lang-client-<数字>）与像显示名的文字，返回自己那条记录最外层容器的序号（path[ownFrom:] 都只含这一个 Key）与 Key 的资源名
// projects/<数字>/…（或整段的 projects/<数字>）里的编号
func noteOwnRecord(candidate *StudioKeyCandidate, path []any) (ownFrom int, resource string) {
	ownFrom = len(path)
	for index := len(path) - 1; index >= 0; index-- {
		values := shallowStrings(path[index], 3)
		if slices.ContainsFunc(values, func(value string) bool { return value != candidate.Key && IsStudioKey(value) }) {
			return ownFrom, resource
		}
		ownFrom = index
		for _, value := range values {
			switch {
			case value == candidate.Key:
			case studioProjectIDPattern.MatchString(value):
				if candidate.ProjectID == "" {
					candidate.ProjectID = value
				}
			case studioProjectResource.MatchString(value):
				if resource == "" {
					resource = studioProjectResource.FindStringSubmatch(value)[1]
				}
			case IsStudioKey(value) || studioKeyScanPattern.MatchString(value) || studioUpperToken.MatchString(value) || !looksLikeDisplayName(value):
			case len(candidate.projectHints) < 8 && !slices.Contains(candidate.projectHints, value):
				candidate.projectHints = append(candidate.projectHints, value)
			}
		}
	}
	return ownFrom, resource
}

// studioStringParent 在容器 limit 层以内找直接含有字符串 target 的那一层容器（与 shallowStrings 同样的顺序，取第一个），返回它与
// target 在其中的字段名（数组里为空）
func studioStringParent(container any, target string, limit int) (any, string, bool) {
	if limit <= 0 {
		return nil, "", false
	}
	switch item := container.(type) {
	case []any:
		for _, child := range item {
			if text, ok := child.(string); ok && strings.TrimSpace(text) == target {
				return item, "", true
			}
			if parent, field, ok := studioStringParent(child, target, limit-1); ok {
				return parent, field, true
			}
		}
	case map[string]any:
		names := make([]string, 0, len(item))
		for name := range item {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			if text, ok := item[name].(string); ok && strings.TrimSpace(text) == target {
				return item, name, true
			}
			if parent, field, ok := studioStringParent(item[name], target, limit-1); ok {
				return parent, field, true
			}
		}
	}
	return nil, "", false
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
			if existing[index].ProjectID == "" {
				existing[index].ProjectID = candidate.ProjectID
			}
			for _, hint := range candidate.projectHints {
				if len(existing[index].projectHints) < 8 && !slices.Contains(existing[index].projectHints, hint) {
					existing[index].projectHints = append(existing[index].projectHints, hint)
				}
			}
		}
		if !merged {
			existing = append(existing, candidate)
		}
	}
	return existing
}

// 新建项目时，页面自己的响应（建项目的接口、项目列表）里会出现新项目的 projects/<数字>。项目接口同样没有公开文档，
// 按与 Key 相同的思路解析：JSON（含 XSSI 前缀、batchexecute 分块、字符串里再包一层 JSON）里整段是 projects/<数字>
// （或其下级资源 projects/<数字>/…）的字符串即一个项目，同一层容器里的 gen-lang-client-<数字>（或对象的 projectId）是项目 ID。
// 显示名只从项目自己的记录里取（对象的 displayName，或同一层像显示名的文字）：下级资源名（如 Key 的 projects/<数字>/…/keys/…）、
// 对象的 parent 字段、同一层（或下一层）含有 Key 或下级资源名的容器都是 Key（或别的下级资源）的记录，那里像显示名的文字多半是
// Key 自己的名字（如“Gemini API Key”），不当项目显示名，只记编号与 ID。正文不是 JSON 时按文本扫描。项目编号至少 6 位
// （实际为 12 位左右），免得把页面脚本里偶然出现的短串当成项目。
var (
	studioProjectResource  = regexp.MustCompile(`^projects/([0-9]{6,})(?:/|$)`)
	studioProjectChild     = regexp.MustCompile(`^projects/[0-9]+/.`)
	studioProjectTextScan  = regexp.MustCompile(`projects/([0-9]{6,})`)
	studioProjectIDPattern = regexp.MustCompile(`^gen-lang-client-[0-9]{6,}$`)
	studioUpperToken       = regexp.MustCompile(`^[A-Z0-9_]+$`)
)

// ExtractStudioProjects 从一段响应正文里找出 Google Cloud 项目（按出现顺序、按编号去重）
func ExtractStudioProjects(body []byte) []StudioProject {
	if !studioProjectTextScan.Match(body) {
		return nil
	}
	text := string(bytes.TrimSpace(body))
	var collector studioProjectCollector
	parsedAny := false
	for _, chunk := range studioJSONChunks(text) {
		var value any
		decoder := json.NewDecoder(strings.NewReader(chunk))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		parsedAny = true
		collector.walk(value, nil, "", 0)
	}
	if parsedAny && len(collector.found) > 0 {
		return collector.found
	}
	var found []StudioProject
	for _, match := range studioProjectTextScan.FindAllStringSubmatchIndex(text, -1) {
		// 前面紧接着字母数字的不算（如 subprojects/…），后面紧接着数字的已被正则吞掉
		if match[0] > 0 && isKeyByte(text[match[0]-1]) {
			continue
		}
		number := text[match[2]:match[3]]
		found = mergeStudioProjects(found, []StudioProject{{Name: "projects/" + number, Number: number}})
	}
	return found
}

type studioProjectCollector struct {
	found []StudioProject
}

// walk 递归遍历 JSON 值；parent 为当前值所在的容器，field 为当前值在对象里的字段名（数组里为空）
func (collector *studioProjectCollector) walk(value any, parent any, field string, depth int) {
	if depth > 64 {
		return
	}
	switch item := value.(type) {
	case []any:
		for _, child := range item {
			collector.walk(child, item, "", depth+1)
		}
	case map[string]any:
		names := make([]string, 0, len(item))
		for name := range item {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			collector.walk(item[name], item, name, depth+1)
		}
	case string:
		trimmed := strings.TrimSpace(item)
		if match := studioProjectResource.FindStringSubmatch(trimmed); match != nil {
			// 只有整段的 projects/<数字>、又不是对象的 parent 字段时，这一层才可能是项目自己的记录
			collector.add(match[1], parent, trimmed == match[0] && field != "parent")
			return
		}
		if len(trimmed) > 1 && (trimmed[0] == '[' || trimmed[0] == '{') && studioProjectTextScan.MatchString(trimmed) {
			var nested any
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			if decoder.Decode(&nested) == nil {
				collector.walk(nested, nil, "", depth+1)
			}
		}
	}
}

// add 记下一个项目：container 为 projects/<数字> 所在的容器，own 表示它可能是项目自己的记录（可以取显示名，见 studioProjectRecordDisplay）
func (collector *studioProjectCollector) add(number string, container any, own bool) {
	project := StudioProject{Name: "projects/" + number, Number: number}
	if object, ok := container.(map[string]any); ok {
		if value, ok := object["projectId"].(string); ok && studioProjectIDPattern.MatchString(strings.TrimSpace(value)) {
			project.ID = strings.TrimSpace(value)
		}
	}
	if container != nil && project.ID == "" {
		for _, value := range shallowStrings(container, 1) {
			if studioProjectIDPattern.MatchString(value) {
				project.ID = value
				break
			}
		}
	}
	if own {
		project.Display = studioProjectRecordDisplay(container)
	}
	collector.found = mergeStudioProjects(collector.found, []StudioProject{project})
}

// studioProjectRecordDisplay 从项目自己的记录（整段 projects/<数字> 所在的那一层容器）里取显示名：对象的 displayName，或同一层第一个
// 像显示名的文字（项目 ID、projects/…、Key、大写枚举除外）。这一层或下一层含有 Key、这一层含有下级资源名 projects/<数字>/… 时，
// 那是 Key（或别的下级资源）的记录，像显示名的文字多半是 Key 自己的名字，不取
func studioProjectRecordDisplay(container any) string {
	if container == nil {
		return ""
	}
	if slices.ContainsFunc(shallowStrings(container, 2), IsStudioKey) ||
		slices.ContainsFunc(shallowStrings(container, 1), studioProjectChild.MatchString) {
		return ""
	}
	if object, ok := container.(map[string]any); ok {
		for _, name := range []string{"displayName", "display_name", "projectDisplayName"} {
			if value, ok := object[name].(string); ok && looksLikeDisplayName(strings.TrimSpace(value)) {
				return strings.TrimSpace(value)
			}
		}
	}
	for _, value := range shallowStrings(container, 1) {
		if studioProjectIDPattern.MatchString(value) || studioProjectScan.MatchString(value) || studioKeyScanPattern.MatchString(value) ||
			studioUpperToken.MatchString(value) || !looksLikeDisplayName(value) {
			continue
		}
		return value
	}
	return ""
}

// mergeStudioProjects 合并项目列表：同一编号只留一条，后来的结果补上缺少的 ID 与显示名
func mergeStudioProjects(existing []StudioProject, more []StudioProject) []StudioProject {
	for _, project := range more {
		if project.Number == "" {
			continue
		}
		merged := false
		for index := range existing {
			if existing[index].Number != project.Number {
				continue
			}
			merged = true
			if existing[index].ID == "" {
				existing[index].ID = project.ID
			}
			if existing[index].Display == "" {
				existing[index].Display = project.Display
			}
		}
		if !merged {
			existing = append(existing, project)
		}
	}
	return existing
}

// 项目对照的依据（写进步骤记录）
const (
	studioKeyMatchRecord  = "Key 的记录"
	studioKeyMatchID      = "项目 ID"
	studioKeyMatchDisplay = "显示名"
	// studioKeyMatchSingle 是最弱的依据：只在打开详情之前用（见 studioKeyRun.match 的 strongOnly）
	studioKeyMatchSingle = "唯一的项目"
)

// correlateStudioProject 在 Key 自己的记录里没有项目编号时，与这次所有响应里看到的项目对照，返回对上的项目与依据；对不上时 ok 为假：
//   - Key 的记录里有项目 ID（gen-lang-client-<数字>），响应里恰好一个项目是这个 ID；
//   - Key 的记录里像显示名的文字，恰好对上一个项目的显示名（逐字，不区分大小写；对上多个项目时不猜）；
//   - 响应里一共只看到一个项目（编号去重后），且它的 ID 与 Key 记录里的项目 ID 不矛盾。
//
// 有多个项目又没有别的依据时不猜
func correlateStudioProject(candidate StudioKeyCandidate, projects []StudioProject) (StudioProject, string, bool) {
	if candidate.ProjectID != "" {
		if project, ok := uniqueStudioProject(projects, func(project StudioProject) bool { return project.ID == candidate.ProjectID }); ok {
			return project, studioKeyMatchID, true
		}
	}
	if len(candidate.projectHints) > 0 {
		if project, ok := uniqueStudioProject(projects, func(project StudioProject) bool {
			return project.Display != "" && slices.ContainsFunc(candidate.projectHints, func(hint string) bool { return strings.EqualFold(hint, project.Display) })
		}); ok {
			return project, studioKeyMatchDisplay, true
		}
	}
	if project, ok := uniqueStudioProject(projects, func(StudioProject) bool { return true }); ok {
		if candidate.ProjectID == "" || project.ID == "" || project.ID == candidate.ProjectID {
			return project, studioKeyMatchSingle, true
		}
	}
	return StudioProject{}, "", false
}

// uniqueStudioProject 返回满足条件的项目：按编号去重后恰好一个时 ok 为真（多个项目的 ID 与显示名合并在同一编号下）
func uniqueStudioProject(projects []StudioProject, match func(StudioProject) bool) (StudioProject, bool) {
	var found []StudioProject
	for _, project := range projects {
		if project.Number != "" && match(project) {
			found = mergeStudioProjects(found, []StudioProject{project})
		}
	}
	if len(found) != 1 {
		return StudioProject{}, false
	}
	return found[0], true
}
