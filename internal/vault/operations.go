package vault

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"
)

// View 返回打码视图；since 等于当前 revision 时只回 revision 与 unchanged
func (store *Store) View(ctx context.Context, since string) (View, error) {
	store.mu.Lock()
	if err := store.loadAll(ctx); err != nil {
		store.mu.Unlock()
		return View{}, err
	}
	revision := store.revisionLocked()
	if since != "" && since == revision {
		store.mu.Unlock()
		return View{Revision: revision, Unchanged: true}, nil
	}
	profiles, families := store.profiles.doc, store.families.doc
	var fileErrors []FileError
	for _, fileErr := range []*FileError{store.profiles.fileErr, store.families.fileErr} {
		if fileErr != nil {
			fileErrors = append(fileErrors, *fileErr)
		}
	}
	backups := store.backupsLocked()
	store.mu.Unlock()

	limits := currentLimits()
	view := View{
		Revision: revision, Dir: store.dir, Persistent: store.dir != "", Profiles: make([]ProfileView, 0, len(profiles.list)),
		Families: make([]FamilyGroup, 0, len(families.groups)), Errors: fileErrors, Backups: backups, Limits: &limits, Palette: PaletteSize,
	}
	for index := range profiles.list {
		view.Profiles = append(view.Profiles, profileView(&profiles.list[index]))
	}
	for _, group := range families.groups {
		group.Members = slices.Clone(group.Members)
		view.Families = append(view.Families, group)
	}
	return view, nil
}

func (store *Store) backupsLocked() []BackupInfo {
	if store.dir == "" {
		return nil
	}
	var backups []BackupInfo
	for _, name := range []string{fileProfiles, fileFamilies} {
		backup := store.backupState(name)
		if !backup.bakExists && backup.snapshots == 0 {
			continue
		}
		info := BackupInfo{File: name, Snapshots: backup.snapshots}
		if backup.bakExists {
			at := backup.bakTime
			info.BakTime = &at
		}
		if backup.snapshots > 0 {
			at := backup.latestTime
			info.SnapshotLatest = &at
		}
		backups = append(backups, info)
	}
	return backups
}

// readProfiles 取当前资料（读路径：不加排他锁、不建目录）；文件处于错误状态时 409
func (store *Store) readProfiles(ctx context.Context) (*profileDoc, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return nil, "", err
	}
	if store.profiles.fileErr != nil {
		return nil, "", unreadable(fileProfiles)
	}
	return store.profiles.doc, store.revisionLocked(), nil
}

func (store *Store) readFamilies(ctx context.Context) (*familyDoc, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return nil, "", err
	}
	if store.families.fileErr != nil {
		return nil, "", unreadable(fileFamilies)
	}
	return store.families.doc, store.revisionLocked(), nil
}

// mutateProfiles 与 mutateFamilies 在 Store.mu 内先刷新两份文件的缓存，再对一份文件执行写操作
func (store *Store) mutateProfiles(ctx context.Context, snapshot bool, apply applyFunc[*profileDoc]) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return "", err
	}
	return store.profiles.mutate(ctx, store, snapshot, apply)
}

func (store *Store) mutateFamilies(ctx context.Context, snapshot bool, apply applyFunc[*familyDoc]) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return "", err
	}
	return store.families.mutate(ctx, store, snapshot, apply)
}

