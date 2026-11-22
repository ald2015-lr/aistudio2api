package aistudio

import "encoding/json"

// deviceBoundMemo 缓存一个账户认证扩展里有没有 Chrome 续签材料：extension 为上次判断时认证扩展的原文（同一份字节，按身份比较，
// 不复制），known 表示已经判断过
type deviceBoundMemo struct {
	extension json.RawMessage
	known     bool
	renewal   bool
}

// deviceBoundLocked 返回账户认证状态是否带设备绑定会话的 Cookie，以及认证扩展里是否保存了 Chrome 续签材料（只在带这个 Cookie 时判断）。
// Cookie 只比名字，每次构建状态都看；续签材料要解析认证扩展，账户上万时每次构建状态都解析开销不小，所以按认证扩展原文的身份缓存在账户上：
// 认证状态在载入、登录、导入、认证文件更新时整份替换，认证扩展是新的一份字节，重新解析一次，之后不再解析。调用方持有账户池 mu
func (account *Account) deviceBoundLocked() (session bool, renewal bool) {
	if !account.StorageState.DeviceBoundSession() {
		return false, false
	}
	raw := account.StorageState.extra[authExtensionKey]
	memo := &account.deviceBound
	if !memo.known || !sameRawMessage(memo.extension, raw) {
		memo.extension, memo.known, memo.renewal = raw, true, account.StorageState.HasChromeRenewal()
	}
	return true, memo.renewal
}

// sameRawMessage 判断两份原文是不是同一份字节（长度相同且起始地址相同；都为空也算同一份）
func sameRawMessage(left, right json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	return len(left) == 0 || &left[0] == &right[0]
}
