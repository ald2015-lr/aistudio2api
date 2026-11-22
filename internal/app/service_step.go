package app

import (
	"time"
)

// 生成服务的启动与停止步骤（移植自上游 c77dd89 的服务端部分，步骤按本项目真实的 Start、Stop 重新映射）：
// 状态接口的 transition、transition_since 写出当前步骤与进入它的时刻，time 为生成状态的服务端时刻，管理页据此换算已用时间。
// 上游的 probe_proxies（启动时检测代理池）与 wait_camoufox（等待 Camoufox 下载）在本项目的 Start 里没有对应的步骤：
// IP 池的检测由管理页手动发起、不在启动路径上，Camoufox 不在启动时下载。
// 约束：启动步骤只在持有 lifecycleMu 且服务仍在启动时写入（enterLaunchStep），停止开始后不再改动；停止步骤由停止与启动失败的
// 清理写入（enterStopStep）；进入运行（finishModelLaunch）与清理结束时清除。步骤变化时推送一次状态快照（publishStatus）
const (
	// stepPrepareCatalog 为准备模型目录：对照账户池排好目录任务、从写盘缓存恢复目录，启动后台目录同步与登录复核
	stepPrepareCatalog = "prepare_catalog"
	// stepWaitCatalog 为没有目录缓存时等待首个账户读取模型目录
	stepWaitCatalog = "wait_catalog"
	// stepStartPrewarm 为开始后台预热 WAA Worker（首轮预热不等结果，服务随即进入运行）
	stepStartPrewarm = "start_prewarm"
	// stepCancelRequests 为停止在途请求
	stepCancelRequests = "cancel_requests"
	// stepWaitCatalogStop 为等待模型目录同步停止（最多 modelCatalogShutdownTimeout）
	stepWaitCatalogStop = "wait_catalog_stop"
	// stepCloseWorkers 为等待预热结束并关闭全部 WAA Worker
	stepCloseWorkers = "close_workers"
	// stepFlushWrites 为写出暂存的认证 Cookie、推迟的模型资格时间，以及坏号记忆、Drive 授权状态与今日发送数
	stepFlushWrites = "flush_writes"
)

// serviceStep 为生成服务的一个启动或停止步骤与进入它的时刻
type serviceStep struct {
	name  string
	since time.Time
}

// Step 返回生成服务正在进行的启动或停止步骤与进入它的时刻，没有步骤时 name 为空
func (service *trackedService) Step() (string, time.Time) {
	if step := service.step.Load(); step != nil {
		return step.name, step.since
	}
	return "", time.Time{}
}

// storeStep 记录进入步骤 name 的时刻，name 为空时清除步骤
func (service *trackedService) storeStep(name string) {
	if name == "" {
		service.step.Store(nil)
		return
	}
	service.step.Store(&serviceStep{name: name, since: time.Now()})
}

// enterLaunchStep 在生成服务仍在启动时进入启动步骤 name 并推送状态；停止开始后（状态已不是启动中）不再改动步骤
func (service *trackedService) enterLaunchStep(name string) {
	service.lifecycleMu.Lock()
	launching := service.state.Load() == serviceLaunching
	if launching {
		service.storeStep(name)
	}
	service.lifecycleMu.Unlock()
	if launching {
		service.publishStatus()
	}
}

// enterStopStep 进入停止步骤 name 并推送状态（停止与启动失败的清理）
func (service *trackedService) enterStopStep(name string) {
	service.storeStep(name)
	service.publishStatus()
}

// publishStatus 向管理页推送一次生成服务状态快照（newRuntime 挂上 runtimeAdmin.publishStatus；测试直接构造的服务不推送）
func (service *trackedService) publishStatus() {
	if publish := service.statusPublisher.Load(); publish != nil {
		(*publish)()
	}
}

// setStatusPublisher 挂上状态推送
func (service *trackedService) setStatusPublisher(publish func()) {
	service.statusPublisher.Store(&publish)
}
