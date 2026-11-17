package camoufoxnative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// 确认读到的 Key 属于这个账户（见 fetchStudioKey）。
//
// AI Studio 的每个页面都把一条公共的浏览器 Key 写在 HTML 配置里（WIZ_global_data），页面发给自己接口的请求也以 X-Goog-Api-Key
// （或 ?key=）带着它。它像 Key，却不是任何账户的 Key。为此：
//   - 拒绝集合：任何请求的地址与请求头里像 Key 的字符串（见 studioKeyListener.observe），页面文档、脚本、样式正文里像 Key 的字符串
//     （见 drain），都记进这次运行的拒绝集合。拒绝集合里的 Key 从不选用、从不当作“已保存的 Key”、从不算作页面上显示的 Key；
//   - 候选：只有页面接口的 JSON 类正文里整段是 Key 的字符串（见 ExtractStudioKeys）；
//   - 核对：只读表达式 studioKeyListExpression 读页面主区域的 Key 列表（每行打码的 Key 的末四位，页面显示完整 Key 时连同完整 Key）。
//     候选要与某一行对上（末四位相同、不在拒绝集合里；行上显示完整 Key 时完整相等）才算这个账户的 Key（见 chooseStudioKeyRow）。
//     列表里有行、响应里却没有对得上的完整 Key 时，打开这一行自己的详情读完整 Key（见 detailsKey，每次运行最多一次）；
//   - 空状态（页面自己写着没有 Key，或主区域既没有 Key 也没有数据行、只有创建按钮）才走创建流程；创建之后只认新出现的 Key
//     （见 newStudioKey）；
//   - 认不出 Key 列表时失败（StudioKeyReasonListUnknown），不拿响应里的 Key 充数；
//   - 排除的 Key（StudioKeyOptions.Exclude：其他账户保存着的、已作废的）同样从不选用，选行时跳过它们的行；页面上的 Key 都被排除时失败
//     （StudioKeyReasonExcluded），不创建。
//
// 拒绝集合里来源一定是页面自己的 Key（页面文档、脚本，或页面发给 AI Studio 自己接口的请求带着的）以 SHA-256 指纹随结果与错误交给调用方
// （ClientFingerprints），调用方据此作废保存着它们的记录（旧版本把它记成了账户的 Key）。
//
// 失败时（以及创建过、打开过详情时的步骤里）附上诊断：每条带像 Key 字符串的响应的域名与 RPC 方法名、MIME、Key 的末四位与它是被拒绝、
// 对上列表还是没对上，拒绝集合的来源，Key 列表的行数与各行末四位。诊断从不带完整 Key，邮箱打码。

// Key 列表的状态（studioKeyListView.State）
const (
	studioKeyListRows    = "rows"
	studioKeyListEmpty   = "empty"
	studioKeyListUnknown = "unknown"
)

// studioKeyListRow 为 Key 列表里的一行：Last4 为打码 Key 的末四位；Full 为页面上显示着的完整 Key（机密：只在内存里比对，不写日志），
// 没有显示完整 Key 时为空
type studioKeyListRow struct {
	Last4 string `json:"l"`
	Full  string `json:"f,omitempty"`
}

// studioKeyListView 为 studioKeyListExpression 的结果
type studioKeyListView struct {
	State string             `json:"state"`
	Rows  []studioKeyListRow `json:"rows"`
	// Create 为页面主区域的“Create API key”：enabled、disabled，没有时为空；Project 表示有新建项目的按钮
	Create  string `json:"create"`
	Project bool   `json:"project"`
	// EmptyText 表示主区域写着没有 Key（多语言）；DataRows 为主区域表格、列表里的数据行数（表头除外）
	EmptyText bool `json:"emptyText"`
	DataRows  int  `json:"dataRows"`
	// Dropped 为显示着拒绝集合里的完整 Key（页面自己的公共 Key）而不算的行数
	Dropped int `json:"-"`
}

// studioKeyPatternNoKeys 为页面写着“没有 API Key”的说法（英文、简体中文、繁体中文、日文、韩文；Go 与 JS 都能编译的子集）
const studioKeyPatternNoKeys = `\bno\s+(api\s+)?keys?\b|don'?t\s+have\s+any\s+(api\s+)?keys?|haven'?t\s+created\s+any\s+(api\s+)?keys?|没有.{0,8}(api\s*)?(密钥|key)|还没有.{0,8}(密钥|key)|暂无.{0,8}(密钥|key)|沒有.{0,8}(api\s*)?(金鑰|key)|尚未.{0,8}(金鑰|key)|キーがありません|キーはありません|키가\s*없`

