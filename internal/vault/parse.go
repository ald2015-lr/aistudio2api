package vault

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// 无法识别的原因（ImportItem.Reason）
const (
	reasonFirstNotEmail = "first_not_email"
	reasonNoFields      = "no_fields"
	reasonLineTooLong   = "line_too_long"
	reasonControlChar   = "control_char"
	reasonFieldTooLong  = "field_too_long"
)

// 第 1 段不是邮箱时的格式提示（ImportItem.Hint）：只给类别，不回显第 1 段（格式错时它可能就是密码）
const (
	hintDashTooShort        = "dash_too_short"
	hintUnsupportedSep      = "unsupported_separator"
	hintMixedSeparators     = "mixed_separators"
	hintLooksLikeFamilyList = "looks_like_family_list"
)

// 逐行提示（ImportItem.Warnings）
const (
	warnDashRun            = "dash_run"
	warnMixedSeparators    = "mixed_separators"
	warnEmptyPassword      = "empty_password"
	warnPasswordLooksEmail = "password_looks_email"
	warnPasswordLooksTOTP  = "password_looks_totp"
	warnExtraEmail         = "extra_email"
	warnExtraTOTP          = "extra_totp"
	warnLooksAppPassword   = "looks_app_password"
	warnTOTPLettersOnly    = "totp_letters_only"
	warnOTPAuthParams      = "otpauth_params"
	warnOTPAuthInvalid     = "otpauth_invalid"
	warnRecoverySame       = "recovery_same"
	warnRepeated           = "repeated"
)

// suspectCodes 为“格式可能整体错位”的提示：任何一种的行数 ≥ 50，或 ≥ 3 且 ≥ 有效行的 20% 时要用户勾选“我已核对”才能确认
var suspectCodes = []string{warnPasswordLooksEmail, warnPasswordLooksTOTP, warnEmptyPassword}

// 字段名（changes、overwrites、kept、clear、reveal 的 fields 共用）
const (
	fieldPassword = "password"
	fieldRecovery = "recovery_email"
	fieldTOTP     = "totp"
	fieldNote     = "note"
)

var profileFields = []string{fieldPassword, fieldRecovery, fieldTOTP, fieldNote}

type lineKind int

const (
	lineBlank lineKind = iota
	lineComment
	lineData
)

func (kind lineKind) String() string {
	switch kind {
	case lineBlank:
		return "blank"
	case lineComment:
		return "comment"
	default:
		return "data"
	}
}

// parsedLine 为一行资料的解析结果（含秘密：打印一律打码）。reason 非空为无法识别，此时只有第 1 段是合法邮箱才带 email，其余字段为空
type parsedLine struct {
	source, line int
	email        string
	password     string
	recovery     string
	totp         string
	totpFlags    []string
	note         string
	reason       string
	hint         string
	field        string
	warnings     []string
}

func (line parsedLine) String() string   { return redactedText }
func (line parsedLine) GoString() string { return redactedText }
func (line parsedLine) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redactedText))
}

// value 返回字段的值（字段名见 profileFields）
func (line *parsedLine) value(field string) string {
	switch field {
	case fieldPassword:
		return line.password
	case fieldRecovery:
		return line.recovery
	case fieldTOTP:
		return line.totp
	default:
		return line.note
	}
}

// dashUnits 为破折号类字符的单位数：半角 - 与各种横线、减号算 1，中文破折号 —（输入法一次打出两个）算 2，其余为 0
func dashUnits(r rune) int {
	switch r {
	case '-', '\u2010', '\u2011', '\u2012', '\u2013', '\u2212', '\uFE63', '\uFF0D':
		return 1
	case '\u2014', '\u2015', '\uFE58':
		return 2
	}
	return 0
}

// dashRun 为一串连续的破折号类字符：[start, end) 为字节范围，units 为单位合计
type dashRun struct {
	start int
	end   int
	units int
}

