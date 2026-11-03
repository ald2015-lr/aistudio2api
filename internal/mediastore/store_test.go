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
	"slices"
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

// TestExtensionInteractionMediaTable Omni（CreateInteractionStream）输出的媒体 MIME 枚举表（与官网前端相同，见 aistudio interactionMediaMIME）
// 解码出的写法都按已知扩展名保存、按标准 Content-Type 提供（video/mov 与 video/quicktime 同为 .mov 等），不存成 .bin 按下载提供；
// 裸 PCM、A-law、μ-law 没有文件头，仍为 .bin。各扩展名都在 extensionTypes 与文件名格式里
func TestExtensionInteractionMediaTable(t *testing.T) {
	hash := strings.Repeat("cd", 32)
	for mimeType, want := range map[string]struct{ extension, served string }{
		"image/png": {".png", "image/png"}, "image/jpeg": {".jpg", "image/jpeg"}, "image/webp": {".webp", "image/webp"},
		"image/heic": {".heic", "image/heic"}, "image/heif": {".heif", "image/heif"}, "image/gif": {".gif", "image/gif"},
		"image/bmp": {".bmp", "image/bmp"}, "image/tiff": {".tiff", "image/tiff"},
		"audio/wav": {".wav", "audio/wav"}, "audio/mp3": {".mp3", "audio/mpeg"}, "audio/aiff": {".aiff", "audio/aiff"},
		"audio/aac": {".aac", "audio/aac"}, "audio/ogg": {".ogg", "audio/ogg"}, "audio/flac": {".flac", "audio/flac"},
		"audio/mpeg": {".mp3", "audio/mpeg"}, "audio/m4a": {".m4a", "audio/mp4"}, "audio/opus": {".opus", "audio/opus"},
		"audio/l16": {".bin", "application/octet-stream"}, "audio/s16le": {".bin", "application/octet-stream"},
		"audio/alaw": {".bin", "application/octet-stream"}, "audio/mulaw": {".bin", "application/octet-stream"}, "application/pdf": {".pdf", "application/pdf"},
		"video/mp4": {".mp4", "video/mp4"}, "video/mpeg": {".mpg", "video/mpeg"}, "video/mpg": {".mpg", "video/mpeg"},
		"video/mov": {".mov", "video/quicktime"}, "video/avi": {".avi", "video/x-msvideo"}, "video/x-flv": {".flv", "video/x-flv"},
		"video/webm": {".webm", "video/webm"}, "video/wmv": {".wmv", "video/x-ms-wmv"}, "video/3gpp": {".3gp", "video/3gpp"},
	} {
		extension := Extension(mimeType)
		name := hash + extension
		if extension != want.extension || !ValidName(name) || ContentType(name) != want.served || !validTempName("."+name+".123.tmp") {
			t.Fatalf("%s: 扩展名 %q（期望 %q），ValidName=%v，提供为 %q（期望 %q）", mimeType, extension, want.extension, ValidName(name), ContentType(name), want.served)
		}
	}
	for mimeType, extension := range typeExtensions {
		if _, ok := extensionTypes[extension]; !ok || !ValidName(hash+extension) {
			t.Fatalf("%s 的扩展名 %s 应在 extensionTypes 与文件名格式里", mimeType, extension)
		}
	}
	for extension := range extensionTypes {
		if !ValidName(hash + extension) {
			t.Fatalf("扩展名 %s 应符合文件名格式", extension)
		}
	}
}

// TestExtensions 按提供时的 Content-Type 列出扩展名（协议层据此拼回传恢复的链接正则）：图片含 Omni 输出枚举解码出的 HEIC、HEIF、BMP、TIFF，
// 视频含 MPEG、AVI、FLV、WMV、3GP；与 extensionTypes 一一对应（不多不少），较长的在前，没有一项是排在它前面的另一项的前缀
func TestExtensions(t *testing.T) {
	images := Extensions("image/")
	if want := []string{"heic", "heif", "tiff", "webp", "bmp", "gif", "jpg", "png"}; strings.Join(images, "|") != strings.Join(want, "|") {
		t.Fatalf("图片扩展名 = %v，期望 %v", images, want)
	}
	videos := Extensions("video/")
	if want := []string{"webm", "3gp", "avi", "flv", "mov", "mp4", "mpg", "wmv"}; strings.Join(videos, "|") != strings.Join(want, "|") {
		t.Fatalf("视频扩展名 = %v，期望 %v", videos, want)
	}
	visual := Extensions("image/", "video/")
	if len(visual) != len(images)+len(videos) {
		t.Fatalf("图片加视频 = %v", visual)
	}
	hash := strings.Repeat("ab", 32)
	counted := 0
	for extension, contentType := range extensionTypes {
		if !strings.HasPrefix(contentType, "image/") && !strings.HasPrefix(contentType, "video/") {
			continue
		}
		counted++
		name := hash + extension
		if !ValidName(name) || !slices.Contains(visual, strings.TrimPrefix(extension, ".")) {
			t.Fatalf("%s（%s）应在图片或视频扩展名里", extension, contentType)
		}
		if IsImage(name) != slices.Contains(images, strings.TrimPrefix(extension, ".")) {
			t.Fatalf("%s: IsImage 与 Extensions(image/) 不一致", extension)
		}
	}
	if counted != len(visual) {
		t.Fatalf("extensionTypes 里的图片与视频有 %d 种，Extensions 列出 %d 种", counted, len(visual))
	}
	for index, extension := range visual {
		for _, earlier := range visual[:index] {
			if strings.HasPrefix(extension, earlier) {
				t.Fatalf("%s 排在 %s 后面，正则分支会先匹配到较短的前缀", extension, earlier)
			}
		}
	}
	if audio := Extensions("audio/"); slices.Contains(audio, "mp4") || !slices.Contains(audio, "m4a") || len(Extensions()) != 0 {
		t.Fatalf("音频扩展名 = %v，没有前缀时 = %v", audio, Extensions())
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