// studioKeyMaskedAfter 为“完整 Key”后面（可隔空白）紧跟着的打码字符（…、⋯、两个以上的点、*、•、●）：这时它只是 Key 的前缀，
// 后面跟着打码的末四位（如“AQ.前缀…末四位”：AQ. 新格式 Key 只要求 AQ. 后至少 20 位，截断的前缀也像完整 Key），不是完整 Key
const studioKeyMaskedAfter = `/^\s*(?:…|⋯|\.{2}|\*|•|●)/`

// studioKeyListHelpers 为找 Key 列表的函数（依赖 pageDOMHelpers、studioKeyDOMHelpers）。打码的 Key 写作“…末四位”“...末四位”
// “****末四位”“••末四位”，或完整的 Key；“完整 Key”后面紧跟着打码字符的是截断的前缀（见 studioKeyMaskedAfter），不算 Key。
// 显示 Key 的元素按文本节点找（见 keyTexts），行里有几条 Key 按这些元素数（见 keysIn），不对整行连起来的文字用正则：真实浏览器里
// 相邻的行内元素之间没有空白时 innerText 会把打码的 Key 与后面复制图标的 content_copy 连成一串
var studioKeyListHelpers = `
  const keyChar = '[0-9A-Za-z_-]';
  const maskPrefix = '(?:\\.{3}|…|\\*{2,}|•{2,}|\\bAIza[0-9A-Za-z_-]{31}|\\bAQ\\.[0-9A-Za-z_-]{16,})';
  const anyKey = new RegExp(maskPrefix + '(' + keyChar + '{4})(?!' + keyChar + ')', 'g');
  const keyHint = /\.{3}|…|\*{2}|•{2}|AIza|AQ\./;
  const fullKey = /^(?:AIza[0-9A-Za-z_-]{35}|AQ\.[0-9A-Za-z_-]{20,})$/;
  const fullStart = /^(?:AIza|AQ\.)/;
  const maskedAfter = ` + studioKeyMaskedAfter + `;
  // keyMatches 为文字里的打码 Key 与完整 Key；后面紧跟着打码字符的“完整 Key”（截断的前缀）不算
  const keyMatches = text => [...text.matchAll(anyKey)]
    .filter(match => !(fullStart.test(match[0]) && maskedAfter.test(text.slice(match.index + match[0].length))));
  // keyTexts 为在元素里找 Key 的文字：自己的文本节点里有 Key 时只看它们（各自一段，不与子元素的文字连起来：后面紧跟着的复制图标等
  // 不会拼进 Key）；没有时看整段文字（打码字符与末四位分在子元素里，如 <span>…</span>TST1）
  const keyTexts = element => {
    const own = ownTexts(element).filter(text => keyMatches(text).length);
    if (own.length) return own;
    const text = textOf(element);
    return keyMatches(text).length ? [text] : [];
  };
  // truncatedNear 判断 leaf 里的“完整 Key”在它与往上几层（不出 root）的文字里后面紧跟着打码字符：前缀与打码的末四位分在两个元素里
  const truncatedNear = (leaf, key, root) => {
    let node = leaf;
    for (let depth = 0; node && depth < 4; depth++, node = node.parentElement) {
      const text = textOf(node);
      for (let index = text.indexOf(key); index >= 0; index = text.indexOf(key, index + 1)) {
        if (maskedAfter.test(text.slice(index + key.length))) return true;
      }
      if (node === root) break;
    }
    return false;
  };
  const rowSelector = 'tr, [role=row], mat-row, .mat-mdc-row, li, [role=listitem], mat-list-item, .mat-mdc-list-item';
  const cellSelector = 'td, th, [role=cell], [role=gridcell], [role=rowheader], mat-cell, .mat-mdc-cell';
  const keyArea = () => document.querySelector('main, [role=main]') || document.body;
  // keyItems 为主区域（不在对话框与弹层里）可见、显示着 Key 的最内层元素（leaf）与其中的 Key（keys：末四位 l，完整 Key 时连同 f；
  // 截断的前缀不算）
  const keyItems = root => {
    const holders = [...root.querySelectorAll('*')].filter(element => outsideOverlays(element) && keyHint.test(element.textContent || '') &&
      visible(element) && keyTexts(element).length);
    const items = [];
    for (const leaf of holders.filter(element => !holders.some(other => other !== element && within(other, element)))) {
      const keys = [];
      for (const text of keyTexts(leaf)) {
        for (const match of keyMatches(text)) {
          if (fullStart.test(match[0]) && truncatedNear(leaf, match[0], root)) continue;
          if (!keys.some(key => key.l === match[1])) keys.push({l: match[1], f: fullKey.test(match[0]) ? match[0] : ''});
        }
      }
      if (keys.length) items.push({leaf, keys});
    }
    return items;
  };
  // keysIn 为 element 里显示着的 Key 条数（按末四位，数 items 里在它里面的元素）
  const keysIn = (element, items) => new Set(items.filter(item => within(item.leaf, element)).flatMap(item => item.keys.map(key => key.l))).size;
  // rowOf 为显示 Key 的元素所在的行：只含这一条 Key 的表格行、列表项；没有这类结构时从它往上，取只含这一条 Key 的最外层
  // （最多 6 层，不到主区域本身）
  const rowOf = (leaf, root, items) => {
    let row = leaf.closest(rowSelector);
    if (row && (!within(row, root) || keysIn(row, items) !== 1)) row = null;
    if (!row) {
      row = leaf;
      for (let depth = 0; depth < 6; depth++) {
        const parent = row.parentElement;
        if (!parent || parent === root || !within(parent, root) || keysIn(parent, items) !== 1) break;
        row = parent;
      }
    }
    return row;
  };
`

