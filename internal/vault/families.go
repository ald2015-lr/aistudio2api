package vault

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

// 家庭组块与组的结果
const (
	familyCreated        = "created"
	familyReplaced       = "replaced"
	familyPrimaryChanged = "primary_changed"
	familyUpdated        = "updated"
	familyUnchanged      = "unchanged"
	familyRejected       = "rejected"
	familyInvalid        = "invalid"
	familyModeBlocks     = "blocks"
	familyModeLines      = "lines"
)

// 家庭组的原因、提示与批级提示
const (
	familyReasonSingle          = "single_email"
	familyReasonTooLarge        = "block_too_large"
	familyReasonTokenNotEmail   = "token_not_email"
	familyReasonLooksProfile    = "looks_like_profile"
	familyReasonMemberIsPrimary = "member_is_primary"
	familyReasonPrimaryIsOther  = "primary_is_other_primary"
	familyReasonLimitGroups     = "limit_groups"
	familyReasonAmbiguous       = "ambiguous_block"
	familyWarnOverFive          = "over_five"
	familyWarnDuplicate         = "duplicate_in_block"
	familyWarnPrimaryInMembers  = "primary_in_members"
	familyWarnMissingBlank      = "maybe_missing_blank"
	familyWarnNameIgnored       = "name_ignored"
	familyWarnCommentSplit      = "comment_split"
	familyHintLooksLikeName     = "looks_like_name"
	familyBatchPrimaryRepeated  = "primary_repeated"
	familyBatchEmailInBlocks    = "email_in_blocks"
)

var familyIDPattern = regexp.MustCompile(`^fam-[0-9a-f]{12}$`)

// familyRef 为一个邮箱所在的组
type familyRef struct {
	index   int
	primary bool
}

// familyDoc 为全部家庭组（按 seq 排序，载入或写入后不再修改）
type familyDoc struct {
	docMeta
	groups  []FamilyGroup
	byID    map[string]int
	byEmail map[string]familyRef
}

func (doc *familyDoc) meta() *docMeta { return &doc.docMeta }

func newFamilyDoc(groups []FamilyGroup) *familyDoc {
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Seq < groups[j].Seq })
	doc := &familyDoc{groups: groups, byID: make(map[string]int, len(groups)), byEmail: map[string]familyRef{}}
	for index := range groups {
		if groups[index].Members == nil {
			groups[index].Members = []string{}
		}
		doc.byID[groups[index].ID] = index
		doc.byEmail[groups[index].Primary] = familyRef{index: index, primary: true}
		for _, member := range groups[index].Members {
			doc.byEmail[member] = familyRef{index: index}
		}
	}
	return doc
}

func emptyFamilyDoc() *familyDoc { return newFamilyDoc(nil) }

func familyRecords(doc *familyDoc) int { return len(doc.groups) }

// familiesFile 为 families.json 的格式（版本 1）
type familiesFile struct {
	Version int           `json:"version"`
	SavedAt time.Time     `json:"saved_at"`
	WriteID string        `json:"write_id"`
	Groups  []FamilyGroup `json:"groups"`
}

// decodeFamilies 载入并校验不变式：id 唯一且格式正确；主邮箱与成员都是合法邮箱；同一邮箱在全部组里最多出现一次；颜色 0–11；seq、rev ≥ 0。
// 人工编辑造出的空成员组照常载入
func decodeFamilies(data []byte) (*familyDoc, *loadProblem) {
	if problem := checkJSON(data); problem != nil {
		return nil, problem
	}
	var file familiesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, jsonProblem(err)
	}
	if file.Version != fileVersion {
		return nil, versionProblem(file.Version)
	}
	ids := map[string]bool{}
	emails := map[string]bool{}
	for index := range file.Groups {
		group := &file.Groups[index]
		if !familyIDPattern.MatchString(group.ID) {
			return nil, invalidProblem(index, "家庭组编号不合法")
		}
		if ids[group.ID] {
			return nil, invalidProblem(index, "家庭组编号重复")
		}
		ids[group.ID] = true
		if group.Color < 0 || group.Color >= PaletteSize {
			return nil, invalidProblem(index, "颜色编号不在 0–11")
		}
		if group.Seq < 0 || group.Rev < 0 {
			return nil, invalidProblem(index, "seq 或 rev 为负数")
		}
		primary, ok := NormalizeEmail(group.Primary)
		if !ok {
			return nil, invalidProblem(index, "主邮箱不合法")
		}
		group.Primary = primary
		if emails[primary] {
			return nil, invalidProblem(index, "同一邮箱出现在多个家庭组里")
		}
		emails[primary] = true
		for position, member := range group.Members {
			normalized, ok := NormalizeEmail(member)
			if !ok {
				return nil, invalidProblem(index, "成员邮箱不合法")
			}
			if emails[normalized] {
				return nil, invalidProblem(index, "同一邮箱出现在多个家庭组里")
			}
			emails[normalized] = true
			group.Members[position] = normalized
		}
	}
	doc := newFamilyDoc(file.Groups)
	doc.writeID = file.WriteID
	return doc, nil
}

