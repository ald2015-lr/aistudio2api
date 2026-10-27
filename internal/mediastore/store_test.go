package mediastore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExtension 媒体类型对应扩展名：不区分大小写、忽略参数，未知类型（含 PCM、SVG）保存为 .bin
func TestExtension(t *testing.T) {
	for mimeType, want := range map[string]string{
		"video/mp4": ".mp4", "IMAGE/JPEG": ".jpg", "image/png": ".png", "image/webp": ".webp", "audio/wav": ".wav",
		"audio/mpeg": ".mp3", "audio/L16;codec=pcm;rate=24000": ".bin", "image/svg+xml": ".bin", "text/html": ".bin", "": ".bin",
		"video/webm; codecs=vp9": ".webm",
	} {
		if got := Extension(mimeType); got != want {
			t.Fatalf("Extension(%q) = %q，期望 %q", mimeType, got, want)
		}
	}
	if ContentType("0000000000000000000000000000000000000000000000000000000000000000.bin") != "application/octet-stream" {
		t.Fatal(".bin 应按 application/octet-stream 提供")
	}
}

// TestValidName 只接受 64 位小写十六进制加已知扩展名
func TestValidName(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for name, want := range map[string]bool{
		hash + ".mp4": true, hash + ".png": true, hash + ".bin": true,
		strings.ToUpper(hash) + ".mp4": false, hash + ".exe": false, hash + ".MP4": false, hash: false,
		hash[:63] + ".mp4": false, "../" + hash + ".mp4": false, hash + ".mp4/x": false, "." + hash + ".mp4.1.tmp": false,
		"a/" + hash + ".mp4": false, hash + ".mp4\x00": false,
	} {
		if ValidName(name) != want {
			t.Fatalf("ValidName(%q) 应为 %v", name, want)
		}
	}
}

// TestSaveByContent 按内容命名保存，重复保存同一内容不再写入、只更新修改时间；文件权限 0644，不留临时文件
func TestSaveByContent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "media")
	store := New(dir)
	data := []byte("\x00\x00\x00\x18ftypmp42 video bytes")
	name, err := store.Save(data, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if name != hex.EncodeToString(sum[:])+".mp4" || name != FileName(data, "video/mp4") {
		t.Fatalf("文件名 = %q", name)
	}
	saved, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("保存的内容不一致: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, name))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("权限 = %v", info.Mode().Perm())
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
		t.Fatal(err)
	}
	again, err := store.Save(data, "video/mp4")
	if err != nil || again != name {
		t.Fatalf("再次保存 = %q %v", again, err)
	}
	info, _ = os.Stat(filepath.Join(dir, name))
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("重复保存应更新修改时间，得到 %s", info.ModTime())
	}
	if _, err := store.Save(nil, "video/mp4"); err == nil {
		t.Fatal("空内容应报错")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("目录里只应有一个文件，得到 %d 个", len(entries))
	}
}

// TestSaveReplacesFileInsteadOfRewriting 保存靠改名换上一个新文件，不在原文件上截断重写：目标位置先有一个大小不同的旧文件
// （例如另一个请求写了一半的内容）并被打开着，保存后旧句柄读到的仍是旧内容，目标换成了另一个文件，不留临时文件。
// 直接写目标文件时旧句柄会读到新内容或被截断的内容，正在读取的请求就会拿到半个文件
func TestSaveReplacesFileInsteadOfRewriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上目标文件被打开时不能改名，由 Save 的回退分支处理")
	}
	dir := t.TempDir()
	store := New(dir)
	data := bytes.Repeat([]byte("frame"), 1000)
	path := filepath.Join(dir, FileName(data, "video/mp4"))
	if err := os.WriteFile(path, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	name, err := store.Save(data, "video/mp4")
	if err != nil || filepath.Join(dir, name) != path {
		t.Fatalf("保存 = %q %v", name, err)
	}
	previous := make([]byte, 64)
	count, err := old.ReadAt(previous, 0)
	if err != nil && count == 0 || string(previous[:count]) != "partial" {
		t.Fatalf("旧句柄应仍读到旧内容，读到 %q（%v）", previous[:count], err)
	}
	newInfo, err := os.Lstat(path)
	if err != nil || os.SameFile(oldInfo, newInfo) {
		t.Fatalf("保存应换上新文件，而不是在原文件上重写: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("保存后的内容不完整: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("目录里应只有媒体文件，得到 %d 个", len(entries))
	}
}

// TestSaveConcurrentSameContent 并发保存同一内容：都得到同一文件名，文件完整，不留临时文件
func TestSaveConcurrentSameContent(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	data := bytes.Repeat([]byte("frame"), 200_000)
	var wait sync.WaitGroup
	names := make([]string, 16)
	errs := make([]error, 16)
	for index := range names {
		wait.Add(1)
		go func() {
			defer wait.Done()
			names[index], errs[index] = store.Save(data, "video/mp4")
		}()
	}
	wait.Wait()
	for index := range names {
		if errs[index] != nil || names[index] != names[0] {
			t.Fatalf("第 %d 次保存 = %q %v", index, names[index], errs[index])
		}
	}
	saved, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("并发保存后的内容不完整: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("目录里应只有媒体文件，得到 %v", names)
	}
}

// TestOpenOnlyRegularFiles Open 与 Read 只处理名字合法的普通文件：目录、符号链接与不合法的名字一律当作不存在；超过上限的文件不读
func TestOpenOnlyRegularFiles(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	name, err := store.Save([]byte("png bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	file, info, err := store.Open(name)
	if err != nil || info.Size() != int64(len("png bytes")) {
		t.Fatalf("打开保存的文件: %v", err)
	}
	file.Close()
	if _, _, err := store.Open("../" + name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("不合法的名字应当作不存在: %v", err)
	}
	directory := strings.Repeat("cd", 32) + ".mp4"
	if err := os.Mkdir(filepath.Join(dir, directory), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(directory); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("目录应当作不存在: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := strings.Repeat("ef", 32) + ".png"
	if err := os.Symlink(outside, filepath.Join(dir, link)); err == nil {
		if _, _, err := store.Open(link); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("符号链接应当作不存在: %v", err)
		}
	}
	if data, err := store.Read(name, 1024); err != nil || string(data) != "png bytes" {
		t.Fatalf("Read = %q %v", data, err)
	}
	if _, err := store.Read(name, 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过上限应返回 ErrTooLarge: %v", err)
	}
	if _, err := New(filepath.Join(dir, "missing")).Read(name, 1024); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("目录不存在时应返回不存在: %v", err)
	}
}
