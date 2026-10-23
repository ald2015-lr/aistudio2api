package app

import (
	"context"
	"strings"
)

// Worker 冷启动的来源：写进“WAA Worker 就绪”日志的“来源=”，冷启动闸门按它区分补覆盖（见 egress_guard.go 的 coldStartPath）
const (
	workerStartPrewarm     = "预热"
	workerStartReplacement = "替补"
	workerStartExpand      = "扩容"
	workerStartOnDemand    = "按需"
	workerStartCoverage    = "补覆盖"
	workerStartReopen      = "重开"
)

type workerStartSourceKey struct{}

// withWorkerStartSource 给冷启动的 ctx 标上来源（预热|替补|扩容|按需|补覆盖|重开）；source 为空时原样返回
func withWorkerStartSource(ctx context.Context, source string) context.Context {
	source = strings.TrimSpace(source)
	if ctx == nil || source == "" {
		return ctx
	}
	return context.WithValue(ctx, workerStartSourceKey{}, source)
}

// workerStartSourceFrom 返回 ctx 上的冷启动来源，没有标记时为空
func workerStartSourceFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	source, _ := ctx.Value(workerStartSourceKey{}).(string)
	return source
}