func dashRuns(text string) []dashRun {
	var runs []dashRun
	current := dashRun{start: -1}
	for index, r := range text {
		if units := dashUnits(r); units > 0 {
			if current.start < 0 {
				current.start = index
			}
			current.units += units
			continue
		}
		if current.start >= 0 {
			current.end = index
			runs = append(runs, current)
			current = dashRun{start: -1}
		}
	}
	if current.start >= 0 {
		current.end = len(text)
		runs = append(runs, current)
	}
	return runs
}

func hasLongDashRun(text string) bool {
	for _, run := range dashRuns(text) {
		if run.units >= 4 {
			return true
		}
	}
	return false
}

// fieldSplit 为一行分段的结果
type fieldSplit struct {
	segments []string
	// tab 为 Tab 模式（行内有 Tab，只按 Tab 分隔、破折号都是内容）
	tab bool
	// dashRun 为破折号模式下有分隔串不正好是 4 个单位
	dashRun bool
	// mixed 为 Tab 模式下某段里有 ≥ 4 个单位的破折号串
	mixed bool
}

// splitFields 分段：行内有 Tab 就只按 Tab 分隔（每个 Tab 一个分隔，连续两个 Tab 之间是空段）；没有 Tab 时连续的破折号类字符
// 合计 ≥ 4 个单位就是一个分隔、不论多长，不足 4 个单位是内容。每段去首尾空白，末尾的空段丢弃
func splitFields(text string) fieldSplit {
	var split fieldSplit
	var raw []string
	if strings.ContainsRune(text, '\t') {
		split.tab = true
		raw = strings.Split(text, "\t")
		for _, part := range raw {
			if hasLongDashRun(part) {
				split.mixed = true
			}
		}
	} else {
		previous := 0
		for _, run := range dashRuns(text) {
			if run.units < 4 {
				continue
			}
			raw = append(raw, text[previous:run.start])
			previous = run.end
			if run.units != 4 {
				split.dashRun = true
			}
		}
		raw = append(raw, text[previous:])
	}
	for index := range raw {
		raw[index] = trimSpace(raw[index])
	}
	for len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	split.segments = raw
	return split
}

// trimSpace 去首尾空白（含全角空格、NBSP）以及 BOM、零宽空格
func trimSpace(text string) string {
	return strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF' || r == '\u200B'
	})
}

// isControl 为行内不接受的控制字符：U+0000–U+001F 中除 Tab 以外的、U+007F、U+2028/2029
func isControl(r rune) bool {
	return (r < 0x20 && r != '\t') || r == 0x7f || r == 0x2028 || r == 0x2029
}

func hasControl(text string) bool {
	return strings.IndexFunc(text, isControl) >= 0
}

func isComment(text string) bool {
	return strings.HasPrefix(text, "#") || strings.HasPrefix(text, "＃")
}

// parseRawLine 解析一个原始行（已按 \n 分好，可能带末尾的 \r）
func parseRawLine(raw string) (lineKind, parsedLine) {
	raw = strings.TrimSuffix(raw, "\r")
	if len(raw) > MaxLineBytes {
		return lineData, parsedLine{reason: reasonLineTooLong}
	}
	text := trimSpace(raw)
	if text == "" {
		return lineBlank, parsedLine{}
	}
	if isComment(text) {
		return lineComment, parsedLine{}
	}
	if hasControl(text) {
		return lineData, parsedLine{reason: reasonControlChar}
	}
	return lineData, classifyLine(text)
}

// segmentItem 为第 3 段起的一段与它的判别结果
type segmentItem struct {
	text     string
	email    string
	totp     totpInfo
	kind     int
	assigned bool
}

const (
	segmentNote = iota
	segmentEmail
	segmentTOTP
)

