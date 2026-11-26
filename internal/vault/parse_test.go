package vault

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestSplitFields 分段的两种模式：有 Tab 只按 Tab；没有 Tab 时连续 ≥4 个单位的破折号不论多长都只算一个分隔，
// 不正好 4 个单位时提示 dash_run；不足 4 个单位是内容；两个分隔之间只有空白是空段；Tab 行里 ---- 是内容并提示 mixed_separators
func TestSplitFields(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    []string
		tab     bool
		dashRun bool
		mixed   bool
	}{
		{"四个半角", "a----b", []string{"a", "b"}, false, false, false},
		{"五个半角", "a-----b", []string{"a", "b"}, false, true, false},
		{"七个半角", "a-------b", []string{"a", "b"}, false, true, false},
		{"八个半角", "a--------b", []string{"a", "b"}, false, true, false},
		{"十二个半角", "a------------b", []string{"a", "b"}, false, true, false},
		{"三个半角是内容", "a---b", []string{"a---b"}, false, false, false},
		{"两个半角是内容", "a--b----c", []string{"a--b", "c"}, false, false, false},
		{"中文破折号一对", "a——b", []string{"a", "b"}, false, false, false},
		{"中文破折号两对", "a————b", []string{"a", "b"}, false, true, false},
		{"中文破折号三对", "a——————b", []string{"a", "b"}, false, true, false},
		{"单个中文破折号是内容", "a—b", []string{"a—b"}, false, false, false},
		{"全角减号四个", "a－－－－b", []string{"a", "b"}, false, false, false},
		{"混合破折号合计四个单位", "a-—-b", []string{"a", "b"}, false, false, false},
		{"混合破折号三个单位", "a—-b", []string{"a—-b"}, false, false, false},
		{"其他横线", "a\u2010\u2011\u2012\u2013b", []string{"a", "b"}, false, false, false},
		{"减号与小横线", "a\u2212\uFE63\u2212\uFE63b", []string{"a", "b"}, false, false, false},
		{"横线与小破折号", "a\u2015\uFE58b", []string{"a", "b"}, false, false, false},
		{"分隔之间只有空白", "a----   ----b", []string{"a", "", "b"}, false, false, false},
		{"末尾空段丢弃", "a----b----", []string{"a", "b"}, false, false, false},
		{"末尾多个空段丢弃", "a----b----  ----", []string{"a", "b"}, false, false, false},
		{"段去首尾空白", " a ---- b ", []string{"a", "b"}, false, false, false},
		{"Tab 分隔", "a\tb\tc", []string{"a", "b", "c"}, true, false, false},
		{"连续 Tab 是空段", "a\t\tc", []string{"a", "", "c"}, true, false, false},
		{"Tab 行里破折号是内容", "a\t----\tc", []string{"a", "----", "c"}, true, false, true},
		{"Tab 行里短破折号", "a\tb-\tc", []string{"a", "b-", "c"}, true, false, false},
		{"Tab 行第 1 段含分隔", "a ----b\tc", []string{"a ----b", "c"}, true, false, true},
		{"Tab 行末尾空段丢弃", "a\tb\t\t", []string{"a", "b"}, true, false, false},
	}
	for _, item := range cases {
		split := splitFields(item.line)
		if !slices.Equal(split.segments, item.want) || split.tab != item.tab || split.dashRun != item.dashRun || split.mixed != item.mixed {
			t.Errorf("%s: splitFields(%q) = %q tab=%v dashRun=%v mixed=%v，期望 %q tab=%v dashRun=%v mixed=%v",
				item.name, item.line, split.segments, split.tab, split.dashRun, split.mixed, item.want, item.tab, item.dashRun, item.mixed)
		}
	}
}

type lineWant struct {
	kind     lineKind
	email    string
	password string
	recovery string
	totp     string
	flags    []string
	note     string
	reason   string
	hint     string
	field    string
	warnings []string
}

