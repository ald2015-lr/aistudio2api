package vault

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// profile 为一条保存着的账号资料（含秘密：打印一律打码，见 redact.go）
type profile struct {
	Email         string    `json:"email"`
	Password      string    `json:"password,omitempty"`
	RecoveryEmail string    `json:"recovery_email,omitempty"`
	TOTP          string    `json:"totp,omitempty"`
	TOTPFlags     []string  `json:"totp_flags,omitempty"`
	Note          string    `json:"note,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (record *profile) get(field string) string {
	switch field {
	case fieldPassword:
		return record.Password
	case fieldRecovery:
		return record.RecoveryEmail
	case fieldTOTP:
		return record.TOTP
	default:
		return record.Note
	}
}

func (record *profile) set(field string, value string, flags []string) {
	switch field {
	case fieldPassword:
		record.Password = value
	case fieldRecovery:
		record.RecoveryEmail = value
	case fieldTOTP:
		record.TOTP = value
		record.TOTPFlags = slices.Clone(flags)
		if value == "" {
			record.TOTPFlags = nil
		}
	default:
		record.Note = value
	}
}

func (record *profile) clone() *profile {
	copied := *record
	copied.TOTPFlags = slices.Clone(record.TOTPFlags)
	return &copied
}

func sameProfileFields(left, right *profile) bool {
	return left.Password == right.Password && left.RecoveryEmail == right.RecoveryEmail && left.TOTP == right.TOTP && left.Note == right.Note
}

// docMeta 为一份已载入文件的标记：writeID 为文件里的 write_id，token 为 revision 用的内容标记（文件内容的摘要；只在内存时为 writeID）
type docMeta struct {
	writeID string
	token   string
}

// profileDoc 为全部资料（按邮箱排序，载入或写入后不再修改）
type profileDoc struct {
	docMeta
	list  []profile
	index map[string]int
}

func (doc *profileDoc) meta() *docMeta { return &doc.docMeta }

func newProfileDoc(list []profile) *profileDoc {
	sort.Slice(list, func(i, j int) bool { return list[i].Email < list[j].Email })
	doc := &profileDoc{list: list, index: make(map[string]int, len(list))}
	for index := range list {
		doc.index[list[index].Email] = index
	}
	return doc
}

func emptyProfileDoc() *profileDoc { return newProfileDoc(nil) }

// get 返回邮箱的资料；不存在时为 nil。返回的记录只读
func (doc *profileDoc) get(email string) *profile {
	if index, ok := doc.index[email]; ok {
		return &doc.list[index]
	}
	return nil
}

// withRecords 返回替换或加入 changes、删除 removed 之后的新 doc（原 doc 不变）
func (doc *profileDoc) withRecords(changes map[string]*profile, removed map[string]bool) *profileDoc {
	list := make([]profile, 0, len(doc.list)+len(changes))
	for _, record := range doc.list {
		if removed[record.Email] {
			continue
		}
		if changed, ok := changes[record.Email]; ok {
			list = append(list, *changed)
			continue
		}
		list = append(list, record)
	}
	for email, record := range changes {
		if _, ok := doc.index[email]; !ok {
			list = append(list, *record)
		}
	}
	return newProfileDoc(list)
}

// profilesFile 为 profiles.json 的格式（版本 1）
type profilesFile struct {
	Version  int       `json:"version"`
	SavedAt  time.Time `json:"saved_at"`
	WriteID  string    `json:"write_id"`
	Profiles []profile `json:"profiles"`
}

func decodeProfiles(data []byte) (*profileDoc, *loadProblem) {
	if problem := checkJSON(data); problem != nil {
		return nil, problem
	}
	var file profilesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, jsonProblem(err)
	}
	if file.Version != fileVersion {
		return nil, versionProblem(file.Version)
	}
	seen := make(map[string]bool, len(file.Profiles))
	for index := range file.Profiles {
		email, ok := NormalizeEmail(file.Profiles[index].Email)
		if !ok {
			return nil, invalidProblem(index, "邮箱不合法")
		}
		if seen[email] {
			return nil, invalidProblem(index, "邮箱重复")
		}
		seen[email] = true
		file.Profiles[index].Email = email
	}
	doc := newProfileDoc(file.Profiles)
	doc.writeID = file.WriteID
	return doc, nil
}

func encodeProfiles(doc *profileDoc, writeID string, savedAt time.Time) []byte {
	list := doc.list
	if list == nil {
		list = []profile{}
	}
	data, _ := json.MarshalIndent(profilesFile{Version: fileVersion, SavedAt: savedAt.UTC(), WriteID: writeID, Profiles: list}, "", "  ")
	return append(data, '\n')
}

func profileRecords(doc *profileDoc) int { return len(doc.list) }

// last4 为 2FA 密钥的末四位
func last4(secret string) string {
	if len(secret) < 4 {
		return ""
	}
	return secret[len(secret)-4:]
}

// profileView 为打码视图
func profileView(record *profile) ProfileView {
	return ProfileView{
		Email: record.Email, HasPassword: record.Password != "", RecoveryEmail: record.RecoveryEmail,
		HasTOTP: record.TOTP != "", TOTPLast4: last4(record.TOTP), TOTPOK: record.TOTP != "" && validStoredSecret(record.TOTP),
		TOTPFlags: slices.Clone(record.TOTPFlags), HasNote: record.Note != "",
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

const (
	statusNew       = "new"
	statusUpdate    = "update"
	statusUnchanged = "unchanged"
	statusInvalid   = "invalid"
	modeMerge       = "merge"
	modeFillEmpty   = "fill_empty"
)

// mergeImport 把解析好的行按顺序合并到 cur 上（等价于一行一行分次导入）：已有记录只覆盖这一行给出的字段、没给出的保持原值；
// fillEmpty 时导入前已有非空值的字段一律不改（列进 kept；同一批里前面的行写进的值不算已有值）。返回结果（不含 dry_run、mode、revision）、新的 doc 与是否有改动。纯函数
func mergeImport(cur *profileDoc, parsed *parsedImport, fillEmpty bool, now time.Time) (ProfileImportResult, *profileDoc, bool) {
	result := ProfileImportResult{Sources: parsed.sources, Items: make([]ImportItem, 0, len(parsed.lines))}
	if result.Sources == nil {
		result.Sources = []SourceSummary{}
	}
	summary := &result.Summary
	for _, source := range parsed.sources {
		summary.Lines += source.Lines
	}
	summary.Blank, summary.Comments, summary.Valid, summary.Invalid = parsed.blank, parsed.comments, parsed.valid, parsed.invalid
	work := map[string]*profile{}
	var order []string
	for _, line := range parsed.lines {
		item := ImportItem{Source: line.source, Line: line.line, Email: line.email}
		if line.reason != "" {
			item.Status, item.Reason, item.Hint, item.Field = statusInvalid, line.reason, line.hint, line.field
			result.Items = append(result.Items, item)
			continue
		}
		item.Fields = &ItemFields{
			Password: line.password != "", RecoveryEmail: line.recovery, TOTP: line.totp != "", TOTPLast4: last4(line.totp),
			TOTPFlags: slices.Clone(line.totpFlags), Note: line.note != "",
		}
		item.Warnings = slices.Clone(line.warnings)
		if refs := parsed.refs[line.email]; len(refs) > 1 {
			for _, ref := range refs {
				if ref.Source != line.source || ref.Line != line.line {
					item.Also = append(item.Also, ref)
				}
			}
		}
		original := cur.get(line.email)
		current := work[line.email]
		if current == nil {
			order = append(order, line.email)
			if original == nil {
				current = &profile{Email: line.email, CreatedAt: now, UpdatedAt: now}
				for _, field := range profileFields {
					if value := line.value(field); value != "" {
						current.set(field, value, line.totpFlags)
					}
				}
				work[line.email] = current
				item.Status = statusNew
				result.Items = append(result.Items, item)
				continue
			}
			current = original.clone()
			work[line.email] = current
		}
		for _, field := range profileFields {
			value := line.value(field)
			existing := current.get(field)
			if value == "" || value == existing {
				continue
			}
			// 只补空字段按导入前的值判断：导入前就有的值不改（列进 kept）；导入前为空的字段同一批里后面的行照常覆盖前面的行（以最后一行为准）
			if fillEmpty && original != nil && original.get(field) != "" {
				item.Kept = append(item.Kept, field)
				summary.Kept.add(field)
				continue
			}
			item.Changes = append(item.Changes, field)
			if original != nil && original.get(field) != "" && original.get(field) != value {
				item.Overwrites = append(item.Overwrites, field)
			}
			current.set(field, value, line.totpFlags)
		}
		if len(item.Changes) > 0 {
			item.Status = statusUpdate
			current.UpdatedAt = now
		} else {
			item.Status = statusUnchanged
		}
		result.Items = append(result.Items, item)
	}
	for _, email := range order {
		final, original := work[email], cur.get(email)
		switch {
		case original == nil:
			summary.New++
		case sameProfileFields(final, original):
			summary.Unchanged++
			delete(work, email)
		default:
			summary.Updated++
			for _, field := range profileFields {
				if before := original.get(field); before != "" && before != final.get(field) {
					summary.Overwrites.add(field)
				}
			}
		}
	}
	for _, refs := range parsed.refs {
		summary.DuplicateLines += len(refs) - 1
	}
	summary.Warnings = make(map[string]int, len(parsed.warnings))
	for code, count := range parsed.warnings {
		summary.Warnings[code] = count
	}
	summary.Suspect = append([]string{}, parsed.suspect...)
	summary.Hints = slices.Clone(parsed.hints)
	summary.TotalAfter = len(cur.list) + summary.New
	summary.OverLimit = summary.TotalAfter > MaxProfiles
	if summary.New == 0 && summary.Updated == 0 {
		return result, cur, false
	}
	return result, cur.withRecords(work, nil), true
}

// profileEdit 为校验过的编辑请求
type profileEdit struct {
	email  string
	set    map[string]string
	flags  []string
	clear  []string
	fields []string
}

func (edit profileEdit) String() string { return redactedText }
func (edit profileEdit) Format(state fmt.State, _ rune) {
	writeRedacted(state)
}

// normalizeEdit 校验编辑请求：邮箱合法；同一字段既设置又清空、不认识的字段 → 400；任何字段都不能含 Tab 或控制字符；
// 密码不去首尾空白（编辑框是唯一能存首尾带空格的密码的地方），其余字段去首尾空白；空串等于没给
func normalizeEdit(input ProfileUpdate) (profileEdit, error) {
	email, ok := NormalizeEmail(input.Email)
	if !ok {
		return profileEdit{}, invalidEmail("邮箱格式不对")
	}
	edit := profileEdit{email: email, set: map[string]string{}}
	values := map[string]*string{fieldPassword: input.Password, fieldRecovery: input.RecoveryEmail, fieldTOTP: input.TOTP, fieldNote: input.Note}
	for _, field := range input.Clear {
		if !slices.Contains(profileFields, field) {
			return profileEdit{}, badRequest("clear 里有不认识的字段")
		}
		if !slices.Contains(edit.clear, field) {
			edit.clear = append(edit.clear, field)
		}
	}
	for _, field := range profileFields {
		pointer := values[field]
		if pointer == nil {
			continue
		}
		value := *pointer
		if field != fieldPassword {
			value = trimSpace(value)
		}
		if value == "" {
			continue
		}
		if slices.Contains(edit.clear, field) {
			return profileEdit{}, badRequest("同一个字段不能既设置又清空")
		}
		if strings.ContainsRune(value, '\t') || hasControl(value) {
			return profileEdit{}, invalidField(fieldLabel(field) + "不能含 Tab 或控制字符")
		}
		switch field {
		case fieldPassword:
			if len(value) > MaxPasswordBytes {
				return profileEdit{}, invalidField(fmt.Sprintf("密码太长（最多 %d 字节）", MaxPasswordBytes))
			}
		case fieldRecovery:
			normalized, ok := NormalizeEmail(value)
			if !ok {
				return profileEdit{}, invalidField("辅助邮箱格式不对")
			}
			value = normalized
		case fieldTOTP:
			info := parseTOTP(value)
			if !info.ok {
				return profileEdit{}, invalidField("2FA 密钥格式不对：应为 16 位以上的 Base32 或 otpauth:// 链接")
			}
			value, edit.flags = info.secret, info.flags()
		case fieldNote:
			if len(value) > MaxNoteBytes {
				return profileEdit{}, invalidField(fmt.Sprintf("备注太长（最多 %d 字节）", MaxNoteBytes))
			}
		}
		edit.set[field] = value
	}
	return edit, nil
}

func fieldLabel(field string) string {
	switch field {
	case fieldPassword:
		return "密码"
	case fieldRecovery:
		return "辅助邮箱"
	case fieldTOTP:
		return "2FA 密钥"
	default:
		return "备注"
	}
}

// applyEdit 在 cur 上应用编辑：不存在则新建；只有明确清空的字段才清空。返回结果（不含 revision）、新的 doc 与是否有改动
func applyEdit(cur *profileDoc, edit profileEdit, now time.Time) (ProfileUpdateResult, *profileDoc, bool, error) {
	original := cur.get(edit.email)
	var record *profile
	created := original == nil
	if created {
		if len(cur.list) >= MaxProfiles {
			return ProfileUpdateResult{}, nil, false, &Error{Status: 409, Code: CodeFull, Message: fmt.Sprintf("账号资料已有 %d 条，达到上限", MaxProfiles)}
		}
		record = &profile{Email: edit.email, CreatedAt: now, UpdatedAt: now}
	} else {
		record = original.clone()
	}
	result := ProfileUpdateResult{Created: created, Changed: []string{}}
	for _, field := range profileFields {
		if value, ok := edit.set[field]; ok {
			if record.get(field) != value {
				record.set(field, value, edit.flags)
				result.Changed = append(result.Changed, field)
			}
			continue
		}
		if slices.Contains(edit.clear, field) && record.get(field) != "" {
			record.set(field, "", nil)
			result.Changed = append(result.Changed, field)
			result.Cleared = append(result.Cleared, field)
		}
	}
	if !created && len(result.Changed) == 0 {
		result.Profile = profileView(original)
		return result, cur, false, nil
	}
	record.UpdatedAt = now
	result.Profile = profileView(record)
	return result, cur.withRecords(map[string]*profile{edit.email: record}, nil), true, nil
}
