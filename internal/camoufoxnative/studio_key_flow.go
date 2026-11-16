package camoufoxnative

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// 创建 Key 的页面流程（见 fetchStudioKey）：点过页面自己的“Create API key”之后，每一轮用只读表达式读一次页面状态
// （studioKeyInspectExpression，结果为 studioKeyPage），由 studioKeyFlow 决定下一步：用真实指针点页面自己的一个控件、
// 在 AI Studio 站内导航、等待，或以具体原因失败。判断以结构为主（对话框、下拉框、选项、勾选框、按钮是否可用），文字为辅
// （studioKeyLabels，英文、简体中文、繁体中文、日文、韩文）。
//
// 新版创建对话框把 Google Cloud 项目放在下拉框里：有项目时选一个（优先 Default Gemini Project 或 gen-lang-client-<数字>），
// 再点“Create key”；没有项目时（需要验证账号的账户常见）经下拉框里的“Create project”或对话框里的新建项目入口，或者 AI Studio 的
// Projects 页（aistudio.google.com/projects）新建一个项目，等它出现（页面自己的响应里出现新的 projects/<数字>，或下拉框有了值）
// 后再创建 Key。API Keys 页上没有可用的“Create API key”、却有新建项目的按钮时（没有项目的空状态），也去一次 Projects 页新建项目，
// 回来再点“Create API key”。
//
// 断定“没有项目”要有依据（防止项目列表还没加载完就新建）：页面自己的响应里出现过 projects/<数字>，或下拉框里出现过可选的已有项目时
// 一律不新建；下拉列表要连续 listSettle 不变、对话框与列表都不在转圈，且列表里有“没有可用项目”“导入”或“Create project”这类
// 只在没有项目时才单独出现的选项，才算没有项目。
//
// 每次运行的上限（防止多建）：新建项目的入口最多点一次、新建项目对话框的确认最多点一次、创建 Key 的确认最多点一次；
// 下拉框里有可选的已有项目时一律复用、不新建；Projects 页最多去一次。“Create API key”每次点了之后要先见到对话框才会再点
// （没有对话框时页面可能直接建好了 Key，只等结果），从 Projects 页回来先等 settle 让 Key 列表加载（新项目可能自带 Key），
// 再点之前像第一次那样核对页面上是否已显示 Key。从不删除任何东西，不点“导入项目”，不点验证账号的按钮，不点站外链接
// （如 Google Cloud 控制台），不在页面里输入文字（项目名为空时失败并说明）。条款勾选框只在明显是服务条款、且是创建所必需
// （标为必选，或确认按钮不可用）时勾选，邮件订阅类不勾。

// studioKeyProjectsPath 为 AI Studio 的 Projects 页（下拉框里没有新建项目的入口时的备选）
const studioKeyProjectsPath = "/projects"

// studioKeyTiming 为创建流程的各段时限（测试里调短）
var studioKeyTiming = struct {
	// poll 为两轮之间的间隔
	poll time.Duration
	// settle 为导航后等页面加载 Key 列表的时间：超过这段时间仍没有从响应里读到 Key 时才创建
	settle time.Duration
	// createWait 为点了创建 Key 的确认（或点了“Create API key”后页面没有弹对话框）之后等新 Key 出现在响应里的时间
	createWait time.Duration
	// projectWait 为点了新建项目之后等新项目出现的时间（Google Cloud 建项目要几秒到几十秒）
	projectWait time.Duration
	// stuck 为对话框停在同一个状态、又没有可做的下一步时的等待上限
	stuck time.Duration
	// noDialog 为点了“Create API key”后一直没有对话框、按页面直接创建处理之前的等待
	noDialog time.Duration
	// retry 为重复打开下拉框、重新打开创建对话框之间的最短间隔
	retry time.Duration
	// listSettle 为断定下拉列表里没有已有项目之前，列表要连续保持不变（且不在转圈）的时间；Projects 页上点新建之前等它的三倍
	listSettle time.Duration
}{
	poll:        500 * time.Millisecond,
	settle:      15 * time.Second,
	createWait:  60 * time.Second,
	projectWait: 90 * time.Second,
	stuck:       20 * time.Second,
	noDialog:    10 * time.Second,
	retry:       2 * time.Second,
	listSettle:  3 * time.Second,
}

// 创建流程里各步卡住时的失败原因（不含 Key）；失败时后面接“ | 步骤=…”与“ | 对话框=…”（打码、截短，见 studioKeyFlow.detail）
const (
	StudioKeyReasonVerifyPage       = "API Keys 页面被跳到了验证账号页（aistudio.google.com/verify-account），页面不可用，没有创建；请先在浏览器里完成账号验证"
	StudioKeyReasonVerifyModal      = "API Keys 页面弹出了验证账号的对话框并挡住了页面，没有创建"
	StudioKeyReasonVerifyBlocked    = "点击创建后页面提示需要先验证账号，没有创建成功"
	StudioKeyReasonCreateDisabled   = "页面上的“Create API key”按钮不可用"
	StudioKeyReasonUnknownDialog    = "创建过程中出现了认不出的对话框，没有继续"
	StudioKeyReasonDialogClosed     = "创建 Key 的对话框关掉了，重新打开也没有成功"
	StudioKeyReasonKeyConfirm       = "创建 Key 的对话框里确认按钮一直不可用"
	StudioKeyReasonKeyName          = "创建 Key 的对话框里 Key 名称为空（本流程不在页面里输入文字），请在浏览器里手动创建一次"
	StudioKeyReasonSelectStuck      = "创建 Key 的对话框里项目下拉框打不开或选不上项目"
	StudioKeyReasonNoProjectEntry   = "创建 Key 需要先有 Google Cloud 项目，但对话框里没有可选的项目，也没有找到新建项目的入口"
	StudioKeyReasonProjectLimit     = "这次已经新建过一个项目，页面仍要求新建项目；为免多建，没有继续"
	StudioKeyReasonProjectName      = "新建项目的对话框里项目名为空（本流程不在页面里输入文字），请在浏览器里手动新建一次项目"
	StudioKeyReasonProjectConfirm   = "新建项目的对话框里确认按钮一直不可用"
	StudioKeyReasonProjectTimeout   = "已确认新建项目，但在下拉框和页面响应里都没有等到新项目（项目可能已经建好，可在浏览器里查看）"
	StudioKeyReasonProjectNoDialog  = "已点击新建项目，但没有出现新建项目的对话框，也没有等到新项目出现"
	StudioKeyReasonProjectFailed    = "新建项目失败"
	StudioKeyReasonKeyFailed        = "创建 Key 失败"
	StudioKeyReasonPageError        = "页面提示出错"
	StudioKeyReasonProjectsNoCreate = "创建 Key 需要先有 Google Cloud 项目，Projects 页上也没有找到新建项目的按钮"
	StudioKeyReasonLeftPage         = "创建过程中页面跳出了 AI Studio，没有继续"
	StudioKeyReasonProjectUnlisted  = "账户已有 Google Cloud 项目（页面响应或下拉框里出现过），但创建 Key 的下拉框里没有可选的项目；为免多建，没有新建项目"
	StudioKeyReasonListLoading      = "创建 Key 的对话框里项目列表一直没有加载完，没有新建项目"
	StudioKeyReasonNoCreateAfter    = "已新建项目，但 API Keys 页面上仍没有可用的“Create API key”按钮"
)