// TestParseProfileLine 设计 §4.5 的判别表逐条断言
func TestParseProfileLine(t *testing.T) {
	const (
		email = "a@example.com"
		key   = "JBSWY3DPEHPK3PXP"
		rfc   = "GEZDGNBVGY3TQOJQ"
	)
	data := lineData
	cases := []struct {
		no   int
		line string
		want lineWant
	}{
		{1, "a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com", totp: key}},
		{2, "a@example.com----Fake-Pass-1----JBSWY3DPEHPK3PXP----r@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com", totp: key}},
		{3, "a@example.com----Fake-Pass-1----r@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com"}},
		{4, "a@example.com----Fake-Pass-1----JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{5, "a@example.com----Fake-Pass-1----jbsw y3dp ehpk 3pxp", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{6, "a@example.com----Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1"}},
		{7, "  A@Example.COM ----  Fake-Pass-1  ", lineWant{kind: data, email: email, password: "Fake-Pass-1"}},
		{8, "a@example.com----p@ss--word.x", lineWant{kind: data, email: email, password: "p@ss--word.x"}},
		{9, "a@example.com----pass---word", lineWant{kind: data, email: email, password: "pass---word"}},
		{10, "a@example.com-----Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1", warnings: []string{"dash_run"}}},
		{11, "a@example.com--------r@example.com", lineWant{kind: data, email: email, password: "r@example.com", warnings: []string{"dash_run", "password_looks_email"}}},
		{12, "a@example.com------------JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: key, warnings: []string{"dash_run", "password_looks_totp"}}},
		{13, "a@example.com----Fake-Pass-1----otpauth://totp/Google:a%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Google", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{14, "a@example.com----Fake-Pass-1----otpauth://totp/Google:a%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Google&digits=8", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key, flags: []string{"otpauth_params"}, warnings: []string{"otpauth_params"}}},
		{14, "a@example.com----Fake-Pass-1----otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&period=60", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key, flags: []string{"otpauth_params"}, warnings: []string{"otpauth_params"}}},
		{14, "a@example.com----Fake-Pass-1----otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&algorithm=SHA256", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key, flags: []string{"otpauth_params"}, warnings: []string{"otpauth_params"}}},
		{15, "a@example.com----Fake-Pass-1----otpauth://hotp/x?secret=JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "otpauth://hotp/x?secret=JBSWY3DPEHPK3PXP", warnings: []string{"otpauth_invalid"}}},
		{16, "a@example.com----Fake-Pass-1----otpauth://totp/x?issuer=G", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "otpauth://totp/x?issuer=G", warnings: []string{"otpauth_invalid"}}},
		{17, "a@example.com----Fake-Pass-1----r1@example.com----r2@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r1@example.com", note: "r2@example.com", warnings: []string{"extra_email"}}},
		{18, "a@example.com----Fake-Pass-1----JBSWY3DPEHPK3PXP----GEZDGNBVGY3TQOJQ", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key, note: rfc, warnings: []string{"extra_totp"}}},
		{19, "a@example.com----Fake-Pass-1----美国----手机 138 0000 0000", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "美国----手机 138 0000 0000"}},
		{20, "a@example.com----Fake-Pass-1----1990-01-01", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "1990-01-01"}},
		{21, "a@example.com----Fake-Pass-1----family main account", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "family main account"}},
		{22, "a@example.com----Fake-Pass-1----familymainaccount", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: "FAMILYMAINACCOUNT", flags: []string{"letters_only"}, warnings: []string{"totp_letters_only"}}},
		{23, "a@example.com----Fake-Pass-1----JbSwY3DpEhPk3PxP", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "JbSwY3DpEhPk3PxP"}},
		{24, "a@example.com----Fake-Pass-1----JBSWY3DPEHPK3PX", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "JBSWY3DPEHPK3PX"}},
		{25, "a@example.com----Fake-Pass-1----abc def ghi jkl mno pqr", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "abc def ghi jkl mno pqr"}},
		{26, "a@example.com----Fake-Pass-1----JBSWY3DPEHPK3PXP====", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{26, "a@example.com----Fake-Pass-1----JBSW Y3DP EHPK 3PXP ====", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{27, "a@example.com", lineWant{kind: data, email: email, reason: "no_fields"}},
		{28, "a@example.com----", lineWant{kind: data, email: email, reason: "no_fields"}},
		{29, "a@example.com----   ----r@example.com", lineWant{kind: data, email: email, recovery: "r@example.com", warnings: []string{"empty_password"}}},
		{30, "Fake-Pass-1----a@example.com", lineWant{kind: data, reason: "first_not_email"}},
		{31, "a@example.com:Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "unsupported_separator"}},
		{32, "a@example.com---Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "dash_too_short"}},
		{33, "a@example.com Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "unsupported_separator"}},
		{34, "----a@example.com----Fake-Pass-1", lineWant{kind: data, reason: "first_not_email"}},
		{35, "# 这是注释", lineWant{kind: lineComment}},
		{35, "＃注释", lineWant{kind: lineComment}},
		{36, "", lineWant{kind: lineBlank}},
		{36, "   \u3000 \u00a0", lineWant{kind: lineBlank}},
		{39, "a@example.com\tFake-Pass-1\tr@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com"}},
		{40, "a@example.com\tFake-Pass-1\t\tJBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{41, "a@example.com——Fake-Pass-1——r@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com"}},
		{42, "a@example.com－－－－Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1"}},
		{43, "a@example.com----Fake—Pass", lineWant{kind: data, email: email, password: "Fake—Pass"}},
		{44, "ａ＠ｅｘａｍｐｌｅ．ｃｏｍ----Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1"}},
		{45, "a@example.com----ＦａｋｅＰａｓｓ", lineWant{kind: data, email: email, password: "ＦａｋｅＰａｓｓ"}},
		{46, "a@example.com----Fake-Pass-1----r@example.com.", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "r@example.com."}},
		{47, "a@example.com----r@example.com----JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "r@example.com", totp: key, warnings: []string{"password_looks_email"}}},
		{48, "a@example.com----JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: key, warnings: []string{"password_looks_totp"}}},
		{49, "a@example.com----Fake-Pass-1----a@example.com", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: email, warnings: []string{"recovery_same"}}},
		{50, "a@example.com----" + strings.Repeat("x", 4097-17), lineWant{kind: data, reason: "line_too_long"}},
		{50, "a@example.com----" + strings.Repeat("x", 4096-17), lineWant{kind: data, reason: "field_too_long", field: "password", email: email}},
		{51, "a@example.com----" + strings.Repeat("p", 513), lineWant{kind: data, email: email, reason: "field_too_long", field: "password"}},
		{51, "a@example.com----" + strings.Repeat("p", 512), lineWant{kind: data, email: email, password: strings.Repeat("p", 512)}},
		{51, "a@example.com----Fake-Pass-1----" + strings.Repeat("备", 683) + "----x", lineWant{kind: data, email: email, reason: "field_too_long", field: "note"}},
		{51, "a@example.com----Fake-Pass-1----" + strings.Repeat("n", 2043) + "----x", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: strings.Repeat("n", 2043) + "----x"}},
		{52, "a@example.com----Fake\x00Pass", lineWant{kind: data, reason: "control_char"}},
		{52, "a@example.com----Fake\x1bPass", lineWant{kind: data, reason: "control_char"}},
		{52, "a@example.com----Fake\u2028Pass", lineWant{kind: data, reason: "control_char"}},
		{52, "a@example.com----Fake\x7fPass", lineWant{kind: data, reason: "control_char"}},
		{53, "\"a@example.com\"----Fake-Pass-1", lineWant{kind: data, reason: "first_not_email"}},
		{55, "a@example.com————Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1", warnings: []string{"dash_run"}}},
		{56, "a@example.com——————Fake-Pass-1", lineWant{kind: data, email: email, password: "Fake-Pass-1", warnings: []string{"dash_run"}}},
		{57, "a@example.com--------普通文本", lineWant{kind: data, email: email, password: "普通文本", warnings: []string{"dash_run"}}},
		{58, "a@example.com——  ——Fake-Pass-1", lineWant{kind: data, email: email, note: "Fake-Pass-1", warnings: []string{"empty_password"}}},
		{59, "a@example.com\t\tr@example.com", lineWant{kind: data, email: email, recovery: "r@example.com", warnings: []string{"empty_password"}}},
		{60, "a@example.com\tAbc123-\tr@example.com", lineWant{kind: data, email: email, password: "Abc123-", recovery: "r@example.com"}},
		{61, "a@example.com\t----\tFake-Pass-1", lineWant{kind: data, email: email, password: "----", note: "Fake-Pass-1", warnings: []string{"mixed_separators"}}},
		{62, "a@example.com ----Fake-Pass\t----r@example.com", lineWant{kind: data, reason: "first_not_email", hint: "mixed_separators"}},
		{63, "a@example.com----Fake-Pass-1----abcd efgh ijkl mnop", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "abcd efgh ijkl mnop", warnings: []string{"looks_app_password"}}},
		{64, "a@example.com----Fake-Pass-1----abcdefghijklmnop", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "abcdefghijklmnop", warnings: []string{"looks_app_password"}}},
		{65, "a@example.com----Fake-Pass-1----ABCDEFGHIJKLMNOP", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: "ABCDEFGHIJKLMNOP", flags: []string{"letters_only"}, warnings: []string{"totp_letters_only"}}},
		{66, "a@example.com----Fake-Pass-1----familymainaccount----JBSWY3DPEHPK3PXP", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key, note: "familymainaccount", warnings: []string{"extra_totp"}}},
		{67, "a@example.com—Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "dash_too_short"}},
		{68, "a@example.com：Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "unsupported_separator"}},
		{69, "a@example.com----abcdefghijklmnopq", lineWant{kind: data, email: email, password: "abcdefghijklmnopq"}},
		{70, "a@example.com----Fake-Pass-1----abcd efgh ijkl mnop qrst uvwx yz23 4567", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"}},
		// 补充：带连字符的邮箱也能给出 dash_too_short；第 2 段为纯字母长串不提示 password_looks_totp；辅助邮箱超长按备注
		{0, "a-b@ex-ample.example.com---Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "dash_too_short"}},
		{0, "a@example.com|Fake-Pass-1", lineWant{kind: data, reason: "first_not_email", hint: "unsupported_separator"}},
		{0, "a@example.com----Fake-Pass-1----" + strings.Repeat("A", 129), lineWant{kind: data, email: email, password: "Fake-Pass-1", note: strings.Repeat("A", 129)}},
		{0, "a@example.com----Fake-Pass-1----" + strings.Repeat("AB23", 32), lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: strings.Repeat("AB23", 32)}},
		{0, "a@example.com----Fake-Pass-1----ｊｂｓｗｙ３ｄｐｅｈｐｋ３ｐｘｐ", lineWant{kind: data, email: email, password: "Fake-Pass-1", totp: key}},
		{0, "a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP----备注 1", lineWant{kind: data, email: email, password: "Fake-Pass-1", recovery: "r@example.com", totp: key, note: "备注 1"}},
		{0, "a@example.com----Fake-Pass-1----x----y", lineWant{kind: data, email: email, password: "Fake-Pass-1", note: "x----y"}},
	}
	for _, item := range cases {
		kind, got := parseRawLine(item.line)
		want := item.want
		if kind != want.kind {
			t.Errorf("#%d %q: kind=%v，期望 %v", item.no, short(item.line), kind, want.kind)
			continue
		}
		if kind != lineData {
			continue
		}
		if got.email != want.email || got.password != want.password || got.recovery != want.recovery || got.totp != want.totp ||
			!slices.Equal(got.totpFlags, want.flags) || got.note != want.note || got.reason != want.reason || got.hint != want.hint ||
			got.field != want.field || !sameSet(got.warnings, want.warnings) {
			t.Errorf("#%d %q:\n 结果 email=%q pw=%q rec=%q totp=%q flags=%v note=%q reason=%q hint=%q field=%q warnings=%v\n 期望 email=%q pw=%q rec=%q totp=%q flags=%v note=%q reason=%q hint=%q field=%q warnings=%v",
				item.no, short(item.line), got.email, got.password, got.recovery, got.totp, got.totpFlags, short(got.note), got.reason, got.hint, got.field, got.warnings,
				want.email, want.password, want.recovery, want.totp, want.flags, short(want.note), want.reason, want.hint, want.field, want.warnings)
		}
		// 无法识别时：只有第 1 段是合法邮箱才带邮箱；其他字段一律为空（不回显内容）
		if got.reason != "" && (got.password != "" || got.note != "" || got.totp != "" || got.recovery != "") {
			t.Errorf("#%d 无法识别的行不应带字段: %+v", item.no, got.warnings)
		}
	}
}

func short(text string) string {
	if len(text) > 80 {
		return text[:60] + "…"
	}
	return text
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, item := range want {
		if !slices.Contains(got, item) {
			return false
		}
	}
	return true
}

// TestParseProfileSources 来源级预处理：BOM、CRLF、注释与空行计数、多来源各自的行号、来源名整理、上限、像家庭组名单的批级提示
func TestParseProfileSources(t *testing.T) {
	sources := []ImportSource{
		{Name: "粘贴\x01的内容", Text: "\ufeffa@example.com----Fake-Pass-1\r\n\r\n# 注释\r\nb@example.com----Fake-Pass-2\r\nbad line\r\n"},
		{Name: strings.Repeat("名", 150), Text: "＃注释\n\nc@example.com\tFake-Pass-3\n"},
	}
	parsed, err := parseImportSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.sources[0].Name != "粘贴的内容" || len([]rune(parsed.sources[1].Name)) != 100 {
		t.Fatalf("来源名 = %q / %d 个字符", parsed.sources[0].Name, len([]rune(parsed.sources[1].Name)))
	}
	if parsed.sources[0].Lines != 5 || parsed.sources[1].Lines != 3 {
		t.Fatalf("来源行数 = %d / %d", parsed.sources[0].Lines, parsed.sources[1].Lines)
	}
	if parsed.blank != 2 || parsed.comments != 2 || len(parsed.lines) != 4 {
		t.Fatalf("blank=%d comments=%d lines=%d", parsed.blank, parsed.comments, len(parsed.lines))
	}
	wantRefs := [][3]int{{0, 1, 1}, {0, 4, 1}, {0, 5, 0}, {1, 3, 1}}
	for index, line := range parsed.lines {
		valid := 0
		if line.reason == "" {
			valid = 1
		}
		if got := [3]int{line.source, line.line, valid}; got != wantRefs[index] {
			t.Fatalf("第 %d 条 = %v，期望 %v", index, got, wantRefs[index])
		}
	}
	if parsed.lines[0].email != "a@example.com" || parsed.lines[0].password != "Fake-Pass-1" || parsed.lines[3].password != "Fake-Pass-3" {
		t.Fatalf("BOM/CRLF/Tab 处理错误: %q %q", parsed.lines[0].email, parsed.lines[0].password)
	}
	if len(parsed.hints) != 0 {
		t.Fatalf("不应有批级提示: %v", parsed.hints)
	}

	// 上限：20 000 个非空行通过，多一行 413；4 MiB 通过、多一个字节 413；51 个来源 413
	many := strings.Repeat("a@example.com----x\n", MaxImportLines)
	if _, err := parseImportSources([]ImportSource{{Text: many}}); err != nil {
		t.Fatalf("20000 行应通过: %v", err)
	}
	assertTooLarge(t, []ImportSource{{Text: many + "# 注释也算\n"}})
	big := strings.Repeat("x", MaxImportBytes-1) + "\n"
	if _, err := parseImportSources([]ImportSource{{Text: big}}); err != nil {
		t.Fatalf("4 MiB 应通过: %v", err)
	}
	assertTooLarge(t, []ImportSource{{Text: big}, {Text: "x"}})
	assertTooLarge(t, make([]ImportSource, MaxImportSources+1))

	// 只有邮箱的行过半且至少 3 行：像家庭组名单
	family := "main@example.com\nm1@example.com\nm2@example.com\nx@example.com----Fake-Pass-1\n"
	if parsed, _ := parseImportSources([]ImportSource{{Text: family}}); !slices.Equal(parsed.hints, []string{"looks_like_family_list"}) {
		t.Fatalf("hints = %v", parsed.hints)
	}
	notHalf := "main@example.com\nm1@example.com\nm2@example.com\nx@example.com----P1\ny@example.com----P2\nz@example.com----P3\n"
	if parsed, _ := parseImportSources([]ImportSource{{Text: notHalf}}); len(parsed.hints) != 0 {
		t.Fatalf("不过半不应提示: %v", parsed.hints)
	}
	two := "main@example.com\nm1@example.com\n"
	if parsed, _ := parseImportSources([]ImportSource{{Text: two}}); len(parsed.hints) != 0 {
		t.Fatalf("不足 3 行不应提示: %v", parsed.hints)
	}
}

func assertTooLarge(t *testing.T, sources []ImportSource) {
	t.Helper()
	_, err := parseImportSources(sources)
	var vaultErr *Error
	if !errors.As(err, &vaultErr) || vaultErr.Status != 413 || vaultErr.Code != CodeTooLarge {
		t.Fatalf("期望 413 vault_too_large，得到 %v", err)
	}
}

// TestNormalizeEmail 大小写、全角、非法写法
func TestNormalizeEmail(t *testing.T) {
	good := map[string]string{
		"A@Example.COM":                          "a@example.com",
		"  a.b+tag@sub.example.com ":             "a.b+tag@sub.example.com",
		"ａ＠ｅｘａｍｐｌｅ．ｃｏｍ":                          "a@example.com",
		"x_y-z@example-mail.example.com":         "x_y-z@example-mail.example.com",
		"a@xn--fiqs8s.example.com":               "a@xn--fiqs8s.example.com",
		"o'neil@example.com":                     "o'neil@example.com",
		strings.Repeat("l", 64) + "@example.com": strings.Repeat("l", 64) + "@example.com",
	}
	for input, want := range good {
		if got, ok := NormalizeEmail(input); !ok || got != want {
			t.Errorf("NormalizeEmail(%q) = %q %v，期望 %q", input, got, ok, want)
		}
	}
	for _, input := range []string{
		"", "a@b", "Name <a@example.com>", "\"a\"@example.com", "a@[127.0.0.1]", "a@127.0.0.1", "a@example.com.", "a..b@example.com",
		".a@example.com", "a.@example.com", "a@example", "a@@example.com", "a@b@example.com", "mailto:a@example.com", "a @example.com",
		"a@-x.example.com", "a@x-.example.com", "a@example.c", "a@example.c0m", strings.Repeat("l", 65) + "@example.com", "a@e.x", "ä@example.com",
		"\u212aa@example.com", "a@exam_ple.com",
	} {
		if got, ok := NormalizeEmail(input); ok {
			t.Errorf("NormalizeEmail(%q) 应不合法，得到 %q", input, got)
		}
	}
	// 末段为 punycode（xn--）的顶级域：测试邮箱只用 example.com，这一支只测域名部分（xn--zckzah 为 IANA 保留的测试用国际化顶级域）
	if !validDomain("example.xn--zckzah") || validDomain("example.x1") {
		t.Error("末段 xn-- 应合法、末段含数字应不合法")
	}
}
