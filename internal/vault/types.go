package vault

import "time"

// 上限（导入、存储、请求）
const (
	MaxImportLines      = 20000
	MaxImportBytes      = 4 << 20
	MaxImportSources    = 50
	MaxLineBytes        = 4096
	MaxPasswordBytes    = 512
	MaxNoteBytes        = 2048
	MaxProfiles         = 50000
	MaxGroups           = 20000
	MaxGroupEmails      = 20
	MaxEmailsPerRequest = 20000
	MaxFamilyBlocks     = 5000
	RequestBytes        = 8 << 20
	MaxSnapshots        = 10
	PaletteSize         = 12
	maxSourceNameRunes  = 100
	maxGroupNameRunes   = 40
	maxEmailBytes       = 254
)

// 文件名（View.Errors 与 start-empty 的 file）
const (
	fileProfiles = "profiles"
	fileFamilies = "families"
)

// Limits 为管理页显示与前端预检用的上限
type Limits struct {
	ImportLines      int `json:"import_lines"`
	ImportBytes      int `json:"import_bytes"`
	ImportSources    int `json:"import_sources"`
	LineBytes        int `json:"line_bytes"`
	PasswordBytes    int `json:"password_bytes"`
	NoteBytes        int `json:"note_bytes"`
	Profiles         int `json:"profiles"`
	Groups           int `json:"groups"`
	GroupEmails      int `json:"group_emails"`
	EmailsPerRequest int `json:"emails_per_request"`
	RequestBytes     int `json:"request_bytes"`
	Snapshots        int `json:"snapshots"`
}

func currentLimits() Limits {
	return Limits{
		ImportLines: MaxImportLines, ImportBytes: MaxImportBytes, ImportSources: MaxImportSources, LineBytes: MaxLineBytes,
		PasswordBytes: MaxPasswordBytes, NoteBytes: MaxNoteBytes, Profiles: MaxProfiles, Groups: MaxGroups, GroupEmails: MaxGroupEmails,
		EmailsPerRequest: MaxEmailsPerRequest, RequestBytes: RequestBytes, Snapshots: MaxSnapshots,
	}
}

// View 为 GET /api/vault 的返回：资料只有打码视图，家庭组完整（邮箱不是秘密）
type View struct {
	// Revision 为“资料文件标记.家庭组文件标记”：文件内容变了标记就变；没有文件为 0，文件出错为 e…，主文件缺失而有备份为 m…
	Revision string `json:"revision"`
	// Unchanged 表示请求带的 since 等于当前 revision，其余字段省略
	Unchanged  bool          `json:"unchanged,omitempty"`
	Dir        string        `json:"dir,omitempty"`
	Persistent bool          `json:"persistent"`
	Profiles   []ProfileView `json:"profiles"`
	Families   []FamilyGroup `json:"families"`
	Errors     []FileError   `json:"errors,omitempty"`
	Backups    []BackupInfo  `json:"backups,omitempty"`
	Limits     *Limits       `json:"limits,omitempty"`
	Palette    int           `json:"palette,omitempty"`
}

