package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPersistRoundTrip 权限、版本、按邮箱排序的数组、读回一致、write_id 每次写都变、未变化不写盘、.bak 等于上一版原样字节、第一次写没有 .bak
func TestPersistRoundTrip(t *testing.T) {
	store, dir, logs := newTestStore(t)
	path := filepath.Join(dir, "profiles.json")
	importLines(t, store, "b@example.com----Fake-Pass-2\na@example.com----Fake-Pass-1----r@example.com")
	if fileExists(path + ".bak") {
		t.Fatal("第一次写不应有 .bak")
	}
	first := readFile(t, path)
	var file profilesFile
	if err := json.Unmarshal(first, &file); err != nil || file.Version != 1 || len(file.Profiles) != 2 || file.Profiles[0].Email != "a@example.com" || len(file.WriteID) != 12 {
		t.Fatalf("文件格式 err=%v %+v", err, file)
	}
	if runtime.GOOS != "windows" {
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{dir, 0o700}, {path, 0o600}, {filepath.Join(dir, ".lock"), 0o600}} {
			info, err := os.Stat(item.path)
			if err != nil || info.Mode().Perm() != item.mode {
				t.Fatalf("%s 权限 = %v %v，期望 %v", item.path, info.Mode().Perm(), err, item.mode)
			}
		}
	}
	firstInfo, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	importLines(t, store, "a@example.com----Fake-Pass-1")
	if info, _ := os.Stat(path); !info.ModTime().Equal(firstInfo.ModTime()) || !bytes.Equal(readFile(t, path), first) {
		t.Fatal("未变化不应写盘")
	}
	importLines(t, store, "a@example.com----Fake-Pass-9")
	second := readFile(t, path)
	if !bytes.Equal(readFile(t, path+".bak"), first) {
		t.Fatal(".bak 应等于上一版原样字节")
	}
	var secondFile profilesFile
	_ = json.Unmarshal(second, &secondFile)
	if secondFile.WriteID == file.WriteID {
		t.Fatal("write_id 每次写都应变化")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path + ".bak"); info.Mode().Perm() != 0o600 {
			t.Fatalf(".bak 权限 = %v", info.Mode().Perm())
		}
	}
	// 另一个 Store 读回一致
	again := openAgain(dir, logs)
	if secret := reveal(t, again, "a@example.com"); secret.Password != "Fake-Pass-9" || secret.RecoveryEmail != "r@example.com" {
		t.Fatal("读回不一致")
	}
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "m@example.com\nm1@example.com"}); err != nil {
		t.Fatal(err)
	}
	var families familiesFile
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, "families.json")), &families); err != nil || families.Version != 1 || len(families.Groups) != 1 {
		t.Fatalf("家庭组文件 err=%v %+v", err, families)
	}
	if warns, _ := logs.counts(); warns != 0 {
		t.Fatalf("正常读写不应有 WARN: %s", logs.all())
	}
}