func encodeFamilies(doc *familyDoc, writeID string, savedAt time.Time) []byte {
	groups := doc.groups
	if groups == nil {
		groups = []FamilyGroup{}
	}
	data, _ := json.MarshalIndent(familiesFile{Version: fileVersion, SavedAt: savedAt.UTC(), WriteID: writeID, Groups: groups}, "", "  ")
	return append(data, '\n')
}

func cloneGroup(group FamilyGroup) *FamilyGroup {
	copied := group
	copied.Members = slices.Clone(group.Members)
	if copied.Members == nil {
		copied.Members = []string{}
	}
	return &copied
}

// ---- 录入文本的分块与解析 ----

type familyLine struct {
	number int
	text   string
	kind   lineKind
}

// emailToken 为块里的一个邮箱与它在文本里的位置
type emailToken struct {
	email    string
	line     int
	position int
}

// familyBlock 为录入文本里的一块（解析之后、应用之前）
type familyBlock struct {
	index       int
	firstLine   int
	lastLine    int
	lines       []familyLine
	name        string
	commentLine int
	primary     string
	members     []string
	tokens      map[string]emailToken
	warnings    []string
	status      string
	reason      string
	hint        string
	detail      *BlockDetail
}

func (block *familyBlock) accepted() bool { return block.status == "" }

// parseFamilyText 分块并解析每一块；超出上限时 413
func parseFamilyText(text string, mode string) ([]*familyBlock, error) {
	if len(text) > MaxImportBytes {
		return nil, tooLarge("录入的文本超过 4 MB，请分批录入")
	}
	text = strings.TrimPrefix(text, "\uFEFF")
	rawLines := strings.Split(text, "\n")
	if rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	lines := make([]familyLine, 0, len(rawLines))
	nonBlank := 0
	for index, raw := range rawLines {
		line := familyLine{number: index + 1, text: trimSpace(strings.TrimSuffix(raw, "\r")), kind: lineData}
		switch {
		case line.text == "":
			line.kind = lineBlank
		case isComment(line.text):
			line.kind = lineComment
		}
		if line.kind != lineBlank {
			nonBlank++
			if nonBlank > MaxImportLines {
				return nil, tooLarge(fmt.Sprintf("一次最多录入 %d 行，请分批录入", MaxImportLines))
			}
		}
		lines = append(lines, line)
	}
	var blocks []*familyBlock
	for index := 0; index < len(lines); {
		if lines[index].kind != lineData {
			index++
			continue
		}
		end := index + 1
		if mode == familyModeBlocks {
			for end < len(lines) && lines[end].kind == lineData {
				end++
			}
		}
		block := &familyBlock{index: len(blocks) + 1, firstLine: lines[index].number, lastLine: lines[end-1].number, lines: lines[index:end]}
		block.name = blockNameCandidate(lines, index)
		if mode == familyModeBlocks {
			block.commentLine = adjacentEmailComment(lines, index, end)
			if block.commentLine > 0 {
				block.warnings = append(block.warnings, familyWarnCommentSplit)
			}
		}
		blocks = append(blocks, block)
		if len(blocks) > MaxFamilyBlocks {
			return nil, tooLarge(fmt.Sprintf("一次最多录入 %d 块，请分批录入", MaxFamilyBlocks))
		}
		index = end
	}
	for _, block := range blocks {
		parseFamilyBlock(block, mode)
	}
	return blocks, nil
}

