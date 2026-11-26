package vault

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	groupA = "fam-aaaaaaaaaaaa"
	groupB = "fam-bbbbbbbbbbbb"
)

// familyFixture 为 §6.3 的初始状态：组 A = main@（m1..m5），组 B = x@（y、z）
func familyFixture() *familyDoc {
	at := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	return newFamilyDoc([]FamilyGroup{
		{ID: groupA, Seq: 1, Rev: 1, Name: "李家", Color: 0, Primary: "main@example.com",
			Members: []string{"m1@example.com", "m2@example.com", "m3@example.com", "m4@example.com", "m5@example.com"}, CreatedAt: at, UpdatedAt: at},
		{ID: groupB, Seq: 2, Rev: 1, Color: 1, Primary: "x@example.com", Members: []string{"y@example.com", "z@example.com"}, CreatedAt: at, UpdatedAt: at},
	})
}

func previewFamilies(t *testing.T, doc *familyDoc, text string, mode string) (FamilyImportResult, *familyDoc) {
	t.Helper()
	if mode == "" {
		mode = familyModeBlocks
	}
	blocks, err := parseFamilyText(text, mode)
	if err != nil {
		t.Fatal(err)
	}
	result, next, _ := applyFamilyImport(doc, blocks, time.Now().UTC(), dryRunGroupIDs())
	return result, next
}

func groupOf(doc *familyDoc, email string) *FamilyGroup {
	if ref, ok := doc.byEmail[email]; ok {
		return &doc.groups[ref.index]
	}
	return nil
}

func emails(prefix string, count int) []string {
	list := make([]string, count)
	for index := range list {
		list[index] = fmt.Sprintf("%s%d@example.com", prefix, index+1)
	}
	return list
}