// studioKeyPattern* 为按文字分类控件用的正则源码：写成 Go（RE2）与 JS 都能编译的子集，两边都不区分大小写
const (
	// 新建项目的入口与确认（“+ Create project”“Create a new project”“新建项目”“新しいプロジェクト”“새 프로젝트”等）；对话框按钮按含有
	// 匹配，下拉选项与下拉框的值要整段都是这类说法或带加号标记（见 isCreateProjectOption）
	studioKeyPatternCreateProject = `create\s+(a\s+)?(new\s+)?(cloud\s+)?project|new\s+(cloud\s+)?project|add\s+(a\s+)?project|创建\s*(新的?|一个)?\s*项目|新建\s*项目|新增\s*项目|建立\s*(新的?)?\s*(專案|项目|項目)|新增\s*(專案|項目)|新專案|新項目|プロジェクトを作成|新しいプロジェクト(を作成)?|新規プロジェクト(を?作成)?|프로젝트\s*(만들기|생성)|새\s*프로젝트(\s*(만들기|생성))?`
	// 导入已有项目（不点）
	studioKeyPatternImportProject = `\bimports?\b|导入|匯入|導入|インポート|가져오기`
	// 没有可用项目的提示（“No Cloud Projects Available”等）；下拉选项与下拉框的值要以它开头才算（见 isNoProjectsHint）
	studioKeyPatternNoProjects = `\bno\s+(cloud\s+)?projects?\b|没有.{0,16}项目|沒有.{0,16}(專案|項目)|无可用.{0,12}项目|プロジェクトがありません|プロジェクトはありません|프로젝트가\s*없`
	// 下拉框的占位文字（“Choose an imported project”等）
	studioKeyPatternPlaceholder = `(choose|select|pick)\b.*project|选择.*项目|選擇.*(專案|項目)|プロジェクトを選択|프로젝트\s*선택`
	// 创建 Key 的确认（另须不含“项目”字样，见 studioKeyLabels.keyConfirm）
	studioKeyPatternKeyConfirm = `create\s+(an?\s+)?(api\s+)?key|generate\s+(an?\s+)?(api\s+)?key|创建\s*(api\s*)?(密钥|key)|生成\s*(api\s*)?(密钥|key)|建立\s*(api\s*)?(金鑰|key)|產生\s*(api\s*)?(金鑰|key)|(api\s*)?キーを作成|키\s*(만들기|생성)`
	// 对话框标题里的“Key”字样
	studioKeyPatternKeyWord = `\bkey\b|密钥|金鑰|キー|키`
	// “项目”字样
	studioKeyPatternProjectWord = `project|项目|項目|專案|专案|プロジェクト|프로젝트`
	// 新建项目对话框里的确认按钮（整段文字只有“Create”“创建”之类）
	studioKeyPatternProjectConfirm = `^\s*\+?\s*(create|创建|建立|作成|만들기|생성|ok|确定|確定|done|完成)\s*$`
	// 取消、关闭类按钮（不当作确认；验证账号弹窗只点这类）；英文按整词（“Send feedback”“Disclose”不算）
	studioKeyPatternCancel = `\b(cancel|close|dismiss|back|not\s+now|later)\b|取消|关闭|關閉|返回|稍后|以后再说|キャンセル|閉じる|戻る|後で|취소|닫기|뒤로|나중에`
	// 验证账号的提示（verify-account）
	studioKeyPatternVerify = `verify\s+(your\s+)?account|account\s+(must\s+be\s+|needs\s+to\s+be\s+)?verified|verify-account|验证(您的|你的)?\s*(帐号|账号|帐户|账户)|驗證(您的)?\s*(帳戶|帳號)|アカウントを(確認|認証)|アカウントの(確認|認証)|계정\s*(인증|확인)`
	// 服务条款类勾选框
	studioKeyPatternTerms = `terms|agree|accept|acknowledge|条款|條款|服务协议|同意|規約|약관|동의`
	// 邮件订阅、资讯类勾选框（不勾）
	studioKeyPatternOptional = `e-?mail|newsletter|news|updates|offers|marketing|tips|邮件|郵件|资讯|資訊|新闻|新聞|ニュース|メール|뉴스|이메일|소식`
	// 页面出错的提示
	studioKeyPatternFailure = `fail|error|denied|suspicious|not\s+allowed|disabled|unable|could\s*not|couldn't|quota|limit|失败|失敗|错误|錯誤|拒绝|拒絕|权限|權限|可疑|停用|无法|無法|上限|できません|エラー|拒否|실패|오류|권한|거부`
	// 优先选用的已有项目
	studioKeyPatternPreferred = `default\s+gemini\s+project|gen-lang-client-\d+`
	// 输入框标签像“名称”
	studioKeyPatternName = `name|名称|名稱|名前|이름`
)

// studioKeyLabels 为编译好的分类正则
var studioKeyLabels = struct {
	createProject, importProject, noProjects, noProjectsStart, placeholder, keyConfirmText, keyWord, projectWord,
	projectConfirm, cancel, verify, terms, optional, failure, preferred, name *regexp.Regexp
}{
	createProject:   studioKeyRegexp(studioKeyPatternCreateProject),
	importProject:   studioKeyRegexp(studioKeyPatternImportProject),
	noProjects:      studioKeyRegexp(studioKeyPatternNoProjects),
	noProjectsStart: studioKeyRegexp(`^\s*(?:` + studioKeyPatternNoProjects + `)`),
	placeholder:     studioKeyRegexp(studioKeyPatternPlaceholder),
	keyConfirmText:  studioKeyRegexp(studioKeyPatternKeyConfirm),
	keyWord:         studioKeyRegexp(studioKeyPatternKeyWord),
	projectWord:     studioKeyRegexp(studioKeyPatternProjectWord),
	projectConfirm:  studioKeyRegexp(studioKeyPatternProjectConfirm),
	cancel:          studioKeyRegexp(studioKeyPatternCancel),
	verify:          studioKeyRegexp(studioKeyPatternVerify),
	terms:           studioKeyRegexp(studioKeyPatternTerms),
	optional:        studioKeyRegexp(studioKeyPatternOptional),
	failure:         studioKeyRegexp(studioKeyPatternFailure),
	preferred:       studioKeyRegexp(studioKeyPatternPreferred),
	name:            studioKeyRegexp(studioKeyPatternName),
}

func studioKeyRegexp(source string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + source)
}

// isKeyConfirmLabel 判断按钮是不是创建 Key 的确认：文字像“Create key”，且不含“项目”字样（“Create project”不算）
func isKeyConfirmLabel(text string) bool {
	return studioKeyLabels.keyConfirmText.MatchString(text) && !studioKeyLabels.projectWord.MatchString(text)
}

// isNoProjectsHint 判断下拉选项或下拉框的值是“没有可用项目”的提示：文字以提示开头，或选项不可用且文字里有提示
// （“Nano project”“没有名字的项目”这类已有项目不算）
func isNoProjectsHint(text string, enabled bool) bool {
	return studioKeyLabels.noProjectsStart.MatchString(text) || !enabled && studioKeyLabels.noProjects.MatchString(text)
}

// isCreateProjectLabel 判断对话框里的按钮是不是新建项目的入口或确认（文字里含“Create project”这类说法，或只有加号图标加“项目”字样）
func isCreateProjectLabel(text string, icon string) bool {
	if studioKeyLabels.createProject.MatchString(text) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(icon), "add") && studioKeyLabels.projectWord.MatchString(text) &&
		!studioKeyLabels.importProject.MatchString(text)
}

// isMarkedCreateOption 判断下拉选项带新建项目的明确标记：加号图标且文字有“项目”字样，或文字以“+”开头且像新建项目（导入除外）
func isMarkedCreateOption(text string, icon string) bool {
	if studioKeyLabels.importProject.MatchString(text) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(icon), "add") && studioKeyLabels.projectWord.MatchString(text) {
		return true
	}
	trimmed := strings.TrimSpace(text)
	return (strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "＋")) && studioKeyLabels.createProject.MatchString(trimmed)
}

// isCreateProjectPhrase 判断整段文字都是新建项目的说法（“Create project”“New project”“新建项目”，可带“+”与标点）；
// 名字里含这些词的已有项目（“My New Project”“New Project Alpha”）不算
func isCreateProjectPhrase(text string) bool {
	if !studioKeyLabels.createProject.MatchString(text) || studioKeyLabels.importProject.MatchString(text) {
		return false
	}
	rest := studioKeyLabels.createProject.ReplaceAllString(text, " ")
	return strings.TrimFunc(rest, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) }) == ""
}

// isCreateProjectOption 判断下拉选项或下拉框的值是不是新建项目（比对话框按钮严格：要有明确标记，或整段文字都是新建项目的说法）
func isCreateProjectOption(text string, icon string) bool {
	return isMarkedCreateOption(text, icon) || isCreateProjectPhrase(text)
}

// studioKeyControl 为页面上一个可点的控件（按钮、链接或下拉选项）。Index 为它在表达式枚举里的序号，Text 为打码、截短后的文字
type studioKeyControl struct {
	Index   int    `json:"i"`
	Text    string `json:"t"`
	Enabled bool   `json:"e"`
	Icon    string `json:"ic,omitempty"`
	// Href 为同站（aistudio.google.com）链接的路径；External 表示指向站外（如 console.cloud.google.com）的链接，一律不点
	Href     string `json:"href,omitempty"`
	External bool   `json:"ext,omitempty"`
	// Button 表示下拉面板里的普通按钮（不是选项）
	Button   bool `json:"btn,omitempty"`
	Selected bool `json:"sel,omitempty"`
}

// link 表示控件是链接（同站或站外）：不当作确认、关闭类按钮点
func (control studioKeyControl) link() bool {
	return control.Href != "" || control.External
}

// studioKeySelect 为对话框里的项目下拉框（mat-select、role=combobox 或原生 select）
type studioKeySelect struct {
	Value       string `json:"v"`
	Empty       bool   `json:"empty"`
	Placeholder string `json:"ph"`
	Disabled    bool   `json:"dis"`
	Expanded    bool   `json:"exp"`
	// Options 为原生 select 的选项（它的下拉列表不在页面 DOM 里，看不到也点不了，只用来判断有没有已有项目），其他下拉框为空
	Options []studioKeyControl `json:"opts,omitempty"`
}

