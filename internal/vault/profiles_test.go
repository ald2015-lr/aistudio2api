package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func itemByLine(t *testing.T, result ProfileImportResult, line int) ImportItem {
	t.Helper()
	for _, item := range result.Items {
		if item.Line == line {
			return item
		}
	}
	t.Fatalf("没有第 %d 行", line)
	return ImportItem{}
}

func viewProfile(t *testing.T, store *Store, email string) ProfileView {
	t.Helper()
	view, err := store.View(bg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Profiles {
		if item.Email == email {
			return item
		}
	}
	t.Fatalf("列表里没有 %s", email)
	return ProfileView{}
}

// TestMergeRules 新建、只覆盖给出的字段、缺字段不清空、备注替换、相同算未变化（不改 updated_at）、changes；
// overwrites 只算“导入前非空且改成不同值”（补空不算、同批前一行写的不算）；fill_empty 下已有值一律保留、列进 kept、overwrites 为 0
func TestMergeRules(t *testing.T) {
	store, _, _ := newTestStore(t)
	first := importLines(t, store, "a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP----备注 1")
	if first.Summary.New != 1 || first.Items[0].Status != statusNew || first.DryRun {
		t.Fatalf("新建 = %+v", first.Summary)
	}
	second := importLines(t, store, "a@example.com----Fake-Pass-2")
	item := second.Items[0]
	if second.Summary.Updated != 1 || item.Status != statusUpdate || !slices.Equal(item.Changes, []string{fieldPassword}) ||
		!slices.Equal(item.Overwrites, []string{fieldPassword}) || second.Summary.Overwrites != (FieldCounts{Password: 1}) {
		t.Fatalf("只改密码 = %+v %+v", second.Summary, item)
	}
	secret := reveal(t, store, "a@example.com")
	if secret.Password != "Fake-Pass-2" || secret.RecoveryEmail != "r@example.com" || secret.TOTP != "JBSWY3DPEHPK3PXP" || secret.Note != "备注 1" {
		t.Fatal("没给出的字段应保持原值")
	}
	third := importLines(t, store, "a@example.com----Fake-Pass-2----备注 2")
	if secret := reveal(t, store, "a@example.com"); secret.Note != "备注 2" || !slices.Equal(third.Items[0].Changes, []string{fieldNote}) ||
		!slices.Equal(third.Items[0].Overwrites, []string{fieldNote}) {
		t.Fatalf("备注应整体替换: changes=%v", third.Items[0].Changes)
	}
	before := viewProfile(t, store, "a@example.com").UpdatedAt
	time.Sleep(5 * time.Millisecond)
	same := importLines(t, store, "a@example.com----Fake-Pass-2")
	if same.Summary.Unchanged != 1 || same.Items[0].Status != statusUnchanged || !viewProfile(t, store, "a@example.com").UpdatedAt.Equal(before) {
		t.Fatalf("相同内容应算未变化且不改 updated_at: %+v", same.Summary)
	}
	// 只补空字段不算覆盖；空密码段不清空原密码
	importLines(t, store, "b@example.com----Fake-Pass-B")
	fill := importLines(t, store, "b@example.com----  ----r2@example.com")
	if fill.Summary.Overwrites != (FieldCounts{}) || !slices.Equal(fill.Items[0].Changes, []string{fieldRecovery}) || len(fill.Items[0].Overwrites) != 0 {
		t.Fatalf("补空字段 = %+v %+v", fill.Summary.Overwrites, fill.Items[0])
	}
	if secret := reveal(t, store, "b@example.com"); secret.Password != "Fake-Pass-B" || secret.RecoveryEmail != "r2@example.com" {
		t.Fatal("空密码段不应清空原密码")
	}
	// 同一批：前一行写进去的值被后一行改掉不算“覆盖已有值”；已有值被两行先后改掉按邮箱计 1
	batch := importLines(t, store, "d@example.com----P1\nd@example.com----P2\na@example.com----X1\na@example.com----X2\n")
	if batch.Summary.Overwrites != (FieldCounts{Password: 1}) || batch.Summary.New != 1 || batch.Summary.Updated != 1 {
		t.Fatalf("同批覆盖计数 = %+v", batch.Summary)
	}
	if item := itemByLine(t, batch, 2); item.Status != statusUpdate || len(item.Overwrites) != 0 {
		t.Fatalf("同批新建后再改不算覆盖: %+v", item)
	}
	// fill_empty：已有值的字段不改，列进 kept，overwrites 为 0；空字段照样补上
	result, err := store.ImportProfiles(bg, ProfileImportInput{Mode: modeFillEmpty, Sources: []ImportSource{{Text: "a@example.com----Fake-Pass-3----r9@example.com\nd@example.com----P9----GEZDGNBVGY3TQOJQ"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != modeFillEmpty || result.Summary.Overwrites != (FieldCounts{}) || result.Summary.Kept != (FieldCounts{Password: 2, RecoveryEmail: 1}) {
		t.Fatalf("fill_empty 汇总 = %+v", result.Summary)
	}
	if item := result.Items[0]; item.Status != statusUnchanged || !slices.Equal(item.Kept, []string{fieldPassword, fieldRecovery}) {
		t.Fatalf("fill_empty 行 = %+v", item)
	}
	if item := result.Items[1]; item.Status != statusUpdate || !slices.Equal(item.Changes, []string{fieldTOTP}) {
		t.Fatalf("fill_empty 补空 = %+v", item)
	}
	if secret := reveal(t, store, "a@example.com"); secret.Password != "X2" || secret.RecoveryEmail != "r@example.com" {
		t.Fatal("fill_empty 不应改已有值")
	}
}

// TestDuplicateLinesSequential 同一批里重复的邮箱按顺序合并：相同字段以后面的行为准，前面行独有的字段保留；每行状态按应用到它之前的状态算
func TestDuplicateLinesSequential(t *testing.T) {
	store, _, _ := newTestStore(t)
	importLines(t, store, "f@example.com----P0")
	result := importLines(t, store, strings.Join([]string{
		"e@example.com----P1----JBSWY3DPEHPK3PXP",
		"x@example.com----PX",
		"e@example.com----P2",
		"f@example.com----P0",
		"f@example.com----P1",
	}, "\n"))
	if secret := reveal(t, store, "e@example.com"); secret.Password != "P2" || secret.TOTP != "JBSWY3DPEHPK3PXP" {
		t.Fatal("后面的行覆盖相同字段、前面行独有的字段保留")
	}
	first, third := itemByLine(t, result, 1), itemByLine(t, result, 3)
	if first.Status != statusNew || third.Status != statusUpdate || !slices.Equal(first.Also, []LineRef{{Source: 0, Line: 3}}) ||
		!slices.Equal(third.Also, []LineRef{{Source: 0, Line: 1}}) || !slices.Contains(first.Warnings, warnRepeated) {
		t.Fatalf("重复行 = %+v / %+v", first, third)
	}
	if fourth, fifth := itemByLine(t, result, 4), itemByLine(t, result, 5); fourth.Status != statusUnchanged || fifth.Status != statusUpdate {
		t.Fatalf("已有邮箱的重复行 = %s %s", fourth.Status, fifth.Status)
	}
	if summary := result.Summary; summary.New != 2 || summary.Updated != 1 || summary.Unchanged != 0 || summary.DuplicateLines != 2 || summary.Warnings[warnRepeated] != 4 {
		t.Fatalf("汇总 = %+v", summary)
	}
}

// TestOverwriteSummaryColumnShift 列序错位的一批（邮箱----辅助邮箱----密码）：覆盖计数、可疑提示、不核对不能确认、fill_empty 的计数；门槛边界
func TestOverwriteSummaryColumnShift(t *testing.T) {
	store, dir, _ := newTestStore(t)
	var existing, shifted strings.Builder
	for index := range 100 {
		fmt.Fprintf(&existing, "u%03d@example.com----Fake-Pass-%03d\n", index, index)
		fmt.Fprintf(&shifted, "u%03d@example.com----r%03d@example.com----Fake-Pass-%03d\n", index, index, index)
	}
	importLines(t, store, existing.String())
	preview := previewLines(t, store, shifted.String(), "")
	if preview.Summary.Overwrites.Password != 100 || preview.Summary.Warnings[warnPasswordLooksEmail] != 100 ||
		!slices.Equal(preview.Summary.Suspect, []string{warnPasswordLooksEmail}) {
		t.Fatalf("预览汇总 = %+v", preview.Summary)
	}
	before := readFile(t, filepath.Join(dir, "profiles.json"))
	_, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: shifted.String()}}, BaseRevision: preview.Revision,
		ExpectOverwrites: &preview.Summary.Overwrites})
	assertCode(t, err, 409, CodeSuspectUnacked)
	if after := readFile(t, filepath.Join(dir, "profiles.json")); !bytes.Equal(before, after) {
		t.Fatal("没核对时不应写盘")
	}
	applied, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: shifted.String()}}, BaseRevision: preview.Revision,
		ExpectOverwrites: &preview.Summary.Overwrites, AcknowledgeSuspect: true})
	if err != nil || applied.Summary.Updated != 100 {
		t.Fatalf("核对后确认 err=%v summary=%+v", err, applied.Summary)
	}
	if snapshots, _ := filepath.Glob(filepath.Join(dir, "snapshots", "profiles-*.json")); len(snapshots) != 1 {
		t.Fatalf("确认导入前应存一份快照: %v", snapshots)
	}
	// fill_empty：一个已有值都不改
	other, _, _ := newTestStore(t)
	importLines(t, other, existing.String())
	fill := previewLines(t, other, shifted.String(), modeFillEmpty)
	if fill.Summary.Overwrites != (FieldCounts{}) || fill.Summary.Kept.Password != 100 || fill.Summary.Kept.RecoveryEmail != 0 {
		t.Fatalf("fill_empty = %+v", fill.Summary)
	}
	// 门槛：≥ 50 行，或 ≥ 3 行且 ≥ 有效行的 20%
	threshold := func(suspicious, normal int) bool {
		var text strings.Builder
		for index := range suspicious {
			fmt.Fprintf(&text, "s%d@example.com----r%d@example.com\n", index, index)
		}
		for index := range normal {
			fmt.Fprintf(&text, "n%d@example.com----Fake-Pass-%d\n", index, index)
		}
		parsed, err := parseImportSources([]ImportSource{{Text: text.String()}})
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(parsed.suspect, warnPasswordLooksEmail)
	}
	for _, item := range []struct {
		suspicious, normal int
		want               bool
	}{{49, 251, false}, {50, 950, true}, {2, 0, false}, {3, 12, true}, {3, 13, false}} {
		if got := threshold(item.suspicious, item.normal); got != item.want {
			t.Fatalf("%d 行可疑 + %d 行正常：suspect=%v，期望 %v", item.suspicious, item.normal, got, item.want)
		}
	}
}