// ImportProfiles 预览或确认导入资料。解析在锁外；确认时在排他锁内按重读的内容重算（与预览同一个函数）：
// 带了 expect_overwrites 而算出的覆盖条数不同 → 409 附新预览；有“格式可能错位”的提示而没带 acknowledge_suspect → 409（不碰磁盘）
func (store *Store) ImportProfiles(ctx context.Context, input ProfileImportInput) (ProfileImportResult, error) {
	mode := input.Mode
	if mode == "" {
		mode = modeMerge
	}
	if mode != modeMerge && mode != modeFillEmpty {
		return ProfileImportResult{}, badRequest("mode 只能是 merge 或 fill_empty")
	}
	if len(input.Sources) == 0 {
		return ProfileImportResult{}, badRequest("没有要导入的内容")
	}
	parsed, err := parseImportSources(input.Sources)
	if err != nil {
		return ProfileImportResult{}, err
	}
	fillEmpty := mode == modeFillEmpty
	now := store.now().UTC()
	if input.DryRun {
		current, revision, err := store.readProfiles(ctx)
		if err != nil {
			return ProfileImportResult{}, err
		}
		result, _, _ := mergeImport(current, parsed, fillEmpty, now)
		result.DryRun, result.Mode, result.Revision = true, mode, revision
		return result, nil
	}
	if len(parsed.suspect) > 0 && !input.AcknowledgeSuspect {
		return ProfileImportResult{}, &Error{Status: http.StatusConflict, Code: CodeSuspectUnacked,
			Message: "预览里有“格式可能错位”的提示，请核对后勾选“我已核对”再确认导入"}
	}
	var result ProfileImportResult
	revision, err := store.mutateProfiles(ctx, true, func(current *profileDoc, revision string) (*profileDoc, bool, error) {
		computed, next, changed := mergeImport(current, parsed, fillEmpty, now)
		computed.Mode = mode
		if input.ExpectOverwrites != nil && *input.ExpectOverwrites != computed.Summary.Overwrites {
			computed.DryRun, computed.Revision = true, revision
			message := "资料在预览之后被修改过，覆盖已有值的条数有变化，请重新核对"
			if base, _ := splitRevision(input.BaseRevision); input.BaseRevision != "" && base == current.token {
				message = "确认的选项与预览不一致（覆盖已有值的条数不同），请重新核对"
			}
			return nil, false, &ConflictError{Err: conflict(message), Preview: computed}
		}
		if computed.Summary.OverLimit {
			return nil, false, &Error{Status: http.StatusConflict, Code: CodeFull,
				Message: fmt.Sprintf("导入后账号资料会超过 %d 条的上限，这一批没有导入", MaxProfiles)}
		}
		result = computed
		return next, changed, nil
	})
	if err != nil {
		return ProfileImportResult{}, err
	}
	result.Revision = revision
	return result, nil
}

// UpdateProfile 编辑或新增一条资料（只有明确清空的字段才清空；单条编辑不存快照，只靠 .bak）
func (store *Store) UpdateProfile(ctx context.Context, input ProfileUpdate) (ProfileUpdateResult, error) {
	edit, err := normalizeEdit(input)
	if err != nil {
		return ProfileUpdateResult{}, err
	}
	now := store.now().UTC()
	var result ProfileUpdateResult
	revision, err := store.mutateProfiles(ctx, false, func(current *profileDoc, _ string) (*profileDoc, bool, error) {
		computed, next, changed, err := applyEdit(current, edit, now)
		result = computed
		return next, changed, err
	})
	if err != nil {
		return ProfileUpdateResult{}, err
	}
	result.Revision = revision
	return result, nil
}

// normalizeEmailList 去掉空白、规范化并去重（保持顺序）：原始个数超过上限的两倍直接 413，去重后超过上限 413；不合法的邮箱原样（去首尾空白）放进 invalid
func normalizeEmailList(emails []string) (valid []string, invalid []string, err error) {
	if len(emails) > 2*MaxEmailsPerRequest {
		return nil, nil, tooLarge(fmt.Sprintf("一次最多 %d 个邮箱，请分批操作", MaxEmailsPerRequest))
	}
	seen := make(map[string]bool, len(emails))
	for _, text := range emails {
		text = trimSpace(text)
		if text == "" {
			continue
		}
		email, ok := NormalizeEmail(text)
		if !ok {
			if len(text) > maxEmailBytes {
				text = text[:maxEmailBytes]
			}
			invalid = append(invalid, text)
			continue
		}
		if !seen[email] {
			seen[email] = true
			valid = append(valid, email)
		}
	}
	if len(valid) > MaxEmailsPerRequest {
		return nil, nil, tooLarge(fmt.Sprintf("一次最多 %d 个邮箱，请分批操作", MaxEmailsPerRequest))
	}
	return valid, invalid, nil
}

// DeleteProfiles 删除资料（不存在的计 missing；删除前存快照；不动家庭组）
func (store *Store) DeleteProfiles(ctx context.Context, input ProfileDeleteInput) (ProfileDeleteResult, error) {
	emails, invalid, err := normalizeEmailList(input.Emails)
	if err != nil {
		return ProfileDeleteResult{}, err
	}
	if len(emails) == 0 && len(invalid) == 0 {
		return ProfileDeleteResult{}, badRequest("emails 不能为空")
	}
	var result ProfileDeleteResult
	revision, err := store.mutateProfiles(ctx, true, func(current *profileDoc, _ string) (*profileDoc, bool, error) {
		result = ProfileDeleteResult{Missing: len(invalid)}
		removed := map[string]bool{}
		for _, email := range emails {
			if current.get(email) != nil {
				removed[email] = true
				result.Deleted++
			} else {
				result.Missing++
			}
		}
		if len(removed) == 0 {
			return current, false, nil
		}
		return current.withRecords(nil, removed), true, nil
	})
	if err != nil {
		return ProfileDeleteResult{}, err
	}
	result.Revision = revision
	return result, nil
}