// TestCorruptNotOverwritten 坏 JSON、重复的键、同一邮箱两次、版本不认识、家庭组不变式破坏：View 带错误、另一个文件照常；
// 写接口、取出、导出都 409；主文件与 .bak 字节不变；WARN 只写一次且不含文件内容；修好后恢复并写一次 INFO
func TestCorruptNotOverwritten(t *testing.T) {
	const sentinel = "Sentinel-Fake-Pass-77"
	cases := []struct {
		name   string
		file   string
		data   string
		reason string
	}{
		{"坏 JSON", "profiles", `{"version":1,"profiles":[{"email":"a@example.com","password":"` + sentinel + `"` + "x", problemJSON},
		{"重复的键", "profiles", `{"version":1,"profiles":[{"email":"a@example.com","password":"` + sentinel + `","Password":"other"}]}`, problemJSON},
		{"同一邮箱两次", "profiles", `{"version":1,"profiles":[{"email":"a@example.com","password":"` + sentinel + `"},{"email":"A@example.com"}]}`, problemInvalid},
		{"版本不认识", "profiles", `{"version":2,"profiles":[{"email":"a@example.com","password":"` + sentinel + `"}]}`, problemVersion},
		{"没有版本号", "profiles", `{"profiles":[]}`, problemVersion},
		{"值类型不对", "profiles", `{"version":1,"profiles":[{"email":"a@example.com","password":123}]}`, problemJSON},
		{"组不变式", "families", `{"version":1,"groups":[{"id":"fam-aaaaaaaaaaaa","color":0,"primary":"a@example.com","members":["b@example.com"]},{"id":"fam-bbbbbbbbbbbb","color":1,"primary":"c@example.com","members":["b@example.com","` + sentinel + `@example.com"]}]}`, problemInvalid},
		{"颜色超范围", "families", `{"version":1,"groups":[{"id":"fam-aaaaaaaaaaaa","color":12,"primary":"a@example.com","members":["b@example.com"]}]}`, problemInvalid},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			store, dir, logs := newTestStore(t)
			importLines(t, store, "seed@example.com----Fake-Pass-1")
			importLines(t, store, "seed@example.com----Fake-Pass-2")
			for _, text := range []string{"f@example.com\nf1@example.com", "f@example.com\nf1@example.com\nf2@example.com"} {
				if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: text}); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, item.file+".json")
			if err := os.WriteFile(path, []byte(item.data), 0o600); err != nil {
				t.Fatal(err)
			}
			bak, _ := os.ReadFile(path + ".bak")
			for range 3 {
				view, err := store.View(bg, "")
				if err != nil || len(view.Errors) != 1 || view.Errors[0].File != item.file || view.Errors[0].Reason != item.reason || view.Errors[0].Code != CodeUnreadable {
					t.Fatalf("View err=%v errors=%+v", err, view.Errors)
				}
				if !strings.HasPrefix(strings.Split(view.Revision, ".")[map[string]int{"profiles": 0, "families": 1}[item.file]], "e") {
					t.Fatalf("错误状态的 revision 应以 e 开头: %s", view.Revision)
				}
				if item.file == "profiles" && (len(view.Profiles) != 0 || len(view.Families) != 1) {
					t.Fatal("另一个文件应照常")
				}
				if item.file == "families" && (len(view.Families) != 0 || len(view.Profiles) != 1) {
					t.Fatal("另一个文件应照常")
				}
			}
			if item.file == "profiles" {
				for _, err := range []error{
					func() error {
						_, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: "n@example.com----P"}}})
						return err
					}(),
					func() error {
						_, err := store.ImportProfiles(bg, ProfileImportInput{DryRun: true, Sources: []ImportSource{{Text: "n@example.com----P"}}})
						return err
					}(),
					func() error {
						_, err := store.UpdateProfile(bg, ProfileUpdate{Email: "n@example.com", Password: strPtr("P")})
						return err
					}(),
					func() error {
						_, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}})
						return err
					}(),
					func() error { _, err := store.RevealProfile(bg, RevealInput{Email: "a@example.com"}); return err }(),
					func() error { _, err := store.ExportProfiles(bg, ExportInput{All: true}); return err }(),
				} {
					assertCode(t, err, 409, CodeUnreadable)
				}
			} else {
				for _, err := range []error{
					func() error {
						_, err := store.ImportFamilies(bg, FamilyImportInput{Text: "n@example.com\nn1@example.com"})
						return err
					}(),
					func() error {
						_, err := store.CreateFamily(bg, FamilyCreate{Primary: "n@example.com", Members: []string{"n1@example.com"}})
						return err
					}(),
					func() error { _, err := store.DissolveFamily(bg, "fam-aaaaaaaaaaaa", nil); return err }(),
				} {
					assertCode(t, err, 409, CodeUnreadable)
				}
			}
			if got := readFile(t, path); string(got) != item.data {
				t.Fatal("坏文件不应被改动")
			}
			if got, _ := os.ReadFile(path + ".bak"); !bytes.Equal(got, bak) {
				t.Fatal(".bak 不应被改动")
			}
			if warns, _ := logs.counts(); warns != 1 || strings.Contains(logs.all(), sentinel) || strings.Contains(logs.all(), "Password") {
				t.Fatalf("WARN 应只有一行且不含内容: %s", logs.all())
			}
			view, _ := store.View(bg, "")
			if data, _ := json.Marshal(view); strings.Contains(string(data), sentinel) {
				t.Fatalf("列表不应含坏文件的内容: %s", data)
			}
			// 修好（用 .bak 替换）后恢复，写一次 INFO
			if err := os.WriteFile(path, bak, 0o600); err != nil {
				t.Fatal(err)
			}
			if view, err := store.View(bg, ""); err != nil || len(view.Errors) != 0 {
				t.Fatalf("修好后 err=%v errors=%+v", err, view.Errors)
			}
			if _, infos := logs.counts(); infos != 1 || !strings.Contains(logs.all(), "已恢复可读") {
				t.Fatalf("恢复后应有一行 INFO: %s", logs.all())
			}
		})
	}
	if os.Getuid() != 0 && runtime.GOOS != "windows" {
		store, dir, _ := newTestStore(t)
		importLines(t, store, "a@example.com----P")
		path := filepath.Join(dir, "profiles.json")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(path, 0o600)
		fresh := openAgain(dir, &testLogs{})
		view, _ := fresh.View(bg, "")
		if len(view.Errors) != 1 || view.Errors[0].Reason != problemRead {
			t.Fatalf("读不出 = %+v", view.Errors)
		}
	}
}