// TestImportBaseRevision 预览之后另一个进程改了数据：覆盖条数变了 → 409 附新预览且不写盘；只改了无关记录 → 照常；换了模式 → 409；两个字段都不带 → 不检查
func TestImportBaseRevision(t *testing.T) {
	store, dir, logs := newTestStore(t)
	importLines(t, store, "x1@example.com----P1\nx2@example.com----  ----r@example.com\ny@example.com----PY")
	other := openAgain(dir, logs)
	text := "x1@example.com----P1-new\nx2@example.com----P2"
	preview := previewLines(t, store, text, "")
	if preview.Summary.Overwrites.Password != 1 {
		t.Fatalf("预览覆盖 = %+v", preview.Summary.Overwrites)
	}
	if _, err := other.UpdateProfile(bg, ProfileUpdate{Email: "x2@example.com", Password: strPtr("Other-Pass")}); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(dir, "profiles.json"))
	_, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: text}}, BaseRevision: preview.Revision, ExpectOverwrites: &preview.Summary.Overwrites})
	assertCode(t, err, 409, CodeConflict)
	conflictErr, ok := err.(*ConflictError)
	if !ok {
		t.Fatalf("应附新预览: %T", err)
	}
	fresh, ok := conflictErr.Preview.(ProfileImportResult)
	if !ok || !fresh.DryRun || fresh.Summary.Overwrites.Password != 2 || fresh.Revision == preview.Revision {
		t.Fatalf("新预览 = %+v", fresh.Summary)
	}
	if after := readFile(t, filepath.Join(dir, "profiles.json")); !bytes.Equal(before, after) {
		t.Fatal("冲突时不应写盘")
	}
	// 另一个进程只改了批外的记录：覆盖条数不变，照常生效
	preview = previewLines(t, store, text, "")
	if _, err := other.UpdateProfile(bg, ProfileUpdate{Email: "y@example.com", Password: strPtr("PY2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: text}}, BaseRevision: preview.Revision, ExpectOverwrites: &preview.Summary.Overwrites}); err != nil {
		t.Fatalf("无关记录变化不应冲突: %v", err)
	}
	// 确认时换成 fill_empty 而 expect_overwrites 仍是合并模式的
	text = "x1@example.com----P1-third"
	preview = previewLines(t, store, text, "")
	_, err = store.ImportProfiles(bg, ProfileImportInput{Mode: modeFillEmpty, Sources: []ImportSource{{Text: text}}, BaseRevision: preview.Revision, ExpectOverwrites: &preview.Summary.Overwrites})
	assertCode(t, err, 409, CodeConflict)
	// 两个字段都不带（脚本直接调接口）时不检查
	if _, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: text}}}); err != nil {
		t.Fatal(err)
	}
	if reveal(t, store, "x1@example.com").Password != "P1-third" {
		t.Fatal("不带检查字段时应直接生效")
	}
}