// TestFamilyBlocks §6.3 的例子与块解析的各条规则
func TestFamilyBlocks(t *testing.T) {
	type check func(t *testing.T, result FamilyImportResult, next *familyDoc)
	status := func(want ...string) check {
		return func(t *testing.T, result FamilyImportResult, _ *familyDoc) {
			t.Helper()
			var got []string
			for _, block := range result.Blocks {
				got = append(got, block.Status)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("状态 = %v，期望 %v", got, want)
			}
		}
	}
	block := func(index int, verify func(t *testing.T, block FamilyBlockResult)) check {
		return func(t *testing.T, result FamilyImportResult, _ *familyDoc) {
			t.Helper()
			verify(t, result.Blocks[index])
		}
	}
	state := func(verify func(t *testing.T, next *familyDoc)) check {
		return func(t *testing.T, _ FamilyImportResult, next *familyDoc) {
			t.Helper()
			verify(t, next)
		}
	}
	cases := []struct {
		name   string
		text   string
		mode   string
		checks []check
	}{
		{"替换成员", "main@example.com\nm1@example.com\nm2@example.com\nm6@example.com", "", []check{status(familyReplaced), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Equal(b.Added, []string{"m6@example.com"}) || !slices.Equal(b.Removed, []string{"m3@example.com", "m4@example.com", "m5@example.com"}) || b.GroupID != groupA {
				t.Fatalf("增减 = +%v −%v", b.Added, b.Removed)
			}
		}), state(func(t *testing.T, next *familyDoc) {
			if groupOf(next, "m3@example.com") != nil || groupOf(next, "m6@example.com").ID != groupA {
				t.Fatal("m3 应变成非家庭组，m6 进 A")
			}
		})}},
		{"顺序不同算未变化", "main@example.com\nm5@example.com, m4@example.com、m3@example.com；m2@example.com m1@example.com", "", []check{status(familyUnchanged)}},
		{"新建并从两组移入", "n@example.com\nm1@example.com\ny@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if len(b.Moved) != 2 || b.Moved[0] != (MovedEmail{"m1@example.com", groupA, "main@example.com"}) || b.Moved[1].FromGroup != groupB || len(b.Dissolved) != 0 {
				t.Fatalf("移动 = %+v", b.Moved)
			}
		})}},
		{"别组主邮箱当成员拒绝", "n@example.com\nx@example.com", "", []check{status(familyRejected), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonMemberIsPrimary || b.Detail == nil || b.Detail.Email != "x@example.com" || b.Detail.Line != 2 || b.Detail.Position != 1 {
				t.Fatalf("拒绝 = %+v %+v", b, b.Detail)
			}
		}), state(func(t *testing.T, next *familyDoc) {
			if len(next.groups) != 2 || groupOf(next, "n@example.com") != nil {
				t.Fatal("拒绝时不应改动")
			}
		})}},
		{"换主", "m1@example.com\nmain@example.com\nm2@example.com", "", []check{status(familyPrimaryChanged), state(func(t *testing.T, next *familyDoc) {
			group := groupOf(next, "m1@example.com")
			if group.ID != groupA || group.Primary != "m1@example.com" || !slices.Equal(group.Members, []string{"main@example.com", "m2@example.com"}) || group.Name != "李家" {
				t.Fatalf("换主后 = %+v", group)
			}
		}), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Equal(b.Removed, []string{"m3@example.com", "m4@example.com", "m5@example.com"}) {
				t.Fatalf("换主的移出 = %v", b.Removed)
			}
		})}},
		{"不含原主邮箱不算换主", "m1@example.com\nm2@example.com\nm3@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if len(b.Moved) != 3 {
				t.Fatalf("应从 A 移入三个: %+v", b.Moved)
			}
		})}},
		{"被移空的组自动解散", "y@example.com\nz@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if len(b.Dissolved) != 1 || b.Dissolved[0].GroupID != groupB || b.Dissolved[0].Primary != "x@example.com" {
				t.Fatalf("应解散 B: %+v", b.Dissolved)
			}
		}), state(func(t *testing.T, next *familyDoc) {
			if groupOf(next, "x@example.com") != nil || len(next.groups) != 2 {
				t.Fatal("x 应变成非家庭组")
			}
		})}},
		{"单邮箱拒绝", "main@example.com", "", []check{status(familyRejected), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonSingle {
				t.Fatal(b.Reason)
			}
		})}},
		{"歧义块拒绝", "n1@example.com----n2@example.com----n3@example.com\np1@example.com----p2@example.com", "", []check{status(familyRejected), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonAmbiguous {
				t.Fatal(b.Reason)
			}
		})}},
		{"每行一组", "n1@example.com----n2@example.com----n3@example.com\np1@example.com----p2@example.com", familyModeLines, []check{status(familyCreated, familyCreated)}},
		{"只有一行多个邮箱照收", "n1@example.com, n2@example.com\nn3@example.com", "", []check{status(familyCreated)}},
		{"组名", "# 张家\nq@example.com\nq1@example.com\nq2@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Name != "张家" {
				t.Fatalf("组名 = %q", b.Name)
			}
		})}},
		{"已有组不按 # 改名", "# 张家\nmain@example.com\nm1@example.com\nm2@example.com\nm3@example.com\nm4@example.com\nm5@example.com", "", []check{status(familyUnchanged), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Contains(b.Warnings, familyWarnNameIgnored) || b.Name != "李家" {
				t.Fatalf("应提示 name_ignored: %+v", b)
			}
		})}},
		{"注释掉的邮箱分块", "q@example.com\n# q1@example.com\nq2@example.com", "", []check{status(familyRejected, familyRejected), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Contains(b.Warnings, familyWarnCommentSplit) || b.CommentLine != 2 || b.Reason != familyReasonSingle {
				t.Fatalf("前一块 = %+v", b)
			}
		}), block(1, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Contains(b.Warnings, familyWarnCommentSplit) || b.Name != "" {
				t.Fatalf("后一块（含 @ 的注释行不当组名）= %+v", b)
			}
		})}},
		{"每行一组的组名", "# 张家\nq@example.com q1@example.com\n# 李家\nr@example.com r1@example.com", familyModeLines, []check{status(familyCreated, familyCreated), block(1, func(t *testing.T, b FamilyBlockResult) {
			if b.Name != "李家" {
				t.Fatalf("组名 = %q", b.Name)
			}
		})}},
		{"12 人多行", strings.Join(emails("t", 12), "\n"), "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Contains(b.Warnings, familyWarnOverFive) || !slices.Contains(b.Warnings, familyWarnMissingBlank) {
				t.Fatalf("提示 = %v", b.Warnings)
			}
		}), func(t *testing.T, result FamilyImportResult, _ *familyDoc) {
			if !slices.Equal(result.Summary.Suspect, []string{familyWarnMissingBlank}) {
				t.Fatalf("suspect = %v", result.Summary.Suspect)
			}
		}}},
		{"12 人写在一行不提示少空行", strings.Join(emails("t", 12), " "), "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if slices.Contains(b.Warnings, familyWarnMissingBlank) || !slices.Contains(b.Warnings, familyWarnOverFive) {
				t.Fatalf("提示 = %v", b.Warnings)
			}
		})}},
		{"21 个邮箱拒绝", strings.Join(emails("t", 21), "\n"), "", []check{status(familyRejected), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonTooLarge {
				t.Fatal(b.Reason)
			}
		})}},
		{"20 个邮箱照收", strings.Join(emails("t", 20), "\n"), "", []check{status(familyCreated)}},
		{"块内重复与主邮箱写进成员", "Q@example.com\nq1@example.com\nq1@example.com\nq@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Equal(b.Members, []string{"q1@example.com"}) || !slices.Contains(b.Warnings, familyWarnDuplicate) || !slices.Contains(b.Warnings, familyWarnPrimaryInMembers) {
				t.Fatalf("去重 = %+v", b)
			}
		})}},
		{"只有主邮箱写进成员", "Q@example.com\nq1@example.com\nq@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if !slices.Equal(b.Members, []string{"q1@example.com"}) || !slices.Equal(b.Warnings, []string{familyWarnPrimaryInMembers}) {
				t.Fatalf("主邮箱写进成员 = %+v", b)
			}
		})}},
		{"非邮箱不回显", "q@example.com\nq1@example.com Secret-Token-99", "", []check{status(familyInvalid), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonTokenNotEmail || b.Detail.Line != 2 || b.Detail.Position != 2 || len(b.Members) != 0 || b.Primary != "" {
				t.Fatalf("无法识别 = %+v %+v", b, b.Detail)
			}
		})}},
		{"第一行像组名", "张家\nq@example.com\nq1@example.com", "", []check{status(familyInvalid), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Hint != familyHintLooksLikeName || b.Detail.Line != 1 || b.Detail.Position != 1 {
				t.Fatalf("hint = %q", b.Hint)
			}
		})}},
		{"像账号资料", "q@example.com----Fake-Pass-1\nq1@example.com", "", []check{status(familyInvalid), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Reason != familyReasonLooksProfile {
				t.Fatal(b.Reason)
			}
		})}},
		{"全角、Tab 与各种分隔", "ｑ＠ｅｘａｍｐｌｅ．ｃｏｍ\tq1@example.com；q2@example.com——q3@example.com", "", []check{status(familyCreated), block(0, func(t *testing.T, b FamilyBlockResult) {
			if b.Primary != "q@example.com" || len(b.Members) != 3 {
				t.Fatalf("分隔 = %+v", b)
			}
		})}},
		{"空行与 # 行分块、批级提示", "q@example.com\nq1@example.com\n\n# 另一组\nq@example.com\nq2@example.com\n\nr@example.com\nq2@example.com", "", []check{
			status(familyCreated, familyReplaced, familyCreated),
			func(t *testing.T, result FamilyImportResult, next *familyDoc) {
				want := []BatchWarning{
					{Code: familyBatchPrimaryRepeated, Email: "q@example.com", Blocks: []int{1, 2}},
					{Code: familyBatchEmailInBlocks, Email: "q2@example.com", Blocks: []int{2, 3}},
				}
				if fmt.Sprint(result.BatchWarnings) != fmt.Sprint(want) {
					t.Fatalf("批级提示 = %+v", result.BatchWarnings)
				}
				// 第三块把 q2 从第二块的结果里移走，q 的组被移空 → 自动解散
				if group := groupOf(next, "q@example.com"); group != nil {
					t.Fatalf("q 的组应自动解散: %+v", group)
				}
				if result.Blocks[2].Moved[0].Email != "q2@example.com" || len(result.Blocks[2].Dissolved) != 1 {
					t.Fatalf("第三块 = %+v", result.Blocks[2])
				}
			}}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			result, next := previewFamilies(t, familyFixture(), item.text, item.mode)
			for _, verify := range item.checks {
				verify(t, result, next)
			}
		})
	}
}

