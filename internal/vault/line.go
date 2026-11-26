package vault

import "strings"

// 整行格式（复制整行、复制选中与导出共用）
const (
	lineFormatDash = "dash"
	lineFormatTab  = "tab"
	// 重新导入后不能完全还原的原因类别
	lossyEdgeWhitespace = "edge_whitespace"
	lossyNoteAmbiguous  = "note_ambiguous"
	lossyEmptyRecord    = "empty_record"
	lossyOther          = "other"
)

// buildLine 生成一条资料的整行：字段按 邮箱、密码、辅助邮箱、2FA（includeNote 时再接备注）的顺序，空字段省略，
// 但密码为空而后面还有字段时保留一个空位。先用 ---- 连接，再用同一个解析器解析回来，五个字段与记录完全一致才用这一行；
// 否则改用 Tab 连接再校验；还不一致时仍输出 Tab 行并给出不能完全还原的原因
func buildLine(record *profile, includeNote bool) (line string, format string, lossy string) {
	note := ""
	if includeNote {
		note = record.Note
	}
	fields := lineFields(record, note)
	dash := strings.Join(fields, "----")
	if lineRoundTrips(dash, record, note) {
		return dash, lineFormatDash, ""
	}
	tab := strings.Join(fields, "\t")
	if lineRoundTrips(tab, record, note) {
		return tab, lineFormatTab, ""
	}
	return tab, lineFormatTab, lossyReason(record, note)
}

func lineFields(record *profile, note string) []string {
	var tail []string
	for _, value := range []string{record.RecoveryEmail, record.TOTP, note} {
		if value != "" {
			tail = append(tail, value)
		}
	}
	fields := []string{record.Email}
	if record.Password != "" || len(tail) > 0 {
		fields = append(fields, record.Password)
	}
	return append(fields, tail...)
}

// lineRoundTrips 判断整行重新导入后五个字段是否与记录完全一致（totp_flags 不参与比较：otpauth 的非默认参数本来就不保存）
func lineRoundTrips(line string, record *profile, note string) bool {
	kind, parsed := parseRawLine(line)
	return kind == lineData && parsed.reason == "" && parsed.email == record.Email && parsed.password == record.Password &&
		parsed.recovery == record.RecoveryEmail && parsed.totp == record.TOTP && parsed.note == note
}

func lossyReason(record *profile, note string) string {
	switch {
	case record.Password == "" && record.RecoveryEmail == "" && record.TOTP == "" && note == "":
		return lossyEmptyRecord
	case record.Password != trimSpace(record.Password):
		return lossyEdgeWhitespace
	case note != "":
		// 去掉备注后能还原（或只剩邮箱）：问题出在备注像邮箱、2FA，或备注里的空白在重新导入时会被整理
		rest := lineFields(record, "")
		if len(rest) == 1 || lineRoundTrips(strings.Join(rest, "\t"), record, "") {
			return lossyNoteAmbiguous
		}
		return lossyOther
	default:
		return lossyOther
	}
}