// ProfileView 为一条资料的打码视图：没有密码、2FA 全文与备注
type ProfileView struct {
	Email         string `json:"email"`
	HasPassword   bool   `json:"has_password"`
	RecoveryEmail string `json:"recovery_email,omitempty"`
	HasTOTP       bool   `json:"has_totp"`
	TOTPLast4     string `json:"totp_last4,omitempty"`
	// TOTPOK 为保存着的 2FA 能算出验证码（没有 2FA 时为假）
	TOTPOK    bool      `json:"totp_ok"`
	TOTPFlags []string  `json:"totp_flags,omitempty"`
	HasNote   bool      `json:"has_note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FamilyGroup 为一个家庭组（文件格式与接口返回相同）
type FamilyGroup struct {
	ID        string    `json:"id"`
	Seq       int64     `json:"seq"`
	Rev       int64     `json:"rev"`
	Name      string    `json:"name,omitempty"`
	Color     int       `json:"color"`
	Primary   string    `json:"primary"`
	Members   []string  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FileError 为处于错误状态（只读）的文件：不含文件内容
type FileError struct {
	File        string     `json:"file"`
	Code        string     `json:"code"`
	Reason      string     `json:"reason"`
	Message     string     `json:"message"`
	Path        string     `json:"path,omitempty"`
	Backup      string     `json:"backup,omitempty"`
	BackupTime  *time.Time `json:"backup_time,omitempty"`
	BackupCount *int       `json:"backup_count,omitempty"`
	Snapshots   int        `json:"snapshots,omitempty"`
}

// BackupInfo 为一个文件的备份情况（只有时间与份数）
type BackupInfo struct {
	File           string     `json:"file"`
	BakTime        *time.Time `json:"bak_time,omitempty"`
	Snapshots      int        `json:"snapshots"`
	SnapshotLatest *time.Time `json:"snapshot_latest,omitempty"`
}

// ImportSource 为一个导入来源（粘贴框或一个 .txt 文件）
type ImportSource struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// FieldCounts 为按字段的计数
type FieldCounts struct {
	Password      int `json:"password"`
	RecoveryEmail int `json:"recovery_email"`
	TOTP          int `json:"totp"`
	Note          int `json:"note"`
}

func (counts *FieldCounts) add(field string) {
	switch field {
	case fieldPassword:
		counts.Password++
	case fieldRecovery:
		counts.RecoveryEmail++
	case fieldTOTP:
		counts.TOTP++
	case fieldNote:
		counts.Note++
	}
}

// ProfileImportInput 为导入请求：dry_run 为预览；确认时带预览返回的 revision（base_revision）与覆盖计数（expect_overwrites），
// 覆盖计数变了就 409 并附新预览；有“格式可能错位”的提示时要带 acknowledge_suspect
type ProfileImportInput struct {
	DryRun             bool           `json:"dry_run"`
	Mode               string         `json:"mode,omitempty"`
	Sources            []ImportSource `json:"sources"`
	BaseRevision       string         `json:"base_revision,omitempty"`
	ExpectOverwrites   *FieldCounts   `json:"expect_overwrites,omitempty"`
	AcknowledgeSuspect bool           `json:"acknowledge_suspect,omitempty"`
}

// SourceSummary 为一个来源的名称与行数
type SourceSummary struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// ImportSummary 为导入的汇总（新增、更新、未变化按邮箱计最终结果）
type ImportSummary struct {
	Lines          int            `json:"lines"`
	Blank          int            `json:"blank"`
	Comments       int            `json:"comments"`
	Valid          int            `json:"valid"`
	Invalid        int            `json:"invalid"`
	New            int            `json:"new"`
	Updated        int            `json:"updated"`
	Unchanged      int            `json:"unchanged"`
	DuplicateLines int            `json:"duplicate_lines"`
	Overwrites     FieldCounts    `json:"overwrites"`
	Kept           FieldCounts    `json:"kept"`
	Warnings       map[string]int `json:"warnings"`
	Suspect        []string       `json:"suspect"`
	Hints          []string       `json:"hints,omitempty"`
	TotalAfter     int            `json:"total_after"`
	OverLimit      bool           `json:"over_limit"`
}

// LineRef 为来源下标与行号
type LineRef struct {
	Source int `json:"source"`
	Line   int `json:"line"`
}

// ItemFields 为一行给出了哪些字段（密码、2FA、备注只给是否有，辅助邮箱完整给出）
type ItemFields struct {
	Password      bool     `json:"password"`
	RecoveryEmail string   `json:"recovery_email,omitempty"`
	TOTP          bool     `json:"totp"`
	TOTPLast4     string   `json:"totp_last4,omitempty"`
	TOTPFlags     []string `json:"totp_flags,omitempty"`
	Note          bool     `json:"note"`
}

// ImportItem 为预览与结果里的一行（空行与注释不出现）；无法识别时只有第 1 段是合法邮箱才带 email
type ImportItem struct {
	Source     int         `json:"source"`
	Line       int         `json:"line"`
	Email      string      `json:"email,omitempty"`
	Status     string      `json:"status"`
	Fields     *ItemFields `json:"fields,omitempty"`
	Changes    []string    `json:"changes,omitempty"`
	Overwrites []string    `json:"overwrites,omitempty"`
	Kept       []string    `json:"kept,omitempty"`
	Warnings   []string    `json:"warnings,omitempty"`
	Also       []LineRef   `json:"also,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Hint       string      `json:"hint,omitempty"`
	Field      string      `json:"field,omitempty"`
}

// ProfileImportResult 为预览与确认导入的结果
type ProfileImportResult struct {
	DryRun   bool            `json:"dry_run"`
	Mode     string          `json:"mode"`
	Revision string          `json:"revision"`
	Sources  []SourceSummary `json:"sources"`
	Summary  ImportSummary   `json:"summary"`
	Items    []ImportItem    `json:"items"`
}