// studioKeyListExpression 只读：读页面主区域的 Key 列表（见 studioKeyListView）。行里显示完整 Key 时连同完整 Key 一起返回（只在 Go 的
// 内存里比对）；state 为 rows（有行）、empty（没有 Key 的文字，且写着没有 Key，或只有创建按钮、没有数据行）或 unknown
var studioKeyListExpression = `(() => {` + pageDOMHelpers + studioKeyDOMHelpers + studioKeyListHelpers + `
  const view = {state: 'unknown', rows: [], create: '', project: false, emptyText: false, dataRows: 0};
  const root = keyArea();
  if (!root) return JSON.stringify(view);
  const found = [];
  const items = keyItems(root);
  for (const item of items) {
    const row = rowOf(item.leaf, root, items);
    for (const key of item.keys) {
      if (found.length >= 100) break;
      if (found.some(other => other.row === row && other.l === key.l)) continue;
      found.push({row, l: key.l, f: key.f});
    }
  }
  view.rows = found.map(item => item.f ? {l: item.l, f: item.f} : {l: item.l});
  const creates = pageCreateButtons();
  view.create = creates.some(buttonEnabled) ? 'enabled' : creates.length ? 'disabled' : '';
  view.project = Boolean(pageProjectButton());
  view.emptyText = pattern(` + jsString(studioKeyPatternNoKeys) + `).test(textOf(root));
  view.dataRows = [...root.querySelectorAll('tbody tr, [role=row], mat-row, .mat-mdc-row')]
    .filter(row => visible(row) && outsideOverlays(row) && !row.closest('thead') && !row.querySelector('th, [role=columnheader]')).length;
  if (view.rows.length) view.state = 'rows';
  else if (view.emptyText || ((view.create || view.project) && !view.dataRows)) view.state = 'empty';
  return JSON.stringify(view);
})()`

// studioKeyFullKeysScript 为在 scope 的文字与输入框的值里找完整 Key 的函数：逐个文本节点（与输入框的值）找，前后不紧接着 Key 字符
// （见 textRuns：不对整段 innerText 找，Key 不会与旁边的标签、复制图标的文字连在一起）。后面紧跟着打码字符的（同一段里，或这一段到此为止、
// 下一段以打码字符开头）是截断的前缀（见 studioKeyMaskedAfter），不算；同一个字符串在别处以前缀的样子出现过的也不算
const studioKeyFullKeysScript = `
  const fullKeysIn = scope => {
    const maskedAfter = ` + studioKeyMaskedAfter + `;
    const keyPattern = /(?<![0-9A-Za-z_-])(?:AIza[0-9A-Za-z_-]{35}|AQ\.[0-9A-Za-z_-]{20,})(?![0-9A-Za-z_-])/g;
    const runs = textRuns(scope);
    const keys = [];
    const truncated = [];
    runs.forEach((run, index) => {
      for (const match of run.text.matchAll(keyPattern)) {
        const rest = run.text.slice(match.index + match[0].length);
        const next = index + 1 < runs.length ? runs[index + 1].text : '';
        if (maskedAfter.test(rest) || !rest.trim() && maskedAfter.test(next)) truncated.push(match[0]);
        else if (!keys.includes(match[0])) keys.push(match[0]);
      }
    });
    return keys.filter(key => !truncated.includes(key));
  };
`

// studioKeyDetailsKeyExpression 只读：在最上层对话框（没有对话框、点了之后页面路径不再是 before 时为主区域）的文字与输入框的值里找
// 末四位为 last4 的完整 Key，恰好一条时返回它（机密：只在内存里用），否则为空
func studioKeyDetailsKeyExpression(last4 string, before string) string {
	return `((last4, before) => {` + pageDOMHelpers + studioKeyDOMHelpers + studioKeyFullKeysScript + `
  const dialog = currentDialog();
  const moved = !dialog && location.pathname !== before;
  const scope = dialog || (moved ? (document.querySelector('main, [role=main]') || document.body) : null);
  if (!scope) return '';
  const keys = fullKeysIn(scope).filter(key => key.endsWith(last4));
  return keys.length === 1 ? keys[0] : '';
})(` + jsString(last4) + `, ` + jsString(before) + `)`
}