// TestReadFailureRetried 读主文件失败（权限、属主不对或一次 I/O 错误）后修好：chmod/chown 只改 ctime，stat 的 inode、修改时间、大小都不变，
// 下一次 View（“重新读取”）也要重试读盘并恢复，不能一直用缓存里的错误；仍失败时不重复写 WARN，.bak 没变时也不每次重新解析
func TestReadFailureRetried(t *testing.T) {
	writer, dir, logs := newTestStore(t)
	importLines(t, writer, "a@example.com----Fake-Pass-1")
	importLines(t, writer, "a@example.com----Fake-Pass-2\nb@example.com----Fake-Pass-3")
	path := filepath.Join(dir, "profiles.json")
	var mu sync.Mutex
	denied, bakReads := true, 0
	setDenied := func(value bool) {
		mu.Lock()
		defer mu.Unlock()
		denied = value
	}
	// 新开一个 Store，让第一次 load 真的读盘（writer 已缓存正常内容）
	store := openAgain(dir, logs)
	store.readFile = func(name string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if name == path+".bak" {
			bakReads++
		}
		if denied && name == path {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return os.ReadFile(name)
	}
	for index := range 3 {
		view, err := store.View(bg, "")
		if err != nil || len(view.Errors) != 1 || view.Errors[0].Reason != problemRead || view.Errors[0].BackupCount == nil || *view.Errors[0].BackupCount != 1 {
			t.Fatalf("第 %d 次 View err=%v errors=%+v", index+1, err, view.Errors)
		}
	}
	_, err := store.RevealProfile(bg, RevealInput{Email: "a@example.com"})
	assertCode(t, err, 409, CodeUnreadable)
	if warns, _ := logs.counts(); warns != 1 || strings.Contains(logs.all(), "Fake-Pass") {
		t.Fatalf("同一读失败只应写一行不含内容的 WARN: %s", logs.all())
	}
	mu.Lock()
	reads := bakReads
	mu.Unlock()
	if reads != 1 {
		t.Fatalf(".bak 没变时只应解析一次，实际 %d 次", reads)
	}
	// 修好权限后点“重新读取”
	setDenied(false)
	view, err := store.View(bg, "")
	if err != nil || len(view.Errors) != 0 || len(view.Profiles) != 2 {
		t.Fatalf("修好权限后应恢复: err=%v errors=%+v profiles=%d", err, view.Errors, len(view.Profiles))
	}
	if secret := reveal(t, store, "a@example.com"); secret.Password != "Fake-Pass-2" {
		t.Fatal("恢复后取出应得到文件里的值")
	}
	if _, infos := logs.counts(); infos != 1 || !strings.Contains(logs.all(), "已恢复可读") {
		t.Fatalf("恢复后应有一行 INFO: %s", logs.all())
	}
	// 恢复后回到按 stat 走缓存：文件没变就不再读盘
	setDenied(true)
	if view, err := store.View(bg, ""); err != nil || len(view.Errors) != 0 {
		t.Fatalf("文件没变时应走缓存: err=%v errors=%+v", err, view.Errors)
	}
}

// TestMissingMainKeepsBackup 主文件不见了而 .bak 或快照还在：停止写入、.bak 不变；复制回来就恢复；从空开始把 .bak 移进快照目录
func TestMissingMainKeepsBackup(t *testing.T) {
	store, dir, logs := newTestStore(t)
	path := filepath.Join(dir, "profiles.json")
	importLines(t, store, "a@example.com----Fake-Pass-1\nb@example.com----Fake-Pass-2")
	importLines(t, store, "a@example.com----Fake-Pass-3")
	bak := readFile(t, path+".bak")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	view, _ := store.View(bg, "")
	if len(view.Errors) != 1 || view.Errors[0].Reason != problemMissingWithBackup || view.Errors[0].BackupCount == nil || *view.Errors[0].BackupCount != 2 ||
		!strings.HasPrefix(strings.Split(view.Revision, ".")[0], "m") || !strings.Contains(view.Errors[0].Message, "2 条") {
		t.Fatalf("缺失有备份 = %+v revision=%s", view.Errors, view.Revision)
	}
	for _, err := range []error{
		func() error {
			_, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: "n@example.com----P"}}})
			return err
		}(),
		func() error {
			_, err := store.UpdateProfile(bg, ProfileUpdate{Email: "n@example.com", Password: strPtr("P")})
			return err
		}(),
		func() error {
			_, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}})
			return err
		}(),
	} {
		assertCode(t, err, 409, CodeUnreadable)
	}
	if !bytes.Equal(readFile(t, path+".bak"), bak) || fileExists(path) {
		t.Fatal(".bak 不应被改动，主文件不应被建出来")
	}
	if warns, _ := logs.counts(); warns != 1 || !strings.Contains(logs.all(), "不见了，但还有备份") {
		t.Fatalf("WARN = %s", logs.all())
	}
	// 家庭组写不受影响
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "m@example.com\nm1@example.com"}); err != nil {
		t.Fatalf("另一个文件照常: %v", err)
	}
	// 复制回来就恢复
	if err := os.WriteFile(path, bak, 0o600); err != nil {
		t.Fatal(err)
	}
	if view, _ := store.View(bg, ""); len(view.Errors) != 0 || len(view.Profiles) != 2 {
		t.Fatalf("恢复后 = %+v", view.Errors)
	}
	if _, infos := logs.counts(); infos != 1 {
		t.Fatalf("恢复应写一次 INFO: %s", logs.all())
	}
	// 再删主文件后从空开始：.bak 移到快照目录（内容不变），主文件是空的 v1，之后写入正常
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartEmpty(bg, "families"); err == nil {
		t.Fatal("家庭组文件不是缺失有备份的状态，应 409")
	}
	result, err := store.StartEmpty(bg, "profiles")
	if err != nil || result.MovedBackup == "" || !strings.HasSuffix(result.MovedBackup, "-before-start-empty.json") {
		t.Fatalf("从空开始 = %+v %v", result, err)
	}
	if !bytes.Equal(readFile(t, result.MovedBackup), bak) || fileExists(path+".bak") {
		t.Fatal(".bak 应原样移到快照目录")
	}
	var file profilesFile
	if err := json.Unmarshal(readFile(t, path), &file); err != nil || file.Version != 1 || len(file.Profiles) != 0 {
		t.Fatalf("主文件应是空的 v1: %v", err)
	}
	if view, _ := store.View(bg, ""); len(view.Errors) != 0 || len(view.Profiles) != 0 {
		t.Fatalf("从空开始后 = %+v", view)
	}
	importLines(t, store, "c@example.com----Fake-Pass-C")
	if _, infos := logs.counts(); infos != 1 {
		t.Fatalf("从空开始不应再写“已恢复可读”: %s", logs.all())
	}
	if _, err := store.StartEmpty(bg, "profiles"); err == nil {
		t.Fatal("正常状态下从空开始应 409")
	}
	if _, err := store.StartEmpty(bg, "other"); err == nil {
		t.Fatal("file 不认识应 400")
	}
	// 只有快照没有 .bak 也算有备份
	only, onlyDir, _ := newTestStore(t)
	importLines(t, only, "a@example.com----P1")
	if _, err := only.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}}); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(onlyDir, "profiles.json"))
	_ = os.Remove(filepath.Join(onlyDir, "profiles.json.bak"))
	if view, _ := only.View(bg, ""); len(view.Errors) != 1 || view.Errors[0].Reason != problemMissingWithBackup || view.Errors[0].Snapshots != 1 {
		t.Fatalf("只有快照 = %+v", view.Errors)
	}
	// 什么都没有 → 空状态、不报错
	empty, _, _ := newTestStore(t)
	if view, err := empty.View(bg, ""); err != nil || len(view.Errors) != 0 || view.Revision != "0.0" {
		t.Fatalf("空状态 = %+v %v", view, err)
	}
}

