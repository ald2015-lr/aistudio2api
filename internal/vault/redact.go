package vault

import (
	"fmt"
	"log/slog"
)

// redactedText 为含秘密的结构体在 fmt、%v/%+v/%#v/%s 与 slog 里的样子：防止误打日志把密码、2FA、备注、验证码带出去
const redactedText = "[redacted]"

func writeRedacted(state fmt.State) { _, _ = state.Write([]byte(redactedText)) }

func (value ImportSource) String() string                 { return redactedText }
func (value ImportSource) GoString() string               { return redactedText }
func (value ImportSource) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value ImportSource) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value ProfileImportInput) String() string                 { return redactedText }
func (value ProfileImportInput) GoString() string               { return redactedText }
func (value ProfileImportInput) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value ProfileImportInput) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value ProfileUpdate) String() string                 { return redactedText }
func (value ProfileUpdate) GoString() string               { return redactedText }
func (value ProfileUpdate) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value ProfileUpdate) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value ProfileSecret) String() string                 { return redactedText }
func (value ProfileSecret) GoString() string               { return redactedText }
func (value ProfileSecret) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value ProfileSecret) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value TOTPCode) String() string                 { return redactedText }
func (value TOTPCode) GoString() string               { return redactedText }
func (value TOTPCode) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value TOTPCode) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value ProfileExport) String() string                 { return redactedText }
func (value ProfileExport) GoString() string               { return redactedText }
func (value ProfileExport) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value ProfileExport) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value FamilyImportInput) String() string                 { return redactedText }
func (value FamilyImportInput) GoString() string               { return redactedText }
func (value FamilyImportInput) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value FamilyImportInput) LogValue() slog.Value           { return slog.StringValue(redactedText) }

func (value profile) String() string                 { return redactedText }
func (value profile) GoString() string               { return redactedText }
func (value profile) Format(state fmt.State, _ rune) { writeRedacted(state) }
func (value profile) LogValue() slog.Value           { return slog.StringValue(redactedText) }
