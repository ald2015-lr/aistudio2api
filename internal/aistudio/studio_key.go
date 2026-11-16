package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// StudioKeyDriver 为能打开 AI Studio API Keys 页面读取（必要时创建；只读时只读）API Key 的登录驱动（见 camoufoxnative.FetchStudioKey）
type StudioKeyDriver interface {
	FetchStudioKey(context.Context, IsolatedLoginRequest, StorageState, camoufoxnative.StudioKeyOptions) (camoufoxnative.StudioKeyResult, error)
}

var _ StudioKeyDriver = (*NativeLoginDriver)(nil)

// FetchStudioKey 用无头隔离 Camoufox 与账户已保存的登录态打开 API Keys 页面，从页面自己的响应里读取 Key（options.ReadOnly 时只读，
// 补已保存 Key 的项目信息）；与 Verify 同样的启动方式（同一份指纹、同一个代理中转）。错误文案不含 Key
func (driver *NativeLoginDriver) FetchStudioKey(
	ctx context.Context, request IsolatedLoginRequest, state StorageState, options camoufoxnative.StudioKeyOptions,
) (camoufoxnative.StudioKeyResult, error) {
	if driver == nil {
		return camoufoxnative.StudioKeyResult{}, errors.New("纯 Go Camoufox 登录驱动未初始化")
	}
	if err := state.Validate(); err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, fmt.Errorf("编码登录状态: %w", err)
	}
	loginOptions, relay, err := driver.relayOptions(request, "获取Key", false)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, err
	}
	defer relay.Release()
	startedAt := time.Now()
	result, err := camoufoxnative.FetchStudioKey(ctx, loginOptions, encoded, options)
	if err != nil {
		return camoufoxnative.StudioKeyResult{}, relay.WrapFailure(err, startedAt, proxyrelay.GoogleTarget)
	}
	return result, nil
}