// ProfileUpdate 为编辑或新增一条资料：只带要改的字段（空串等于没给），Clear 列出要清空的字段
type ProfileUpdate struct {
	Email         string   `json:"email"`
	Password      *string  `json:"password,omitempty"`
	RecoveryEmail *string  `json:"recovery_email,omitempty"`
	TOTP          *string  `json:"totp,omitempty"`
	Note          *string  `json:"note,omitempty"`
	Clear         []string `json:"clear,omitempty"`
}

// ProfileUpdateResult 为编辑结果
type ProfileUpdateResult struct {
	Profile  ProfileView `json:"profile"`
	Created  bool        `json:"created"`
	Changed  []string    `json:"changed"`
	Cleared  []string    `json:"cleared,omitempty"`
	Revision string      `json:"revision"`
}

// ProfileDeleteInput 为要删除的邮箱
type ProfileDeleteInput struct {
	Emails []string `json:"emails"`
}

// ProfileDeleteResult 为删除结果
type ProfileDeleteResult struct {
	Deleted  int    `json:"deleted"`
	Missing  int    `json:"missing"`
	Revision string `json:"revision"`
}

// RevealInput 为取出请求：Fields 为空时取出全部字段
type RevealInput struct {
	Email  string   `json:"email"`
	Fields []string `json:"fields,omitempty"`
}

// TOTPCode 为服务端按 RFC 6238（SHA1、30 秒、6 位）算出的验证码
type TOTPCode struct {
	Code       string    `json:"code"`
	NextCode   string    `json:"next_code"`
	Period     int       `json:"period"`
	Remaining  int       `json:"remaining"`
	ServerTime time.Time `json:"server_time"`
	Flags      []string  `json:"flags,omitempty"`
}

// ProfileSecret 为取出的明文（只由 reveal 返回）
type ProfileSecret struct {
	Email         string    `json:"email"`
	Password      string    `json:"password,omitempty"`
	RecoveryEmail string    `json:"recovery_email,omitempty"`
	TOTP          string    `json:"totp,omitempty"`
	Note          string    `json:"note,omitempty"`
	Line          string    `json:"line,omitempty"`
	LineFormat    string    `json:"line_format,omitempty"`
	LineLossy     string    `json:"line_lossy,omitempty"`
	TOTPCode      *TOTPCode `json:"totp_code,omitempty"`
	TOTPCodeError string    `json:"totp_code_error,omitempty"`
}

// ExportInput 为导出请求：Emails（按请求顺序）或 All（按邮箱排序）
type ExportInput struct {
	Emails      []string `json:"emails,omitempty"`
	All         bool     `json:"all,omitempty"`
	IncludeNote bool     `json:"include_note,omitempty"`
}

// LossyLine 为重新导入后不能完全还原的一行（邮箱不是秘密）
type LossyLine struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// ProfileExport 为导出的整行（只由 export 返回）
type ProfileExport struct {
	Lines    []string    `json:"lines"`
	Count    int         `json:"count"`
	Missing  []string    `json:"missing"`
	TabLines int         `json:"tab_lines"`
	Lossy    []LossyLine `json:"lossy"`
}

// FamilyImportInput 为录入家庭组：Mode 为 blocks（默认，空行或 # 行分块）或 lines（每行是一个家庭组）
type FamilyImportInput struct {
	DryRun             bool   `json:"dry_run"`
	Text               string `json:"text"`
	Mode               string `json:"mode,omitempty"`
	BaseRevision       string `json:"base_revision,omitempty"`
	AcknowledgeSuspect bool   `json:"acknowledge_suspect,omitempty"`
}

// FamilySummary 为录入的汇总
type FamilySummary struct {
	Blocks         int            `json:"blocks"`
	Created        int            `json:"created"`
	Replaced       int            `json:"replaced"`
	PrimaryChanged int            `json:"primary_changed"`
	Unchanged      int            `json:"unchanged"`
	Rejected       int            `json:"rejected"`
	Invalid        int            `json:"invalid"`
	Moved          int            `json:"moved"`
	Removed        int            `json:"removed"`
	Dissolved      int            `json:"dissolved"`
	Warnings       map[string]int `json:"warnings"`
	Suspect        []string       `json:"suspect"`
}

// BatchWarning 为批级提示：同一个主邮箱写在两块里（primary_repeated）、同一个邮箱出现在两块里（email_in_blocks）
type BatchWarning struct {
	Code   string `json:"code"`
	Email  string `json:"email"`
	Blocks []int  `json:"blocks"`
}

