package vault

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP 的固定参数：RFC 6238、HMAC-SHA1、30 秒、6 位（otpauth 链接里的其他参数不保存，只记 otpauth_params 标志）
const (
	totpPeriod     = 30
	totpMinLength  = 16
	totpMaxLength  = 128
	flagLetters    = "letters_only"
	flagParams     = "otpauth_params"
	otpauthPrefix  = "otpauth://"
	totpDigitsMod  = 1_000_000
	totpDigitsText = "%06d"
)

// totpInfo 为一段文本按 2FA 密钥判别的结果（secret 是秘密，不进日志、错误与列表）
type totpInfo struct {
	ok     bool
	secret string // 规范化 Base32：大写、无空格、无 =
	// definite 为“确定”候选：含数字 2–7，或来自 otpauth 链接；纯字母的是“可能不是 2FA”候选
	definite bool
	// appPassword 表示像 Google 应用专用密码（正好 16 个小写字母，连写或 4 组 4 个、单个空格隔开）：导入时不当 2FA
	appPassword bool
	// otpauth 表示以 otpauth:// 开头（取不出有效 secret 时 ok 为假）
	otpauth bool
	// params 表示 otpauth 链接的 algorithm、digits 或 period 不是默认值（面板按 6 位/30 秒/SHA1 算的码可能不准）
	params bool
}

func (info totpInfo) String() string   { return redactedText }
func (info totpInfo) GoString() string { return redactedText }
func (info totpInfo) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redactedText))
}

// flags 为保存记录时的 totp_flags
func (info totpInfo) flags() []string {
	var flags []string
	if !info.definite {
		flags = append(flags, flagLetters)
	}
	if info.params {
		flags = append(flags, flagParams)
	}
	return flags
}

// parseTOTP 判别 2FA 密钥：otpauth://totp 链接取其中的 secret；裸密钥允许小写、全角、按空白分组（各组等长 4–8，最后一组可短，
// 最后一组末尾的 = 去掉），拼接后 16–128 位、不能大小写混写、只含 A–Z 与 2–7
func parseTOTP(text string) totpInfo {
	text = strings.TrimSpace(text)
	if len(text) >= len(otpauthPrefix) && strings.EqualFold(text[:len(otpauthPrefix)], otpauthPrefix) {
		return parseOTPAuth(text)
	}
	secret, definite, ok := normalizeBase32Key(text)
	if !ok {
		return totpInfo{}
	}
	return totpInfo{ok: true, secret: secret, definite: definite, appPassword: looksAppPassword(text)}
}

// parseOTPAuth 解析 otpauth 链接；解析失败的错误文本含完整原文，一律丢弃
func parseOTPAuth(text string) totpInfo {
	invalid := totpInfo{otpauth: true}
	parsed, err := url.Parse(text)
	if err != nil || !strings.EqualFold(parsed.Host, "totp") {
		return invalid
	}
	query := parsed.Query()
	secret, _, ok := normalizeBase32Key(query.Get("secret"))
	if !ok {
		return invalid
	}
	info := totpInfo{ok: true, secret: secret, definite: true, otpauth: true}
	if value := strings.TrimSpace(query.Get("algorithm")); value != "" && !strings.EqualFold(value, "SHA1") {
		info.params = true
	}
	if value := strings.TrimSpace(query.Get("digits")); value != "" {
		if digits, err := strconv.Atoi(value); err != nil || digits != 6 {
			info.params = true
		}
	}
	if value := strings.TrimSpace(query.Get("period")); value != "" {
		if period, err := strconv.Atoi(value); err != nil || period != totpPeriod {
			info.params = true
		}
	}
	return info
}