// studioKeyCreatedKeyExpression 只读：返回最上层对话框（创建结果）里的完整 Key（JSON 数组，最多 4 条；机密：只在内存里用）
var studioKeyCreatedKeyExpression = `(() => {` + pageDOMHelpers + studioKeyDOMHelpers + studioKeyFullKeysScript + `
  const dialog = currentDialog();
  return JSON.stringify(dialog ? fullKeysIn(dialog).slice(0, 4) : []);
})()`

// studioKeyChoice 为在 Key 列表里选定的一行（hasRow）与确认到的 Key（confirmed）
type studioKeyChoice struct {
	row       studioKeyListRow
	hasRow    bool
	candidate StudioKeyCandidate
	confirmed bool
}

// chooseStudioKeyRow 在 Key 列表里选一行并确认它的 Key（denied 为拒绝集合；saved 为已保存的 Key，在拒绝集合里时调用方传空）：
//   - 一行的 Key 确认为：响应候选里不在拒绝集合里、末四位与这一行相同的那条（行上显示着完整 Key 时还要与它完整相等；有多条时取
//     已保存的那条，否则恰好一条才算）；或行上显示的完整 Key 就是已保存的那条。行上显示的完整 Key 单独不算确认（页面可能把 Key 的
//     前缀与打码的末四位写在一起，截断的前缀也像完整 Key）：没有响应印证时之后打开这一行的详情，读到的要与它相等；只读时还要与 saved
//     完整相等；
//   - 只读：只看 saved 末四位所在的行，没有这一行时 hasRow 为假；
//   - 否则：saved 所在的行优先（即使它还没确认：之后打开它的详情），再取第一条能确认的行，再取第一行（之后打开它的详情）
func chooseStudioKeyRow(rows []studioKeyListRow, candidates []StudioKeyCandidate, denied func(string) bool, saved string, readOnly bool) studioKeyChoice {
	confirm := func(row studioKeyListRow) (StudioKeyCandidate, bool) {
		if row.Full != "" && (denied(row.Full) || !IsStudioKey(row.Full) || StudioKeyLast4(row.Full) != row.Last4 || readOnly && row.Full != saved) {
			return StudioKeyCandidate{}, false
		}
		var matches []StudioKeyCandidate
		for _, candidate := range candidates {
			if !denied(candidate.Key) && StudioKeyLast4(candidate.Key) == row.Last4 && (row.Full == "" || candidate.Key == row.Full) {
				matches = append(matches, candidate)
			}
		}
		if row.Full != "" && len(matches) == 0 && row.Full == saved {
			// 行上显示的就是已保存的那条（以前确认过的）
			return StudioKeyCandidate{Key: row.Full}, true
		}
		for _, candidate := range matches {
			if saved != "" && candidate.Key == saved {
				return candidate, true
			}
		}
		if readOnly || len(matches) != 1 {
			return StudioKeyCandidate{}, false
		}
		return matches[0], true
	}
	var choice studioKeyChoice
	if readOnly && saved == "" {
		// 只读却没有可用的已保存 Key（在拒绝集合里）：没有要找的行
		return choice
	}
	if saved != "" {
		for _, row := range rows {
			if row.Last4 != StudioKeyLast4(saved) {
				continue
			}
			if candidate, ok := confirm(row); ok {
				return studioKeyChoice{row: row, hasRow: true, candidate: candidate, confirmed: true}
			}
			if !choice.hasRow {
				choice.row, choice.hasRow = row, true
			}
		}
		if choice.hasRow || readOnly {
			return choice
		}
	}
	for _, row := range rows {
		if candidate, ok := confirm(row); ok {
			return studioKeyChoice{row: row, hasRow: true, candidate: candidate, confirmed: true}
		}
	}
	if len(rows) > 0 {
		choice.row, choice.hasRow = rows[0], true
	}
	return choice
}