// studioKeyInput 为对话框里的文字输入框（只读是否为空，不写值）
type studioKeyInput struct {
	Index    int    `json:"i"`
	Empty    bool   `json:"empty"`
	Label    string `json:"l"`
	Required bool   `json:"req"`
}

// studioKeyBox 为对话框里的勾选框
type studioKeyBox struct {
	Index    int    `json:"i"`
	Label    string `json:"l"`
	Checked  bool   `json:"on"`
	Required bool   `json:"req"`
	Disabled bool   `json:"dis"`
}

// studioKeyDialog 为最上层的对话框
type studioKeyDialog struct {
	Title   string             `json:"title"`
	Text    string             `json:"text"`
	Buttons []studioKeyControl `json:"buttons"`
	Select  *studioKeySelect   `json:"select"`
	Inputs  []studioKeyInput   `json:"inputs"`
	Boxes   []studioKeyBox     `json:"boxes"`
	Error   string             `json:"error"`
	Busy    bool               `json:"busy"`
}

// studioKeyPage 为一轮读到的页面状态（studioKeyInspectExpression 的结果）
type studioKeyPage struct {
	Host    string             `json:"host"`
	Path    string             `json:"path"`
	Dialogs int                `json:"dialogs"`
	Dialog  *studioKeyDialog   `json:"dialog"`
	Options []studioKeyControl `json:"options"`
	Alerts  []string           `json:"alerts"`
	// Create 为页面主区域（不在对话框里）的“Create API key”按钮：enabled、disabled，没有时为空
	Create string `json:"create"`
	// Project 为页面主区域可点的新建项目按钮的文字（Projects 页），没有时为空
	Project string `json:"project"`
	// Verify 表示页面上有验证账号的提示（横幅、提示条或指向 verify-account 的链接）
	Verify bool `json:"verify"`
	// ListBusy 表示展开着的下拉列表里在转圈（项目列表还在加载）
	ListBusy bool `json:"listBusy"`
}

// 对话框的种类
const (
	studioKeyDialogNone    = ""
	studioKeyDialogKey     = "key"
	studioKeyDialogProject = "project"
	studioKeyDialogVerify  = "verify"
	studioKeyDialogCreated = "created"
	studioKeyDialogOther   = "other"
)

// classifyStudioKeyDialog 判断对话框是哪一种：
//   - created：对话框里显示着 Key（表达式已把 Key 换成 <key>），是创建完成后的展示；
//   - verify：验证账号的提示（没有项目下拉框与创建 Key 的按钮）；
//   - project：标题带“项目”字样、不带“Key”字样（新建项目对话框）；
//   - key：有创建 Key 的按钮、项目下拉框，或标题带“Key”字样；
//   - other：认不出。
func classifyStudioKeyDialog(dialog *studioKeyDialog) string {
	if dialog == nil {
		return studioKeyDialogNone
	}
	hasKeyConfirm := false
	for _, button := range dialog.Buttons {
		if isKeyConfirmLabel(button.Text) {
			hasKeyConfirm = true
		}
	}
	title := dialog.Title
	switch {
	case strings.Contains(dialog.Text, "<key>"):
		return studioKeyDialogCreated
	case studioKeyLabels.verify.MatchString(dialog.Text) && dialog.Select == nil && !hasKeyConfirm:
		return studioKeyDialogVerify
	case title != "" && studioKeyLabels.projectWord.MatchString(title) && !studioKeyLabels.keyWord.MatchString(title):
		return studioKeyDialogProject
	case hasKeyConfirm || dialog.Select != nil || studioKeyLabels.keyWord.MatchString(title):
		return studioKeyDialogKey
	case title == "" && studioKeyLabels.projectWord.MatchString(dialog.Text) && len(dialog.Inputs) > 0:
		return studioKeyDialogProject
	}
	return studioKeyDialogOther
}

// 下拉选项的种类
const (
	studioKeyOptionExisting = "existing"
	studioKeyOptionCreate   = "create"
	studioKeyOptionImport   = "import"
	studioKeyOptionNone     = "none"
	studioKeyOptionOther    = "other"
)

// classifyStudioKeyOption 按文字给一个下拉选项分类（不看同一列表里的其他选项，见 classifyStudioKeyOptions）
func classifyStudioKeyOption(option studioKeyControl) string {
	return classifyStudioKeyOptionIn(option, false)
}

// classifyStudioKeyOptionIn 给下拉选项分类：站外链接不点；新建项目（markedOnly 时只认带明确标记的，见 isMarkedCreateOption）、导入、
// 没有项目的提示、占位，其余可用的选项是已有项目（下拉面板里的普通按钮不算已有项目）
func classifyStudioKeyOptionIn(option studioKeyControl, markedOnly bool) string {
	text := option.Text
	switch {
	case option.External:
		return studioKeyOptionOther
	case isMarkedCreateOption(text, option.Icon) || !markedOnly && isCreateProjectPhrase(text):
		return studioKeyOptionCreate
	case studioKeyLabels.importProject.MatchString(text):
		return studioKeyOptionImport
	case isNoProjectsHint(text, option.Enabled):
		return studioKeyOptionNone
	case strings.TrimSpace(text) == "" || option.Button || !option.Enabled || studioKeyLabels.placeholder.MatchString(text):
		return studioKeyOptionOther
	}
	return studioKeyOptionExisting
}

// classifyStudioKeyOptions 给同一个下拉列表里的选项分类：列表里有带明确标记（加号图标或前导“+”）的新建项目选项时，只认它是新建项目，
// 文字碰巧整段像新建项目的其他选项（名为“New Project”的已有项目）按已有项目算
func classifyStudioKeyOptions(options []studioKeyControl) []string {
	markedOnly := false
	for _, option := range options {
		if !option.External && isMarkedCreateOption(option.Text, option.Icon) {
			markedOnly = true
		}
	}
	kinds := make([]string, len(options))
	for index, option := range options {
		kinds[index] = classifyStudioKeyOptionIn(option, markedOnly)
	}
	return kinds
}

// hasExistingOption 判断选项里有可选的已有项目
func hasExistingOption(options []studioKeyControl) bool {
	return slices.Contains(classifyStudioKeyOptions(options), studioKeyOptionExisting)
}

// selectHasProject 判断下拉框里已经选着一个项目（值不是占位、新建、导入或没有项目的提示）；值整段像新建项目（“New Project”）时
// 只看文字认不出，见 studioKeyFlow.selectedProject
func selectHasProject(selection *studioKeySelect) bool {
	if selection == nil || selection.Empty {
		return false
	}
	value := strings.TrimSpace(selection.Value)
	return value != "" && !studioKeyLabels.placeholder.MatchString(value) && !isCreateProjectOption(value, "") &&
		!studioKeyLabels.importProject.MatchString(value) && !isNoProjectsHint(value, true)
}

// selectedProject 判断下拉框里选着项目：selectHasProject，或下拉框的值就是这次选过的已有项目（名为“New Project”这类的已有项目，
// 只看文字会当成新建项目）
func (flow *studioKeyFlow) selectedProject(selection *studioKeySelect) bool {
	if selectHasProject(selection) {
		return true
	}
	if selection == nil || selection.Empty || flow.picked == "" {
		return false
	}
	value := strings.TrimSpace(selection.Value)
	return value != "" && (value == flow.picked || strings.HasPrefix(flow.picked, value+" "))
}

// selectShowsNoProject 判断下拉框本身表明没有可选的项目（下拉框点不开、展开后看不到选项时用）：原生 select 的选项里没有已有项目，
// 或下拉框的值、占位文字与对话框文字里有“没有可用项目”的提示
func selectShowsNoProject(dialog *studioKeyDialog) bool {
	selection := dialog.Select
	if selection == nil {
		return false
	}
	if len(selection.Options) > 0 {
		return !hasExistingOption(selection.Options)
	}
	return studioKeyLabels.noProjects.MatchString(selection.Value + " " + selection.Placeholder + " " + dialog.Text)
}

// studioKeyStepKind 为下一步的动作
type studioKeyStepKind int

const (
	studioKeyStepWait studioKeyStepKind = iota
	studioKeyStepClick
	studioKeyStepNavigate
	studioKeyStepFail
)