// TestUpdateProfile 设置、清空、既设又清 400、校验 400、不存在则新建、全部清空保留记录、密码不去首尾空白、Tab 与控制字符 400、编辑框的 2FA 规则
func TestUpdateProfile(t *testing.T) {
	store, _, _ := newTestStore(t)
	created, err := store.UpdateProfile(bg, ProfileUpdate{Email: " New@Example.com ", Password: strPtr("  Fake Pass  "), RecoveryEmail: strPtr(" R@Example.com ")})
	if err != nil || !created.Created || !slices.Equal(created.Changed, []string{fieldPassword, fieldRecovery}) || created.Profile.Email != "new@example.com" {
		t.Fatalf("新建 err=%v result=%+v", err, created)
	}
	if secret := reveal(t, store, "new@example.com"); secret.Password != "  Fake Pass  " || secret.RecoveryEmail != "r@example.com" {
		t.Fatal("编辑框的密码不去首尾空白，辅助邮箱规范化")
	}
	updated, err := store.UpdateProfile(bg, ProfileUpdate{Email: "new@example.com", TOTP: strPtr("familymainaccount"), Clear: []string{fieldPassword}})
	if err != nil || updated.Created || !slices.Equal(updated.Changed, []string{fieldPassword, fieldTOTP}) || !slices.Equal(updated.Cleared, []string{fieldPassword}) {
		t.Fatalf("编辑 err=%v result=%+v", err, updated)
	}
	if view := updated.Profile; view.HasPassword || !view.HasTOTP || !slices.Equal(view.TOTPFlags, []string{flagLetters}) || view.TOTPLast4 != "OUNT" {
		t.Fatalf("纯字母 2FA 收下并标 letters_only: %+v", view)
	}
	if result, err := store.UpdateProfile(bg, ProfileUpdate{Email: "new@example.com", TOTP: strPtr("abcd efgh ijkl mnop")}); err != nil || !result.Profile.HasTOTP {
		t.Fatalf("编辑框不套用应用专用密码判断: %v", err)
	}
	same, err := store.UpdateProfile(bg, ProfileUpdate{Email: "new@example.com", RecoveryEmail: strPtr("r@example.com"), Note: strPtr("")})
	if err != nil || len(same.Changed) != 0 {
		t.Fatalf("没变化 = %+v %v", same, err)
	}
	cleared, err := store.UpdateProfile(bg, ProfileUpdate{Email: "new@example.com", Clear: []string{fieldRecovery, fieldTOTP, fieldNote}})
	if err != nil || cleared.Profile.HasTOTP || cleared.Profile.RecoveryEmail != "" || cleared.Profile.HasPassword {
		t.Fatalf("全部清空 = %+v %v", cleared, err)
	}
	if secret := reveal(t, store, "new@example.com"); secret.Email != "new@example.com" {
		t.Fatal("全部清空后记录保留")
	}
	for _, item := range []struct {
		input  ProfileUpdate
		status int
		code   string
	}{
		{ProfileUpdate{Email: "bad"}, 400, CodeInvalidEmail},
		{ProfileUpdate{Email: "new@example.com", Password: strPtr("x"), Clear: []string{fieldPassword}}, 400, CodeInvalidRequest},
		{ProfileUpdate{Email: "new@example.com", Clear: []string{"cookie"}}, 400, CodeInvalidRequest},
		{ProfileUpdate{Email: "new@example.com", RecoveryEmail: strPtr("not-an-email")}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", TOTP: strPtr("JBSWY3DPEHPK3PX")}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", Password: strPtr("a\tb")}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", Note: strPtr("a\nb")}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", Note: strPtr("x\x00")}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", Password: strPtr(strings.Repeat("p", 513))}, 400, CodeInvalidField},
		{ProfileUpdate{Email: "new@example.com", Note: strPtr(strings.Repeat("n", 2049))}, 400, CodeInvalidField},
	} {
		_, err := store.UpdateProfile(bg, item.input)
		assertCode(t, err, item.status, item.code)
	}
}