// TestFamilyAtomicPerBlock 一批里被拒绝的块不改任何组、其他块照常；dry-run 与确认结果一致，dry-run 不写盘、不建目录
func TestFamilyAtomicPerBlock(t *testing.T) {
	store, dir, _ := newTestStore(t)
	setup := "main@example.com\nm1@example.com\nm2@example.com\n\nx@example.com\ny@example.com\n"
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: setup}); err != nil {
		t.Fatal(err)
	}
	text := "n@example.com\nm1@example.com\n\nk@example.com\nx@example.com\nm2@example.com\n\nw@example.com\ny@example.com\n"
	preview, err := store.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(dir, "families.json"))
	if got := []string{preview.Blocks[0].Status, preview.Blocks[1].Status, preview.Blocks[2].Status}; !slices.Equal(got, []string{familyCreated, familyRejected, familyCreated}) {
		t.Fatalf("预览状态 = %v", got)
	}
	if preview.Summary.Dissolved != 1 || preview.Summary.Moved != 2 || preview.Summary.Created != 2 || preview.Summary.Rejected != 1 {
		t.Fatalf("预览汇总 = %+v", preview.Summary)
	}
	if after := readFile(t, filepath.Join(dir, "families.json")); !bytes.Equal(before, after) {
		t.Fatal("dry-run 不应写盘")
	}
	confirmed, err := store.ImportFamilies(bg, FamilyImportInput{Text: text, BaseRevision: preview.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(confirmed.Summary) != fmt.Sprint(preview.Summary) || confirmed.DryRun {
		t.Fatalf("确认与预览不一致: %+v / %+v", confirmed.Summary, preview.Summary)
	}
	view, _ := store.View(bg, "")
	byPrimary := map[string]FamilyGroup{}
	for _, group := range view.Families {
		byPrimary[group.Primary] = group
	}
	if _, ok := byPrimary["x@example.com"]; ok {
		t.Fatal("x 的组被移空，应解散")
	}
	if group := byPrimary["main@example.com"]; !slices.Equal(group.Members, []string{"m2@example.com"}) {
		t.Fatalf("被拒绝的块不应移走 m2: %+v", group)
	}
	if len(view.Families) != 3 || byPrimary["w@example.com"].Members[0] != "y@example.com" || byPrimary["n@example.com"].Members[0] != "m1@example.com" {
		t.Fatalf("组 = %+v", view.Families)
	}
	// dry-run 不建目录
	fresh, freshDir, _ := newTestStore(t)
	if _, err := fresh.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: setup}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(freshDir)); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应建目录: %v", err)
	}
}