// studioKeyTarget 为要点的控件：scope 为 dialog（对话框里的按钮与链接）、option（下拉选项）、select（对话框里的下拉框）、
// box（对话框里的勾选框）、page-create（页面主区域的“Create API key”）或 page-project（页面主区域的新建项目按钮）；
// index 与 text 必须与读页面时一致，点击前在页面里再核对一次（页面变了就不点）
type studioKeyTarget struct {
	Scope string `json:"scope"`
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// 点击之后要记下的标记
const (
	studioKeyMarkOpen           = "open"
	studioKeyMarkSelect         = "select"
	studioKeyMarkPick           = "pick"
	studioKeyMarkProjectEntry   = "project-entry"
	studioKeyMarkProjectConfirm = "project-confirm"
	studioKeyMarkKeyConfirm     = "key-confirm"
	studioKeyMarkTick           = "tick"
	studioKeyMarkDismiss        = "dismiss"
	studioKeyMarkProjectsPage   = "projects-page"
	studioKeyMarkReturn         = "return"
)

// studioKeyStep 为 studioKeyFlow 决定的下一步
type studioKeyStep struct {
	Kind   studioKeyStepKind
	Target studioKeyTarget
	// Path 为站内导航的路径（aistudio.google.com 下）
	Path string
	// Mark 为点击或导航之后要记下的标记（见 studioKeyFlow.apply）
	Mark string
	// Note 为写进步骤记录的说明
	Note string
	// Reason 为失败原因（不含步骤与对话框摘要）
	Reason string
}

// StudioProject 为在页面响应里看到的一个 Google Cloud 项目
type StudioProject struct {
	// Name 为 projects/<数字>，Number 为其中的数字
	Name   string
	Number string
	// ID 为项目 ID（如 gen-lang-client-0123456789，尽力而为，可为空）
	ID string
	// Display 为项目显示名（尽力而为，可为空）
	Display string
}

// studioKeyFlow 为一次运行里创建流程的状态
type studioKeyFlow struct {
	opens    int
	openedAt time.Time
	// dialogSeen 表示这次出现过创建 Key 或新建项目的对话框；dialogSinceOpen 表示最近一次点“Create API key”之后出现过
	// （没出现过时不再点，见 decideNoDialog）
	dialogSeen       bool
	dialogSinceOpen  bool
	noDialogSince    time.Time
	keyConfirmAt     time.Time
	projectEntryAt   time.Time
	projectConfirmAt time.Time
	projectReadyAt   time.Time
	projectsPageAt   time.Time
	returnedAt       time.Time
	selectOpens      int
	selectAt         time.Time
	optionsSeen      bool
	optionsSig       string
	optionsChangedAt time.Time
	noProjectsAt     time.Time
	unlistedAt       time.Time
	existingOffered  bool
	picks            int
	pickAt           time.Time
	picked           string
	ticks            int
	ticked           map[string]bool
	dismissAt        time.Time
	verifyDialogAt   time.Time
	lastAction       time.Time
	signature        string
	changedAt        time.Time
	steps            []string
	baseAlerts       map[string]bool
	baseCaptured     bool
	known            map[string]bool
	projects         []StudioProject
	newProjects      []StudioProject
	lastPage         *studioKeyPage
}

func newStudioKeyFlow() *studioKeyFlow {
	return &studioKeyFlow{ticked: map[string]bool{}, baseAlerts: map[string]bool{}, known: map[string]bool{}}
}

// projectStarted 表示这次已经点过新建项目的入口或确认
func (flow *studioKeyFlow) projectStarted() bool {
	return !flow.projectEntryAt.IsZero() || !flow.projectConfirmAt.IsZero()
}

// mayCreateProject 表示还可以开始新建项目：这次还没点过新建项目的入口与确认，下拉框里没出现过可选的已有项目，
// 页面自己的响应里也没出现过项目（账户已有项目时不新建，哪怕下拉框里一时没列出）
func (flow *studioKeyFlow) mayCreateProject() bool {
	return !flow.projectStarted() && !flow.existingOffered && len(flow.known) == 0
}

// running 表示创建流程已经开始：点过（或页面自己打开了）“Create API key”，或为新建项目去了 Projects 页
func (flow *studioKeyFlow) running() bool {
	return flow.opens > 0 || !flow.projectsPageAt.IsZero()
}

// projectCreated 表示这次新建了项目（点过新建项目的确认；或点过入口后出现了新项目）
func (flow *studioKeyFlow) projectCreated() bool {
	return !flow.projectConfirmAt.IsZero() || !flow.projectEntryAt.IsZero() && len(flow.newProjects) > 0
}

// note 追加一条步骤记录（与上一条相同时不重复）
func (flow *studioKeyFlow) note(text string) {
	text = studioKeyText(text, 60)
	if text == "" || len(flow.steps) > 0 && flow.steps[len(flow.steps)-1] == text {
		return
	}
	if len(flow.steps) >= 24 {
		return
	}
	flow.steps = append(flow.steps, text)
}

// noteProjects 记下响应里看到的项目：开始新建项目之前看到的算已有，之后才出现的算新项目
func (flow *studioKeyFlow) noteProjects(projects []StudioProject) {
	for _, project := range projects {
		flow.projects = mergeStudioProjects(flow.projects, []StudioProject{project})
		if flow.known[project.Number] {
			continue
		}
		if !flow.projectStarted() {
			flow.known[project.Number] = true
			continue
		}
		flow.newProjects = mergeStudioProjects(flow.newProjects, []StudioProject{project})
	}
	// 补上新项目后来才读到的 ID 与显示名
	for index := range flow.newProjects {
		for _, project := range flow.projects {
			if project.Number == flow.newProjects[index].Number {
				flow.newProjects[index] = mergeStudioProjects([]StudioProject{flow.newProjects[index]}, []StudioProject{project})[0]
			}
		}
	}
}

// project 返回响应里看到的项目（按编号），没有时为空
func (flow *studioKeyFlow) project(number string) (StudioProject, bool) {
	for _, project := range flow.projects {
		if project.Number == number {
			return project, true
		}
	}
	return StudioProject{}, false
}

// opened 记下点了页面的“Create API key”
func (flow *studioKeyFlow) opened(page *studioKeyPage, now time.Time) {
	if flow.opens == 0 && page != nil {
		flow.captureAlerts(*page)
	}
	flow.opens++
	flow.openedAt = now
	flow.lastAction = now
	flow.dialogSinceOpen = false
	flow.noDialogSince = time.Time{}
	flow.note("打开创建对话框")
}

// captureAlerts 记下点创建之前页面上已有的提示（如需要验证账号的横幅），之后只把新出现的提示当作出错
func (flow *studioKeyFlow) captureAlerts(page studioKeyPage) {
	for _, alert := range page.Alerts {
		flow.baseAlerts[alert] = true
	}
	flow.baseCaptured = true
}

// observe 记下这一轮读到的页面状态（对话框出现过、下拉选项、新项目是否就绪、页面是否有变化）
func (flow *studioKeyFlow) observe(page studioKeyPage, now time.Time) {
	copied := page
	flow.lastPage = &copied
	if !flow.baseCaptured {
		// 点创建之前没读到页面：以点击后第一次读到的提示为基准
		flow.captureAlerts(page)
	}
	kind := classifyStudioKeyDialog(page.Dialog)
	if kind == studioKeyDialogKey || kind == studioKeyDialogProject {
		flow.dialogSeen = true
		flow.dialogSinceOpen = true
	}
	if page.Dialog == nil {
		if flow.noDialogSince.IsZero() {
			flow.noDialogSince = now
		}
	} else {
		flow.noDialogSince = time.Time{}
	}
	existing := hasExistingOption(page.Options)
	if len(page.Options) > 0 {
		flow.optionsSeen = true
	}
	if page.Dialog != nil && page.Dialog.Select != nil && hasExistingOption(page.Dialog.Select.Options) {
		// 原生 select 的选项看得到、点不了：有已有项目时同样不新建
		existing = true
	}
	if existing && !flow.projectStarted() {
		flow.existingOffered = true
	}
	if signature := studioKeyOptionsSignature(page.Options); signature != flow.optionsSig || flow.optionsChangedAt.IsZero() {
		flow.optionsSig = signature
		flow.optionsChangedAt = now
	}
	if flow.projectStarted() && flow.projectReadyAt.IsZero() {
		ready := len(flow.newProjects) > 0 || existing
		if kind == studioKeyDialogKey && flow.selectedProject(page.Dialog.Select) {
			ready = true
		}
		if ready {
			flow.projectReadyAt = now
			if len(flow.newProjects) > 0 {
				flow.note("新项目已出现（" + flow.newProjects[0].Name + "）")
			} else {
				flow.note("新项目已出现")
			}
		}
	}
	signature := studioKeySignature(page, kind)
	if signature != flow.signature {
		flow.signature = signature
		flow.changedAt = now
	}
}

// studioKeyOptionsSignature 为判断下拉列表有没有变化用的摘要（列表收起时为空，重新展开后重新计时）
func studioKeyOptionsSignature(options []studioKeyControl) string {
	var builder strings.Builder
	for _, option := range options {
		fmt.Fprintf(&builder, "%s:%v:%v|", option.Text, option.Enabled, option.Button)
	}
	return builder.String()
}

// listSettled 表示展开着的下拉列表已连续 listSettle 没变，对话框与列表都不在转圈（项目列表应已加载完）
func (flow *studioKeyFlow) listSettled(page studioKeyPage, now time.Time) bool {
	busy := page.ListBusy || page.Dialog != nil && page.Dialog.Busy
	return !busy && len(page.Options) > 0 && !flow.optionsChangedAt.IsZero() && now.Sub(flow.optionsChangedAt) >= studioKeyTiming.listSettle
}

// studioKeySignature 为判断页面有没有变化用的摘要
func studioKeySignature(page studioKeyPage, kind string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s|%s|%d|%s|%d", page.Path, kind, len(page.Options), page.Create, len(page.Alerts))
	if dialog := page.Dialog; dialog != nil {
		fmt.Fprintf(&builder, "|%s|%v|%s", dialog.Title, dialog.Busy, dialog.Error)
		for _, button := range dialog.Buttons {
			fmt.Fprintf(&builder, "|%s:%v", button.Text, button.Enabled)
		}
		if dialog.Select != nil {
			fmt.Fprintf(&builder, "|select:%s:%v:%v", dialog.Select.Value, dialog.Select.Empty, dialog.Select.Disabled)
		}
		for _, box := range dialog.Boxes {
			fmt.Fprintf(&builder, "|box:%v", box.Checked)
		}
		for _, input := range dialog.Inputs {
			fmt.Fprintf(&builder, "|input:%v", input.Empty)
		}
	}
	return builder.String()
}

// apply 记下这一步已经做了（点击已发出、导航已开始）
func (flow *studioKeyFlow) apply(step studioKeyStep, now time.Time) {
	flow.lastAction = now
	switch step.Mark {
	case studioKeyMarkOpen:
		flow.opens++
		flow.openedAt = now
		flow.dialogSinceOpen = false
		flow.noDialogSince = time.Time{}
	case studioKeyMarkSelect:
		flow.selectOpens++
		flow.selectAt = now
	case studioKeyMarkPick:
		flow.picks++
		flow.pickAt = now
		flow.picked = step.Target.Text
	case studioKeyMarkProjectEntry:
		flow.projectEntryAt = now
	case studioKeyMarkProjectConfirm:
		flow.projectConfirmAt = now
	case studioKeyMarkKeyConfirm:
		flow.keyConfirmAt = now
	case studioKeyMarkTick:
		flow.ticks++
		flow.ticked[step.Target.Text] = true
	case studioKeyMarkDismiss:
		flow.dismissAt = now
	case studioKeyMarkProjectsPage:
		flow.projectsPageAt = now
	case studioKeyMarkReturn:
		flow.returnedAt = now
	}
	flow.note(step.Note)
}

func studioKeyWaitStep() studioKeyStep {
	return studioKeyStep{Kind: studioKeyStepWait}
}

func studioKeyFailStep(reason string) studioKeyStep {
	return studioKeyStep{Kind: studioKeyStepFail, Reason: reason}
}

func studioKeyClickStep(scope string, control studioKeyControl, mark string, note string) studioKeyStep {
	return studioKeyStep{
		Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: scope, Index: control.Index, Text: control.Text}, Mark: mark, Note: note,
	}
}