// TestDeleteAndExport 删除计数与快照；导出从不 404、顺序、整行格式（空字段省略、含备注、---- 与 Tab、不能完全还原的原因）
func TestDeleteAndExport(t *testing.T) {
	store, dir, _ := newTestStore(t)
	importLines(t, store, strings.Join([]string{
		"a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP----备注",
		"b@example.com----Fake-Pass-2",
		"c@example.com----Abc----d",
		"d@example.com----  ----r@example.com",
		"e@example.com----Fake-Pass-5",
	}, "\n"))
	for _, update := range []ProfileUpdate{
		{Email: "c@example.com", Password: strPtr("-Abc-"), Clear: []string{fieldNote}},
		{Email: "e@example.com", Password: strPtr(" edge ")},
		{Email: "f@example.com", Note: strPtr("r9@example.com")},
		{Email: "g@example.com", Password: strPtr("ab----cd")},
		{Email: "h@example.com", Password: strPtr("x")},
	} {
		if _, err := store.UpdateProfile(bg, update); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.UpdateProfile(bg, ProfileUpdate{Email: "h@example.com", Clear: []string{fieldPassword}}); err != nil {
		t.Fatal(err)
	}
	export, err := store.ExportProfiles(bg, ExportInput{Emails: []string{"b@example.com", "A@example.com", "zz@example.com", "a@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com", "g@example.com", "h@example.com"}, IncludeNote: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"b@example.com----Fake-Pass-2",
		"a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP----备注",
		"c@example.com\t-Abc-",
		"d@example.com\t\tr@example.com",
		"e@example.com\t edge ",
		"f@example.com\t\tr9@example.com",
		"g@example.com\tab----cd",
		"h@example.com",
	}
	if !slices.Equal(export.Lines, want) || export.Count != 8 || !slices.Equal(export.Missing, []string{"zz@example.com"}) || export.TabLines != 6 {
		t.Fatalf("导出 = %q count=%d missing=%v tab=%d", export.Lines, export.Count, export.Missing, export.TabLines)
	}
	wantLossy := []LossyLine{{"e@example.com", lossyEdgeWhitespace}, {"f@example.com", lossyNoteAmbiguous}, {"h@example.com", lossyEmptyRecord}}
	if !slices.Equal(export.Lossy, wantLossy) {
		t.Fatalf("lossy = %+v", export.Lossy)
	}
	// 不含备注时备注不进整行
	if export, _ := store.ExportProfiles(bg, ExportInput{Emails: []string{"a@example.com"}}); export.Lines[0] != "a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP" {
		t.Fatalf("不含备注 = %q", export.Lines)
	}
	if secret := reveal(t, store, "c@example.com"); secret.Line != "c@example.com\t-Abc-" || secret.LineFormat != lineFormatTab || secret.LineLossy != "" {
		t.Fatalf("取出整行 = %q %s %s", secret.Line, secret.LineFormat, secret.LineLossy)
	}
	all, _ := store.ExportProfiles(bg, ExportInput{All: true})
	if all.Count != 8 || !strings.HasPrefix(all.Lines[0], "a@example.com----") {
		t.Fatalf("全部导出按邮箱排序: %q", all.Lines)
	}
	if _, err := store.ExportProfiles(bg, ExportInput{}); err == nil {
		t.Fatal("没有邮箱也不是全部时应 400")
	}
	// 删除：不存在的计 missing，删除前存快照
	deleted, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com", "A@EXAMPLE.COM", "zz@example.com", "bad"}})
	if err != nil || deleted.Deleted != 1 || deleted.Missing != 2 {
		t.Fatalf("删除 = %+v %v", deleted, err)
	}
	if _, err := store.RevealProfile(bg, RevealInput{Email: "a@example.com"}); err == nil {
		t.Fatal("删除后取出应 404")
	}
	if snapshots, _ := filepath.Glob(filepath.Join(dir, "snapshots", "profiles-*.json")); len(snapshots) != 1 {
		t.Fatalf("删除前应存快照: %v", snapshots)
	}
	if export, err := store.ExportProfiles(bg, ExportInput{Emails: []string{"a@example.com"}}); err != nil || export.Count != 0 || len(export.Missing) != 1 {
		t.Fatalf("导出不存在的邮箱不应 404: %+v %v", export, err)
	}
}

// TestLineRoundTrip 整行往返的性质测试：随机记录生成整行后用解析器解析，要么五个字段完全一致，要么标出不能完全还原且原因符合构造；
// 没有首尾空白、备注不像邮箱或 2FA 的记录必须一致。再把整批导出（\r\n）重新导入到空 Store，比较全部记录
func TestLineRoundTrip(t *testing.T) {
	random := rand.New(rand.NewSource(20261126))
	pieces := []string{"-", "--", "----", "—", "——", "－", " ", "  ", "中", "文", "@", "JBSW", "Y3DP", "a", "B", "7", ".", "x", "=", "#", "otpauth://", ":", "|"}
	secrets := []string{"JBSWY3DPEHPK3PXP", "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "FAMILYMAINACCOUNT", "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"}
	compose := func() string {
		var builder strings.Builder
		for range 1 + random.Intn(8) {
			builder.WriteString(pieces[random.Intn(len(pieces))])
		}
		return builder.String()
	}
	records := make([]profile, 0, 10000)
	for index := range 10000 {
		record := profile{Email: fmt.Sprintf("u%05d@example.com", index)}
		switch roll := random.Intn(10); {
		case roll < 2:
		case roll < 3:
			record.Password = " " + trimSpace(compose()) + "x "
		default:
			record.Password = trimSpace(compose())
		}
		if random.Intn(2) == 0 {
			record.RecoveryEmail = fmt.Sprintf("r%05d@example.com", index)
		}
		if random.Intn(2) == 0 {
			record.TOTP = secrets[random.Intn(len(secrets))]
		}
		if random.Intn(2) == 0 {
			record.Note = trimSpace(compose())
		}
		if random.Intn(20) == 0 {
			record.Note = fmt.Sprintf("n%05d@example.com", index)
		}
		records = append(records, record)
	}
	exact := 0
	for index := range records {
		record := &records[index]
		includeNote := index%2 == 0
		note := ""
		if includeNote {
			note = record.Note
		}
		line, format, lossy := buildLine(record, includeNote)
		matches := lineRoundTrips(line, record, note)
		edge := record.Password != trimSpace(record.Password)
		_, noteEmail := NormalizeEmail(note)
		ambiguousNote := note != "" && (noteEmail || parseTOTP(note).ok)
		empty := record.Password == "" && record.RecoveryEmail == "" && record.TOTP == "" && note == ""
		switch {
		case matches:
			exact++
			if lossy != "" {
				t.Fatalf("能还原的行不应标 lossy: %q %s", line, lossy)
			}
			if format == lineFormatDash && strings.Contains(line, "\t") {
				t.Fatalf("dash 行不应含 Tab: %q", line)
			}
		case empty:
			if lossy != lossyEmptyRecord {
				t.Fatalf("只有邮箱的记录应标 empty_record: %q %s", line, lossy)
			}
		case edge:
			if lossy != lossyEdgeWhitespace {
				t.Fatalf("首尾空白的密码应标 edge_whitespace: %q %s", line, lossy)
			}
		case ambiguousNote:
			if lossy != lossyNoteAmbiguous {
				t.Fatalf("像邮箱或 2FA 的备注应标 note_ambiguous: %q %s", line, lossy)
			}
		default:
			t.Fatalf("应能完全还原: record=%q/%q/%q/%q/%q line=%q", record.Email, record.Password, record.RecoveryEmail, record.TOTP, note, line)
		}
	}
	if exact < 7000 {
		t.Fatalf("能完全还原的行太少: %d", exact)
	}
	// 整批导出再导入到空 Store
	source := Open("", Options{})
	source.mu.Lock()
	source.profiles.setOK(source, newProfileDoc(slices.Clone(records)), nil)
	source.profiles.doc.token = "seeded"
	source.mu.Unlock()
	export, err := source.ExportProfiles(bg, ExportInput{All: true, IncludeNote: true})
	if err != nil || export.Count != len(records) {
		t.Fatalf("导出 err=%v count=%d", err, export.Count)
	}
	lossy := map[string]bool{}
	for _, item := range export.Lossy {
		lossy[item.Email] = true
	}
	target := Open("", Options{})
	if _, err := target.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: strings.Join(export.Lines, "\r\n")}}, AcknowledgeSuspect: true}); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	imported := target.profiles.doc
	target.mu.Unlock()
	compared := 0
	for index := range records {
		record := &records[index]
		if lossy[record.Email] {
			continue
		}
		got := imported.get(record.Email)
		if got == nil || !sameProfileFields(got, record) {
			t.Fatalf("重新导入后不一致: %s", record.Email)
		}
		compared++
	}
	if compared < 7000 {
		t.Fatalf("比较的记录太少: %d", compared)
	}
}