// TestFamilyEditAndDissolve 改名、改色、设为主邮箱、移出成员、别组主邮箱当新主拒绝、成员为空 400、移入别组成员使那个组解散、rev 不符 409、解散、不存在 404
func TestFamilyEditAndDissolve(t *testing.T) {
	store, dir, _ := newTestStore(t)
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "main@example.com\nm1@example.com\nm2@example.com\n\nx@example.com\ny@example.com\n"}); err != nil {
		t.Fatal(err)
	}
	view, _ := store.View(bg, "")
	a, b := view.Families[0], view.Families[1]
	renamed, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Rev: &a.Rev, Name: strPtr(" 张家 "), Color: intPtr(7)})
	if err != nil || renamed.Status != familyUpdated || renamed.Group.Name != "张家" || renamed.Group.Color != 7 || renamed.Group.Rev != a.Rev+1 {
		t.Fatalf("改名改色 = %+v %v", renamed, err)
	}
	// 旧 rev → 409
	_, err = store.UpdateFamily(bg, a.ID, FamilyUpdate{Rev: &a.Rev, Name: strPtr("x")})
	assertCode(t, err, 409, CodeConflict)
	rev := renamed.Group.Rev
	swapped, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Rev: &rev, Primary: strPtr("m2@example.com")})
	if err != nil || swapped.Status != familyPrimaryChanged || swapped.Group.Primary != "m2@example.com" ||
		!slices.Equal(swapped.Group.Members, []string{"m1@example.com", "main@example.com"}) || len(swapped.Removed) != 0 {
		t.Fatalf("设为主邮箱 = %+v %v", swapped.Group, err)
	}
	rev = swapped.Group.Rev
	removed, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Rev: &rev, Members: &[]string{"main@example.com"}})
	if err != nil || removed.Status != familyReplaced || !slices.Equal(removed.Removed, []string{"m1@example.com"}) {
		t.Fatalf("移出成员 = %+v %v", removed, err)
	}
	rejected, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Primary: strPtr("x@example.com")})
	if err != nil || rejected.Status != familyRejected || rejected.Reason != familyReasonPrimaryIsOther || rejected.Detail.GroupPrimary != "x@example.com" {
		t.Fatalf("别组主邮箱当新主 = %+v %v", rejected, err)
	}
	memberPrimary, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Members: &[]string{"main@example.com", "x@example.com"}})
	if err != nil || memberPrimary.Status != familyRejected || memberPrimary.Reason != familyReasonMemberIsPrimary {
		t.Fatalf("别组主邮箱当成员 = %+v %v", memberPrimary, err)
	}
	_, err = store.UpdateFamily(bg, a.ID, FamilyUpdate{Members: &[]string{}})
	assertCode(t, err, 400, CodeFamilyEmpty)
	_, err = store.UpdateFamily(bg, a.ID, FamilyUpdate{Members: &[]string{"bad"}})
	assertCode(t, err, 400, CodeInvalidEmail)
	_, err = store.UpdateFamily(bg, a.ID, FamilyUpdate{Name: strPtr(strings.Repeat("名", 41))})
	assertCode(t, err, 400, CodeInvalidField)
	_, err = store.UpdateFamily(bg, a.ID, FamilyUpdate{Color: intPtr(12)})
	assertCode(t, err, 400, CodeInvalidField)
	// dry-run：把 B 唯一的成员拉进来 → 预计 B 自动解散，不写盘
	before := readFile(t, filepath.Join(dir, "families.json"))
	dry, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{DryRun: true, Members: &[]string{"main@example.com", "y@example.com"}})
	if err != nil || !dry.DryRun || len(dry.Dissolved) != 1 || dry.Dissolved[0].GroupID != b.ID {
		t.Fatalf("dry-run = %+v %v", dry, err)
	}
	if after := readFile(t, filepath.Join(dir, "families.json")); !bytes.Equal(before, after) {
		t.Fatal("dry-run 不应写盘")
	}
	moved, err := store.UpdateFamily(bg, a.ID, FamilyUpdate{Members: &[]string{"main@example.com", "y@example.com"}})
	if err != nil || len(moved.Dissolved) != 1 || len(moved.Moved) != 1 {
		t.Fatalf("移入 = %+v %v", moved, err)
	}
	snapshotsBefore, _ := filepath.Glob(filepath.Join(dir, "snapshots", "families-*.json"))
	_, err = store.DissolveFamily(bg, a.ID, int64Ptr(1))
	assertCode(t, err, 409, CodeConflict)
	dissolved, err := store.DissolveFamily(bg, a.ID, nil)
	if err != nil || !dissolved.Dissolved || dissolved.Members != 3 {
		t.Fatalf("解散 = %+v %v", dissolved, err)
	}
	if snapshots, _ := filepath.Glob(filepath.Join(dir, "snapshots", "families-*.json")); len(snapshots) != len(snapshotsBefore)+1 {
		t.Fatalf("解散前应存快照: %v", snapshots)
	}
	_, err = store.DissolveFamily(bg, a.ID, nil)
	assertCode(t, err, 404, CodeFamilyNotFound)
	_, err = store.UpdateFamily(bg, "fam-000000000000", FamilyUpdate{Name: strPtr("x")})
	assertCode(t, err, 404, CodeFamilyNotFound)
	if view, _ := store.View(bg, ""); len(view.Families) != 0 {
		t.Fatalf("应没有家庭组了: %+v", view.Families)
	}
	// 新建：成员为空 400；主邮箱已是某组主邮箱 → 按替换
	_, err = store.CreateFamily(bg, FamilyCreate{Primary: "p@example.com"})
	assertCode(t, err, 400, CodeFamilyEmpty)
	created, err := store.CreateFamily(bg, FamilyCreate{Primary: "p@example.com", Members: []string{"p1@example.com"}, Name: strPtr("王家"), Color: intPtr(3)})
	if err != nil || created.Status != familyCreated || created.Group == nil || created.Group.Color != 3 || created.Group.Name != "王家" {
		t.Fatalf("新建 = %+v %v", created, err)
	}
	replaced, err := store.CreateFamily(bg, FamilyCreate{Primary: "p@example.com", Members: []string{"p2@example.com"}})
	if err != nil || replaced.Status != familyReplaced || replaced.GroupID != created.GroupID {
		t.Fatalf("主邮箱已有组时按替换 = %+v %v", replaced, err)
	}
}

