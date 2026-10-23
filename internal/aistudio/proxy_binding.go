package aistudio

// ProxyBinding 为 IP 池分给账户的出口（见 app/proxy_pool.go）：URL 为上游代理（可能带账号密码，只交给网络层，
// 不能写进日志、错误与接口；为空表示沿用默认代理），Slot 为管理页显示的端口号或会话号（为空不显示），
// Active 表示有浏览器正在用它（静态 IP 时该端口归它独占），EgressTag 在会话号写在密码里时为会话号（出口统计据此区分）
type ProxyBinding struct {
	URL       string
	Slot      string
	Active    bool
	EgressTag string
}

// EgressKey 返回绑定作为出口时的出口键：EgressKey(URL)，带 EgressTag 时追加 "#" + EgressTag（与账户池挂通道用的键相同，
// 见 egressKeyOfLocked；app 层 IP 池按它核对出口判定的是不是账户当前的会话）。URL 为空（沿用默认代理）时为空串
func (b ProxyBinding) EgressKey() string {
	if b.URL == "" {
		return ""
	}
	key := EgressKey(b.URL)
	if b.EgressTag != "" {
		key += "#" + b.EgressTag
	}
	return key
}

// ProxyBindingUpdate 为一次推送：Binding 为 nil 表示清除
type ProxyBindingUpdate struct {
	AccountID string
	Binding   *ProxyBinding
}

// SetProxyBinding 原子替换账户的 IP 池出口（nil 为清除）；只用于还没进入账户池的对象（app 层 prime），
// 进入账户池后只经 AccountPool.SetProxyBindings 写。方法对 nil 账户安全
func (a *Account) SetProxyBinding(binding *ProxyBinding) {
	if a == nil {
		return
	}
	a.storeProxyBinding(binding)
}

// storeProxyBinding 写入绑定的副本，返回绑定是否有变化（按内容比较）
func (a *Account) storeProxyBinding(binding *ProxyBinding) bool {
	previous := a.proxyBinding.Load()
	if binding == nil {
		if previous == nil {
			return false
		}
		a.proxyBinding.Store(nil)
		return true
	}
	if previous != nil && *previous == *binding {
		return false
	}
	value := *binding
	a.proxyBinding.Store(&value)
	return true
}

// CurrentProxyBinding 返回当前绑定的副本；没有绑定时 ok 为 false
func (a *Account) CurrentProxyBinding() (ProxyBinding, bool) {
	if a == nil {
		return ProxyBinding{}, false
	}
	binding := a.proxyBinding.Load()
	if binding == nil {
		return ProxyBinding{}, false
	}
	return *binding, true
}

// SetProxyBindings 在 p.mu 内按账户 ID 写绑定（不在号池中的 ID 忽略：重建期间旧实例浏览器的归还推给新号池时，
// 新号池可能已经没有这个账户），返回实际写入（内容有变化）的个数。出口通道在同一次加锁内随绑定重挂，有账户换了通道时唤醒等待方
// （见 proxyBindingChangedLocked）。调用方持有 app 层 proxyPool.mu（锁序 proxyPool.mu → p.mu），本方法不回调任何外部代码
func (p *AccountPool) SetProxyBindings(updates []ProxyBindingUpdate) int {
	if p == nil || len(updates) == 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	written, moved := 0, false
	for _, update := range updates {
		account := p.byID[update.AccountID]
		if account == nil {
			continue
		}
		if account.storeProxyBinding(update.Binding) {
			written++
			if p.proxyBindingChangedLocked(account, true) {
				moved = true
			}
		}
	}
	if moved {
		p.notifyLocked()
	}
	return written
}

// UpdateProxyBindings 在 p.mu 内对号池全部账户调用 compute（账户 ID 与它自己的代理原文），按返回值写绑定（nil 为清除）；
// 出口通道同样随绑定重挂、有账户换了通道时唤醒等待方。compute 只能读调用方已持有锁保护的状态，不能取锁、不能回调账户池
func (p *AccountPool) UpdateProxyBindings(compute func(accountID, accountProxy string) *ProxyBinding) {
	if p == nil || compute == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed, moved := 0, false
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		if account.storeProxyBinding(compute(account.ID, account.Config.Proxy)) {
			changed++
			if p.proxyBindingChangedLocked(account, false) {
				moved = true
			}
		}
	}
	if changed > 0 {
		// 整池重推可能改动上万个账户：状态缓存整份作废，比逐项重建便宜
		p.invalidateStatusLocked()
	}
	if moved {
		p.notifyLocked()
	}
}

// proxyBindingChangedLocked 在账户的 IP 池出口变化后调用：按新的出口键重挂出口通道（attachEgressLocked，与写绑定在同一次加锁内，
// verifyEgressLocked 看不到中间态），refreshStatus 为真时就地更新状态缓存里的这一项（管理页的端口兜底显示）。返回是否换了通道
// （调用方据此 notifyLocked：换到不限速或还有令牌的通道后，按出口排队的请求可以重新选号）。调用方持有 p.mu
func (p *AccountPool) proxyBindingChangedLocked(account *Account, refreshStatus bool) bool {
	moved := p.attachEgressLocked(account)
	if refreshStatus {
		p.refreshStatusEntryLocked(account)
	}
	return moved
}

// AccountIDs 返回号池中的账户 ID（快照，按号池顺序）
func (p *AccountPool) AccountIDs() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account != nil {
			ids = append(ids, account.ID)
		}
	}
	return ids
}