var revealFields = []string{fieldPassword, fieldRecovery, fieldTOTP, fieldNote, "totp_code", "line"}

// RevealProfile 取出明文（只返回要的字段；缺省为全部）。只要验证码（fields 恰好是 ["totp_code"]）时没有 2FA 404、算不出 409；
// 其他组合里 2FA 缺失或算不出只给 totp_code_error，其余字段照常返回
func (store *Store) RevealProfile(ctx context.Context, input RevealInput) (ProfileSecret, error) {
	fields := input.Fields
	if len(fields) == 0 {
		fields = revealFields
	}
	for _, field := range fields {
		if !slices.Contains(revealFields, field) {
			return ProfileSecret{}, badRequest("fields 里有不认识的字段")
		}
	}
	email, ok := NormalizeEmail(input.Email)
	if !ok {
		return ProfileSecret{}, invalidEmail("邮箱格式不对")
	}
	current, _, err := store.readProfiles(ctx)
	if err != nil {
		return ProfileSecret{}, err
	}
	record := current.get(email)
	if record == nil {
		return ProfileSecret{}, &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: "没有这个邮箱的账号资料"}
	}
	secret := ProfileSecret{Email: record.Email}
	codeOnly := len(fields) == 1 && fields[0] == "totp_code"
	for _, field := range fields {
		switch field {
		case fieldPassword:
			secret.Password = record.Password
		case fieldRecovery:
			secret.RecoveryEmail = record.RecoveryEmail
		case fieldTOTP:
			secret.TOTP = record.TOTP
		case fieldNote:
			secret.Note = record.Note
		case "line":
			secret.Line, secret.LineFormat, secret.LineLossy = buildLine(record, false)
		case "totp_code":
			now := store.now()
			code, next, remaining, valid := totpCodes(record.TOTP, now)
			switch {
			case record.TOTP == "" && codeOnly:
				return ProfileSecret{}, &Error{Status: http.StatusNotFound, Code: CodeTOTPMissing, Message: "这条资料没有 2FA 密钥"}
			case !valid && codeOnly:
				return ProfileSecret{}, &Error{Status: http.StatusConflict, Code: CodeTOTPInvalid, Message: "2FA 密钥格式不对，无法算出验证码"}
			case record.TOTP == "":
				secret.TOTPCodeError = "missing"
			case !valid:
				secret.TOTPCodeError = "invalid"
			default:
				secret.TOTPCode = &TOTPCode{Code: code, NextCode: next, Period: totpPeriod, Remaining: remaining, ServerTime: now.UTC(),
					Flags: slices.Clone(record.TOTPFlags)}
			}
		}
	}
	return secret, nil
}

// ExportProfiles 导出整行（emails 按请求顺序，all 按邮箱排序）；从不 404，不存在的列进 missing
func (store *Store) ExportProfiles(ctx context.Context, input ExportInput) (ProfileExport, error) {
	var emails, invalid []string
	if !input.All {
		var err error
		emails, invalid, err = normalizeEmailList(input.Emails)
		if err != nil {
			return ProfileExport{}, err
		}
		if len(emails) == 0 && len(invalid) == 0 {
			return ProfileExport{}, badRequest("emails 不能为空（或用 all 导出全部）")
		}
	}
	current, _, err := store.readProfiles(ctx)
	if err != nil {
		return ProfileExport{}, err
	}
	export := ProfileExport{Lines: []string{}, Missing: append([]string{}, invalid...), Lossy: []LossyLine{}}
	add := func(record *profile) {
		line, format, lossy := buildLine(record, input.IncludeNote)
		export.Lines = append(export.Lines, line)
		if format == lineFormatTab {
			export.TabLines++
		}
		if lossy != "" {
			export.Lossy = append(export.Lossy, LossyLine{Email: record.Email, Reason: lossy})
		}
	}
	if input.All {
		for index := range current.list {
			add(&current.list[index])
		}
	} else {
		for _, email := range emails {
			if record := current.get(email); record != nil {
				add(record)
			} else {
				export.Missing = append(export.Missing, email)
			}
		}
	}
	export.Count = len(export.Lines)
	return export, nil
}

func familyMode(mode string) (string, error) {
	switch mode {
	case "", familyModeBlocks:
		return familyModeBlocks, nil
	case familyModeLines:
		return familyModeLines, nil
	}
	return "", badRequest("mode 只能是 blocks 或 lines")
}

func familyConflict(message string, preview any) *ConflictError {
	return &ConflictError{Err: conflict(message), Preview: preview}
}