// TestSnapshots 批量操作各存一份等于上一版原样字节的快照；单条编辑、新建与编辑家庭组、dry-run 不存；只保留最新 10 份；不碰别的文件；写不了快照 500 且不动主文件
func TestSnapshots(t *testing.T) {
	store, dir, _ := newTestStore(t)
	snapshots := func(prefix string) []string {
		names, _ := filepath.Glob(filepath.Join(dir, "snapshots", prefix+"-*.json"))
		sort.Strings(names)
		return names
	}
	importLines(t, store, "a@example.com----P1")
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "f@example.com\nf1@example.com\n\ng@example.com\ng1@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(snapshots("profiles")) != 0 || len(snapshots("families")) != 0 {
		t.Fatal("第一次写（之前没有文件）不存快照")
	}
	before := readFile(t, filepath.Join(dir, "profiles.json"))
	importLines(t, store, "a@example.com----P2")
	if names := snapshots("profiles"); len(names) != 1 || !bytes.Equal(readFile(t, names[0]), before) || !snapshotPattern.MatchString(filepath.Base(names[0])) {
		t.Fatalf("确认导入的快照 = %v", names)
	}
	if _, err := store.UpdateProfile(bg, ProfileUpdate{Email: "a@example.com", Password: strPtr("P3")}); err != nil {
		t.Fatal(err)
	}
	previewLines(t, store, "a@example.com----P4", "")
	if len(snapshots("profiles")) != 1 {
		t.Fatal("单条编辑与 dry-run 不存快照")
	}
	if _, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}}); err != nil || len(snapshots("profiles")) != 2 {
		t.Fatalf("删除应存快照: %v %v", err, snapshots("profiles"))
	}
	view, _ := store.View(bg, "")
	if _, err := store.CreateFamily(bg, FamilyCreate{Primary: "h@example.com", Members: []string{"h1@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateFamily(bg, view.Families[0].ID, FamilyUpdate{Name: strPtr("名")}); err != nil || len(snapshots("families")) != 0 {
		t.Fatalf("新建与编辑家庭组不存快照: %v %v", err, snapshots("families"))
	}
	familiesBefore := readFile(t, filepath.Join(dir, "families.json"))
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "f@example.com\nf2@example.com"}); err != nil || len(snapshots("families")) != 1 ||
		!bytes.Equal(readFile(t, snapshots("families")[0]), familiesBefore) {
		t.Fatalf("确认录入应存快照: %v", err)
	}
	if _, err := store.DissolveFamily(bg, view.Families[1].ID, nil); err != nil || len(snapshots("families")) != 2 {
		t.Fatalf("解散应存快照: %v", err)
	}
	// 诱饵：轮转只删名字合格的快照
	decoys := []string{"profiles-manual.json", "profiles-20261125T150405.123Z-before-start-empty.json", "notes.txt"}
	for _, name := range decoys {
		if err := os.WriteFile(filepath.Join(dir, "snapshots", name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 12 {
		importLines(t, store, fmt.Sprintf("z%d@example.com----P", index))
	}
	var regular []string
	for _, name := range snapshots("profiles") {
		if snapshotPattern.MatchString(filepath.Base(name)) {
			regular = append(regular, name)
		}
	}
	if len(regular) != MaxSnapshots {
		t.Fatalf("应只保留最新 %d 份: %d", MaxSnapshots, len(regular))
	}
	for _, name := range decoys {
		if !fileExists(filepath.Join(dir, "snapshots", name)) {
			t.Fatalf("轮转不应删 %s", name)
		}
	}
	// 快照目录不可用（变成了普通文件）→ 500，主文件与 .bak 不变
	broken, brokenDir, logs := newTestStore(t)
	importLines(t, broken, "a@example.com----P1")
	importLines(t, broken, "a@example.com----P2")
	_ = os.RemoveAll(filepath.Join(brokenDir, "snapshots"))
	if err := os.WriteFile(filepath.Join(brokenDir, "snapshots"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	main, bak := readFile(t, filepath.Join(brokenDir, "profiles.json")), readFile(t, filepath.Join(brokenDir, "profiles.json.bak"))
	_, err := broken.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: "a@example.com----P3"}}})
	assertCode(t, err, 500, CodeWriteFailed)
	if !bytes.Equal(readFile(t, filepath.Join(brokenDir, "profiles.json")), main) || !bytes.Equal(readFile(t, filepath.Join(brokenDir, "profiles.json.bak")), bak) {
		t.Fatal("写快照失败时主文件与 .bak 不应改动")
	}
	if !strings.Contains(logs.all(), "账号资料写入失败") || strings.Contains(logs.all(), "P3") {
		t.Fatalf("写入失败的 WARN = %s", logs.all())
	}
}

