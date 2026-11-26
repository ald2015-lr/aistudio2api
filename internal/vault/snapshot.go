package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 快照：批量操作（资料确认导入、资料删除、家庭组确认录入、解散）确实要改动一个已存在的主文件时，在写 .bak 之前把旧版原样字节另存一份，
// 每个文件保留最近 MaxSnapshots 份。轮转只删除名字匹配 snapshotPattern 的文件；“从空开始”移过去的 …-before-start-empty.json 与用户自己放的文件不碰
var (
	snapshotPattern = regexp.MustCompile(`^(profiles|families)-\d{8}T\d{6}\.\d{3}Z-[0-9a-f]{12}\.json$`)
	hexIDPattern    = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

const snapshotTimeLayout = "20060102T150405.000Z"

// snapshotID 为快照名里被保存那一版的 write_id；文件里没有合格的 write_id（人工编辑过）时用内容标记
func snapshotID(writeID string, data []byte) string {
	if hexIDPattern.MatchString(writeID) {
		return writeID
	}
	return hashToken(data)
}

// uniquePath 返回 snapshots 目录下不存在的文件名：同一毫秒里已有同名文件时往后挪一毫秒
func (store *Store) uniquePath(format func(time.Time) string) string {
	at := store.now().UTC()
	for {
		path := filepath.Join(store.snapshotDir(), format(at))
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return path
		}
		at = at.Add(time.Millisecond)
	}
}

func (store *Store) writeSnapshot(name string, data []byte, id string) (string, error) {
	directory := store.snapshotDir()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return directory, err
	}
	path := store.uniquePath(func(at time.Time) string {
		return fmt.Sprintf("%s-%s-%s.json", name, at.Format(snapshotTimeLayout), id)
	})
	if err := atomicWrite(path, data, 0o600); err != nil {
		return path, err
	}
	store.rotateSnapshots(name)
	return path, nil
}

// rotateSnapshots 每个文件只保留最新的 MaxSnapshots 份（按名字即时间排序）；删除失败的留到下一次
func (store *Store) rotateSnapshots(name string) {
	entries, err := os.ReadDir(store.snapshotDir())
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), name+"-") && snapshotPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for len(names) > MaxSnapshots {
		_ = os.Remove(filepath.Join(store.snapshotDir(), names[0]))
		names = names[1:]
	}
}

// startEmptyLocked 为“从空开始”（调用方持有 Store.mu 与排他文件锁）：只在主文件缺失而有备份时可用；
// 把 .bak（如有）移到 snapshots/<file>-<时间>-before-start-empty.json（不参与轮转、从不自动删除），写一个空的主文件
func (store *Store) startEmptyLocked(name string, encodeEmpty func(string, time.Time) []byte) (string, error) {
	path := store.path(name)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return "", conflict("状态已变化（主文件已经存在或读不出），请重新读取")
	}
	backup := store.backupState(name)
	if !backup.present() {
		return "", conflict("状态已变化（已经没有备份），请重新读取")
	}
	moved := ""
	if backup.bakExists {
		if err := os.MkdirAll(store.snapshotDir(), 0o700); err != nil {
			return "", store.writeFailed(labelOf(name), store.snapshotDir(), err)
		}
		target := store.uniquePath(func(at time.Time) string {
			return fmt.Sprintf("%s-%s-before-start-empty.json", name, at.Format(snapshotTimeLayout))
		})
		if err := os.Rename(backup.bakPath, target); err != nil {
			return "", store.writeFailed(labelOf(name), backup.bakPath, err)
		}
		moved = target
	}
	if err := atomicWrite(path, encodeEmpty(store.newHexID(), store.now()), 0o600); err != nil {
		return moved, store.writeFailed(labelOf(name), path, err)
	}
	return moved, nil
}

func labelOf(name string) string {
	if name == fileFamilies {
		return "家庭组"
	}
	return "账号资料"
}