// readList 读一次 Key 列表；显示着拒绝集合里的完整 Key（页面自己的公共 Key）的行不算，完整 Key 与末四位对不上的只留末四位
func (run *studioKeyRun) readList(ctx context.Context) (studioKeyListView, bool) {
	encoded, err := run.session.client.evaluateString(ctx, run.session.contextID, studioKeyListExpression)
	if err != nil || encoded == "" {
		return studioKeyListView{}, false
	}
	var view studioKeyListView
	if json.Unmarshal([]byte(encoded), &view) != nil {
		return studioKeyListView{}, false
	}
	rows := view.Rows[:0]
	for _, row := range view.Rows {
		if !studioKeyLast4Pattern.MatchString(row.Last4) {
			continue
		}
		if row.Full != "" && (!IsStudioKey(row.Full) || StudioKeyLast4(row.Full) != row.Last4) {
			row.Full = ""
		}
		if row.Full != "" && run.isDenied(row.Full) {
			view.Dropped++
			continue
		}
		rows = append(rows, row)
	}
	view.Rows = rows
	if len(view.Rows) == 0 && view.State == studioKeyListRows {
		// 显示的只有拒绝集合里的完整 Key（如页面示例代码里的公共 Key）或认不出的行：它们不算页面上的 Key，按其余的样子重新判断是不是
		// 空状态（与 studioKeyListExpression 的判断相同）
		view.State = studioKeyListUnknown
		if view.EmptyText || (view.Create != "" || view.Project) && view.DataRows == 0 {
			view.State = studioKeyListEmpty
		}
	}
	run.list, run.listed = view, true
	return view, true
}

// isDenied 判断 Key 在这次的拒绝集合里
func (run *studioKeyRun) isDenied(key string) bool {
	_, ok := run.denied[key]
	return ok
}

// studioKeyClientLimit 为交给调用方的页面公共 Key 指纹的上限
const studioKeyClientLimit = 4

// StudioKeyFingerprint 返回 Key 的 SHA-256 指纹（十六进制）：只用来比对（记住作废过的 Key、排除只剩指纹的 Key），不写日志
func StudioKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// clientFingerprints 返回拒绝集合里来源一定是页面自己的 Key 的指纹（按进集合的顺序，最多 studioKeyClientLimit 条）
func (run *studioKeyRun) clientFingerprints() []string {
	var fingerprints []string
	for _, key := range run.deniedOrder {
		if run.denied[key].strong && len(fingerprints) < studioKeyClientLimit {
			fingerprints = append(fingerprints, StudioKeyFingerprint(key))
		}
	}
	return fingerprints
}

// loadExclusions 整理 StudioKeyOptions 里排除的 Key（完整 Key、它们的末四位与只剩指纹的）
func (run *studioKeyRun) loadExclusions() {
	run.exclude, run.excludeLast4, run.excludeFingerprints = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, key := range run.options.Exclude {
		if IsStudioKey(key) {
			run.exclude[key] = true
			run.excludeLast4[StudioKeyLast4(key)] = true
		}
	}
	for _, fingerprint := range run.options.ExcludeFingerprints {
		run.excludeFingerprints[fingerprint] = true
	}
}

// isExcluded 判断 Key 是排除的（StudioKeyOptions.Exclude 里的，或指纹在 ExcludeFingerprints 里）
func (run *studioKeyRun) isExcluded(key string) bool {
	if key == "" || len(run.exclude) == 0 && len(run.excludeFingerprints) == 0 {
		return false
	}
	return run.exclude[key] || len(run.excludeFingerprints) > 0 && run.excludeFingerprints[StudioKeyFingerprint(key)]
}

// unusable 判断 Key 不能当作这个账户的：在拒绝集合里，或是排除的
func (run *studioKeyRun) unusable(key string) bool {
	return run.isDenied(key) || run.isExcluded(key)
}

// excludedRow 判断 Key 列表的这一行显示的是排除的 Key：行上显示着完整 Key 时看它，否则看末四位是否与 Exclude 里某条相同
func (run *studioKeyRun) excludedRow(row studioKeyListRow) bool {
	if row.Full != "" {
		return run.isExcluded(row.Full)
	}
	return run.excludeLast4[row.Last4]
}

// usableRows 返回 Key 列表里不是排除的 Key 的行（只读时原样返回：只找已保存的那一行）
func (run *studioKeyRun) usableRows(rows []studioKeyListRow) []studioKeyListRow {
	if run.options.ReadOnly {
		return rows
	}
	var usable []studioKeyListRow
	for _, row := range rows {
		if !run.excludedRow(row) {
			usable = append(usable, row)
		}
	}
	return usable
}

// deny 把 Key 记进拒绝集合（已在里面时只在来源更强时更新来源）
func (run *studioKeyRun) deny(key string, source studioKeyDenySource) {
	if run.denied == nil {
		run.denied = make(map[string]studioKeyDenySource)
	}
	if existing, ok := run.denied[key]; ok {
		if source.strong && !existing.strong {
			run.denied[key] = source
		}
		return
	}
	if len(run.denied) >= studioKeyDenyLimit {
		return
	}
	run.denied[key] = source
	run.deniedOrder = append(run.deniedOrder, key)
}