// TestFamilyBaseRevisionAndAck 预览之后另一个进程改了家庭组 → 409 附新预览；只改了资料 → 照常；可能少了空行要核对
func TestFamilyBaseRevisionAndAck(t *testing.T) {
	store, dir, logs := newTestStore(t)
	other := openAgain(dir, logs)
	text := "a@example.com\na1@example.com\n"
	preview, err := store.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ImportFamilies(bg, FamilyImportInput{Text: "b@example.com\nb1@example.com\n"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.ImportFamilies(bg, FamilyImportInput{Text: text, BaseRevision: preview.Revision})
	assertCode(t, err, 409, CodeConflict)
	conflictErr := err.(*ConflictError)
	if fresh, ok := conflictErr.Preview.(FamilyImportResult); !ok || !fresh.DryRun || fresh.Revision == preview.Revision {
		t.Fatalf("新预览 = %+v", conflictErr.Preview)
	}
	preview, _ = store.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: text})
	importLines(t, other, "z@example.com----Fake-Pass-1")
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: text, BaseRevision: preview.Revision}); err != nil {
		t.Fatalf("资料变化不影响家庭组确认: %v", err)
	}
	twelve := strings.Join(emails("t", 12), "\n")
	_, err = store.ImportFamilies(bg, FamilyImportInput{Text: twelve})
	assertCode(t, err, 409, CodeSuspectUnacked)
	if result, err := store.ImportFamilies(bg, FamilyImportInput{Text: twelve, AcknowledgeSuspect: true}); err != nil || result.Summary.Created != 1 {
		t.Fatalf("核对后 = %+v %v", result.Summary, err)
	}
	// 新建与编辑同样检查 base_revision
	view, _ := store.View(bg, "")
	if _, err := other.CreateFamily(bg, FamilyCreate{Primary: "c@example.com", Members: []string{"c1@example.com"}}); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateFamily(bg, FamilyCreate{Primary: "d@example.com", Members: []string{"d1@example.com"}, BaseRevision: view.Revision})
	assertCode(t, err, 409, CodeConflict)
	_, err = store.UpdateFamily(bg, view.Families[0].ID, FamilyUpdate{Name: strPtr("x"), BaseRevision: view.Revision})
	assertCode(t, err, 409, CodeConflict)
}