// classifyLine 判别一行的字段：第 1 段必须是邮箱，第 2 段按位置是密码，第 3 段起按内容判别（邮箱→辅助邮箱，2FA 密钥→2FA，其余→备注）
func classifyLine(text string) parsedLine {
	split := splitFields(text)
	if len(split.segments) == 0 {
		return parsedLine{reason: reasonFirstNotEmail}
	}
	email, ok := NormalizeEmail(split.segments[0])
	if !ok {
		return parsedLine{reason: reasonFirstNotEmail, hint: firstSegmentHint(split.segments[0], split.tab)}
	}
	if len(split.segments) == 1 {
		return parsedLine{email: email, reason: reasonNoFields}
	}
	result := parsedLine{email: email}
	if split.dashRun {
		result.warnings = append(result.warnings, warnDashRun)
	}
	if split.mixed {
		result.warnings = append(result.warnings, warnMixedSeparators)
	}
	password := split.segments[1]
	switch {
	case password == "":
		result.warnings = append(result.warnings, warnEmptyPassword)
	case len(password) > MaxPasswordBytes:
		return parsedLine{email: email, reason: reasonFieldTooLong, field: fieldPassword}
	default:
		if _, looksEmail := NormalizeEmail(password); looksEmail {
			result.warnings = append(result.warnings, warnPasswordLooksEmail)
		} else if info := parseTOTP(password); info.ok && info.definite {
			result.warnings = append(result.warnings, warnPasswordLooksTOTP)
		}
	}
	result.password = password

	var items []segmentItem
	for _, segment := range split.segments[2:] {
		if segment == "" {
			continue
		}
		item := segmentItem{text: segment}
		if address, isEmail := NormalizeEmail(segment); isEmail {
			item.kind, item.email = segmentEmail, address
		} else if looksAppPassword(segment) {
			result.warnings = appendOnce(result.warnings, warnLooksAppPassword)
		} else if info := parseTOTP(segment); info.ok {
			item.kind, item.totp = segmentTOTP, info
		} else if info.otpauth {
			result.warnings = appendOnce(result.warnings, warnOTPAuthInvalid)
		}
		items = append(items, item)
	}
	// 辅助邮箱：第一个归位，其余进备注
	for index := range items {
		if items[index].kind != segmentEmail {
			continue
		}
		if result.recovery == "" {
			result.recovery = items[index].email
			items[index].assigned = true
		} else {
			result.warnings = appendOnce(result.warnings, warnExtraEmail)
		}
	}
	// 2FA：含数字 2–7 的“确定”候选优先，同等时取第一个；其余像 2FA 的进备注
	chosen := -1
	for index := range items {
		if items[index].kind == segmentTOTP && items[index].totp.definite {
			chosen = index
			break
		}
	}
	if chosen < 0 {
		for index := range items {
			if items[index].kind == segmentTOTP {
				chosen = index
				break
			}
		}
	}
	if chosen >= 0 {
		info := items[chosen].totp
		items[chosen].assigned = true
		result.totp, result.totpFlags = info.secret, info.flags()
		if !info.definite {
			result.warnings = append(result.warnings, warnTOTPLettersOnly)
		}
		if info.params {
			result.warnings = append(result.warnings, warnOTPAuthParams)
		}
		for index := range items {
			if index != chosen && items[index].kind == segmentTOTP {
				result.warnings = appendOnce(result.warnings, warnExtraTOTP)
			}
		}
	}
	var notes []string
	for _, item := range items {
		if !item.assigned {
			notes = append(notes, item.text)
		}
	}
	result.note = strings.Join(notes, "----")
	if len(result.note) > MaxNoteBytes {
		return parsedLine{email: email, reason: reasonFieldTooLong, field: fieldNote}
	}
	if result.recovery != "" && result.recovery == email {
		result.warnings = append(result.warnings, warnRecoverySame)
	}
	if result.password == "" && result.recovery == "" && result.totp == "" && result.note == "" {
		return parsedLine{email: email, reason: reasonNoFields}
	}
	return result
}

func appendOnce(list []string, value string) []string {
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}