// normalizeBase32Key 把裸密钥规范化为大写 Base32；definite 为拼接后含数字 2–7
func normalizeBase32Key(text string) (secret string, definite bool, ok bool) {
	groups := strings.Fields(foldWidth(text))
	// 填充的 = 可能单独写成一组（“JBSW Y3DP EHPK 3PXP ====”）：先丢掉只由 = 组成的尾组，再去掉最后一组末尾的 =，与连写一致
	for len(groups) > 0 && strings.Trim(groups[len(groups)-1], "=") == "" {
		groups = groups[:len(groups)-1]
	}
	if len(groups) == 0 {
		return "", false, false
	}
	groups[len(groups)-1] = strings.TrimRight(groups[len(groups)-1], "=")
	if len(groups) > 1 {
		size := len(groups[0])
		if size < 4 || size > 8 {
			return "", false, false
		}
		for _, group := range groups[:len(groups)-1] {
			if len(group) != size {
				return "", false, false
			}
		}
		if last := len(groups[len(groups)-1]); last < 1 || last > size {
			return "", false, false
		}
	}
	joined := strings.Join(groups, "")
	if len(joined) < totpMinLength || len(joined) > totpMaxLength {
		return "", false, false
	}
	hasUpper, hasLower := false, false
	for index := 0; index < len(joined); index++ {
		c := joined[index]
		switch {
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= '2' && c <= '7':
			definite = true
		default:
			return "", false, false
		}
	}
	if hasUpper && hasLower {
		return "", false, false
	}
	return strings.ToUpper(joined), definite, true
}

// looksAppPassword 判断是否为 Google 应用专用密码的写法：正好 16 个小写字母，连写或 4 组 4 个、组间单个空格
func looksAppPassword(text string) bool {
	text = strings.TrimSpace(foldWidth(text))
	letters := func(part string) bool {
		for index := 0; index < len(part); index++ {
			if part[index] < 'a' || part[index] > 'z' {
				return false
			}
		}
		return true
	}
	switch len(text) {
	case 16:
		return letters(text)
	case 19:
		parts := strings.Split(text, " ")
		if len(parts) != 4 {
			return false
		}
		for _, part := range parts {
			if len(part) != 4 || !letters(part) {
				return false
			}
		}
		return true
	}
	return false
}

// validStoredSecret 判断保存着的 2FA 密钥能否算出验证码（规范化后的 Base32；人工改坏的为假）
func validStoredSecret(secret string) bool {
	if len(secret) < totpMinLength || len(secret) > totpMaxLength {
		return false
	}
	for index := 0; index < len(secret); index++ {
		c := secret[index]
		if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')) {
			return false
		}
	}
	return true
}

// decodeBase32 宽松解码：每个字符 5 位，凑满 8 位出一个字节，末尾不足 8 位的丢弃（任何长度都能解码，与常见验证器一致）。
// 调用方先用 validStoredSecret 校验字符
func decodeBase32(secret string) []byte {
	output := make([]byte, 0, len(secret)*5/8)
	var buffer uint32
	bits := 0
	for index := 0; index < len(secret); index++ {
		c := secret[index]
		var value uint32
		switch {
		case c >= 'A' && c <= 'Z':
			value = uint32(c - 'A')
		case c >= '2' && c <= '7':
			value = uint32(c-'2') + 26
		default:
			continue
		}
		buffer = buffer<<5 | value
		bits += 5
		if bits >= 8 {
			bits -= 8
			output = append(output, byte(buffer>>uint(bits)))
			buffer &= 1<<uint(bits) - 1
		}
	}
	return output
}

// hotp 为 RFC 4226 的动态截断，取 6 位
func hotp(key []byte, counter uint64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf(totpDigitsText, value%totpDigitsMod)
}

// totpCodes 按 RFC 6238（SHA1、30 秒、6 位）算出 now 所在时间片的验证码与下一个时间片的验证码，remaining 为当前码还剩的秒数（1–30）
func totpCodes(secret string, now time.Time) (code string, next string, remaining int, ok bool) {
	if !validStoredSecret(secret) {
		return "", "", 0, false
	}
	key := decodeBase32(secret)
	unix := now.Unix()
	counter := uint64(unix / totpPeriod)
	return hotp(key, counter), hotp(key, counter+1), totpPeriod - int(unix%totpPeriod), true
}
