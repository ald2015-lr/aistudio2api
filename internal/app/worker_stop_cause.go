package app

// Worker 停止与就绪的起因（参照上游 Mag1cFall/AIStudio2API c77dd89 的 trigger，按本项目的调用方与文本日志格式写）。
//
// 停止的起因写进“WAA Worker 停止”“WAA Worker 已停止”“WAA Worker 停止失败”的“起因=”（追加在原有键之后，“停止失败”写在“错误=”之前），
// 用来分清反复冷启动里关掉的 Worker 是被谁关的；没有起因的调用（测试）不写这个键
const (
	// workerStopIdleReclaim 为超出常驻数、空闲超时的 Worker 被空闲回收（reapIdleWorkerIn）
	workerStopIdleReclaim = "空闲回收"
	// workerStopReplace 为按需启动换掉同分区的一个空闲 Worker（ensureWorker 的按需替换）
	workerStopReplace = "替换"
	// workerStopCoolingRetire 为长时间冷却的死槽位被淘汰（worker_retire.go 的冷却淘汰）
	workerStopCoolingRetire = "冷却淘汰"
	// workerStopUnschedulable 为账户不可调度（需要登录、组织停用、需要验证账号等）后释放驻留 Worker（worker_retire.go）
	workerStopUnschedulable = "不可调度释放"
	// workerStopReclaimPlaceholder 为请求现场回收冷却中占着槽位的 Worker（worker_retire.go 的回收冷却占位）
	workerStopReclaimPlaceholder = "回收冷却占位"
	// workerStopBalanced 为平均调用轮换换下今日发送多的号（balanced_rotation.go）
	workerStopBalanced = "平均调用轮换"
	// workerStopCoolingSweep 为冷却清扫关闭全局冷却的空闲 Worker（prewarm_keeper.go 的 sweepDeadWorkers）
	workerStopCoolingSweep = "冷却清扫"
	// workerStopCoolingRotation 为冷却轮换换下热门模型长时间冷却的 Worker（prewarm_keeper.go 的 rotateCooledWorkers）
	workerStopCoolingRotation = "冷却轮换"
	// workerStopRecovery 为 Worker 失败、被替换或 WAA 运行态失败后的后台关闭（ResetIfGeneration，见 worker_reset.go）
	workerStopRecovery = "失败恢复"
	// workerStopService 为生成服务停止、重启或启动失败时关闭全部 Worker（ResetAll）
	workerStopService = "服务停止"
	// workerStopDisable 为管理页停用账户的收尾
	workerStopDisable = "停用"
	// workerStopDelete 为删除账户
	workerStopDelete = "删除"
	// workerStopLogin 为管理页浏览器登录前关闭账户的 Worker
	workerStopLogin = "登录"
	// workerStopReimport 为 Chrome 就地重新导入
	workerStopReimport = "重新导入"
	// workerStopCredentials 为磁盘上的认证文件更新后重启或重新载入
	workerStopCredentials = "认证文件更新"
	// workerStopAuthRefresh 为请求遇到登录失效、用 Chrome 续签材料刷新 Cookie 后重置协议运行时（auth_retry.go 的认证续签）
	workerStopAuthRefresh = "认证续签"
	// workerStopDetach 为账户目录已不在、移出账户池
	workerStopDetach = "账户移出"
	// workerStopExtraRelease 为补充池 Worker 的账号已不是补充池成员、家分区又没有空位时空闲关闭（见 extra_pool.go 的 extraPass）
	workerStopExtraRelease = "补充池释放"
)

// 就绪的起因写进“WAA Worker 就绪”的“起因=”（追加在“来源=”之后）：新 Worker 的槽位是怎么来的。来源说明是谁要启动（预热、按需等），
// 起因说明它占了空槽位、换掉了别的账户的空闲 Worker，还是在本账户原来的槽位上重开
const (
	// workerReadyFreeSlot 为分区有空槽位（含按需替换等待期间空出的槽位）
	workerReadyFreeSlot = "空槽位"
	// workerReadyReplace 为换掉同分区另一个账户的空闲 Worker（与停止的“起因=替换”成对出现）
	workerReadyReplace = "替换"
	// workerReadyOwnSlot 为本账户已占着槽位（旧 Worker 失败、页面模型变了等），新 Worker 发布时关掉自己的旧实例
	workerReadyOwnSlot = "原槽位"
)

// workerCauseField 返回追加在日志里的“ | 起因=<cause>”；cause 为空时返回空串
func workerCauseField(cause string) string {
	if cause == "" {
		return ""
	}
	return " | 起因=" + cause
}