// savedKey 返回可用的已保存 Key：在拒绝集合里或是排除的时为空
func (run *studioKeyRun) savedKey() string {
	if run.options.SavedKey == "" || run.unusable(run.options.SavedKey) {
		return ""
	}
	return run.options.SavedKey
}

// savedDenied 表示已保存的 Key 是页面自己的公共 Key（来源是页面文档、脚本，或发给 AI Studio 自己接口的请求）
func (run *studioKeyRun) savedDenied() bool {
	source, ok := run.denied[run.options.SavedKey]
	return run.options.SavedKey != "" && ok && source.strong
}

// savedPending 表示已保存的 Key 可用、列表里却还没有它那一行（列表可能还在渲染：settle 之前先不选别的行）
func (run *studioKeyRun) savedPending(list studioKeyListView) bool {
	saved := run.savedKey()
	return saved != "" && !slices.ContainsFunc(list.Rows, func(row studioKeyListRow) bool { return row.Last4 == StudioKeyLast4(saved) })
}

// liveCandidates 表示响应里有不在拒绝集合里、也不是创建前就有的 Key（排除的 Key 也算：页面上其实有 Key，不该创建）
func (run *studioKeyRun) liveCandidates() bool {
	return slices.ContainsFunc(run.keys, func(candidate StudioKeyCandidate) bool {
		return IsStudioKey(candidate.Key) && !run.isDenied(candidate.Key) && (run.before == nil || !run.before.keys[candidate.Key])
	})
}

// studioKeyBefore 为开始创建之前的状态：之后只认这里没有的 Key
type studioKeyBefore struct {
	keys  map[string]bool
	last4 map[string]bool
}

// markBefore 记下开始创建之前响应里的 Key 与列表里各行的末四位（只记第一次）
func (run *studioKeyRun) markBefore(list studioKeyListView) {
	if run.before != nil {
		return
	}
	run.before = &studioKeyBefore{keys: map[string]bool{}, last4: map[string]bool{}}
	for _, candidate := range run.keys {
		run.before.keys[candidate.Key] = true
	}
	for _, row := range list.Rows {
		run.before.last4[row.Last4] = true
		if row.Full != "" {
			run.before.keys[row.Full] = true
		}
	}
}

// freshKey 判断 Key 可以当作这次读到（或新建）的：是完整的 Key、不在拒绝集合里、不是排除的、不是开始创建之前就有的
func (run *studioKeyRun) freshKey(key string) bool {
	return IsStudioKey(key) && !run.unusable(key) && (run.before == nil || !run.before.keys[key])
}

// readCreatedKeys 读创建结果对话框里显示的完整 Key
func (run *studioKeyRun) readCreatedKeys(ctx context.Context) []string {
	encoded, err := run.session.client.evaluateString(ctx, run.session.contextID, studioKeyCreatedKeyExpression)
	if err != nil || encoded == "" {
		return nil
	}
	var keys []string
	if json.Unmarshal([]byte(encoded), &keys) != nil {
		return nil
	}
	return keys
}

