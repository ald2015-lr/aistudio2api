package app

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// accountTimezoneInput 校验管理端录入的账户时区（添加账户、Chrome 导入、修改账户时区），返回去掉首尾空白的值；为空时返回空串
// （由调用方取默认时区或按必填报错）。时区会写进 Camoufox 指纹与 WAA 的 Date 字符串，原先只去掉首尾空白，拼错或带零宽字符的值
// 要到 Worker 启动加载时区时才失败。这里用 time.LoadLocation 校验（程序内嵌了时区数据，见 aistudio/quota.go 的 time/tzdata），
// 含空白、零宽或其他不可见字符、Local（不是 IANA 时区名）时同样拒绝，返回 400 invalid_timezone，错误里的值按 %q 显示
// （不可见字符显示为 \u200b 这类转义）。只校验录入，不在读入 account.json 时静默规范化：那会改变已有账户实际使用的身份输入
func accountTimezoneInput(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	for _, r := range value {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			return "", invalidTimezone(value)
		}
	}
	if value == "Local" {
		return "", invalidTimezone(value)
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", invalidTimezone(value)
	}
	return value, nil
}

// invalidTimezone 返回账户时区无效的错误（400 invalid_timezone）；管理页按错误码显示本地化文案
func invalidTimezone(value string) error {
	return &adminOperationError{
		status: http.StatusBadRequest, code: "invalid_timezone",
		message: fmt.Sprintf("账户时区无效: %q（需要 IANA 时区名，如 Asia/Shanghai，不能含空白或不可见字符）", value),
	}
}
