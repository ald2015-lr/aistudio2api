package vault

import "net/http"

// 错误码（管理接口响应体 {"error":{"code","message"}} 的 code）
const (
	CodeInvalidRequest = "invalid_request"
	CodeInvalidEmail   = "vault_invalid_email"
	CodeInvalidField   = "vault_invalid_field"
	CodeFamilyEmpty    = "vault_family_empty"
	CodeTooLarge       = "vault_too_large"
	CodeUnreadable     = "vault_unreadable"
	CodeFull           = "vault_full"
	CodeConflict       = "vault_conflict"
	CodeSuspectUnacked = "vault_suspect_unacked"
	CodeNotFound       = "vault_not_found"
	CodeFamilyNotFound = "vault_family_not_found"
	CodeTOTPMissing    = "vault_totp_missing"
	CodeTOTPInvalid    = "vault_totp_invalid"
	CodeBusy           = "vault_busy"
	CodeWriteFailed    = "vault_write_failed"
	CodeUnsupported    = "vault_unsupported"
)

// Error 为账号资料与家庭组接口的错误：Message 是固定的中文说明，从不包含密码、2FA、备注、来源名或文件内容
type Error struct {
	Status  int
	Code    string
	Message string
}

func (err *Error) Error() string     { return err.Message }
func (err *Error) HTTPStatus() int   { return err.Status }
func (err *Error) ErrorCode() string { return err.Code }

// ConflictError 为确认时发现数据在预览之后变了（或覆盖条数有变化）：Preview 是按当前内容重算的预览（与 dry-run 的返回同形，打码视图）
type ConflictError struct {
	Err     *Error
	Preview any
}

func (err *ConflictError) Error() string     { return err.Err.Message }
func (err *ConflictError) HTTPStatus() int   { return err.Err.Status }
func (err *ConflictError) ErrorCode() string { return err.Err.Code }
func (err *ConflictError) Unwrap() error     { return err.Err }

func badRequest(message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: message}
}

func invalidEmail(message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidEmail, Message: message}
}

func invalidField(message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidField, Message: message}
}

func conflict(message string) *Error {
	return &Error{Status: http.StatusConflict, Code: CodeConflict, Message: message}
}

func unreadable(file string) *Error {
	label := "账号资料文件"
	if file == fileFamilies {
		label = "家庭组文件"
	}
	return &Error{Status: http.StatusConflict, Code: CodeUnreadable,
		Message: label + "无法读取（损坏、读不出或主文件不见了而备份还在），已停止写入以免覆盖；请在“资料与家庭组”页查看原因与修复办法"}
}

func busy() *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeBusy, Message: "另一个进程正在写账号资料，请稍后再试"}
}