// stuck 表示页面停在同一个状态超过 studioKeyTiming.stuck，期间也没有点过东西
func (flow *studioKeyFlow) stuck(now time.Time) bool {
	last := flow.changedAt
	if flow.lastAction.After(last) {
		last = flow.lastAction
	}
	return !last.IsZero() && now.Sub(last) >= studioKeyTiming.stuck
}

// projectCreating 表示点过新建项目、新项目还没出现且没超时
func (flow *studioKeyFlow) projectCreating(now time.Time) bool {
	started := flow.projectConfirmAt
	if started.IsZero() {
		started = flow.projectEntryAt
	}
	return !started.IsZero() && flow.projectReadyAt.IsZero() && now.Sub(started) < studioKeyTiming.projectWait
}

// selectLimit 为一次运行里点项目下拉框的次数上限（新建项目之后要反复收起、展开等新项目出现，上限放宽）
func (flow *studioKeyFlow) selectLimit() int {
	if flow.projectStarted() {
		return 16
	}
	return 6
}

// projectTimedOut 表示点过新建项目的确认后等新项目超时
func (flow *studioKeyFlow) projectTimedOut(now time.Time) bool {
	return !flow.projectConfirmAt.IsZero() && flow.projectReadyAt.IsZero() && now.Sub(flow.projectConfirmAt) >= studioKeyTiming.projectWait
}

// newFailure 返回点过创建之后新出现的出错提示（页面提示条或对话框里的错误），没有时为空。新出现的验证账号提示只在页面没有
// 打开创建或新建项目的对话框（页面拒绝了创建），或点过确认之后才算出错；对话框正常打开时不管页面上的横幅
func (flow *studioKeyFlow) newFailure(page studioKeyPage) string {
	kind := classifyStudioKeyDialog(page.Dialog)
	confirmed := !flow.keyConfirmAt.IsZero() || !flow.projectConfirmAt.IsZero()
	for _, alert := range page.Alerts {
		if flow.baseAlerts[alert] {
			continue
		}
		if studioKeyLabels.verify.MatchString(alert) {
			if confirmed || kind != studioKeyDialogKey && kind != studioKeyDialogProject {
				return alert
			}
			continue
		}
		if studioKeyLabels.failure.MatchString(alert) {
			return alert
		}
	}
	if page.Dialog != nil && page.Dialog.Error != "" && studioKeyLabels.failure.MatchString(page.Dialog.Error) {
		return page.Dialog.Error
	}
	return ""
}

// decide 决定点过“Create API key”之后的下一步（先调用 observe）
func (flow *studioKeyFlow) decide(page studioKeyPage, now time.Time) studioKeyStep {
	if failure := flow.newFailure(page); failure != "" {
		reason := StudioKeyReasonPageError
		switch {
		case studioKeyLabels.verify.MatchString(failure):
			reason = StudioKeyReasonVerifyBlocked
		case !flow.keyConfirmAt.IsZero():
			reason = StudioKeyReasonKeyFailed
		case flow.projectStarted():
			reason = StudioKeyReasonProjectFailed
		}
		return studioKeyFailStep(reason + "：" + studioKeyText(failure, 120))
	}
	if flow.projectTimedOut(now) {
		return studioKeyFailStep(StudioKeyReasonProjectTimeout)
	}
	if reason := flow.expired(now); reason != "" {
		return studioKeyFailStep(reason)
	}
	if !flow.projectEntryAt.IsZero() && flow.projectConfirmAt.IsZero() && flow.projectReadyAt.IsZero() &&
		now.Sub(flow.projectEntryAt) >= studioKeyTiming.projectWait {
		return studioKeyFailStep(StudioKeyReasonProjectNoDialog)
	}
	if strings.HasPrefix(page.Path, "/projects") {
		return flow.decideProjectsPage(page, now)
	}
	kind := classifyStudioKeyDialog(page.Dialog)
	if len(page.Options) > 0 && (kind == studioKeyDialogKey || kind == studioKeyDialogNone) && flow.keyConfirmAt.IsZero() {
		return flow.decideOptions(page, now)
	}
	switch kind {
	case studioKeyDialogVerify:
		return flow.decideVerifyDialog(page.Dialog, now)
	case studioKeyDialogProject:
		return flow.decideProjectDialog(page.Dialog, now)
	case studioKeyDialogKey:
		return flow.decideKeyDialog(page.Dialog, now)
	case studioKeyDialogCreated:
		// 对话框里显示着 Key：等页面自己的响应
		return studioKeyWaitStep()
	case studioKeyDialogOther:
		if !flow.keyConfirmAt.IsZero() || flow.projectCreating(now) || !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonUnknownDialog)
	}
	return flow.decideNoDialog(page, now)
}

// expired 返回等新 Key 超时的失败原因，没到时为空：点了创建 Key 的确认后 createWait 内没读到新 Key；或点了“Create API key”
// （第一次或重新打开）之后一直没有出现对话框、也没读到新 Key（页面可能直接创建了 Key，这期间不再点，见 decideNoDialog）。
// 新建项目还没就绪时不算（由 projectWait 兜底）
func (flow *studioKeyFlow) expired(now time.Time) string {
	switch {
	case !flow.keyConfirmAt.IsZero():
		if now.Sub(flow.keyConfirmAt) > studioKeyTiming.createWait {
			return StudioKeyReasonCreateFailed
		}
		return ""
	case flow.openedAt.IsZero() || flow.dialogSinceOpen || flow.projectCreating(now) || now.Sub(flow.openedAt) <= studioKeyTiming.createWait:
		return ""
	case !flow.verifyDialogAt.IsZero():
		// 其间弹过验证账号的提示：按它报
		return StudioKeyReasonVerifyBlocked
	case flow.opens > 1:
		return StudioKeyReasonDialogClosed
	}
	return StudioKeyReasonCreateFailed
}

// returnPending 表示为新建项目去过 Projects 页、回到 API Keys 页之后还没点过“Create API key”
func (flow *studioKeyFlow) returnPending() bool {
	return !flow.returnedAt.IsZero() && !flow.returnedAt.Before(flow.openedAt)
}