// baseChanged 判断确认请求带的 base_revision 里家庭组的部分是否与当前不同（省略时不检查）
func baseChanged(base string, current *familyDoc) bool {
	if base == "" {
		return false
	}
	_, families := splitRevision(base)
	return families != current.token
}

// ImportFamilies 预览或确认录入家庭组（逐块原子、按顺序；确认时整批一次写盘，写盘前存快照）
func (store *Store) ImportFamilies(ctx context.Context, input FamilyImportInput) (FamilyImportResult, error) {
	mode, err := familyMode(input.Mode)
	if err != nil {
		return FamilyImportResult{}, err
	}
	blocks, err := parseFamilyText(input.Text, mode)
	if err != nil {
		return FamilyImportResult{}, err
	}
	now := store.now().UTC()
	if input.DryRun {
		current, revision, err := store.readFamilies(ctx)
		if err != nil {
			return FamilyImportResult{}, err
		}
		result, _, _ := applyFamilyImport(current, blocks, now, dryRunGroupIDs())
		result.DryRun, result.Mode, result.Revision = true, mode, revision
		return result, nil
	}
	if suspect := familySuspect(blocks); len(suspect) > 0 && !input.AcknowledgeSuspect {
		return FamilyImportResult{}, &Error{Status: http.StatusConflict, Code: CodeSuspectUnacked,
			Message: "预览里有“可能少了空行”的提示，请核对后勾选“我已核对”再确认录入"}
	}
	var result FamilyImportResult
	revision, err := store.mutateFamilies(ctx, true, func(current *familyDoc, revision string) (*familyDoc, bool, error) {
		if baseChanged(input.BaseRevision, current) {
			preview, _, _ := applyFamilyImport(current, blocks, now, dryRunGroupIDs())
			preview.DryRun, preview.Mode, preview.Revision = true, mode, revision
			return nil, false, familyConflict("家庭组在预览之后被修改过，请重新核对", preview)
		}
		computed, next, changed := applyFamilyImport(current, blocks, now, store.newGroupID)
		computed.Mode = mode
		result = computed
		return next, changed, nil
	})
	if err != nil {
		return FamilyImportResult{}, err
	}
	result.Revision = revision
	return result, nil
}

// CreateFamily 新建家庭组（规则与录入的一块相同）
func (store *Store) CreateFamily(ctx context.Context, input FamilyCreate) (FamilyBlockResult, error) {
	membership, err := normalizeMembership(input.Primary, input.Members)
	if err != nil {
		return FamilyBlockResult{}, err
	}
	name, err := normalizeGroupName(input.Name)
	if err != nil {
		return FamilyBlockResult{}, err
	}
	if err := validateColor(input.Color); err != nil {
		return FamilyBlockResult{}, err
	}
	nameText := ""
	if name != nil {
		nameText = *name
	}
	now := store.now().UTC()
	if input.DryRun {
		current, revision, err := store.readFamilies(ctx)
		if err != nil {
			return FamilyBlockResult{}, err
		}
		result, _, _ := applyFamilyCreate(current, membership, nameText, input.Color, now, dryRunGroupIDs())
		result.DryRun, result.Revision = true, revision
		return result, nil
	}
	var result FamilyBlockResult
	revision, err := store.mutateFamilies(ctx, false, func(current *familyDoc, revision string) (*familyDoc, bool, error) {
		if baseChanged(input.BaseRevision, current) {
			preview, _, _ := applyFamilyCreate(current, membership, nameText, input.Color, now, dryRunGroupIDs())
			preview.DryRun, preview.Revision = true, revision
			return nil, false, familyConflict("家庭组在预览之后被修改过，请重新核对", preview)
		}
		computed, next, changed := applyFamilyCreate(current, membership, nameText, input.Color, now, store.newGroupID)
		result = computed
		return next, changed, nil
	})
	if err != nil {
		return FamilyBlockResult{}, err
	}
	result.Revision = revision
	return result, nil
}

func familyNotFound() *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeFamilyNotFound, Message: "家庭组不存在（可能已被解散）"}
}