// MovedEmail 为从别的组移过来的邮箱
type MovedEmail struct {
	Email       string `json:"email"`
	FromGroup   string `json:"from_group"`
	FromPrimary string `json:"from_primary"`
}

// DissolvedGroup 为成员都被移走、自动解散的组
type DissolvedGroup struct {
	GroupID string `json:"group_id"`
	Primary string `json:"primary"`
	Name    string `json:"name,omitempty"`
	Color   int    `json:"color"`
}

// BlockDetail 为拒绝或无法识别的具体位置（line/position 为文本里的行号与这一行的第几个），不含非邮箱的内容
type BlockDetail struct {
	Email        string `json:"email,omitempty"`
	GroupPrimary string `json:"group_primary,omitempty"`
	GroupName    string `json:"group_name,omitempty"`
	Line         int    `json:"line,omitempty"`
	Position     int    `json:"position,omitempty"`
}

// FamilyBlockResult 为一块（或新建、编辑一个组）的结果。Status：created、replaced、primary_changed、updated（只改了名称或颜色）、
// unchanged、rejected、invalid
type FamilyBlockResult struct {
	Index       int              `json:"index"`
	FirstLine   int              `json:"first_line,omitempty"`
	LastLine    int              `json:"last_line,omitempty"`
	Status      string           `json:"status"`
	Primary     string           `json:"primary,omitempty"`
	Members     []string         `json:"members,omitempty"`
	GroupID     string           `json:"group_id,omitempty"`
	Color       *int             `json:"color,omitempty"`
	Name        string           `json:"name,omitempty"`
	Added       []string         `json:"added,omitempty"`
	Removed     []string         `json:"removed,omitempty"`
	Moved       []MovedEmail     `json:"moved,omitempty"`
	Dissolved   []DissolvedGroup `json:"dissolved,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	Hint        string           `json:"hint,omitempty"`
	Detail      *BlockDetail     `json:"detail,omitempty"`
	Warnings    []string         `json:"warnings,omitempty"`
	CommentLine int              `json:"comment_line,omitempty"`
	// Renamed、Recolored 为编辑改了名称、颜色
	Renamed   bool         `json:"renamed,omitempty"`
	Recolored bool         `json:"recolored,omitempty"`
	Group     *FamilyGroup `json:"group,omitempty"`
	DryRun    bool         `json:"dry_run,omitempty"`
	Revision  string       `json:"revision,omitempty"`
}

// FamilyImportResult 为录入预览与确认的结果
type FamilyImportResult struct {
	DryRun        bool                `json:"dry_run"`
	Mode          string              `json:"mode"`
	Revision      string              `json:"revision"`
	Summary       FamilySummary       `json:"summary"`
	BatchWarnings []BatchWarning      `json:"batch_warnings"`
	Blocks        []FamilyBlockResult `json:"blocks"`
}

// FamilyCreate 为新建家庭组（规则与录入的一块相同）
type FamilyCreate struct {
	DryRun       bool     `json:"dry_run"`
	Primary      string   `json:"primary"`
	Members      []string `json:"members"`
	Name         *string  `json:"name,omitempty"`
	Color        *int     `json:"color,omitempty"`
	BaseRevision string   `json:"base_revision,omitempty"`
}

// FamilyUpdate 为编辑家庭组：Rev 为组的 rev（不符 409）；Primary、Members 缺省时取原值
type FamilyUpdate struct {
	DryRun       bool      `json:"dry_run"`
	Rev          *int64    `json:"rev,omitempty"`
	Name         *string   `json:"name,omitempty"`
	Color        *int      `json:"color,omitempty"`
	Primary      *string   `json:"primary,omitempty"`
	Members      *[]string `json:"members,omitempty"`
	BaseRevision string    `json:"base_revision,omitempty"`
}

// FamilyDissolveResult 为解散结果：Members 为解散前组里的人数（主邮箱 + 成员）
type FamilyDissolveResult struct {
	Dissolved bool   `json:"dissolved"`
	Members   int    `json:"members"`
	Revision  string `json:"revision"`
}

// StartEmptyResult 为“从空开始”的结果：MovedBackup 为 .bak 移到的位置（没有 .bak 时省略）
type StartEmptyResult struct {
	File        string `json:"file"`
	MovedBackup string `json:"moved_backup,omitempty"`
	Revision    string `json:"revision"`
}