// decideNoDialog 处理没有对话框的情况。每次点“Create API key”之后要先见到对话框才会再点（页面可能不弹对话框直接创建了 Key，
// 再点就会多建）；三次打开为上限
func (flow *studioKeyFlow) decideNoDialog(page studioKeyPage, now time.Time) studioKeyStep {
	if !flow.keyConfirmAt.IsZero() {
		// 点过创建 Key 的确认：等响应（由 createWait 兜底）
		return studioKeyWaitStep()
	}
	if flow.returnPending() {
		return flow.decideReturned(page, now)
	}
	if !flow.dialogSinceOpen {
		// 点了“Create API key”（第一次或重新打开）后还没出现过对话框：不再点，等响应（由 createWait 兜底，见 expired）
		return studioKeyWaitStep()
	}
	// 创建对话框出现过、又关掉了（新建项目时页面关了它）：重新打开一次，在下拉框里看新项目是否已经出现（页面自己的响应里不一定读得到
	// 新项目的编号）。新建项目还没就绪时多等一会儿（noDialog）再打开
	since := flow.noDialogSince
	creating := flow.projectCreating(now)
	delay := studioKeyTiming.retry
	if creating {
		delay = studioKeyTiming.noDialog
	}
	if page.Create == "enabled" && flow.opens < 3 && !since.IsZero() && now.Sub(since) >= delay && now.Sub(flow.lastAction) >= studioKeyTiming.retry {
		return studioKeyStep{Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "page-create"}, Mark: studioKeyMarkOpen, Note: "重新打开创建对话框"}
	}
	if creating {
		return studioKeyWaitStep()
	}
	if flow.stuck(now) && !since.IsZero() && now.Sub(since) >= studioKeyTiming.stuck {
		if page.Create == "disabled" {
			return studioKeyFailStep(StudioKeyReasonCreateDisabled)
		}
		return studioKeyFailStep(StudioKeyReasonDialogClosed)
	}
	return studioKeyWaitStep()
}

// decideReturned 处理从 Projects 页回到 API Keys 页、还没点过“Create API key”：先等 settle 让 Key 列表的响应到达（新项目可能自带 Key，
// 读到新 Key 时主循环直接返回、不再创建），再点一次“Create API key”（点之前主循环像第一次那样核对页面上是否已显示 Key）
func (flow *studioKeyFlow) decideReturned(page studioKeyPage, now time.Time) studioKeyStep {
	if now.Sub(flow.returnedAt) < studioKeyTiming.settle {
		return studioKeyWaitStep()
	}
	if page.Create == "enabled" && flow.opens < 3 && now.Sub(flow.lastAction) >= studioKeyTiming.retry {
		return studioKeyStep{Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "page-create"}, Mark: studioKeyMarkOpen, Note: "打开创建对话框"}
	}
	if now.Sub(flow.returnedAt) < studioKeyTiming.settle+studioKeyTiming.stuck {
		return studioKeyWaitStep()
	}
	switch {
	case page.Create == "disabled":
		return studioKeyFailStep(StudioKeyReasonCreateDisabled)
	case flow.opens == 0:
		// 从 Projects 页新建项目回来，API Keys 页上仍没有“Create API key”
		return studioKeyFailStep(StudioKeyReasonNoCreateAfter)
	}
	return studioKeyFailStep(StudioKeyReasonDialogClosed)
}

// decideVerifyDialog 处理验证账号的对话框：不点验证，只点一次它自己的关闭类按钮；关不掉时失败
func (flow *studioKeyFlow) decideVerifyDialog(dialog *studioKeyDialog, now time.Time) studioKeyStep {
	if flow.verifyDialogAt.IsZero() {
		flow.verifyDialogAt = now
	}
	if flow.dismissAt.IsZero() {
		for _, button := range dialog.Buttons {
			if button.Enabled && !button.link() && studioKeyLabels.cancel.MatchString(button.Text) && !studioKeyLabels.verify.MatchString(button.Text) {
				return studioKeyClickStep("dialog", button, studioKeyMarkDismiss, "关闭验证账号提示“"+button.Text+"”")
			}
		}
	}
	if now.Sub(flow.verifyDialogAt) >= studioKeyTiming.noDialog {
		return studioKeyFailStep(StudioKeyReasonVerifyModal)
	}
	return studioKeyWaitStep()
}

// pendingTermsBox 返回该勾的条款勾选框：未勾、可勾、标签像服务条款、不是邮件订阅类，且标为必选或确认按钮不可用；每个只勾一次，
// 一次运行最多勾三次
func (flow *studioKeyFlow) pendingTermsBox(dialog *studioKeyDialog, confirmBlocked bool) (studioKeyBox, bool) {
	if flow.ticks >= 3 {
		return studioKeyBox{}, false
	}
	for _, box := range dialog.Boxes {
		if box.Checked || box.Disabled || flow.ticked[box.Label] {
			continue
		}
		if !studioKeyLabels.terms.MatchString(box.Label) || studioKeyLabels.optional.MatchString(box.Label) {
			continue
		}
		if box.Required || confirmBlocked {
			return box, true
		}
	}
	return studioKeyBox{}, false
}

func studioKeyTickStep(box studioKeyBox) studioKeyStep {
	return studioKeyStep{
		Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "box", Index: box.Index, Text: box.Label},
		Mark: studioKeyMarkTick, Note: "勾选条款“" + box.Label + "”",
	}
}

// projectNameEmpty 判断新建项目对话框的项目名为空：第一个输入框为空，且它是必填、标签像“名称”，或对话框里只有这一个输入框
func projectNameEmpty(dialog *studioKeyDialog) bool {
	if len(dialog.Inputs) == 0 || !dialog.Inputs[0].Empty {
		return false
	}
	first := dialog.Inputs[0]
	return first.Required || len(dialog.Inputs) == 1 || studioKeyLabels.name.MatchString(first.Label)
}

// emptyInput 返回对话框里第一个空的输入框
func emptyInput(dialog *studioKeyDialog) (studioKeyInput, bool) {
	for _, input := range dialog.Inputs {
		if input.Empty {
			return input, true
		}
	}
	return studioKeyInput{}, false
}

// projectEntryControl 返回对话框里新建项目的入口（按钮或站内链接；站外链接如“在 Google Cloud 控制台新建项目”不算）
func projectEntryControl(dialog *studioKeyDialog) (studioKeyControl, bool) {
	for _, button := range dialog.Buttons {
		if button.Enabled && !button.External && isCreateProjectLabel(button.Text, button.Icon) && !isKeyConfirmLabel(button.Text) {
			return button, true
		}
	}
	return studioKeyControl{}, false
}

// projectEntryStep 返回点新建项目入口的一步：站内链接改为在当前页导航（链接可能在新标签页打开），站外链接不跟
func (flow *studioKeyFlow) projectEntryStep(control studioKeyControl) (studioKeyStep, bool) {
	if control.External {
		return studioKeyStep{}, false
	}
	if control.Href != "" {
		if !flow.projectsPageAt.IsZero() || !strings.HasPrefix(control.Href, "/") {
			return studioKeyStep{}, false
		}
		return studioKeyStep{Kind: studioKeyStepNavigate, Path: control.Href, Mark: studioKeyMarkProjectsPage, Note: "打开新建项目页 " + control.Href}, true
	}
	return studioKeyClickStep("dialog", control, studioKeyMarkProjectEntry, "点击新建项目“"+control.Text+"”"), true
}

// emptyStateStep 返回 API Keys 页上没有可用的“Create API key”、页面主区域却有新建项目的按钮（账户还没有项目时的空状态）时的
// 一步：去一次 Projects 页新建项目（规则同 decideProjectsPage），回来再点“Create API key”。页面自己的响应里出现过项目时不去
func (flow *studioKeyFlow) emptyStateStep(page studioKeyPage) (studioKeyStep, bool) {
	if page.Project == "" || page.Dialog != nil || !flow.mayCreateProject() || !flow.projectsPageAt.IsZero() {
		return studioKeyStep{}, false
	}
	if !flow.baseCaptured {
		flow.captureAlerts(page)
	}
	flow.lastPage = &page
	return studioKeyStep{
		Kind: studioKeyStepNavigate, Path: studioKeyProjectsPath, Mark: studioKeyMarkProjectsPage,
		Note: "打开 Projects 页新建项目（API Keys 页没有可用的“Create API key”）",
	}, true
}