// TestReadOnlyNoCreate 目录不存在时，只读操作与作用在空状态上没有改动的写操作都不建目录与 .lock
func TestReadOnlyNoCreate(t *testing.T) {
	store, dir, _ := newTestStore(t)
	runtimeDir := filepath.Dir(dir)
	_, _ = store.View(bg, "")
	previewLines(t, store, "a@example.com----P1", "")
	_, _ = store.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: "a@example.com\nb@example.com"})
	_, _ = store.CreateFamily(bg, FamilyCreate{DryRun: true, Primary: "a@example.com", Members: []string{"b@example.com"}})
	if _, err := store.RevealProfile(bg, RevealInput{Email: "a@example.com"}); err == nil {
		t.Fatal("不存在的邮箱取出应 404")
	}
	_, _ = store.ExportProfiles(bg, ExportInput{All: true})
	_, _ = store.ExportProfiles(bg, ExportInput{Emails: []string{"a@example.com"}})
	if result, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}}); err != nil || result.Missing != 1 {
		t.Fatalf("删除不存在的邮箱 = %+v %v", result, err)
	}
	_, _ = store.DissolveFamily(bg, "fam-aaaaaaaaaaaa", nil)
	_, _ = store.StartEmpty(bg, "profiles")
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("不应建出 %s: %v", runtimeDir, err)
	}
	// 第一次真正有改动的写操作才建目录、.lock 与文件
	importLines(t, store, "a@example.com----P1")
	if !fileExists(filepath.Join(dir, ".lock")) || !fileExists(filepath.Join(dir, "profiles.json")) || fileExists(filepath.Join(dir, "families.json")) {
		t.Fatal("第一次写入应建出目录、.lock 与 profiles.json，不建 families.json")
	}
}