// firstSegmentHint 为第 1 段不是邮箱时的提示类别：Tab 行第 1 段里有 ≥ 4 个单位的破折号 → mixed_separators；
// 某个 1–3 个单位的破折号串之前是合法邮箱 → dash_too_short；某个 : | , ; 或空白之前是合法邮箱 → unsupported_separator
func firstSegmentHint(first string, tab bool) string {
	folded := foldWidth(first)
	if tab && hasLongDashRun(folded) {
		return hintMixedSeparators
	}
	for _, run := range dashRuns(folded) {
		if run.units < 4 && run.start > 0 {
			if _, ok := NormalizeEmail(folded[:run.start]); ok {
				return hintDashTooShort
			}
		}
	}
	for index, r := range folded {
		if index > 0 && (strings.ContainsRune(":|,;", r) || unicode.IsSpace(r)) {
			if _, ok := NormalizeEmail(folded[:index]); ok {
				return hintUnsupportedSep
			}
		}
	}
	return ""
}

// parsedImport 为一次导入的全部来源的解析结果（锁外完成，只依赖文本）
type parsedImport struct {
	sources  []SourceSummary
	lines    []parsedLine // 非空、非注释的行，按来源与行号顺序
	blank    int
	comments int
	valid    int
	invalid  int
	// refs 为每个邮箱的有效行（同一批里重复的邮箱）
	refs     map[string][]LineRef
	warnings map[string]int
	suspect  []string
	hints    []string
}

// parseImportSources 检查上限并逐来源、逐行解析
func parseImportSources(sources []ImportSource) (*parsedImport, error) {
	if len(sources) > MaxImportSources {
		return nil, tooLarge(fmt.Sprintf("一次最多导入 %d 个来源（文件与粘贴框），请分批导入", MaxImportSources))
	}
	total := 0
	for _, source := range sources {
		total += len(source.Text)
	}
	if total > MaxImportBytes {
		return nil, tooLarge("一次导入的文本超过 4 MB，请分批导入")
	}
	parsed := &parsedImport{refs: map[string][]LineRef{}, warnings: map[string]int{}}
	nonBlank, noFields := 0, 0
	for sourceIndex, source := range sources {
		text := strings.TrimPrefix(source.Text, "\uFEFF")
		rawLines := strings.Split(text, "\n")
		if rawLines[len(rawLines)-1] == "" {
			rawLines = rawLines[:len(rawLines)-1]
		}
		parsed.sources = append(parsed.sources, SourceSummary{Name: cleanSourceName(source.Name), Lines: len(rawLines)})
		for lineIndex, raw := range rawLines {
			kind, line := parseRawLine(raw)
			switch kind {
			case lineBlank:
				parsed.blank++
				continue
			case lineComment:
				parsed.comments++
			}
			nonBlank++
			if nonBlank > MaxImportLines {
				return nil, tooLarge(fmt.Sprintf("一次最多导入 %d 行，请分批导入", MaxImportLines))
			}
			if kind == lineComment {
				continue
			}
			line.source, line.line = sourceIndex, lineIndex+1
			if line.reason == "" {
				parsed.valid++
				parsed.refs[line.email] = append(parsed.refs[line.email], LineRef{Source: sourceIndex, Line: lineIndex + 1})
			} else {
				parsed.invalid++
				if line.reason == reasonNoFields {
					noFields++
				}
			}
			parsed.lines = append(parsed.lines, line)
		}
	}
	for index := range parsed.lines {
		line := &parsed.lines[index]
		if line.reason == "" && len(parsed.refs[line.email]) > 1 {
			line.warnings = append(line.warnings, warnRepeated)
		}
		for _, warning := range line.warnings {
			parsed.warnings[warning]++
		}
	}
	for _, code := range suspectCodes {
		if count := parsed.warnings[code]; count >= 50 || (count >= 3 && count*5 >= parsed.valid) {
			parsed.suspect = append(parsed.suspect, code)
		}
	}
	if noFields >= 3 && noFields*2 > parsed.valid+noFields {
		parsed.hints = append(parsed.hints, hintLooksLikeFamilyList)
	}
	return parsed, nil
}

// cleanSourceName 去掉来源名里的控制字符、去首尾空白、截到 100 个字符
func cleanSourceName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, name)
	name = trimSpace(name)
	if runes := []rune(name); len(runes) > maxSourceNameRunes {
		name = string(runes[:maxSourceNameRunes])
	}
	return name
}

func tooLarge(message string) *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeTooLarge, Message: message}
}