// decideKeyDialog 处理创建 Key 的对话框
func (flow *studioKeyFlow) decideKeyDialog(dialog *studioKeyDialog, now time.Time) studioKeyStep {
	if !flow.keyConfirmAt.IsZero() {
		return studioKeyWaitStep()
	}
	var confirm *studioKeyControl
	for index := range dialog.Buttons {
		button := dialog.Buttons[index]
		if !button.link() && isKeyConfirmLabel(button.Text) && !studioKeyLabels.cancel.MatchString(button.Text) {
			confirm = &dialog.Buttons[index]
		}
	}
	confirmBlocked := confirm == nil || !confirm.Enabled
	if box, ok := flow.pendingTermsBox(dialog, confirmBlocked); ok {
		return studioKeyTickStep(box)
	}
	selection := dialog.Select
	if selection != nil && !flow.selectedProject(selection) {
		return flow.decideEmptySelect(dialog, now)
	}
	if confirm != nil && confirm.Enabled {
		return studioKeyClickStep("dialog", *confirm, studioKeyMarkKeyConfirm, "确认创建 Key“"+confirm.Text+"”")
	}
	if selection == nil && confirm == nil {
		// 没有下拉框、也没有创建 Key 的按钮，只有新建项目的入口：先新建项目（对话框停住 listSettle、不在转圈之后）
		if entry, ok := projectEntryControl(dialog); ok {
			if !flow.mayCreateProject() {
				return flow.createRefused(now)
			}
			if dialog.Busy || now.Sub(flow.changedAt) < studioKeyTiming.listSettle {
				return studioKeyWaitStep()
			}
			if step, ok := flow.projectEntryStep(entry); ok {
				return step
			}
		}
	}
	if flow.projectCreating(now) || !flow.stuck(now) {
		return studioKeyWaitStep()
	}
	if confirm == nil {
		return studioKeyFailStep(StudioKeyReasonDialog)
	}
	if input, ok := emptyInput(dialog); ok && (input.Required || len(dialog.Inputs) == 1) {
		return studioKeyFailStep(StudioKeyReasonKeyName)
	}
	return studioKeyFailStep(StudioKeyReasonKeyConfirm)
}

// decideEmptySelect 处理项目下拉框没有选中项目：先展开看有没有已有项目；有就选，没有再新建（最多一次）
func (flow *studioKeyFlow) decideEmptySelect(dialog *studioKeyDialog, now time.Time) studioKeyStep {
	selection := dialog.Select
	if flow.projectCreating(now) && now.Sub(flow.lastAction) < studioKeyTiming.retry*3 {
		// 刚点过新建项目：给页面一点时间自己选上新项目
		return studioKeyWaitStep()
	}
	if !selection.Disabled && !selection.Expanded && flow.selectOpens < flow.selectLimit() &&
		now.Sub(flow.selectAt) >= studioKeyTiming.retry && now.Sub(flow.lastAction) >= studioKeyTiming.retry/2 {
		return studioKeyStep{
			Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "select"}, Mark: studioKeyMarkSelect, Note: "展开项目下拉框",
		}
	}
	if flow.projectCreating(now) {
		return studioKeyWaitStep()
	}
	if !flow.noProjectsAt.IsZero() && flow.mayCreateProject() {
		// 展开过的列表已断定没有已有项目（见 decideOptions），列表却收起了：用对话框里的新建项目入口
		if entry, ok := projectEntryControl(dialog); ok {
			if step, ok := flow.projectEntryStep(entry); ok {
				return step
			}
		}
	}
	if !flow.stuck(now) {
		return studioKeyWaitStep()
	}
	if flow.selectOpens > 0 && !flow.optionsSeen && flow.mayCreateProject() && selectShowsNoProject(dialog) {
		// 下拉框点不开（或展开后看不到选项），下拉框本身又表明没有项目：对话框里另有新建项目的入口时用它
		if entry, ok := projectEntryControl(dialog); ok {
			if step, ok := flow.projectEntryStep(entry); ok {
				return step
			}
		}
	}
	if !flow.unlistedAt.IsZero() {
		return studioKeyFailStep(StudioKeyReasonProjectUnlisted)
	}
	return studioKeyFailStep(StudioKeyReasonSelectStuck)
}

// decideOptions 处理展开着的下拉选项
func (flow *studioKeyFlow) decideOptions(page studioKeyPage, now time.Time) studioKeyStep {
	var existing, creates []studioKeyControl
	importOnly := true
	kinds := classifyStudioKeyOptions(page.Options)
	for index, option := range page.Options {
		switch kinds[index] {
		case studioKeyOptionExisting:
			existing = append(existing, option)
			importOnly = false
		case studioKeyOptionCreate:
			if option.Enabled {
				creates = append(creates, option)
			}
			importOnly = false
		case studioKeyOptionImport, studioKeyOptionNone:
		default:
			importOnly = false
		}
	}
	if page.Dialog != nil && page.Dialog.Select != nil && flow.selectedProject(page.Dialog.Select) && len(existing) == 0 {
		// 已经选着项目，列表却开着（挡住对话框，这时点确认只会点在遮罩上）：点下拉框所在位置把它收起来
		if flow.selectOpens < flow.selectLimit() && now.Sub(flow.lastAction) >= studioKeyTiming.retry {
			return studioKeyStep{Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "select"}, Mark: studioKeyMarkSelect, Note: "收起项目下拉框"}
		}
		if !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonSelectStuck)
	}
	if len(existing) > 0 {
		if flow.picks >= 3 {
			return studioKeyFailStep(StudioKeyReasonSelectStuck)
		}
		if now.Sub(flow.pickAt) < studioKeyTiming.retry {
			return studioKeyWaitStep()
		}
		chosen := flow.preferredOption(existing)
		return studioKeyClickStep("option", chosen, studioKeyMarkPick, "选择项目“"+chosen.Text+"”")
	}
	if flow.projectStarted() {
		// 这次已经新建过项目、列表里还没有它：隔一会儿把列表收起来（点下拉框所在位置）再展开，让页面刷新列表；不再点新建
		if page.Dialog != nil && page.Dialog.Select != nil && flow.selectOpens < flow.selectLimit() &&
			now.Sub(flow.lastAction) >= studioKeyTiming.retry*2 {
			return studioKeyStep{Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "select"}, Mark: studioKeyMarkSelect, Note: "收起项目下拉框等新项目"}
		}
		if flow.projectCreating(now) || !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonProjectTimeout)
	}
	if len(creates) == 0 && !importOnly {
		// 列表里只有占位、普通按钮或不可用的选项（可能还在加载）：不据此断定没有项目
		if !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonSelectStuck)
	}
	// 列表里只有新建项目、导入或“没有可用项目”的提示：账户已有项目时不新建；否则等列表稳定（不变、不转圈）再断定没有项目
	if !flow.mayCreateProject() {
		return flow.awaitUnlisted(page, now)
	}
	if !flow.listSettled(page, now) {
		if !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonListLoading)
	}
	if flow.noProjectsAt.IsZero() {
		flow.noProjectsAt = now
		flow.note("下拉框里没有已有项目")
	}
	if len(creates) > 0 {
		return studioKeyClickStep("option", creates[0], studioKeyMarkProjectEntry, "点击新建项目“"+creates[0].Text+"”")
	}
	// 只有导入项目或“没有可用项目”的提示：先看对话框里有没有新建项目的入口，再试 Projects 页（各一次）
	if page.Dialog != nil {
		if entry, ok := projectEntryControl(page.Dialog); ok {
			if step, ok := flow.projectEntryStep(entry); ok {
				return step
			}
		}
	}
	if flow.projectsPageAt.IsZero() {
		return studioKeyStep{Kind: studioKeyStepNavigate, Path: studioKeyProjectsPath, Mark: studioKeyMarkProjectsPage, Note: "下拉框里没有项目，打开 Projects 页"}
	}
	if !flow.stuck(now) {
		return studioKeyWaitStep()
	}
	return studioKeyFailStep(StudioKeyReasonNoProjectEntry)
}

// awaitUnlisted 处理账户已有项目（页面响应或下拉框里出现过）、展开的列表里却没有可选项目：不新建；列表停住后隔一会儿把它收起来
// （点下拉框所在位置）让页面刷新、再展开（见 decideEmptySelect），过了 stuck 仍没有就失败
func (flow *studioKeyFlow) awaitUnlisted(page studioKeyPage, now time.Time) studioKeyStep {
	if flow.unlistedAt.IsZero() {
		flow.unlistedAt = now
		flow.note("账户已有项目，下拉框里还没列出")
	}
	if now.Sub(flow.unlistedAt) >= studioKeyTiming.stuck {
		return studioKeyFailStep(StudioKeyReasonProjectUnlisted)
	}
	if page.Dialog != nil && page.Dialog.Select != nil && flow.selectOpens < flow.selectLimit() && flow.listSettled(page, now) &&
		now.Sub(flow.lastAction) >= studioKeyTiming.retry*2 {
		return studioKeyStep{Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "select"}, Mark: studioKeyMarkSelect, Note: "收起项目下拉框等已有项目"}
	}
	return studioKeyWaitStep()
}

// createRefused 返回有新建项目的入口、却不能新建时的下一步：新建中等待；这次已经建过时失败（防多建）；账户已有项目
// （页面响应或下拉框里出现过）时等一会儿，仍是这样就失败
func (flow *studioKeyFlow) createRefused(now time.Time) studioKeyStep {
	switch {
	case flow.projectCreating(now):
		return studioKeyWaitStep()
	case flow.projectStarted():
		return studioKeyFailStep(StudioKeyReasonProjectLimit)
	case !flow.stuck(now):
		return studioKeyWaitStep()
	}
	return studioKeyFailStep(StudioKeyReasonProjectUnlisted)
}