// TestSecretsNeverLeak 把哨兵串放进每个位置：只在取出与导出的结果里出现，预览、确认结果、列表、错误、冲突预览、日志里都没有
func TestSecretsNeverLeak(t *testing.T) {
	store, dir, logs := newTestStore(t)
	sentinels := map[string]string{
		"first":    "Leak-First-Segment-1",
		"password": "Leak-Password-2",
		"note":     "Leak-Note-3",
		"otpParam": "LeakIssuer4",
		"long":     "Leak-Long-5",
		"control":  "Leak-Control-6",
		"family":   "Leak-Family-Token-7",
		"badjson":  "Leak-Bad-Json-8",
		"tooLong":  "Leak-Too-Long-Line-9",
	}
	text := strings.Join([]string{
		sentinels["first"] + "----Fake-Pass-0",
		"a@example.com----" + sentinels["password"] + "----" + sentinels["note"] + "----otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&issuer=" + sentinels["otpParam"],
		"b@example.com----Fake-Pass-1----" + sentinels["long"] + strings.Repeat("x", 3000),
		"c@example.com----" + sentinels["control"] + "\x01",
		sentinels["tooLong"] + strings.Repeat("y", 5000),
		"d@example.com----Fake-Pass-2----otpauth://hotp/" + sentinels["otpParam"] + "?secret=JBSWY3DPEHPK3PXP",
	}, "\n")
	collected := []string{}
	record := func(value any, err error) {
		if err != nil {
			collected = append(collected, err.Error())
			var conflictErr *ConflictError
			if errors.As(err, &conflictErr) {
				data, _ := json.Marshal(conflictErr.Preview)
				collected = append(collected, string(data))
			}
		}
		data, _ := json.Marshal(value)
		collected = append(collected, string(data))
	}
	record(store.ImportProfiles(bg, ProfileImportInput{DryRun: true, Sources: []ImportSource{{Name: "n", Text: text}}}))
	record(store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Name: "n", Text: text}}}))
	record(store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: "a@example.com----Other-Pass"}}, ExpectOverwrites: &FieldCounts{}}))
	record(store.View(bg, ""))
	record(store.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: "q@example.com\nq1@example.com " + sentinels["family"]}))
	record(store.ImportFamilies(bg, FamilyImportInput{Text: "q@example.com\nq1@example.com " + sentinels["family"]}))
	record(store.UpdateProfile(bg, ProfileUpdate{Email: "e@example.com", Password: strPtr(sentinels["password"] + "\t")}))
	record(store.UpdateProfile(bg, ProfileUpdate{Email: "e@example.com", TOTP: strPtr(sentinels["password"])}))
	record(store.UpdateProfile(bg, ProfileUpdate{Email: "e@example.com", Note: strPtr(sentinels["note"])}))
	record(store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: strings.Repeat("u@example.com----"+sentinels["password"]+"\n", 3) + "\x00"}}, Mode: "bad"}))
	if err := os.WriteFile(filepath.Join(dir, "families.json"), []byte(`{"version":1,"groups":[`+sentinels["badjson"]), 0o600); err != nil {
		t.Fatal(err)
	}
	record(store.View(bg, ""))
	record(store.ImportFamilies(bg, FamilyImportInput{Text: "q@example.com\nq1@example.com"}))
	joined := strings.Join(collected, "\n") + "\n" + logs.all()
	for name, sentinel := range sentinels {
		if strings.Contains(joined, sentinel) {
			t.Fatalf("哨兵 %s 泄露: %s", name, joined)
		}
	}
	if strings.Contains(joined, "Other-Pass") {
		t.Fatal("冲突预览不应含密码")
	}
	// 存进记录的哨兵只在取出与导出里出现
	secret := reveal(t, store, "a@example.com")
	export, _ := store.ExportProfiles(bg, ExportInput{All: true, IncludeNote: true})
	if secret.Password != sentinels["password"] || !strings.Contains(strings.Join(export.Lines, "\n"), sentinels["note"]) {
		t.Fatal("取出与导出应有明文")
	}
}