// blockNameCandidate 为紧挨在块第一行上面的注释行（# 名称）：去掉 # 与首尾空白后 1–40 个字符、不含控制字符与 @
func blockNameCandidate(lines []familyLine, first int) string {
	if first == 0 || lines[first-1].kind != lineComment {
		return ""
	}
	name := trimSpace(strings.TrimLeft(lines[first-1].text, "#＃"))
	if name == "" || strings.ContainsRune(name, '@') || hasControl(name) || strings.ContainsRune(name, '\t') ||
		len([]rune(name)) > maxGroupNameRunes {
		return ""
	}
	return name
}

// adjacentEmailComment 返回紧挨着块（上方或下方的连续注释行里）含 @ 的注释行行号：多半是被注释掉的邮箱把一组分成了两块
func adjacentEmailComment(lines []familyLine, start, end int) int {
	for index := start - 1; index >= 0 && lines[index].kind == lineComment; index-- {
		if strings.ContainsRune(lines[index].text, '@') {
			return lines[index].number
		}
	}
	for index := end; index < len(lines) && lines[index].kind == lineComment; index++ {
		if strings.ContainsRune(lines[index].text, '@') {
			return lines[index].number
		}
	}
	return 0
}

// splitFamilyTokens 按行内分隔切分：≥ 4 个单位的破折号串、Tab、逗号、顿号、分号（半角与全角）与任意空白
func splitFamilyTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	runs := dashRuns(text)
	runIndex := 0
	for index, r := range text {
		for runIndex < len(runs) && runs[runIndex].end <= index {
			runIndex++
		}
		if runIndex < len(runs) && index >= runs[runIndex].start && index < runs[runIndex].end && runs[runIndex].units >= 4 {
			flush()
			continue
		}
		if unicode.IsSpace(r) || strings.ContainsRune(",，、;；", r) {
			flush()
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return tokens
}

// parseFamilyBlock 解析一块：每个 token 必须是邮箱，第一个是主邮箱，其余是成员（块内重复的与主邮箱重复的去掉并提示）
func parseFamilyBlock(block *familyBlock, mode string) {
	block.tokens = map[string]emailToken{}
	lineTokens := make([][]string, len(block.lines))
	multi := 0
	for lineIndex, line := range block.lines {
		lineTokens[lineIndex] = splitFamilyTokens(line.text)
		if len(lineTokens[lineIndex]) >= 2 {
			multi++
		}
	}
	for lineIndex, tokens := range lineTokens {
		for position, token := range tokens {
			email, ok := NormalizeEmail(token)
			if !ok {
				block.status, block.reason = familyInvalid, familyReasonTokenNotEmail
				if hasLongDashRun(block.lines[lineIndex].text) {
					block.reason = familyReasonLooksProfile
				}
				block.detail = &BlockDetail{Line: block.lines[lineIndex].number, Position: position + 1}
				if lineIndex == 0 && len(block.lines) >= 2 && !strings.ContainsAny(foldWidth(block.lines[0].text), "@") && restAreEmails(lineTokens[1:]) {
					block.hint = familyHintLooksLikeName
				}
				block.primary, block.members = "", nil
				return
			}
			if block.primary == "" {
				block.primary = email
				block.tokens[email] = emailToken{email: email, line: block.lines[lineIndex].number, position: position + 1}
				continue
			}
			if email == block.primary {
				block.warnings = appendOnce(block.warnings, familyWarnPrimaryInMembers)
				continue
			}
			if _, seen := block.tokens[email]; seen {
				block.warnings = appendOnce(block.warnings, familyWarnDuplicate)
				continue
			}
			block.tokens[email] = emailToken{email: email, line: block.lines[lineIndex].number, position: position + 1}
			block.members = append(block.members, email)
		}
	}
	total := 1 + len(block.members)
	switch {
	case mode == familyModeBlocks && multi >= 2:
		block.status, block.reason = familyRejected, familyReasonAmbiguous
	case len(block.members) == 0:
		block.status, block.reason = familyRejected, familyReasonSingle
	case total > MaxGroupEmails:
		block.status, block.reason = familyRejected, familyReasonTooLarge
	}
	if len(block.members) > 5 {
		block.warnings = append(block.warnings, familyWarnOverFive)
	}
	if mode == familyModeBlocks && len(block.lines) >= 2 && total > 6 && total%6 == 0 {
		block.warnings = append(block.warnings, familyWarnMissingBlank)
	}
}

func restAreEmails(lines [][]string) bool {
	for _, tokens := range lines {
		for _, token := range tokens {
			if _, ok := NormalizeEmail(token); !ok {
				return false
			}
		}
	}
	return true
}

// batchWarnings 为批级提示：同一个主邮箱写在两块里、同一个邮箱出现在两块里（只看解析通过的块）
func batchWarnings(blocks []*familyBlock) []BatchWarning {
	type occurrence struct {
		blocks      []int
		onlyPrimary bool
	}
	seen := map[string]*occurrence{}
	var order []string
	note := func(email string, block int, primary bool) {
		entry := seen[email]
		if entry == nil {
			entry = &occurrence{onlyPrimary: true}
			seen[email] = entry
			order = append(order, email)
		}
		if !slices.Contains(entry.blocks, block) {
			entry.blocks = append(entry.blocks, block)
		}
		entry.onlyPrimary = entry.onlyPrimary && primary
	}
	for _, block := range blocks {
		if !block.accepted() {
			continue
		}
		note(block.primary, block.index, true)
		for _, member := range block.members {
			note(member, block.index, false)
		}
	}
	warnings := []BatchWarning{}
	for _, email := range order {
		entry := seen[email]
		if len(entry.blocks) < 2 {
			continue
		}
		code := familyBatchEmailInBlocks
		if entry.onlyPrimary {
			code = familyBatchPrimaryRepeated
		}
		warnings = append(warnings, BatchWarning{Code: code, Email: email, Blocks: entry.blocks})
	}
	return warnings
}

// ---- 应用：冲突规则、移动、自动解散、颜色 ----

// familyWork 为应用家庭组操作时的可变副本（从 doc 深拷贝，doc 本身不变）
// groups 按 seq 升序（载入时排好，新建的组 seq 最大、追加在末尾），解散的组留在原位、以 byID 不再指向它为准；
// colors 为还在的组各颜色的组数（新建、解散、改色时增量维护），新建一个组的选色与找 seq 最大的组都不必遍历全部组
type familyWork struct {
	groups  []*FamilyGroup
	byID    map[string]*FamilyGroup
	owner   map[string]*FamilyGroup
	colors  [PaletteSize]int
	maxSeq  int64
	now     time.Time
	newID   func(exists func(string) bool) string
	changed bool
}

func newFamilyWork(doc *familyDoc, now time.Time, newID func(exists func(string) bool) string) *familyWork {
	work := &familyWork{byID: make(map[string]*FamilyGroup, len(doc.groups)), owner: map[string]*FamilyGroup{}, now: now, newID: newID}
	for _, group := range doc.groups {
		copied := cloneGroup(group)
		work.groups = append(work.groups, copied)
		work.byID[copied.ID] = copied
		work.colors[copied.Color]++
		work.owner[copied.Primary] = copied
		for _, member := range copied.Members {
			work.owner[member] = copied
		}
		if copied.Seq > work.maxSeq {
			work.maxSeq = copied.Seq
		}
	}
	return work
}

// result 为应用之后还在的组（按 seq）
func (work *familyWork) result() *familyDoc {
	groups := make([]FamilyGroup, 0, len(work.byID))
	for _, group := range work.groups {
		if work.byID[group.ID] == group {
			groups = append(groups, *group)
		}
	}
	return newFamilyDoc(groups)
}

// last 为 seq 最大的还在的组（显示时紧挨在新组前面）：groups 按 seq 升序，从末尾往前找第一个还在的组，
// 顺手去掉末尾已解散的组（result 本来就跳过它们），每个解散的组最多被跳过一次
func (work *familyWork) last() *FamilyGroup {
	for len(work.groups) > 0 {
		group := work.groups[len(work.groups)-1]
		if work.byID[group.ID] == group {
			return group
		}
		work.groups = work.groups[:len(work.groups)-1]
	}
	return nil
}

// recolor 改一个还在的组的颜色并更新颜色计数
func (work *familyWork) recolor(group *FamilyGroup, color int) {
	work.colors[group.Color]--
	group.Color = color
	work.colors[color]++
}

// autoColor 为新组选颜色：避开 seq 最大的组的颜色，在其余颜色里取使用次数最少的，从“那个组的颜色 + 1”开始按编号循环遇到的第一个；
// 没有组时为 0。颜色没用完时选未用的，都用过时按编号轮转，解散留下的空缺优先补上
func (work *familyWork) autoColor() int {
	last := work.last()
	if last == nil {
		return 0
	}
	counts := &work.colors
	best, bestCount := -1, 0
	for step := 1; step <= PaletteSize; step++ {
		color := (last.Color + step) % PaletteSize
		if color == last.Color {
			continue
		}
		if best < 0 || counts[color] < bestCount {
			best, bestCount = color, counts[color]
		}
	}
	return best
}

func (work *familyWork) removeMember(group *FamilyGroup, email string) {
	group.Members = slices.DeleteFunc(group.Members, func(member string) bool { return member == email })
	group.Rev++
	group.UpdatedAt = work.now
	delete(work.owner, email)
}

func (work *familyWork) dissolve(group *FamilyGroup) {
	delete(work.byID, group.ID)
	work.colors[group.Color]--
	delete(work.owner, group.Primary)
	for _, member := range group.Members {
		delete(work.owner, member)
	}
	work.changed = true
}

// blockTarget 为一块的目标组：主邮箱已是某组主邮箱 → 那个组；主邮箱是某组成员、且那个组的原主邮箱写在成员里 → 那个组（换主）；
// 否则新建（nil）
func (work *familyWork) blockTarget(primary string, members []string) *FamilyGroup {
	group := work.owner[primary]
	if group == nil {
		return nil
	}
	if group.Primary == primary || slices.Contains(members, group.Primary) {
		return group
	}
	return nil
}

// memberPrimaryConflict 找出写成成员的别的组的主邮箱
func (work *familyWork) memberPrimaryConflict(target *FamilyGroup, members []string) *FamilyGroup {
	for _, member := range members {
		if group := work.owner[member]; group != nil && group.Primary == member && group != target {
			return group
		}
	}
	return nil
}

// assign 让 target（nil 为新建）成为“primary + members”：其他组里的这些邮箱移过来（被移空的组自动解散），
// target 原有、不在新名单里的移出。调用前已检查过拒绝条件
func (work *familyWork) assign(target *FamilyGroup, primary string, members []string, name string, color *int, result *FamilyBlockResult) *FamilyGroup {
	if target != nil && target.Primary == primary && sameStringSet(target.Members, members) {
		result.Status = familyUnchanged
		return target
	}
	for _, email := range append([]string{primary}, members...) {
		from := work.owner[email]
		if from == nil || from == target {
			continue
		}
		work.removeMember(from, email)
		result.Moved = append(result.Moved, MovedEmail{Email: email, FromGroup: from.ID, FromPrimary: from.Primary})
		if len(from.Members) == 0 {
			result.Dissolved = append(result.Dissolved, DissolvedGroup{GroupID: from.ID, Primary: from.Primary, Name: from.Name, Color: from.Color})
			work.dissolve(from)
		}
	}
	work.changed = true
	if target == nil {
		groupColor := work.autoColor()
		if color != nil {
			groupColor = *color
		}
		work.maxSeq++
		group := &FamilyGroup{
			ID: work.newID(func(id string) bool { return work.byID[id] != nil }), Seq: work.maxSeq, Rev: 1, Name: name, Color: groupColor,
			Primary: primary, Members: slices.Clone(members), CreatedAt: work.now, UpdatedAt: work.now,
		}
		work.groups = append(work.groups, group)
		work.byID[group.ID] = group
		work.colors[groupColor]++
		work.owner[primary] = group
		for _, member := range members {
			work.owner[member] = group
		}
		result.Status = familyCreated
		return group
	}
	newSet := map[string]bool{primary: true}
	for _, member := range members {
		newSet[member] = true
	}
	oldSet := map[string]bool{target.Primary: true}
	for _, email := range append([]string{target.Primary}, target.Members...) {
		oldSet[email] = true
		if !newSet[email] {
			result.Removed = append(result.Removed, email)
			delete(work.owner, email)
		}
	}
	for _, email := range append([]string{primary}, members...) {
		if !oldSet[email] {
			result.Added = append(result.Added, email)
		}
	}
	result.Status = familyReplaced
	if target.Primary != primary {
		result.Status = familyPrimaryChanged
	}
	target.Primary, target.Members = primary, slices.Clone(members)
	target.Rev++
	target.UpdatedAt = work.now
	work.owner[primary] = target
	for _, member := range members {
		work.owner[member] = target
	}
	return target
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	set := make(map[string]bool, len(left))
	for _, item := range left {
		set[item] = true
	}
	for _, item := range right {
		if !set[item] {
			return false
		}
	}
	return true
}

// describe 把组的身份写进结果
func describe(result *FamilyBlockResult, group *FamilyGroup) {
	color := group.Color
	result.GroupID, result.Color, result.Name = group.ID, &color, group.Name
	result.Primary, result.Members = group.Primary, slices.Clone(group.Members)
}

// rejectMemberIsPrimary 为“写成成员的是别的组的主邮箱”：detail 给出那个邮箱、它的组与它在文本里的位置
func rejectMemberIsPrimary(result *FamilyBlockResult, conflictGroup *FamilyGroup, tokens map[string]emailToken) {
	result.Status, result.Reason = familyRejected, familyReasonMemberIsPrimary
	detail := &BlockDetail{Email: conflictGroup.Primary, GroupPrimary: conflictGroup.Primary, GroupName: conflictGroup.Name}
	if token, ok := tokens[conflictGroup.Primary]; ok {
		detail.Line, detail.Position = token.line, token.position
	}
	result.Detail = detail
}

// applyBlock 按冲突规则应用一块（录入与新建共用）；被拒绝的块不改动任何组
func (work *familyWork) applyBlock(result *FamilyBlockResult, primary string, members []string, tokens map[string]emailToken, name string, color *int) {
	target := work.blockTarget(primary, members)
	if conflictGroup := work.memberPrimaryConflict(target, members); conflictGroup != nil {
		rejectMemberIsPrimary(result, conflictGroup, tokens)
		return
	}
	if target == nil && len(work.byID) >= MaxGroups {
		result.Status, result.Reason = familyRejected, familyReasonLimitGroups
		return
	}
	if target != nil && name != "" && name != target.Name {
		result.Warnings = appendOnce(result.Warnings, familyWarnNameIgnored)
	}
	group := work.assign(target, primary, members, name, color, result)
	describe(result, group)
}

// summarizeFamilies 汇总各块的结果
func summarizeFamilies(blocks []FamilyBlockResult) FamilySummary {
	summary := FamilySummary{Blocks: len(blocks), Warnings: map[string]int{}, Suspect: []string{}}
	for _, block := range blocks {
		switch block.Status {
		case familyCreated:
			summary.Created++
		case familyReplaced:
			summary.Replaced++
		case familyPrimaryChanged:
			summary.PrimaryChanged++
		case familyUnchanged:
			summary.Unchanged++
		case familyRejected:
			summary.Rejected++
		case familyInvalid:
			summary.Invalid++
		}
		summary.Moved += len(block.Moved)
		summary.Removed += len(block.Removed)
		summary.Dissolved += len(block.Dissolved)
		for _, warning := range block.Warnings {
			summary.Warnings[warning]++
		}
	}
	return summary
}

// familySuspect 为录入的“可疑”提示（只看文本）：解析通过的块里有“可能少了空行”
func familySuspect(blocks []*familyBlock) []string {
	for _, block := range blocks {
		if block.accepted() && slices.Contains(block.warnings, familyWarnMissingBlank) {
			return []string{familyWarnMissingBlank}
		}
	}
	return []string{}
}

// applyFamilyImport 在 doc 上按顺序应用各块（逐块原子：被拒绝或无法识别的块不改动；后面的块看到前面块的结果）
func applyFamilyImport(doc *familyDoc, blocks []*familyBlock, now time.Time, newID func(exists func(string) bool) string) (FamilyImportResult, *familyDoc, bool) {
	work := newFamilyWork(doc, now, newID)
	results := make([]FamilyBlockResult, 0, len(blocks))
	for _, block := range blocks {
		result := FamilyBlockResult{Index: block.index, FirstLine: block.firstLine, LastLine: block.lastLine, Warnings: slices.Clone(block.warnings),
			CommentLine: block.commentLine}
		if !block.accepted() {
			result.Status, result.Reason, result.Hint, result.Detail = block.status, block.reason, block.hint, block.detail
			if block.status == familyRejected {
				result.Primary, result.Members = block.primary, slices.Clone(block.members)
			}
			results = append(results, result)
			continue
		}
		result.Primary, result.Members = block.primary, slices.Clone(block.members)
		work.applyBlock(&result, block.primary, block.members, block.tokens, block.name, nil)
		results = append(results, result)
	}
	imported := FamilyImportResult{Summary: summarizeFamilies(results), BatchWarnings: batchWarnings(blocks), Blocks: results}
	imported.Summary.Suspect = familySuspect(blocks)
	if !work.changed {
		return imported, doc, false
	}
	return imported, work.result(), true
}

// familyMembership 为新建与编辑接口校验过的主邮箱与成员（去掉重复与主邮箱）
type familyMembership struct {
	primary  string
	members  []string
	tokens   map[string]emailToken
	warnings []string
}

func normalizeMembership(primaryText string, memberTexts []string) (familyMembership, error) {
	primary, ok := NormalizeEmail(primaryText)
	if !ok {
		return familyMembership{}, invalidEmail("主邮箱格式不对")
	}
	membership := familyMembership{primary: primary, tokens: map[string]emailToken{primary: {email: primary}}}
	for position, text := range memberTexts {
		if trimSpace(text) == "" {
			continue
		}
		email, ok := NormalizeEmail(text)
		if !ok {
			return familyMembership{}, invalidEmail(fmt.Sprintf("第 %d 个成员的邮箱格式不对", position+1))
		}
		if email == primary {
			membership.warnings = appendOnce(membership.warnings, familyWarnPrimaryInMembers)
			continue
		}
		if _, seen := membership.tokens[email]; seen {
			membership.warnings = appendOnce(membership.warnings, familyWarnDuplicate)
			continue
		}
		membership.tokens[email] = emailToken{email: email, position: position + 1}
		membership.members = append(membership.members, email)
	}
	if len(membership.members) == 0 {
		return familyMembership{}, &Error{Status: http.StatusBadRequest, Code: CodeFamilyEmpty, Message: "家庭组至少要有一个成员；要解散请用解散"}
	}
	return membership, nil
}

// normalizeGroupName 校验名称：去首尾空白后最多 40 个字符、不含控制字符与 Tab
func normalizeGroupName(name *string) (*string, error) {
	if name == nil {
		return nil, nil
	}
	value := trimSpace(*name)
	if hasControl(value) || strings.ContainsRune(value, '\t') || len([]rune(value)) > maxGroupNameRunes {
		return nil, invalidField(fmt.Sprintf("家庭组名称最多 %d 个字符，不能含控制字符", maxGroupNameRunes))
	}
	return &value, nil
}

func validateColor(color *int) error {
	if color != nil && (*color < 0 || *color >= PaletteSize) {
		return invalidField(fmt.Sprintf("颜色编号应为 0–%d", PaletteSize-1))
	}
	return nil
}

// applyFamilyCreate 新建家庭组（规则与录入的一块相同：主邮箱已是某组主邮箱时按替换处理）
func applyFamilyCreate(doc *familyDoc, membership familyMembership, name string, color *int, now time.Time, newID func(exists func(string) bool) string) (FamilyBlockResult, *familyDoc, bool) {
	work := newFamilyWork(doc, now, newID)
	result := FamilyBlockResult{Index: 1, Primary: membership.primary, Members: slices.Clone(membership.members), Warnings: slices.Clone(membership.warnings)}
	if 1+len(membership.members) > MaxGroupEmails {
		result.Status, result.Reason = familyRejected, familyReasonTooLarge
		return result, doc, false
	}
	if len(membership.members) > 5 {
		result.Warnings = append(result.Warnings, familyWarnOverFive)
	}
	work.applyBlock(&result, membership.primary, membership.members, membership.tokens, name, color)
	if result.Status == familyRejected || !work.changed {
		return result, doc, false
	}
	next := work.result()
	attachGroup(&result, next)
	return result, next, true
}

func attachGroup(result *FamilyBlockResult, doc *familyDoc) {
	if index, ok := doc.byID[result.GroupID]; ok {
		group := doc.groups[index]
		group.Members = slices.Clone(group.Members)
		result.Group = &group
	}
}

// familyEdit 为校验过的编辑请求
type familyEdit struct {
	rev        *int64
	name       *string
	color      *int
	primary    string
	hasPrimary bool
	members    []string
	hasMembers bool
}

// applyFamilyEdit 编辑一个组：目标组固定为这个 id。新主邮箱是别的组的主邮箱 → 拒绝；成员里有别的组的主邮箱 → 拒绝；
// 结果成员为空 → 400；其余按冲突规则（别组成员移入、被移空的组自动解散；本组不在新名单里的移出）
func applyFamilyEdit(doc *familyDoc, id string, edit familyEdit, now time.Time, newID func(exists func(string) bool) string) (FamilyBlockResult, *familyDoc, bool, error) {
	index, ok := doc.byID[id]
	if !ok {
		return FamilyBlockResult{}, nil, false, &Error{Status: http.StatusNotFound, Code: CodeFamilyNotFound, Message: "家庭组不存在（可能已被解散）"}
	}
	if edit.rev != nil && *edit.rev != doc.groups[index].Rev {
		return FamilyBlockResult{}, nil, false, conflict("家庭组在打开编辑之后被修改过，请刷新后重新编辑")
	}
	work := newFamilyWork(doc, now, newID)
	target := work.byID[id]
	result := FamilyBlockResult{Index: 1}
	describe(&result, target)
	if edit.hasPrimary || edit.hasMembers {
		primary, members := target.Primary, slices.Clone(target.Members)
		if edit.hasPrimary {
			primary = edit.primary
		}
		if edit.hasMembers {
			members = edit.members
		} else if edit.hasPrimary && primary != target.Primary {
			// 只换主邮箱：新主邮箱是本组成员时与原主邮箱互换位置；否则原主邮箱不再出现，按移出处理
			if position := slices.Index(members, primary); position >= 0 {
				members[position] = target.Primary
			}
		}
		membership, err := normalizeMembership(primary, members)
		if err != nil {
			return FamilyBlockResult{}, nil, false, err
		}
		result.Primary, result.Members, result.Warnings = membership.primary, slices.Clone(membership.members), membership.warnings
		if other := work.owner[membership.primary]; other != nil && other != target && other.Primary == membership.primary {
			result.Status, result.Reason = familyRejected, familyReasonPrimaryIsOther
			result.Detail = &BlockDetail{Email: other.Primary, GroupPrimary: other.Primary, GroupName: other.Name}
			return result, doc, false, nil
		}
		if conflictGroup := work.memberPrimaryConflict(target, membership.members); conflictGroup != nil {
			rejectMemberIsPrimary(&result, conflictGroup, membership.tokens)
			return result, doc, false, nil
		}
		if 1+len(membership.members) > MaxGroupEmails {
			result.Status, result.Reason = familyRejected, familyReasonTooLarge
			return result, doc, false, nil
		}
		if len(membership.members) > 5 {
			result.Warnings = append(result.Warnings, familyWarnOverFive)
		}
		work.assign(target, membership.primary, membership.members, "", nil, &result)
	} else {
		result.Status = familyUnchanged
	}
	metaChanged := false
	if edit.name != nil && *edit.name != target.Name {
		target.Name, metaChanged, result.Renamed = *edit.name, true, true
	}
	if edit.color != nil && *edit.color != target.Color {
		work.recolor(target, *edit.color)
		metaChanged, result.Recolored = true, true
	}
	if metaChanged {
		work.changed = true
		if result.Status == familyUnchanged {
			result.Status = familyUpdated
			target.Rev++
			target.UpdatedAt = now
		}
	}
	describe(&result, target)
	if !work.changed {
		return result, doc, false, nil
	}
	next := work.result()
	attachGroup(&result, next)
	return result, next, true, nil
}
