package vault

import "strings"

// foldWidth 把全角 ASCII（U+FF01–U+FF5E）折叠成对应的半角字符、全角空格 U+3000 折叠成空格。
// 只用于邮箱、2FA 判别与格式提示，不用于密码与备注（它们原样保存）
func foldWidth(text string) string {
	needs := false
	for _, r := range text {
		if (r >= 0xFF01 && r <= 0xFF5E) || r == 0x3000 {
			needs = true
			break
		}
	}
	if !needs {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	for _, r := range text {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			builder.WriteRune(r - 0xFF01 + 0x21)
		case r == 0x3000:
			builder.WriteByte(' ')
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// NormalizeEmail 返回规范化邮箱（全角折叠、去首尾空白、转小写）与是否合法。资料、家庭组与编辑共用同一规则：
// 总长 6–254；恰好一个 @；本地部分 1–64 个 [a-z0-9.!#$%&'*+/=?^_`{|}~-]，不以 . 开头或结尾、不含 ..；
// 域名至少两段，每段 1–63 个 [a-z0-9-] 且不以 - 开头结尾，末段为至少 2 个字母或以 xn-- 开头。
// 不接受 Name <a@b>、引号本地部分、IP 域名与 mailto:
func NormalizeEmail(text string) (string, bool) {
	text = strings.TrimSpace(foldWidth(text))
	if len(text) < 6 || len(text) > 254 {
		return "", false
	}
	for index := 0; index < len(text); index++ {
		if text[index] >= 0x80 {
			return "", false
		}
	}
	text = strings.ToLower(text)
	at := strings.IndexByte(text, '@')
	if at <= 0 || strings.IndexByte(text[at+1:], '@') >= 0 {
		return "", false
	}
	local, domain := text[:at], text[at+1:]
	if !validLocalPart(local) || !validDomain(domain) {
		return "", false
	}
	return text, true
}

func validLocalPart(local string) bool {
	if len(local) == 0 || len(local) > 64 || local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return false
	}
	for index := 0; index < len(local); index++ {
		c := local[index]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte(".!#$%&'*+/=?^_`{|}~-", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			c := label[index]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.HasPrefix(last, "xn--") {
		return true
	}
	if len(last) < 2 {
		return false
	}
	for index := 0; index < len(last); index++ {
		if last[index] < 'a' || last[index] > 'z' {
			return false
		}
	}
	return true
}
