package camoufoxnative

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const camoufoxRelease = "152.0.4-beta.29"

// camoufoxSHA256 为固定版本各平台发行包的 SHA-256。下载后先校验再解压执行：只依赖 HTTPS 时，
// 发行包被替换（发布账号被盗、中间人代理）会让服务直接执行被篡改的浏览器。升级 camoufoxRelease 时同步更新
var camoufoxSHA256 = map[string]string{
	"camoufox-152.0.4-beta.29-win.x86_64.zip": "b9ccdc298330e96f807999fe22aa212e9847fc9e210bc36b2e965b6cce03b25b",
	"camoufox-152.0.4-beta.29-lin.x86_64.zip": "1bea4b55a51c88e82dc7d426d9c75093d942d2afc8c911cb8fc78ebf723d686c",
	"camoufox-152.0.4-beta.29-lin.arm64.zip":  "c8844c03f3c233d6a466d59fd3f4e6f21470bd685e1b7cc82d650379a9b23e35",
	"camoufox-152.0.4-beta.29-mac.x86_64.zip": "eeada250746edefc5d7b63fc371d51ed3252b21134e971ac64ca590ff78e7bb7",
	"camoufox-152.0.4-beta.29-mac.arm64.zip":  "620d33289b5d52154cc7d92a9cca5915329aa6684105ab0f371d01480768eb53",
}

// installCamoufox 下载当前协议传输已对齐的 Camoufox 版本
func installCamoufox(ctx context.Context, executableName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	asset, err := camoufoxAssetName()
	if err != nil {
		return "", err
	}
	root, err := camoufoxInstallRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return "", fmt.Errorf("创建 Camoufox 目录: %w", err)
	}
	archive, err := os.CreateTemp(filepath.Dir(root), "camoufox-*.zip")
	if err != nil {
		return "", fmt.Errorf("创建 Camoufox 下载文件: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	url := fmt.Sprintf("https://github.com/daijro/camoufox/releases/download/v%s/%s", camoufoxRelease, asset)
	slog.Info("正在下载 Camoufox", "version", camoufoxRelease, "platform", runtime.GOOS+"/"+runtime.GOARCH)
	client := &http.Client{Timeout: 30 * time.Minute}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		archive.Close()
		return "", err
	}
	request.Header.Set("User-Agent", "AIStudio2API")
	response, err := client.Do(request)
	if err != nil {
		archive.Close()
		return "", fmt.Errorf("下载 Camoufox: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		archive.Close()
		return "", fmt.Errorf("下载 Camoufox: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > 0 {
		slog.Info("Camoufox 下载已开始", "size_mib", response.ContentLength/(1024*1024))
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(archive, hasher), contextReader{ctx: ctx, reader: response.Body})
	closeErr := response.Body.Close()
	archiveCloseErr := archive.Close()
	if copyErr != nil || closeErr != nil || archiveCloseErr != nil {
		return "", fmt.Errorf("保存 Camoufox: %w", firstError(copyErr, closeErr, archiveCloseErr))
	}
	if err := verifyCamoufoxArchive(asset, hex.EncodeToString(hasher.Sum(nil))); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(root), ".camoufox-stage-*")
	if err != nil {
		return "", fmt.Errorf("创建 Camoufox 临时目录: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := extractCamoufoxArchive(ctx, archivePath, staging); err != nil {
		return "", err
	}
	stagedExecutable := filepath.Join(staging, executableName)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stagedExecutable, 0o755); err != nil {
			return "", fmt.Errorf("设置 Camoufox 执行权限: %w", err)
		}
	}
	if _, err := validateCamoufoxExecutable(stagedExecutable); err != nil {
		return "", fmt.Errorf("校验 Camoufox 临时目录: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.RemoveAll(root); err != nil {
		return "", fmt.Errorf("清理旧 Camoufox 目录: %w", err)
	}
	if err := os.Rename(staging, root); err != nil {
		return "", fmt.Errorf("发布 Camoufox 目录: %w", err)
	}
	executable := filepath.Join(root, executableName)
	slog.Info("Camoufox 已就绪", "path", executable)
	return executable, nil
}

// verifyCamoufoxArchive 校验下载的发行包；没有固定校验值的平台（例如 32 位）记录警告后继续
func verifyCamoufoxArchive(asset string, actual string) error {
	expected, known := camoufoxSHA256[asset]
	if !known {
		slog.Warn("Camoufox 发行包没有固定校验值，跳过校验", "asset", asset, "sha256", actual)
		return nil
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("Camoufox 发行包校验失败（%s）：期望 SHA-256 %s，实际 %s；文件可能被篡改或下载不完整，已拒绝执行",
			asset, expected, actual)
	}
	slog.Info("Camoufox 发行包校验通过", "asset", asset)
	return nil
}

func camoufoxInstallRoot() (string, error) {
	root, err := filepath.Abs(filepath.Join("runtime", "camoufox"))
	if err != nil {
		return "", fmt.Errorf("定位 Camoufox 目录: %w", err)
	}
	return root, nil
}

func camoufoxAssetName() (string, error) {
	platform := map[string]string{"windows": "win", "linux": "lin", "darwin": "mac"}[runtime.GOOS]
	architecture := map[string]string{"amd64": "x86_64", "386": "i686", "arm64": "arm64"}[runtime.GOARCH]
	if platform == "" || architecture == "" || runtime.GOOS == "darwin" && runtime.GOARCH == "386" {
		return "", fmt.Errorf("Camoufox 没有 %s/%s 发行包", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("camoufox-%s-%s.%s.zip", camoufoxRelease, platform, architecture), nil
}

func extractCamoufoxArchive(ctx context.Context, archivePath string, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开 Camoufox 压缩包: %w", err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(entry.Name))
		relative, err := filepath.Rel(destination, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("Camoufox 压缩包包含无效路径 %q", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, entry.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entry.Mode())
		if err != nil {
			source.Close()
			return err
		}
		_, copyErr := io.Copy(targetFile, contextReader{ctx: ctx, reader: source})
		closeTargetErr := targetFile.Close()
		closeSourceErr := source.Close()
		if copyErr != nil || closeTargetErr != nil || closeSourceErr != nil {
			return fmt.Errorf("解压 Camoufox %s: %w", entry.Name, firstError(copyErr, closeTargetErr, closeSourceErr))
		}
	}
	return nil
}

// contextReader 在复制过程中传播装配取消
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read 在每个数据块前检查装配取消
func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func firstError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}