// TestColorAutoAssign 空表从 0 开始；未用的先用；全部用过后按编号轮转；跳过与 seq 最大的组相同的颜色；解散后补空缺；同批连续新建互不相同
func TestColorAutoAssign(t *testing.T) {
	work := newFamilyWork(emptyFamilyDoc(), time.Now(), dryRunGroupIDs())
	if color := work.autoColor(); color != 0 {
		t.Fatalf("空表 = %d", color)
	}
	var text strings.Builder
	for index := range 26 {
		fmt.Fprintf(&text, "p%d@example.com\nq%d@example.com\n\n", index, index)
	}
	result, next := previewFamilies(t, emptyFamilyDoc(), text.String(), "")
	var colors []int
	for _, block := range result.Blocks {
		colors = append(colors, *block.Color)
	}
	for index, color := range colors {
		if color != index%PaletteSize {
			t.Fatalf("颜色 = %v", colors)
		}
		if index > 0 && color == colors[index-1] {
			t.Fatalf("相邻的组颜色相同: %v", colors)
		}
	}
	// 解散颜色 5 的组后，下一个新组补上 5；跳过 seq 最大的组的颜色
	groups := slices.Clone(next.groups)
	groups = slices.DeleteFunc(groups, func(group FamilyGroup) bool { return group.Color == 5 && group.Seq == 6 })
	work = newFamilyWork(newFamilyDoc(groups), time.Now(), dryRunGroupIDs())
	if color := work.autoColor(); color != 5 {
		t.Fatalf("应补上解散留下的空缺 5，得到 %d", color)
	}
	same := newFamilyDoc([]FamilyGroup{
		{ID: "fam-000000000001", Seq: 1, Color: 3, Primary: "a@example.com", Members: []string{"a1@example.com"}},
		{ID: "fam-000000000002", Seq: 2, Color: 4, Primary: "b@example.com", Members: []string{"b1@example.com"}},
	})
	work = newFamilyWork(same, time.Now(), dryRunGroupIDs())
	if color := work.autoColor(); color != 5 {
		t.Fatalf("未用的颜色里从上一组 +1 开始: %d", color)
	}
	// seq 最大的组的颜色用得最少时也不选它（否则相邻两组同色）
	var lopsided []FamilyGroup
	for index := range 23 {
		color := 1 + index%11
		if index == 22 {
			color = 0
		}
		lopsided = append(lopsided, FamilyGroup{ID: fmt.Sprintf("fam-%012d", index), Seq: int64(index + 1), Color: color,
			Primary: fmt.Sprintf("l%d@example.com", index), Members: []string{fmt.Sprintf("l%d-1@example.com", index)}})
	}
	if color := newFamilyWork(newFamilyDoc(lopsided), time.Now(), dryRunGroupIDs()).autoColor(); color == 0 {
		t.Fatal("不应与 seq 最大的组同色")
	}
	only := newFamilyDoc([]FamilyGroup{{ID: "fam-000000000001", Seq: 9, Color: 11, Primary: "a@example.com", Members: []string{"a1@example.com"}}})
	if color := newFamilyWork(only, time.Now(), dryRunGroupIDs()).autoColor(); color != 0 {
		t.Fatalf("循环到 0: %d", color)
	}
}

