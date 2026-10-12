package aistudio

import (
	"sync"
	"time"
)

// backgroundWrites 记录账户池在后台进行的运行态写入（如生成成功后保存模型资格）。请求路径不等磁盘，
// 但停止服务或删除账户目录前要先等这些写入结束，否则临时文件会与目录删除并发（测试清理时表现为“目录非空”）。
// 用互斥锁加条件变量而不是 WaitGroup：计数归零后再登记新写入也不违反使用约定
type backgroundWrites struct {
	mu    sync.Mutex
	cond  *sync.Cond
	count int
}

// begin 登记一次后台写入，返回的函数在写入结束时调用（重复调用无副作用）
func (writes *backgroundWrites) begin() func() {
	writes.mu.Lock()
	if writes.cond == nil {
		writes.cond = sync.NewCond(&writes.mu)
	}
	writes.count++
	writes.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			writes.mu.Lock()
			writes.count--
			if writes.count <= 0 {
				writes.count = 0
				writes.cond.Broadcast()
			}
			writes.mu.Unlock()
		})
	}
}

// wait 阻塞到没有进行中的后台写入
func (writes *backgroundWrites) wait() {
	writes.mu.Lock()
	if writes.cond == nil {
		writes.cond = sync.NewCond(&writes.mu)
	}
	for writes.count > 0 {
		writes.cond.Wait()
	}
	writes.mu.Unlock()
}

// MarkModelAccessVerifiedAsync 在后台保存当前目录代际中的模型生成成功记录，写入结束后调用 done（可为 nil），
// 参数为是否有变化与错误。调用方不必等待磁盘写入；WaitBackground 可在停止或清理账户目录前等待这类写入结束
func (p *AccountPool) MarkModelAccessVerifiedAsync(
	accountID string,
	modelID string,
	generation uint64,
	checkedAt time.Time,
	done func(changed bool, err error),
) {
	finish := p.background.begin()
	go func() {
		defer finish()
		changed, err := p.markModelAccessVerified(accountID, modelID, generation, checkedAt)
		if done != nil {
			done(changed, err)
		}
	}()
}

// WaitBackground 等待账户池的后台运行态写入全部结束（含写入后的回调）
func (p *AccountPool) WaitBackground() {
	p.background.wait()
}
