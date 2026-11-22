package app

import (
	"context"
	"errors"
)

// 停止或重启生成服务时进行中的请求（参照上游 Mag1cFall/AIStudio2API 792d67a，按本项目的请求上下文重写）：原先它们和管理端取消一样
// 以 context.Canceled 结束，请求日志记“取消”、OpenAI 错误码为 request_canceled，分不清是服务停止还是取消。现在请求上下文记下是服务停止
// 结束的（生成请求见 generateContext.stopService，其他数据请求见 errServiceStoppedCause），上下文的 Err() 仍是 context.Canceled：
// 换号循环里照旧按取消处理（不换号、不冷却、不按 MODEL_FALLBACKS 改用其他模型），只在请求的最终出口（generateForModel 结束、
// forwardEvents 收尾与错误事件、各数据接口返回）经 endedRequestError 换成 serviceStoppedError（503，错误码 service_stopped，对外仍是
// 官方的不可用文案）。客户端先断开、期限先到期、管理端先取消的请求不受影响，照旧按原来的原因结束。
//
// 请求上下文的取消函数（生成与转写登记在请求登记表里，管理端取消与停止时的 requests.cancelAll 调用它）先经 watchServiceStop 返回的
// detach 解除服务停止的登记：数据上下文已经结束、登记的回调已经触发时同样按服务停止结束，再按原逻辑取消。
// 停止时 Stop 先 dataCancel()、后台的 finishStop 再 cancelAll：dataCancel 只在新 goroutine 里调用登记的回调，cancelAll 几乎总是先到，
// 原先它按普通取消结束请求，回调随后看到已结束什么也不做，进行中的请求（含排队等首轮预热的）被记成取消（请求登记为取消、访问日志 499），
// 不是 503 service_stopped。数据上下文只在停止、重启与关闭时结束，这样判定只是把“回调先到”的次序固定下来，不产生新的结局：
// 服务运行中管理端单独取消照旧是取消（两处同时调用同一个取消函数也是，见 watchServiceStop），父上下文（客户端）已经结束的照旧按
// 父上下文的原因（见 generateContext.stopService），已经结束的请求再调用取消函数不改结局

// errServiceStoppedCause 为生成服务停止或重启时取消数据请求（CountTokens、视频、文件、转写、Live）的原因（context.Cause）
var errServiceStoppedCause = errors.New("生成服务已停止，进行中的请求被取消")

// watchServiceStop 在数据上下文 data 结束（生成服务停止、重启或关闭）时调用 stop（在新 goroutine 里，见 context.AfterFunc），
// 返回请求上下文的取消函数开头调用的 detach：解除这次登记，并报告这次取消要不要按服务停止结束。
//
// 解除失败（context.AfterFunc 返回的 stop 为 false）有两种情况：一是数据上下文已经结束、登记的回调已经触发，按服务停止结束；
// 二是同一个取消函数已经被调用过一次、解除了登记，还没结束请求，第二次调用就到了（服务运行中两处同时取消：管理页连点、两个管理页
// 同时取消、管理端取消与降级判定拒绝撞在一起），这时不是服务停止，按原逻辑取消。两者按 data.Err() 区分：cancelCtx 先记下 Err()
// 再触发子上下文的回调，回调已触发时 data.Err() 一定不为 nil；后一种发生在服务运行中，data.Err() 为 nil。原先只看解除失败，
// 后一种被记成服务停止（503 service_stopped、请求登记为失败），不是取消
func watchServiceStop(data context.Context, stop func()) (detach func() bool) {
	stopData := context.AfterFunc(data, stop)
	return func() bool {
		return !stopData() && data.Err() != nil
	}
}

// serviceStoppedRequest 判断请求上下文是不是被生成服务停止或重启结束的
func serviceStoppedRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if phase := generateContextFrom(ctx); phase != nil {
		return phase.stoppedByService()
	}
	return errors.Is(context.Cause(ctx), errServiceStoppedCause)
}

// endedRequestError 在请求的最终出口把生成服务停止造成的失败换成 serviceStoppedError；没有失败、或请求不是被服务停止结束的，原样返回 err。
// 只能用在最终出口：换号循环里换掉 context.Canceled 会让它被当成可换号、可冷却、可改用其他模型的失败
func endedRequestError(ctx context.Context, err error) error {
	if err == nil || !serviceStoppedRequest(ctx) {
		return err
	}
	var stopped *serviceStoppedError
	if errors.As(err, &stopped) {
		return err
	}
	return &serviceStoppedError{}
}