// TestConcurrentStores 两个 Store 共用一个目录：并发导入不丢更新、同一邮箱不同字段为并集；同时编辑同一组一个 409；
// 另一个 Store 写过后 revision 改变；同一个 Store 上并发读写不死锁、不超时
func TestConcurrentStores(t *testing.T) {
	_, dir, logs := newTestStore(t)
	left, right := openAgain(dir, logs), openAgain(dir, logs)
	var wait sync.WaitGroup
	errs := make(chan error, 64)
	for worker := range 8 {
		for side, store := range []*Store{left, right} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				text := fmt.Sprintf("w%d-%d@example.com----Fake-Pass-%d\n", worker, side, worker)
				if side == 0 {
					text += "shared@example.com----Fake-Pass-Shared\n"
				} else {
					text += "shared@example.com----  ----shared-r@example.com\n"
				}
				if _, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: text}}, AcknowledgeSuspect: true}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发导入失败: %v", err)
	}
	view, _ := openAgain(dir, logs).View(bg, "")
	if len(view.Profiles) != 17 {
		t.Fatalf("应有 17 条资料，得到 %d", len(view.Profiles))
	}
	if shared := reveal(t, left, "shared@example.com"); shared.Password != "Fake-Pass-Shared" || shared.RecoveryEmail != "shared-r@example.com" {
		t.Fatal("同一邮箱不同字段应为并集")
	}
	// revision 随另一个 Store 的写入改变
	before, _ := left.View(bg, "")
	importLines(t, right, "late@example.com----P")
	if after, _ := left.View(bg, before.Revision); after.Unchanged || after.Revision == before.Revision || len(after.Profiles) != 18 {
		t.Fatalf("另一个 Store 写过后应看到: %+v", after.Revision)
	}
	// 同时编辑同一组：一个成功、一个 409
	if _, err := left.ImportFamilies(bg, FamilyImportInput{Text: "g@example.com\ng1@example.com"}); err != nil {
		t.Fatal(err)
	}
	group, _ := left.View(bg, "")
	rev := group.Families[0].Rev
	results := make(chan error, 2)
	for index, store := range []*Store{left, right} {
		go func() {
			_, err := store.UpdateFamily(bg, group.Families[0].ID, FamilyUpdate{Rev: &rev, Name: strPtr(fmt.Sprintf("名%d", index))})
			results <- err
		}()
	}
	var failures []error
	for range 2 {
		if err := <-results; err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("应恰好一个 409: %v", failures)
	}
	assertCode(t, failures[0], 409, CodeConflict)
	// 同一个 Store 上 16 个协程并发读写
	var group16 sync.WaitGroup
	failed := make(chan error, 64)
	for index := range 16 {
		group16.Add(1)
		go func() {
			defer group16.Done()
			var err error
			switch index % 8 {
			case 0:
				_, err = left.View(bg, "")
			case 1:
				_, err = left.ImportProfiles(bg, ProfileImportInput{DryRun: true, Sources: []ImportSource{{Text: "a@example.com----P"}}})
			case 2:
				_, err = left.RevealProfile(bg, RevealInput{Email: "shared@example.com"})
			case 3:
				_, err = left.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: fmt.Sprintf("c%d@example.com----P", index)}}})
			case 4:
				_, err = left.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{fmt.Sprintf("w%d-0@example.com", index%8)}})
			case 5:
				_, err = left.ImportFamilies(bg, FamilyImportInput{Text: fmt.Sprintf("h%d@example.com\nh%d-1@example.com", index, index)})
			case 6:
				_, err = left.ExportProfiles(bg, ExportInput{All: true})
			default:
				_, err = left.ImportFamilies(bg, FamilyImportInput{DryRun: true, Text: "x@example.com\nx1@example.com"})
			}
			if err != nil {
				failed <- err
			}
		}()
	}
	done := make(chan struct{})
	go func() { group16.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("同一个 Store 并发读写超时")
	}
	close(failed)
	for err := range failed {
		t.Fatalf("并发读写失败: %v", err)
	}
}