// TestViewMasked 列表里没有任何秘密，有 2FA 末四位、标志与完整的辅助邮箱
func TestViewMasked(t *testing.T) {
	store, _, _ := newTestStore(t)
	importLines(t, store, "a@example.com----Fake-Pass-0001----r0001@example.com----familymainaccount----备注 Secret-Note-1")
	view, err := store.View(bg, "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(view)
	text := string(data)
	for _, secret := range []string{"Fake-Pass-0001", "FAMILYMAINACCOUNT", "familymainaccount", "Secret-Note-1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("列表不应含 %s: %s", secret, text)
		}
	}
	for _, want := range []string{`"totp_last4":"OUNT"`, `"totp_flags":["letters_only"]`, `"recovery_email":"r0001@example.com"`, `"has_password":true`, `"has_note":true`, `"totp_ok":true`} {
		if !strings.Contains(text, want) {
			t.Fatalf("列表缺 %s: %s", want, text)
		}
	}
}

// TestRedaction 含秘密的结构体在 fmt 与 slog 里一律打码
func TestRedaction(t *testing.T) {
	const secret = "Fake-Pass-Redact"
	values := []any{
		ImportSource{Name: "n", Text: secret},
		ProfileImportInput{Sources: []ImportSource{{Text: secret}}},
		ProfileUpdate{Email: "a@example.com", Password: strPtr(secret)},
		ProfileSecret{Password: secret},
		TOTPCode{Code: secret},
		ProfileExport{Lines: []string{secret}},
		FamilyImportInput{Text: secret},
		profile{Password: secret},
		parsedLine{password: secret},
		totpInfo{secret: secret},
		profileEdit{set: map[string]string{fieldPassword: secret}},
		&ProfileSecret{Password: secret},
		[]profile{{Password: secret}},
	}
	for _, value := range values {
		text := fmt.Sprintf("%v %+v %#v %s", value, value, value, value)
		if strings.Contains(text, secret) {
			t.Fatalf("%T 打印出了秘密: %s", value, text)
		}
		if _, isSlice := value.([]profile); isSlice {
			continue // slog 按 JSON 编码切片的元素，管不到；只要求结构体本身打码
		}
		var buffer bytes.Buffer
		slog.New(slog.NewJSONHandler(&buffer, nil)).Info("x", "value", value)
		if strings.Contains(buffer.String(), secret) {
			t.Fatalf("%T 的 slog 输出含秘密: %s", value, buffer.String())
		}
	}
}

