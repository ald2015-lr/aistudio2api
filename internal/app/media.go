package app

import (
	"path/filepath"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// mediaDir 为生成媒体的保存目录（MEDIA_OUTPUT=url），与 requests.db 同在工作目录下的 runtime/
var mediaDir = filepath.Join("runtime", "media")

// mediaSettings 返回生效的媒体设置；还没有应用过设置（测试直接构造的管理器）时为默认设置
func (manager *runtimeManager) mediaSettings() config.MediaSettings {
	if settings := manager.media.Load(); settings != nil {
		return *settings
	}
	return config.DefaultMediaSettings()
}

// mediaLinks 返回媒体输出是否使用本服务链接与设置的服务地址，公开 API 每次请求读取，保存配置后立即生效
func (manager *runtimeManager) mediaLinks() (bool, string) {
	settings := manager.mediaSettings().Normalized()
	return settings.LinkMode(), settings.BaseURL
}

// mediaCleanupSettings 返回媒体目录清理使用的保存期限与总量上限
func (manager *runtimeManager) mediaCleanupSettings() (time.Duration, int64) {
	settings := manager.mediaSettings()
	return settings.Retention, settings.MaxBytes
}

// applyMediaSettings 立即切换媒体设置：之后进入的请求按新的输出方式与链接地址输出，进行中的请求不受影响；
// 保存期限或总量上限变化时立即清理一次。设置没有变化时什么也不做
func (manager *runtimeManager) applyMediaSettings(settings config.MediaSettings) {
	settings = settings.Normalized()
	previous := manager.media.Swap(&settings)
	if previous == nil || previous.Equal(settings) {
		return
	}
	manager.requests.log("service", "INFO", "媒体输出设置已更新并立即生效 | "+settings.Summary())
	if previous.Retention != settings.Retention || previous.MaxBytes != settings.MaxBytes {
		manager.mediaCleaner.Kick()
	}
}

// newMediaCleaner 创建媒体目录的清理器：每次清理读取生效的设置，日志写进管理页与控制台
func newMediaCleaner(manager *runtimeManager) *mediastore.Cleaner {
	return mediastore.NewCleaner(mediaDir, manager.mediaCleanupSettings, func(level string, message string) {
		manager.requests.log("service", level, message)
	})
}