// preferredOption 选出要用的已有项目：这次新建的项目优先，其次 Default Gemini Project 或 gen-lang-client-<数字>，再次第一个
func (flow *studioKeyFlow) preferredOption(options []studioKeyControl) studioKeyControl {
	for _, project := range flow.newProjects {
		for _, option := range options {
			for _, name := range []string{project.Display, project.ID, project.Number} {
				if name != "" && strings.Contains(option.Text, name) {
					return option
				}
			}
		}
	}
	for _, option := range options {
		if studioKeyLabels.preferred.MatchString(option.Text) {
			return option
		}
	}
	return options[0]
}

// decideProjectDialog 处理新建项目的对话框：勾必需的条款，项目名已填好时点它自己的确认（一次运行最多一次）
func (flow *studioKeyFlow) decideProjectDialog(dialog *studioKeyDialog, now time.Time) studioKeyStep {
	if !flow.projectConfirmAt.IsZero() {
		// 已确认：等它建好（对话框里转圈或关闭）
		return studioKeyWaitStep()
	}
	if flow.projectEntryAt.IsZero() && (flow.existingOffered || len(flow.known) > 0) {
		// 账户已有项目（下拉框或页面响应里出现过），却出现了新建项目的对话框（不是这次点出来的）：不建
		if !flow.stuck(now) {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonUnknownDialog)
	}
	var confirm *studioKeyControl
	for index := range dialog.Buttons {
		button := dialog.Buttons[index]
		if button.link() || studioKeyLabels.cancel.MatchString(button.Text) || studioKeyLabels.importProject.MatchString(button.Text) {
			continue
		}
		if studioKeyLabels.projectConfirm.MatchString(button.Text) || isCreateProjectLabel(button.Text, button.Icon) {
			confirm = &dialog.Buttons[index]
		}
	}
	if projectNameEmpty(dialog) {
		// 项目名可能稍后才预填：页面停住一会儿仍为空才判；不勾、不点确认（空名字提交多半报错，也不在页面里输入文字）
		if now.Sub(flow.changedAt) < studioKeyTiming.retry*2 {
			return studioKeyWaitStep()
		}
		return studioKeyFailStep(StudioKeyReasonProjectName)
	}
	confirmBlocked := confirm == nil || !confirm.Enabled
	if box, ok := flow.pendingTermsBox(dialog, confirmBlocked); ok {
		return studioKeyTickStep(box)
	}
	if confirm != nil && confirm.Enabled {
		return studioKeyClickStep("dialog", *confirm, studioKeyMarkProjectConfirm, "确认新建项目“"+confirm.Text+"”")
	}
	if !flow.stuck(now) || dialog.Busy && now.Sub(flow.changedAt) < 3*studioKeyTiming.stuck {
		return studioKeyWaitStep()
	}
	if confirm == nil {
		return studioKeyFailStep(StudioKeyReasonUnknownDialog)
	}
	return studioKeyFailStep(StudioKeyReasonProjectConfirm)
}

// decideProjectsPage 处理 Projects 页（备选流程）：点一次页面自己的新建项目按钮，在对话框里确认，新项目出现后回到 API Keys 页
func (flow *studioKeyFlow) decideProjectsPage(page studioKeyPage, now time.Time) studioKeyStep {
	kind := classifyStudioKeyDialog(page.Dialog)
	if kind == studioKeyDialogProject || kind == studioKeyDialogOther && page.Dialog != nil && len(page.Dialog.Inputs) > 0 &&
		flow.projectConfirmAt.IsZero() && !flow.projectEntryAt.IsZero() {
		return flow.decideProjectDialog(page.Dialog, now)
	}
	if !flow.projectReadyAt.IsZero() || !flow.projectConfirmAt.IsZero() && page.Dialog == nil && now.Sub(flow.projectConfirmAt) >= studioKeyTiming.retry*3 {
		if now.Sub(flow.lastAction) < studioKeyTiming.retry {
			return studioKeyWaitStep()
		}
		return studioKeyStep{Kind: studioKeyStepNavigate, Path: StudioKeyPath, Mark: studioKeyMarkReturn, Note: "回到 API Keys 页"}
	}
	if flow.projectStarted() {
		if flow.projectCreating(now) && (!flow.projectConfirmAt.IsZero() || !flow.stuck(now)) {
			return studioKeyWaitStep()
		}
		if flow.projectConfirmAt.IsZero() {
			return studioKeyFailStep(StudioKeyReasonProjectConfirm)
		}
		return studioKeyFailStep(StudioKeyReasonProjectTimeout)
	}
	if !flow.mayCreateProject() {
		// 页面自己的响应里出现了项目（Projects 页列出了已有项目），或下拉框里出现过已有项目：不新建
		return studioKeyFailStep(StudioKeyReasonProjectUnlisted)
	}
	if page.Project != "" {
		if now.Sub(flow.projectsPageAt) < 3*studioKeyTiming.listSettle {
			// 先等 Projects 页加载项目列表（列出已有项目时不新建）
			return studioKeyWaitStep()
		}
		return studioKeyStep{
			Kind: studioKeyStepClick, Target: studioKeyTarget{Scope: "page-project", Text: page.Project},
			Mark: studioKeyMarkProjectEntry, Note: "点击 Projects 页的新建项目“" + page.Project + "”",
		}
	}
	if now.Sub(flow.projectsPageAt) < studioKeyTiming.settle+studioKeyTiming.stuck {
		return studioKeyWaitStep()
	}
	return studioKeyFailStep(StudioKeyReasonProjectsNoCreate)
}

// studioKeyText 打码（Key、邮箱）、把“|”换成“¦”、合并空白并截到 limit 个字符
func studioKeyText(text string, limit int) string {
	return snapshotText(strings.Join(strings.Fields(RedactStudioKeys(text)), " "), limit)
}

// detail 返回失败原因后面接的步骤记录与页面摘要（打码、截短；不用 PageSnapshotMarker，免得被 StripPageSnapshots 去掉）
func (flow *studioKeyFlow) detail() string {
	var parts []string
	if len(flow.steps) > 0 {
		parts = append(parts, "步骤="+studioKeyText(strings.Join(flow.steps, "→"), 200))
	}
	if flow.lastPage != nil {
		if summary := studioKeyPageSummary(*flow.lastPage); summary != "" {
			parts = append(parts, "对话框="+summary)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " | " + strings.Join(parts, " | ")
}

// studioKeyPageSummary 为页面状态的简短摘要：对话框标题、按钮、下拉框、选项、输入框、勾选框、错误与提示（打码、截短）
func studioKeyPageSummary(page studioKeyPage) string {
	var parts []string
	if page.Path != "" && page.Path != StudioKeyPath {
		parts = append(parts, "地址="+page.Host+page.Path)
	}
	if dialog := page.Dialog; dialog != nil {
		if dialog.Title != "" {
			parts = append(parts, "标题="+dialog.Title)
		}
		var buttons []string
		for _, button := range dialog.Buttons {
			if len(buttons) >= 6 {
				break
			}
			text := button.Text
			if text == "" {
				text = button.Icon
			}
			if !button.Enabled {
				text += "(不可用)"
			}
			buttons = append(buttons, text)
		}
		if len(buttons) > 0 {
			parts = append(parts, "按钮="+strings.Join(buttons, "/"))
		}
		if selection := dialog.Select; selection != nil {
			value := selection.Value
			if !selectHasProject(selection) {
				value = "未选(" + firstNonEmpty(selection.Value, selection.Placeholder) + ")"
			}
			parts = append(parts, "下拉框="+value)
		}
		for _, input := range dialog.Inputs {
			if input.Empty {
				parts = append(parts, "输入框为空="+input.Label)
				break
			}
		}
		for _, box := range dialog.Boxes {
			state := "未勾"
			if box.Checked {
				state = "已勾"
			}
			parts = append(parts, "勾选框="+box.Label+"("+state+")")
		}
		if dialog.Error != "" {
			parts = append(parts, "错误="+dialog.Error)
		}
	} else if page.Create != "" {
		parts = append(parts, "创建按钮="+page.Create)
	}
	if len(page.Options) > 0 {
		var options []string
		for _, option := range page.Options {
			if len(options) >= 6 {
				break
			}
			text := option.Text
			if !option.Enabled {
				text += "(不可用)"
			}
			options = append(options, text)
		}
		parts = append(parts, "选项="+strings.Join(options, "/"))
	}
	if len(page.Alerts) > 0 {
		parts = append(parts, "提示="+strings.Join(page.Alerts, "/"))
	}
	if page.Verify {
		parts = append(parts, "页面提示需要验证账号")
	}
	return studioKeyText(strings.Join(parts, "；"), 240)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