// TestFillEmptyDuplicateLines 只补空字段时同一批里重复的邮箱也以最后一行为准：只有导入前就有的值才保留（列进 kept），
// 同一批前面的行写进的值不算“已有值”
func TestFillEmptyDuplicateLines(t *testing.T) {
	store, _, _ := newTestStore(t)
	importLines(t, store, "old@example.com----Fake-Pass-KEEP")
	result, err := store.ImportProfiles(bg, ProfileImportInput{Mode: modeFillEmpty, Sources: []ImportSource{{Text: strings.Join([]string{
		"new@example.com----Fake-Pass-OLD",
		"new@example.com----Fake-Pass-NEW",
		"old@example.com----Fake-Pass-A----r1@example.com",
		"old@example.com----Fake-Pass-B----r2@example.com",
	}, "\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if secret := reveal(t, store, "new@example.com"); secret.Password != "Fake-Pass-NEW" {
		t.Fatal("导入前没有记录的邮箱：同一批以最后一行为准")
	}
	if secret := reveal(t, store, "old@example.com"); secret.Password != "Fake-Pass-KEEP" || secret.RecoveryEmail != "r2@example.com" {
		t.Fatal("导入前已有的密码保留；导入前为空的辅助邮箱以最后一行为准")
	}
	if result.Summary.Kept != (FieldCounts{Password: 2}) || result.Summary.Overwrites != (FieldCounts{}) || result.Summary.New != 1 || result.Summary.Updated != 1 {
		t.Fatalf("汇总 = %+v", result.Summary)
	}
	if item := itemByLine(t, result, 2); item.Status != statusUpdate || !slices.Equal(item.Changes, []string{fieldPassword}) || len(item.Kept) != 0 || len(item.Overwrites) != 0 {
		t.Fatalf("第 2 行 = %+v", item)
	}
	if item := itemByLine(t, result, 4); item.Status != statusUpdate || !slices.Equal(item.Changes, []string{fieldRecovery}) || !slices.Equal(item.Kept, []string{fieldPassword}) {
		t.Fatalf("第 4 行 = %+v", item)
	}
}