// TestRevision since 等于当前 revision 时只回 unchanged；写入后 revision 改变；人工改了文件（write_id 不变）也能看出
func TestRevision(t *testing.T) {
	store, dir, _ := newTestStore(t)
	empty, _ := store.View(bg, "")
	if empty.Revision != "0.0" {
		t.Fatalf("没有文件时 = %s", empty.Revision)
	}
	if same, _ := store.View(bg, "0.0"); !same.Unchanged || same.Profiles != nil || same.Revision != "0.0" {
		t.Fatalf("since 相同 = %+v", same)
	}
	importLines(t, store, "a@example.com----P")
	view, _ := store.View(bg, "0.0")
	if view.Unchanged || view.Revision == "0.0" || !strings.HasSuffix(view.Revision, ".0") {
		t.Fatalf("写入后 = %s", view.Revision)
	}
	// 另一个 Store 从磁盘读到的 revision；人工改了文件而 write_id 没变，也要看出变化
	reader := openAgain(dir, &testLogs{})
	fromDisk, _ := reader.View(bg, "")
	if fromDisk.Revision != view.Revision {
		t.Fatalf("同一份文件的 revision 应一致: %s / %s", fromDisk.Revision, view.Revision)
	}
	path := filepath.Join(dir, "profiles.json")
	data := readFile(t, path)
	edited := bytes.Replace(data, []byte(`"password": "P"`), []byte(`"password": "Q"`), 1)
	if bytes.Equal(edited, data) {
		t.Fatal("没找到要改的字段")
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if again, _ := reader.View(bg, fromDisk.Revision); again.Unchanged {
		t.Fatal("人工改了文件应看出变化")
	}
	if again, _ := store.View(bg, view.Revision); again.Unchanged {
		t.Fatal("人工改了文件应看出变化")
	}
}

// TestMemoryOnly Open("") 的全部操作不碰磁盘
func TestMemoryOnly(t *testing.T) {
	cwd, _ := os.Getwd()
	before, _ := os.ReadDir(cwd)
	store := Open("", Options{})
	importLines(t, store, "a@example.com----P1----r@example.com")
	if _, err := store.UpdateProfile(bg, ProfileUpdate{Email: "a@example.com", Note: strPtr("n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "a@example.com\nb@example.com"}); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(bg, "")
	if err != nil || view.Persistent || view.Dir != "" || len(view.Profiles) != 1 || len(view.Families) != 1 || view.Revision == "0.0" {
		t.Fatalf("只在内存 = %+v %v", view, err)
	}
	if _, err := store.DissolveFamily(bg, view.Families[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartEmpty(bg, "profiles"); err == nil {
		t.Fatal("只在内存时没有备份，从空开始应 409")
	}
	after, _ := os.ReadDir(cwd)
	if len(before) != len(after) || fileExists(filepath.Join(cwd, "runtime")) {
		t.Fatal("只在内存时不应创建任何文件")
	}
	names := func(entries []os.DirEntry) []string {
		var list []string
		for _, entry := range entries {
			list = append(list, entry.Name())
		}
		return list
	}
	if !slices.Equal(names(before), names(after)) {
		t.Fatal("包目录不应有新文件")
	}
}

// TestReadFailureWarnOnceAcrossWrites 主文件读不出（权限）时交替“看一次、写一次”：写接口一律 409、不拿锁重读，WARN 只写一行，文件不变
func TestReadFailureWarnOnceAcrossWrites(t *testing.T) {
	writer, dir, logs := newTestStore(t)
	importLines(t, writer, "a@example.com----Fake-Pass-1")
	importLines(t, writer, "a@example.com----Fake-Pass-2")
	path := filepath.Join(dir, "profiles.json")
	// 新开一个 Store，让第一次 load 真的读盘（writer 已缓存正常内容）
	store := openAgain(dir, logs)
	before, bak := readFile(t, path), readFile(t, path+".bak")
	var mu sync.Mutex
	mainReads := 0
	store.readFile = func(name string) ([]byte, error) {
		if name == path {
			mu.Lock()
			mainReads++
			mu.Unlock()
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return os.ReadFile(name)
	}
	for round := range 3 {
		if view, err := store.View(bg, ""); err != nil || len(view.Errors) != 1 || view.Errors[0].Reason != problemRead {
			t.Fatalf("第 %d 轮 View err=%v errors=%+v", round+1, err, view.Errors)
		}
		mu.Lock()
		readsBefore := mainReads
		mu.Unlock()
		_, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Text: "b@example.com----Fake-Pass-3"}}})
		assertCode(t, err, 409, CodeUnreadable)
		_, err = store.UpdateProfile(bg, ProfileUpdate{Email: "a@example.com", Password: strPtr("Fake-Pass-4")})
		assertCode(t, err, 409, CodeUnreadable)
		_, err = store.DeleteProfiles(bg, ProfileDeleteInput{Emails: []string{"a@example.com"}})
		assertCode(t, err, 409, CodeUnreadable)
		mu.Lock()
		// 每个写接口前的 loadAll 读失败要重试读盘（修好权限后才能恢复），但 mutate 不再拿锁重读
		if mainReads-readsBefore != 3 {
			t.Fatalf("第 %d 轮三次写接口读了主文件 %d 次，应为 3 次（只有 loadAll）", round+1, mainReads-readsBefore)
		}
		mu.Unlock()
	}
	if warns, _ := logs.counts(); warns != 1 || strings.Contains(logs.all(), "Fake-Pass") {
		t.Fatalf("同一读失败只应写一行不含内容的 WARN: %s", logs.all())
	}
	if !bytes.Equal(readFile(t, path), before) || !bytes.Equal(readFile(t, path+".bak"), bak) {
		t.Fatal("读不出时主文件与 .bak 都不应改动")
	}
	if _, err := store.ImportFamilies(bg, FamilyImportInput{Text: "f@example.com\nf1@example.com"}); err != nil {
		t.Fatalf("另一个文件照常可写: %v", err)
	}
}
