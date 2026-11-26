package vault

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// TestParseTOTP 裸密钥的规范化与拒绝条件、候选分类（确定 / 纯字母 / 应用专用密码）、otpauth 链接
func TestParseTOTP(t *testing.T) {
	cases := []struct {
		input       string
		ok          bool
		secret      string
		definite    bool
		appPassword bool
		otpauth     bool
		params      bool
	}{
		{"JBSWY3DPEHPK3PXP", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"jbswy3dpehpk3pxp", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"jbsw y3dp ehpk 3pxp", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"JBSWY3DP EHPK3PXP", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"jbsw  y3dp\tehpk 3pxp", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"JBSWY3DPEHPK3PXP====", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"JBSW Y3DP EHPK 3PXP JB==", true, "JBSWY3DPEHPK3PXPJB", true, false, false, false},
		// 填充的 = 单独写成一组（或拆成几组）时与连写一致
		{"JBSW Y3DP EHPK 3PXP ====", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"jbsw y3dp ehpk 3pxp == ==", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"JBSW Y3DP EHPK 3PXP JB ==", true, "JBSWY3DPEHPK3PXPJB", true, false, false, false},
		{"==== ====", false, "", false, false, false, false},
		{"JBSW Y3DP == EHPK 3PXP", false, "", false, false, false, false},
		{"ｊｂｓｗ\u3000ｙ３ｄｐ\u3000ｅｈｐｋ\u3000３ｐｘｐ", true, "JBSWY3DPEHPK3PXP", true, false, false, false},
		{"JbSwY3DpEhPk3PxP", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PX", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PX0", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PX1", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PX8", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PX9", false, "", false, false, false, false},
		{"JBS WY3D PEHP K3PX P", false, "", false, false, false, false},
		{"JBSWY3 DPEH PK3PXP", false, "", false, false, false, false},
		{"JBSWY3DPE HPK3PXPA", false, "", false, false, false, false},
		{"abc def ghi jkl mno pqr", false, "", false, false, false, false},
		{"JBSWY3DPEHPK3PXP JBSWY3DPEHPK3PXPA", false, "", false, false, false, false},
		{strings.Repeat("AB23", 32), true, strings.Repeat("AB23", 32), true, false, false, false},
		{strings.Repeat("AB23", 32) + "A", false, "", false, false, false, false},
		{"FAMILYMAINACCOUNT", true, "FAMILYMAINACCOUNT", false, false, false, false},
		{"familymainaccount", true, "FAMILYMAINACCOUNT", false, false, false, false},
		{"abcdefghijklmnop", true, "ABCDEFGHIJKLMNOP", false, true, false, false},
		{"abcd efgh ijkl mnop", true, "ABCDEFGHIJKLMNOP", false, true, false, false},
		{"abcd  efgh ijkl mnop", true, "ABCDEFGHIJKLMNOP", false, false, false, false},
		{"ABCDEFGHIJKLMNOP", true, "ABCDEFGHIJKLMNOP", false, false, false, false},
		{"abcdefgh ijklmnop", true, "ABCDEFGHIJKLMNOP", false, false, false, false},
		{"abcd efgh ijkl mnop qrst uvwx yz23 4567", true, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", true, false, false, false},
		{"otpauth://totp/Google:a%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Google", true, "JBSWY3DPEHPK3PXP", true, false, true, false},
		{"OTPAUTH://TOTP/x?secret=jbswy3dpehpk3pxp", true, "JBSWY3DPEHPK3PXP", true, false, true, false},
		{"otpauth://totp/x?secret=FAMILYMAINACCOUNT", true, "FAMILYMAINACCOUNT", true, false, true, false},
		{"otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&digits=6&period=30&algorithm=sha1", true, "JBSWY3DPEHPK3PXP", true, false, true, false},
		{"otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&digits=8", true, "JBSWY3DPEHPK3PXP", true, false, true, true},
		{"otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&period=60", true, "JBSWY3DPEHPK3PXP", true, false, true, true},
		{"otpauth://totp/x?secret=JBSWY3DPEHPK3PXP&algorithm=SHA256", true, "JBSWY3DPEHPK3PXP", true, false, true, true},
		{"otpauth://hotp/x?secret=JBSWY3DPEHPK3PXP&counter=1", false, "", false, false, true, false},
		{"otpauth://totp/x?issuer=G", false, "", false, false, true, false},
		{"otpauth://totp/x?secret=JBSW", false, "", false, false, true, false},
		{"otpauth://totp/%zz?secret=JBSWY3DPEHPK3PXP", false, "", false, false, true, false},
		{"", false, "", false, false, false, false},
	}
	for _, item := range cases {
		got := parseTOTP(item.input)
		if got.ok != item.ok || got.secret != item.secret || got.definite != item.definite || got.appPassword != item.appPassword ||
			got.otpauth != item.otpauth || got.params != item.params {
			t.Errorf("parseTOTP(%q) = ok=%v secret=%q definite=%v app=%v otpauth=%v params=%v，期望 ok=%v secret=%q definite=%v app=%v otpauth=%v params=%v",
				item.input, got.ok, got.secret, got.definite, got.appPassword, got.otpauth, got.params,
				item.ok, item.secret, item.definite, item.appPassword, item.otpauth, item.params)
		}
	}
}

// TestTOTPVectors RFC 6238 附录 B 的 SHA1 向量（取 8 位结果的后 6 位）、下一个码、剩余秒数与宽松 Base32
func TestTOTPVectors(t *testing.T) {
	const seed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := []struct {
		unix      int64
		code      string
		next      string
		remaining int
	}{
		{59, "287082", "359152", 1},
		{1111111109, "081804", "050471", 1},
		{1111111111, "050471", "266759", 29},
		{1234567890, "005924", "590587", 30},
		{2000000000, "279037", "637009", 10},
		{20000000000, "353130", "128202", 10},
	}
	for _, item := range cases {
		code, next, remaining, ok := totpCodes(seed, time.Unix(item.unix, 0))
		if !ok || code != item.code || next != item.next || remaining != item.remaining {
			t.Errorf("T=%d: code=%s next=%s remaining=%d ok=%v，期望 %s %s %d", item.unix, code, next, remaining, ok, item.code, item.next, item.remaining)
		}
	}
	if _, _, remaining, _ := totpCodes(seed, time.Unix(60, 0)); remaining != 30 {
		t.Fatalf("T=60 remaining = %d", remaining)
	}
	if _, _, remaining, _ := totpCodes(seed, time.Unix(60, 999_000_000)); remaining != 30 {
		t.Fatalf("T=60.999 remaining = %d", remaining)
	}
	// 宽松 Base32：标准长度与 encoding/base32 一致；非标准长度也能解码（多余的不足 8 位丢弃）
	for _, input := range []string{"JBSWY3DPEHPK3PXP", seed, "MFRGGZDFMZTWQ2LK"} {
		want, err := base32.StdEncoding.DecodeString(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeBase32(input); string(got) != string(want) {
			t.Fatalf("decodeBase32(%q) = %x，期望 %x", input, got, want)
		}
	}
	if got := decodeBase32("JBSWY3DPEHPK3PXPJB"); len(got) != 11 || string(got[:10]) != "Hello!\xde\xad\xbe\xef" {
		t.Fatalf("非标准长度 = %x", got)
	}
	if _, _, _, ok := totpCodes("JBSWY3DPEHPK3PXPJB", time.Unix(59, 0)); !ok {
		t.Fatal("非标准长度的密钥也应能算出验证码")
	}
	if _, _, _, ok := totpCodes("not-base32!", time.Unix(59, 0)); ok {
		t.Fatal("不合格的密钥不应算出验证码")
	}
	if _, _, _, ok := totpCodes("", time.Unix(59, 0)); ok {
		t.Fatal("空密钥不应算出验证码")
	}
}
