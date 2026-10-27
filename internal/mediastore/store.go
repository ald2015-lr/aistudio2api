// Package mediastore 按内容保存生成的媒体（MEDIA_OUTPUT=url），并按保存期限与总量上限清理。
// 文件名为内容的 SHA-256（64 位小写十六进制）加按媒体类型确定的扩展名，同一内容只保存一份；
// 读取、提供与清理都只处理名字符合这个格式的普通文件，目录里的其他文件一律不碰
package mediastore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// extensionTypes 为保存文件的扩展名与提供时的 Content-Type；不在 typeExtensions 里的媒体类型保存为 .bin，按下载提供
var extensionTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".flac": "audio/flac",
	".bin":  "application/octet-stream",
}

// typeExtensions 为媒体类型（不含参数、小写）对应的扩展名
var typeExtensions = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/jpg":       ".jpg",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
	"audio/mpeg":      ".mp3",
	"audio/mp3":       ".mp3",
	"audio/wav":       ".wav",
	"audio/x-wav":     ".wav",
	"audio/wave":      ".wav",
	"audio/vnd.wave":  ".wav",
	"audio/ogg":       ".ogg",
	"audio/mp4":       ".m4a",
	"audio/m4a":       ".m4a",
	"audio/x-m4a":     ".m4a",
	"audio/aac":       ".aac",
	"audio/flac":      ".flac",
	"audio/x-flac":    ".flac",
}

// namePattern 为保存文件的名字：64 位小写十六进制加已知扩展名，不含路径分隔符与 ..
var namePattern = regexp.MustCompile(`^[0-9a-f]{64}\.(png|jpg|webp|gif|mp4|webm|mov|mp3|wav|ogg|m4a|aac|flac|bin)$`)

// tempPattern 为写入中的临时文件名（os.CreateTemp 的随机部分为十进制数字）：.<文件名>.<随机数>.tmp
var tempPattern = regexp.MustCompile(`^\.[0-9a-f]{64}\.(png|jpg|webp|gif|mp4|webm|mov|mp3|wav|ogg|m4a|aac|flac|bin)\.[0-9]+\.tmp$`)

// ValidName 判断 name 是否为保存文件的名字
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// validTempName 判断 name 是否为写入中的临时文件名
func validTempName(name string) bool {
	return tempPattern.MatchString(name)
}

// Extension 返回媒体类型保存时的扩展名；类型带参数（如 audio/l16;rate=24000）时按不含参数的类型判断，未知类型为 .bin
func Extension(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		mediaType, _, _ = strings.Cut(mimeType, ";")
	}
	if extension, ok := typeExtensions[strings.ToLower(strings.TrimSpace(mediaType))]; ok {
		return extension
	}
	return ".bin"
}

// ContentType 返回保存文件提供时的 Content-Type；name 必须已通过 ValidName
func ContentType(name string) string {
	if contentType, ok := extensionTypes[filepath.Ext(name)]; ok {
		return contentType
	}
	return "application/octet-stream"
}

// IsImage 判断保存文件是否为图片
func IsImage(name string) bool {
	return strings.HasPrefix(ContentType(name), "image/")
}

// FileName 返回内容保存时的文件名
func FileName(data []byte, mimeType string) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + Extension(mimeType)
}

// Hash 返回文件名中的内容哈希（不含扩展名）；name 必须已通过 ValidName
func Hash(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// Store 为媒体目录
type Store struct {
	dir string
}

// New 返回目录 dir 的媒体存储；目录在第一次保存时创建
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Dir 返回媒体目录
func (store *Store) Dir() string {
	return store.dir
}

// Save 按内容保存 data 并返回文件名。先写入同目录的临时文件再改名：并发请求生成同一内容时各写各的临时文件，
// 读取方只会看到不存在或完整的文件，不会读到写了一半的文件。同名文件已存在且大小一致时不再写入，只把修改时间改为现在，
// 让重新生成的相同内容重新计算保存期限
func (store *Store) Save(data []byte, mimeType string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("媒体内容为空")
	}
	name := FileName(data, mimeType)
	path := filepath.Join(store.dir, name)
	size := int64(len(data))
	if store.present(path, size) {
		now := time.Now()
		if err := os.Chtimes(path, now, now); err == nil {
			return name, nil
		}
		// 刚好被清理掉时照常重新写入
	}
	if err := os.MkdirAll(store.dir, 0o755); err != nil {
		return "", fmt.Errorf("创建媒体目录: %w", err)
	}
	temp, err := os.CreateTemp(store.dir, "."+name+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("创建媒体临时文件: %w", err)
	}
	tempPath := temp.Name()
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("写入媒体文件: %w", err)
	}
	// os.CreateTemp 建出的文件只有属主可读，/media/ 由同一进程读取，这里与其他运行时文件一样改为 0644
	if err := os.Chmod(tempPath, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("设置媒体文件权限: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		// 另一个请求已写好同一内容（Windows 上目标正被读取时改名会失败）
		if store.present(path, size) {
			return name, nil
		}
		return "", fmt.Errorf("保存媒体文件: %w", err)
	}
	return name, nil
}

// present 判断 path 是大小为 size 的普通文件（不跟随符号链接）
func (store *Store) present(path string, size int64) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}

// Open 打开名字合法的普通文件；名字不合法、不存在或不是普通文件（目录、符号链接等）时返回 fs.ErrNotExist
func (store *Store) Open(name string) (*os.File, fs.FileInfo, error) {
	if !ValidName(name) {
		return nil, nil, fs.ErrNotExist
	}
	path := filepath.Join(store.dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fs.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	// 以打开的文件为准再确认一次：Lstat 与 Open 之间被换成其他文件（符号链接、目录）时不提供
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, fs.ErrNotExist
	}
	return file, opened, nil
}

// Read 读取名字合法的普通文件；文件大于 limit 字节时返回 ErrTooLarge，不读取内容
func (store *Store) Read(name string, limit int64) ([]byte, error) {
	file, info, err := store.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	data := make([]byte, info.Size())
	if _, err := file.ReadAt(data, 0); err != nil {
		return nil, err
	}
	return data, nil
}

// ErrTooLarge 表示文件超过读取上限
var ErrTooLarge = errors.New("媒体文件超过读取上限")