// newStudioKey 在创建之后找新 Key，只认新出现、得到印证的：
//   - 创建结果对话框里显示过恰好一条新的完整 Key（记在 shownKeys，对话框关掉之后也算）：列表里新出现的一行末四位与它相同（行上显示着
//     完整 Key 时要完整相等），或还没有新出现的行、响应里有同一条 Key 时才认；有新出现的行却都对不上时不用。对话框里的文字单独不算：
//     读错了（如与旁边的文字连在一起）也不会被保存；
//   - 列表里新出现的一行（末四位不是创建前就有的）与响应里恰好一条新 Key 末四位相同（行上显示着完整 Key 时要完整相等；行上显示的完整 Key
//     单独不算，见 chooseStudioKeyRow）。
//
// 都没有、却有新出现的行时返回这一行（pending，之后打开它的详情）；awaiting 表示对话框里显示着新 Key，还没有新出现的行、响应里也没有它
// （之后关掉对话框等新行）
func (run *studioKeyRun) newStudioKey(list studioKeyListView, dialogKeys []string) (candidate StudioKeyCandidate, pending *studioKeyListRow, awaiting bool, ok bool) {
	var fresh []studioKeyListRow
	for _, row := range list.Rows {
		if run.before == nil || !run.before.last4[row.Last4] {
			fresh = append(fresh, row)
		}
	}
	for _, key := range dialogKeys {
		if run.freshKey(key) && !slices.Contains(run.shownKeys, key) && len(run.shownKeys) < 4 {
			run.shownKeys = append(run.shownKeys, key)
		}
	}
	var shown []string
	for _, key := range run.shownKeys {
		if run.freshKey(key) {
			shown = append(shown, key)
		}
	}
	if len(shown) == 1 {
		key := shown[0]
		switch {
		case slices.ContainsFunc(fresh, func(row studioKeyListRow) bool {
			return row.Last4 == StudioKeyLast4(key) && (row.Full == "" || row.Full == key)
		}):
			run.note("新 Key：创建结果对话框里显示的，与列表里新出现的一行对上（…" + StudioKeyLast4(key) + "）")
			return run.candidate(key), nil, false, true
		case len(fresh) == 0 && slices.ContainsFunc(run.keys, func(candidate StudioKeyCandidate) bool { return candidate.Key == key }):
			run.note("新 Key：创建结果对话框里显示的，与响应里的一致（…" + StudioKeyLast4(key) + "）")
			return run.candidate(key), nil, false, true
		case len(fresh) == 0:
			awaiting = true
			if !run.shownNoted {
				run.shownNoted = true
				run.note("创建结果对话框里显示了新 Key（…" + StudioKeyLast4(key) + "），还没有新出现的行或响应印证")
			}
		case !run.dialogMismatch:
			run.dialogMismatch = true
			run.note("创建结果对话框里的完整 Key（…" + StudioKeyLast4(key) + "）与列表里新出现的行对不上，不用")
		}
	}
	for index := range fresh {
		row := fresh[index]
		var matches []StudioKeyCandidate
		for _, candidate := range run.keys {
			if run.freshKey(candidate.Key) && StudioKeyLast4(candidate.Key) == row.Last4 && (row.Full == "" || candidate.Key == row.Full) {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 1 {
			run.note("新 Key：响应里与列表新出现的一行对上（…" + row.Last4 + "）")
			return matches[0], nil, false, true
		}
		if pending == nil {
			pending = &fresh[index]
		}
	}
	return StudioKeyCandidate{}, pending, awaiting, false
}

// detailsKey 打开这一行自己的详情读完整 Key（每次运行最多一次，见 openRowDetails）：读到的 Key 末四位与这一行相同、不在拒绝集合里、
// 行上显示着完整 Key 时与它相等，且 accept 认可时返回它（连同响应里同一条 Key 的记录）
func (run *studioKeyRun) detailsKey(ctx context.Context, row studioKeyListRow, accept func(string) bool) (StudioKeyCandidate, bool) {
	if run.details == nil {
		if run.detailsOpened {
			return StudioKeyCandidate{}, false
		}
		outcome := run.openRowDetails(ctx, row.Last4, "")
		run.details = &outcome
	}
	key := run.details.key
	switch {
	case key == "":
		run.note("Key 详情里没有读到末四位为 …" + row.Last4 + " 的完整 Key")
		return StudioKeyCandidate{}, false
	case row.Full != "" && key != row.Full || !accept(key):
		run.note("Key 详情里的完整 Key 不是要找的那条（…" + row.Last4 + "）")
		return StudioKeyCandidate{}, false
	}
	run.note("完整 Key：从" + run.details.keyFrom + "读到（…" + row.Last4 + "）")
	return run.candidate(key), true
}

// rowKeyFromResponses 返回响应里末四位为 last4、不在拒绝集合里的 Key（恰好一条时）
func (run *studioKeyRun) rowKeyFromResponses(last4 string) (string, bool) {
	var matches []string
	for _, candidate := range run.keys {
		if run.freshKey(candidate.Key) && StudioKeyLast4(candidate.Key) == last4 {
			matches = append(matches, candidate.Key)
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// failure 为失败：原因后面接创建流程的步骤与页面摘要、补项目信息与详情的记录，以及诊断（见 diagnostics）
func (run *studioKeyRun) failure(reason string, cause error) *StudioKeyError {
	detail := run.flow.detail()
	if len(run.notes) > 0 {
		detail += " | 记录=" + studioKeyText(strings.Join(run.notes, "→"), 200)
	}
	if diag := run.diagnostics(); diag != "" {
		detail += " | 诊断=" + diag
	}
	return &StudioKeyError{Reason: reason, Detail: detail, Cause: cause, SavedIsClient: run.savedDenied(), ClientFingerprints: run.clientFingerprints()}
}

// studioKeyDiag 为带像 Key 字符串的一类响应（同一域名、名字、MIME 合并，诊断用）：keys 为正文里像 Key 的字符串（机密：只在内存里，
// 写出时只用末四位），count 为条数
type studioKeyDiag struct {
	host, name, mime, kind string
	count                  int
	keys                   []string
	candidates             map[string]bool
}

// studioKeyDiagLimit 为诊断里最多列出的响应种类
const studioKeyDiagLimit = 24

// noteResponse 记下一条读过正文的响应（带像 Key 的字符串的按域名、名字、MIME 合并列出，其余只计数）
func (run *studioKeyRun) noteResponse(response studioKeyResponse, kind string, found []string, candidates []StudioKeyCandidate) {
	run.responses++
	if len(found) == 0 {
		return
	}
	index := slices.IndexFunc(run.diag, func(item studioKeyDiag) bool {
		return item.host == response.host && item.name == response.name && item.mime == response.mime && item.kind == kind
	})
	if index < 0 {
		if len(run.diag) >= studioKeyDiagLimit {
			run.diagDropped++
			return
		}
		run.diag = append(run.diag, studioKeyDiag{host: response.host, name: response.name, mime: response.mime, kind: kind, candidates: map[string]bool{}})
		index = len(run.diag) - 1
	}
	item := &run.diag[index]
	item.count++
	for _, key := range found {
		if !slices.Contains(item.keys, key) && len(item.keys) < 8 {
			item.keys = append(item.keys, key)
		}
	}
	for _, candidate := range candidates {
		item.candidates[candidate.Key] = true
	}
}

// diagnostics 返回诊断文字（只有末四位，邮箱打码，截短）：Key 列表、拒绝来源、带 Key 的响应（页面接口的在前）
func (run *studioKeyRun) diagnostics() string {
	listed := map[string]bool{}
	for _, row := range run.list.Rows {
		listed[row.Last4] = true
	}
	var parts []string
	if run.listed {
		list := run.list
		switch list.State {
		case studioKeyListRows:
			var tails []string
			for _, row := range list.Rows {
				if len(tails) >= 8 {
					tails = append(tails, "…")
					break
				}
				tails = append(tails, "…"+row.Last4)
			}
			parts = append(parts, fmt.Sprintf("列表 %d 行(%s)", len(list.Rows), strings.Join(tails, "/")))
		case studioKeyListEmpty:
			parts = append(parts, "列表为空")
		default:
			parts = append(parts, fmt.Sprintf("列表认不出(数据行 %d，创建按钮 %s)", list.DataRows, orNone(list.Create)))
		}
		if list.Dropped > 0 {
			parts = append(parts, fmt.Sprintf("列表里 %d 行显示的是页面自己的公共 Key，不算", list.Dropped))
		}
		if excluded := len(list.Rows) - len(run.usableRows(list.Rows)); excluded > 0 {
			parts = append(parts, fmt.Sprintf("列表里 %d 行是其他账户保存着的或已作废的 Key，排除", excluded))
		}
	}
	if len(run.deniedOrder) > 0 {
		var sources []string
		for _, key := range run.deniedOrder {
			if len(sources) >= 3 {
				sources = append(sources, fmt.Sprintf("共 %d 条", len(run.deniedOrder)))
				break
			}
			sources = append(sources, "…"+StudioKeyLast4(key)+"←"+run.denied[key].where)
		}
		parts = append(parts, "拒绝 "+strings.Join(sources, "、"))
	}
	var responses []string
	for _, kind := range []string{studioKeyBodyJSON, studioKeyBodyOther, studioKeyBodyPage} {
		for _, item := range run.diag {
			if item.kind != kind {
				continue
			}
			var marks []string
			for _, key := range item.keys {
				state := "没对上"
				switch {
				case key == run.chosen:
					state = "选用"
				case run.isDenied(key):
					state = "拒绝"
				case run.isExcluded(key):
					state = "排除"
				case item.kind != studioKeyBodyJSON:
					state = "不读"
				case !item.candidates[key]:
					state = "嵌在文字里"
				case listed[StudioKeyLast4(key)]:
					state = "对上列表"
				}
				marks = append(marks, "…"+StudioKeyLast4(key)+" "+state)
				if len(marks) >= 4 {
					break
				}
			}
			mime := item.mime
			if mime == "" {
				mime = "无类型"
			}
			text := fmt.Sprintf("%s/%s(%s)", item.host, item.name, mime)
			if item.count > 1 {
				text += fmt.Sprintf("×%d", item.count)
			}
			responses = append(responses, fmt.Sprintf("%s %d 个[%s]", text, len(item.keys), strings.Join(marks, "、")))
		}
	}
	if len(responses) > 0 || run.responses > 0 {
		text := fmt.Sprintf("响应 %d 条", run.responses)
		if len(responses) > 0 {
			text += "，带 Key 的：" + strings.Join(responses, "；")
		}
		if run.diagDropped > 0 {
			text += fmt.Sprintf("；另 %d 条带 Key 的没列出", run.diagDropped)
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return ""
	}
	return studioKeyText(strings.Join(parts, "；"), 480)
}

func orNone(value string) string {
	if value == "" {
		return "无"
	}
	return value
}