// TestFamilyWorkIncremental 新建、移入移空自动解散、替换、换主、改色交替进行时，增量维护的颜色计数与“seq 最大的组”
// 始终等于按全部组重新数的结果（autoColor 因此与原来逐组统计的选色一致），最后的结果与组列表一致
func TestFamilyWorkIncremental(t *testing.T) {
	random := rand.New(rand.NewSource(20261126))
	var groups []FamilyGroup
	for index := range 40 {
		groups = append(groups, FamilyGroup{ID: fmt.Sprintf("fam-%012d", index+1), Seq: int64(index*2 + 1), Rev: 1, Color: random.Intn(PaletteSize),
			Primary: fmt.Sprintf("p%d@example.com", index), Members: []string{fmt.Sprintf("p%d-a@example.com", index), fmt.Sprintf("p%d-b@example.com", index)}})
	}
	work := newFamilyWork(newFamilyDoc(groups), time.Now(), dryRunGroupIDs())
	check := func(step int) {
		t.Helper()
		var counts [PaletteSize]int
		var last *FamilyGroup
		for _, group := range work.byID {
			counts[group.Color]++
			if last == nil || group.Seq > last.Seq {
				last = group
			}
		}
		if counts != work.colors {
			t.Fatalf("第 %d 步颜色计数 = %v，重新数为 %v", step, work.colors, counts)
		}
		if got := work.last(); got != last {
			t.Fatalf("第 %d 步 seq 最大的组不一致", step)
		}
	}
	check(0)
	pick := func() string {
		alive := make([]*FamilyGroup, 0, len(work.byID))
		for _, group := range work.groups {
			if work.byID[group.ID] == group {
				alive = append(alive, group)
			}
		}
		if len(alive) == 0 || random.Intn(4) == 0 {
			return fmt.Sprintf("n%d@example.com", random.Intn(400))
		}
		group := alive[random.Intn(len(alive))]
		if len(group.Members) == 0 || random.Intn(3) == 0 {
			return group.Primary
		}
		return group.Members[random.Intn(len(group.Members))]
	}
	for step := 1; step <= 600; step++ {
		if random.Intn(10) == 0 && len(work.byID) > 0 {
			for _, group := range work.byID {
				work.recolor(group, random.Intn(PaletteSize))
				break
			}
			check(step)
			continue
		}
		primary := pick()
		seen := map[string]bool{primary: true}
		var members []string
		for range 1 + random.Intn(3) {
			if email := pick(); !seen[email] {
				seen[email] = true
				members = append(members, email)
			}
		}
		if len(members) == 0 {
			continue
		}
		result := FamilyBlockResult{Index: step}
		work.applyBlock(&result, primary, members, map[string]emailToken{}, "", nil)
		check(step)
	}
	next := work.result()
	if len(next.groups) != len(work.byID) {
		t.Fatalf("结果 %d 组，byID %d 组", len(next.groups), len(work.byID))
	}
	for index := 1; index < len(next.groups); index++ {
		if next.groups[index-1].Seq >= next.groups[index].Seq {
			t.Fatal("结果应按 seq 升序")
		}
	}
}