// UpdateFamily 编辑家庭组：改名、改色、换主邮箱、改成员（dry_run 先看结果）
func (store *Store) UpdateFamily(ctx context.Context, id string, input FamilyUpdate) (FamilyBlockResult, error) {
	if !familyIDPattern.MatchString(id) {
		return FamilyBlockResult{}, familyNotFound()
	}
	name, err := normalizeGroupName(input.Name)
	if err != nil {
		return FamilyBlockResult{}, err
	}
	if err := validateColor(input.Color); err != nil {
		return FamilyBlockResult{}, err
	}
	edit := familyEdit{rev: input.Rev, name: name, color: input.Color}
	if input.Primary != nil {
		edit.primary, edit.hasPrimary = *input.Primary, true
	}
	if input.Members != nil {
		edit.members, edit.hasMembers = slices.Clone(*input.Members), true
	}
	now := store.now().UTC()
	if input.DryRun {
		current, revision, err := store.readFamilies(ctx)
		if err != nil {
			return FamilyBlockResult{}, err
		}
		result, _, _, err := applyFamilyEdit(current, id, edit, now, dryRunGroupIDs())
		if err != nil {
			return FamilyBlockResult{}, err
		}
		result.DryRun, result.Revision = true, revision
		return result, nil
	}
	var result FamilyBlockResult
	revision, err := store.mutateFamilies(ctx, false, func(current *familyDoc, revision string) (*familyDoc, bool, error) {
		if baseChanged(input.BaseRevision, current) {
			preview, _, _, err := applyFamilyEdit(current, id, edit, now, dryRunGroupIDs())
			if err != nil {
				return nil, false, err
			}
			preview.DryRun, preview.Revision = true, revision
			return nil, false, familyConflict("家庭组在预览之后被修改过，请重新核对", preview)
		}
		computed, next, changed, err := applyFamilyEdit(current, id, edit, now, store.newGroupID)
		result = computed
		return next, changed, err
	})
	if err != nil {
		return FamilyBlockResult{}, err
	}
	result.Revision = revision
	return result, nil
}

// DissolveFamily 解散家庭组（邮箱都变成非家庭组；rev 不符 409；解散前存快照）
func (store *Store) DissolveFamily(ctx context.Context, id string, rev *int64) (FamilyDissolveResult, error) {
	if !familyIDPattern.MatchString(id) {
		return FamilyDissolveResult{}, familyNotFound()
	}
	var result FamilyDissolveResult
	revision, err := store.mutateFamilies(ctx, true, func(current *familyDoc, _ string) (*familyDoc, bool, error) {
		index, ok := current.byID[id]
		if !ok {
			return nil, false, familyNotFound()
		}
		group := current.groups[index]
		if rev != nil && *rev != group.Rev {
			return nil, false, conflict("家庭组在打开之后被修改过，请刷新后再解散")
		}
		groups := make([]FamilyGroup, 0, len(current.groups)-1)
		for position, item := range current.groups {
			if position != index {
				item.Members = slices.Clone(item.Members)
				groups = append(groups, item)
			}
		}
		result = FamilyDissolveResult{Dissolved: true, Members: 1 + len(group.Members)}
		return newFamilyDoc(groups), true, nil
	})
	if err != nil {
		return FamilyDissolveResult{}, err
	}
	result.Revision = revision
	return result, nil
}

// StartEmpty 只在“主文件缺失而有备份”时可用：.bak 移进快照目录保留，写一个空的主文件
func (store *Store) StartEmpty(ctx context.Context, file string) (StartEmptyResult, error) {
	if file != fileProfiles && file != fileFamilies {
		return StartEmptyResult{}, badRequest("file 只能是 profiles 或 families")
	}
	if store.dir == "" {
		return StartEmptyResult{}, conflict("状态已变化（没有备份），请重新读取")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return StartEmptyResult{}, err
	}
	if _, err := os.Stat(store.dir); err != nil {
		return StartEmptyResult{}, conflict("状态已变化（没有备份），请重新读取")
	}
	lock, err := store.lockExclusive(ctx, labelOf(file))
	if err != nil {
		return StartEmptyResult{}, err
	}
	defer lock.Unlock()
	var moved string
	if file == fileProfiles {
		moved, err = store.startEmptyLocked(file, func(id string, at time.Time) []byte { return encodeProfiles(emptyProfileDoc(), id, at) })
	} else {
		moved, err = store.startEmptyLocked(file, func(id string, at time.Time) []byte { return encodeFamilies(emptyFamilyDoc(), id, at) })
	}
	if err != nil {
		return StartEmptyResult{}, err
	}
	// 重新载入（不写“已恢复可读”：从空开始另有一行 INFO）
	if file == fileProfiles {
		store.profiles.inError, store.profiles.loaded = false, false
	} else {
		store.families.inError, store.families.loaded = false, false
	}
	_ = lock.Unlock()
	if err := store.loadAll(ctx); err != nil {
		return StartEmptyResult{}, err
	}
	return StartEmptyResult{File: file, MovedBackup: moved, Revision: store.revisionLocked()}, nil
}
