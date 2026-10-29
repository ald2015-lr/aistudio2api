package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// mediaOutput 决定 Chat Completions、Messages 与 Gemini 原生接口的正文引用生成媒体的方式（MEDIA_OUTPUT）：
// 零值为 inline，与原先一样内联 data URI（Gemini 为 inlineData）；links 为真时把媒体按内容保存到媒体目录，正文改为本服务的链接。
// Responses、Interactions、Live 等其他协议不使用它，输出保持不变
type mediaOutput struct {
	links bool
	// base 为链接前缀（协议与主机，可带路径前缀，不带末尾的 /）；请求主机无法写进链接时为空，链接为 /media/ 开头的相对地址
	base  string
	store *mediastore.Store
	// ctx 为请求的 context，保存失败时把原因写进请求日志
	ctx context.Context
}

// mediaLink 为保存后的媒体链接
type mediaLink struct {
	url string
	// mime 为保存内容的类型：PCM 音频包装成 WAV 后为 audio/wav，其余与上游相同
	mime string
	// label 为链接文字：image、video、audio 或 file
	label string
}

// mediaOutputFor 按当前设置与请求地址返回本次请求的媒体输出方式；没有媒体目录或没有读取设置的函数时为 inline
func (s *server) mediaOutputFor(r *http.Request) mediaOutput {
	if s.mediaStore == nil || s.config.MediaLinks == nil {
		return mediaOutput{}
	}
	enabled, base := s.config.MediaLinks()
	if !enabled {
		return mediaOutput{}
	}
	if base == "" {
		base = requestOrigin(r)
	}
	return mediaOutput{links: true, base: strings.TrimRight(base, "/"), store: s.mediaStore, ctx: r.Context()}
}

// mediaLinksEnabled 判断当前是否为链接模式（每次读取，保存设置后立即生效）
func (s *server) mediaLinksEnabled() bool {
	if s.mediaStore == nil || s.config.MediaLinks == nil {
		return false
	}
	enabled, _ := s.config.MediaLinks()
	return enabled
}

// requestOrigin 返回客户端访问本服务使用的协议与主机：协议取 X-Forwarded-Proto（只认 http 与 https），主机取 X-Forwarded-Host 或 Host。
// 主机含有不能写进链接的字符时返回空串（链接改为相对地址）。经 new-api 等中转访问时这里多半是中转使用的内网地址，应设置 MEDIA_BASE_URL
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.ToLower(firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	host := r.Host
	if forwarded := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	if !linkHostPattern.MatchString(host) {
		return ""
	}
	return scheme + "://" + host
}

// linkHostPattern 为能直接写进链接的主机：域名或 IPv4（字母、数字、- _ .）、带方括号的 IPv6，可带端口
var linkHostPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*\.?|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$`)

// firstHeaderValue 返回逗号分隔的请求头里的第一个值（多级代理会追加）
func firstHeaderValue(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(first)
}

// link 在链接模式下保存带内容的媒体并返回链接。PCM 音频（audio/l16）先包装成 WAV：裸 PCM 没有文件头，浏览器与播放器都打不开。
// 不是链接模式、媒体只有地址没有内容或保存失败时 ok 为 false，调用方按 inline 输出；保存失败的原因写进请求日志
func (output mediaOutput) link(media aistudio.Media) (mediaLink, bool) {
	if !output.links || output.store == nil || len(media.Data) == 0 {
		return mediaLink{}, false
	}
	mimeType, data := media.MIME, media.Data
	if isPCMAudio(mimeType) {
		if wav, contentType, err := encodeSpeechResponse(media, "wav"); err == nil {
			mimeType, data = contentType, wav
		}
	}
	name, err := output.store.Save(data, mimeType)
	if err != nil {
		if output.ctx != nil {
			if metadata, ok := output.ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
				metadata.setErrorIfEmpty("媒体保存失败，本次改为内联输出: " + err.Error())
			}
		}
		return mediaLink{}, false
	}
	return mediaLink{url: output.base + "/media/" + name, mime: mimeType, label: mediaLinkLabel(mimeType)}, true
}

// mediaLinkLabel 返回链接文字：图片 image、视频 video、音频 audio，其他 file
func mediaLinkLabel(mimeType string) string {
	mimeType = strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "file"
	}
}

// markdown 返回链接的 Markdown：图片 ![image](url)，其他 [video](url)、[audio](url)、[file](url)
func (link mediaLink) markdown() string {
	if link.label == "image" {
		return "![image](" + link.url + ")"
	}
	return "[" + link.label + "](" + link.url + ")"
}

// markdown 把媒体写成 Markdown：链接模式下为本服务链接，否则（以及保存失败时）与原先一样内联 data URI
func (output mediaOutput) markdown(media aistudio.Media) string {
	if link, ok := output.link(media); ok {
		return link.markdown()
	}
	return renderMediaMarkdown(media)
}

// geminiParts 在链接模式下把媒体写成两个 Gemini part：先是 Markdown 链接文本，new-api 等把 Gemini 回复转成 OpenAI 格式时
// 只保留文本、思考与图片 inlineData，视频与音频的 inlineData 会被丢掉，文本链接能带给最终用户；再附一个 fileData（mimeType
// 与 fileUri），原生 SDK 从这里取得地址，这些中转把它当作空文本，不会报错或丢掉回复。签名放在 fileData 上，与内联时签名所在的
// inlineData 对应。afterText 为前面的正文没有以换行结尾，链接前补一个换行；链接后总带一个换行，与随后的正文分开。
// 不是链接模式或保存失败时返回 nil，调用方按 inlineData 输出
func (output mediaOutput) geminiParts(media aistudio.Media, signature string, afterText bool) []map[string]any {
	link, ok := output.link(media)
	if !ok {
		return nil
	}
	text := link.markdown() + "\n"
	if afterText {
		text = "\n" + text
	}
	return []map[string]any{
		{"text": text},
		geminiSignedPart(map[string]any{"fileData": map[string]any{"mimeType": link.mime, "fileUri": link.url}}, signature),
	}
}

// joinStreamAudio 在链接模式下合并流式输出中连续、不带签名的 PCM 音频分片（与非流式合并的规则相同）：每个分片各存一个文件
// 会得到几十个只有零点几秒的链接；合并后在下一个其他事件（至少有结束事件）到达时作为一个 WAV 文件输出一次链接。
// 不是链接模式时原样返回 emit
func (output mediaOutput) joinStreamAudio(emit func(aistudio.Event) error) func(aistudio.Event) error {
	if !output.links {
		return emit
	}
	var pending *aistudio.Event
	flush := func() error {
		if pending == nil {
			return nil
		}
		event := *pending
		pending = nil
		return emit(event)
	}
	return func(event aistudio.Event) error {
		if event.Kind == aistudio.EventMedia && event.Media != nil && isPCMAudio(event.Media.MIME) && len(event.Media.Data) > 0 &&
			event.Media.URL == "" && event.ThoughtSignature == "" {
			if pending != nil && pending.Media.MIME == event.Media.MIME {
				pending.Media.Data = append(pending.Media.Data, event.Media.Data...)
				return nil
			}
			if err := flush(); err != nil {
				return err
			}
			media := *event.Media
			media.Data = append([]byte(nil), media.Data...)
			copied := event
			copied.Media = &media
			pending = &copied
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		return emit(event)
	}
}

// handleMedia 提供链接模式下保存的生成媒体。不校验 API Key（链接要能直接在浏览器与聊天客户端里打开），只读（GET 与 HEAD），
// 只提供媒体目录里名字为 64 位十六进制加已知扩展名的普通文件，不跟随符号链接；Content-Type 按扩展名给出、不按内容猜测，
// 支持 Range（视频拖动进度），内容按哈希命名、不会改变，带一年的 immutable 缓存头。不是链接模式时一律 404
func (s *server) handleMedia(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/media/")
	if !ok || !mediastore.ValidName(name) || !s.mediaLinksEnabled() {
		http.NotFound(w, r)
		return
	}
	file, info, err := s.mediaStore.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	header.Set("Content-Type", mediastore.ContentType(name))
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	header.Set("ETag", `"`+mediastore.Hash(name)+`"`)
	header.Set("Access-Control-Allow-Origin", "*")
	if !strings.HasPrefix(mediastore.ContentType(name), "image/") && !strings.HasPrefix(mediastore.ContentType(name), "video/") &&
		!strings.HasPrefix(mediastore.ContentType(name), "audio/") {
		header.Set("Content-Disposition", "attachment")
	}
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// maxRestoredMediaBytes 为一次请求从媒体目录恢复的内容总量上限：恢复出的媒体要随请求编码或逐个上传 Drive，回传的历史只有几 KB，
// 展开后不能远远超出前置中转允许的请求体。同一文件一次请求只恢复一处，超过上限时较早消息里的引用不恢复
const maxRestoredMediaBytes int64 = 64 << 20

// mediaImageLinkPattern 匹配正文里指向本服务媒体目录的 Markdown 图片（绝对地址或 /media/ 开头的相对地址），子匹配为文件名
var mediaImageLinkPattern = regexp.MustCompile(`!\[[^\]\n]*\]\((?:https?://[^\s()<>]+)?/media/([0-9a-f]{64}\.(?:png|jpg|webp|gif))\)`)

// mediaVisualLinkPattern 匹配正文里指向本服务媒体目录的 Markdown 图片或视频链接（![image](…)、[video](…)），子匹配为文件名：
// 目标模型是能输出视频的 Omni 时 assistant 正文与用户正文里的这类链接恢复为媒体
var mediaVisualLinkPattern = regexp.MustCompile(`!?\[[^\]\n]*\]\((?:https?://[^\s()<>]+)?/media/([0-9a-f]{64}\.(?:png|jpg|webp|gif|mp4|webm|mov))\)`)

// mediaBareLinkPattern 匹配正文里指向本服务媒体目录的图片或视频裸地址（http/https 绝对地址或 /media/ 开头的相对地址），子匹配为文件名：
// 目标模型是能输出视频的 Omni 时用户正文里粘贴的这类地址恢复为媒体（前后的字符另行检查，见 bareMediaLinkAt）。主机与路径按段匹配
// （段内不含斜杠），路径段取最短：原先主机部分贪婪、可以跨过逗号、顿号与中文，两个链接之间不是空白（“A.mp4，https://…/B.mp4”）时
// 只匹配出一处，范围却覆盖两个链接，前一个链接连同它的文字一起被换成后一个视频
var mediaBareLinkPattern = regexp.MustCompile(`(?:https?://[^\s()<>\[\]"'/]+(?:/[^\s()<>\[\]"'/]+)*?)?/media/([0-9a-f]{64}\.(?:png|jpg|webp|gif|mp4|webm|mov))`)

// mediaReference 为回传历史里一处指向本服务媒体的引用：content 与 part 为所在位置，offset 为链接在文本 part 里的
// 起始位置，fileData 与外部地址的 part 为 -1
type mediaReference struct {
	content int
	part    int
	offset  int
}

// mediaTextLink 为文本 part 里一处可以恢复的链接：start、end 为链接（Markdown 链接或裸地址）在文本里的范围
type mediaTextLink struct {
	start int
	end   int
	name  string
}

// mediaRestorer 在一次请求内选定要恢复的引用并读取文件：同一文件只恢复一处，恢复的总量不超过 limit。
// omni 为目标模型是能输出视频的 Omni（aistudio.OmniVideoModel）：assistant 正文与 fileData 里的视频、用户正文里的图片与视频链接也恢复
type mediaRestorer struct {
	store *mediastore.Store
	limit int64
	used  int64
	omni  bool
	// restored 为已选定恢复的文件；skipped 为不存在、读取失败或超出剩余总量的文件（剩余总量只减不增，之后的引用不再重读）
	restored map[string]bool
	skipped  map[string]bool
	data     map[mediaReference][]byte
	// missing 为不在媒体目录（已清理、读取失败或不是本服务的链接）的文件，overLimit 为超出单次请求恢复总量的文件，按首次遇到的顺序，
	// 写进该请求的管理日志
	missing   []string
	overLimit []string
}

// take 读取 name 并选定在 reference 处恢复；文件已在别处恢复或已确定不能恢复时什么也不做
func (restorer *mediaRestorer) take(name string, reference mediaReference) {
	if restorer.restored[name] || restorer.skipped[name] {
		return
	}
	data, err := restorer.store.Read(name, restorer.limit-restorer.used)
	if err != nil || len(data) == 0 {
		restorer.skipped[name] = true
		if errors.Is(err, mediastore.ErrTooLarge) {
			restorer.overLimit = append(restorer.overLimit, name)
		} else {
			restorer.missing = append(restorer.missing, name)
		}
		return
	}
	restorer.used += int64(len(data))
	restorer.restored[name] = true
	restorer.data[reference] = data
}

// restoreMediaLinks 把回传历史里指向本服务媒体目录的内容恢复为媒体输入，与 inline 模式回传 data URI 时上游看到的内容一致：
//   - assistant 正文里的 Markdown 图片链接拆成文本与图片；视频、音频链接保持文本（模型只看到链接文字，不再把几 MB 的视频当输入）
//   - 用户消息里的 fileData、image_url 等外部地址指向已保存的媒体时恢复为内联媒体（含视频）：fileData 原本会被当作已上传文件的 ID
//     查找而返回 404，外部地址原本要由上游从公网下载
//   - assistant 消息里的 fileData（Gemini 原生 SDK 回传的历史同时带文本链接与 fileData）：图片恢复为内联图片；视频与音频不恢复，
//     同一条消息里已有它的文本链接时去掉 fileData（签名单独保留），没有时改为文本链接（模型不需要把自己生成的几 MB 视频再当输入；
//     Interaction 模型会把模型消息里的附件移到随后的用户 step 上传发送，恢复出来就会再发一遍）
//   - 目标模型是能输出视频的 Omni（aistudio.OmniVideoModel）时另有：assistant 正文里的视频链接（[video](…)）与 assistant 消息里的视频
//     fileData 同样恢复为视频（编辑或延长上一轮生成的视频要让模型看到原视频；new-api 等中转回传的历史只剩链接文本），用户正文里粘贴的
//     本服务图片、视频链接（Markdown 或裸地址）恢复为媒体附件；不能恢复的外部地址改为文本链接（Omni 不接受外部地址，保持原样会让
//     整个请求返回 400）
//   - 同一文件一次请求只恢复一处：从最新的消息往前选（同一条消息里 fileData 与外部地址先于文本链接），其余引用不恢复，文本链接保持文本，
//     fileData 与外部地址按上一条去掉或改为文本链接；恢复的总量不超过 maxRestoredMediaBytes，超出时较早的引用不恢复。
//     每处引用各展开一份的话，同一个视频在几 KB 的请求里引用上百次就能展开成几百 MB
//   - 文件已被清理或超出总量时：fileData 改为文本链接（不让整个请求失败），其余保持原样；按规则该恢复却没能恢复的文件（不在媒体目录、
//     超出总量）写一行说明进该请求的管理日志（request.Notes），不再静默
//
// 不区分是否为链接模式：切回 inline 后，历史里已有的链接在文件保留期间照常恢复
func (s *server) restoreMediaLinks(ctx context.Context, request *aistudio.GenerateRequest) {
	if s.mediaStore == nil {
		return
	}
	notes := restoreMediaLinksWithin(s.mediaStore, request.Contents, maxRestoredMediaBytes, s.omniVideoTarget(ctx, request.Model))
	request.Notes = append(request.Notes, notes...)
}

// omniVideoTarget 判断请求的模型名（可带后缀与目录别名）解析为能输出视频的 Omni 模型；目录取不到时为 false（按其他模型恢复）
func (s *server) omniVideoTarget(ctx context.Context, name string) bool {
	models, err := s.aliasCatalog(ctx)
	if err != nil {
		return false
	}
	model, _, ok := resolveRequestModel(models, name)
	return ok && aistudio.OmniVideoModel(model)
}

// restoreMediaLinksWithin 按 restoreMediaLinks 的规则恢复 store 里的媒体，恢复的总量不超过 limit；omni 为目标模型是能输出视频的 Omni。
// 返回要写进该请求管理日志的说明（没有恢复失败时为空）
func restoreMediaLinksWithin(store *mediastore.Store, contents []aistudio.Content, limit int64, omni bool) []string {
	restorer := &mediaRestorer{
		store: store, limit: limit, omni: omni,
		restored: make(map[string]bool), skipped: make(map[string]bool), data: make(map[mediaReference][]byte),
	}
	for index := len(contents) - 1; index >= 0; index-- {
		restorer.choose(index, contents[index])
	}
	for index := range contents {
		contents[index].Parts = restorer.rewrite(index, contents[index])
	}
	// 最后一条用户消息（这一轮）里恢复的媒体是客户端这一轮明确给出的附件（粘贴的链接、指向本服务的 image_url 与 fileData），不算回传历史：
	// 去掉 RestoredLink，与客户端直接带的附件一样处理（Omni 没有可用的 Drive 授权账号时按需要 Drive 授权返回，不换回链接文本）
	if last := len(contents) - 1; last >= 0 && contents[last].Role == aistudio.RoleUser {
		for partIndex := range contents[last].Parts {
			contents[last].Parts[partIndex].RestoredLink, contents[last].Parts[partIndex].RestoredText = false, ""
		}
	}
	return restorer.notes(limit)
}

// restoreMediaNoteNames 为说明里每类最多列出的文件名个数
const restoreMediaNoteNames = 3

// notes 返回恢复失败的说明：一次请求一行，按原因分别给出文件数与最多 restoreMediaNoteNames 个文件名（哈希只取前 12 位）
func (restorer *mediaRestorer) notes(limit int64) []string {
	if len(restorer.missing) == 0 && len(restorer.overLimit) == 0 {
		return nil
	}
	list := func(names []string) string {
		shown := make([]string, 0, min(len(names), restoreMediaNoteNames))
		for _, name := range names[:min(len(names), restoreMediaNoteNames)] {
			shown = append(shown, mediastore.Hash(name)[:12]+"…"+path.Ext(name))
		}
		if len(names) > restoreMediaNoteNames {
			shown = append(shown, "…")
		}
		return strings.Join(shown, "、")
	}
	parts := []string{"回传历史里本服务的媒体链接没能恢复为媒体输入，按链接文本发送"}
	if len(restorer.missing) > 0 {
		parts = append(parts, fmt.Sprintf("不在媒体目录（已清理或不是本服务的链接）=%d（%s）", len(restorer.missing), list(restorer.missing)))
	}
	if len(restorer.overLimit) > 0 {
		parts = append(parts, fmt.Sprintf("超出单次请求恢复上限 %dMB=%d（%s，较新的消息优先）", limit>>20, len(restorer.overLimit), list(restorer.overLimit)))
	}
	return []string{strings.Join(parts, " | ")}
}

// restorableMedia 判断一处 fileData 或外部地址按规则恢复为媒体：用户消息里的任何媒体；assistant 消息里的图片，目标为 Omni 时还有视频
func (restorer *mediaRestorer) restorableMedia(assistant bool, name string) bool {
	return !assistant || mediastore.IsImage(name) || restorer.omni && mediaIsVideo(name)
}

// mediaIsVideo 判断保存文件是否为视频
func mediaIsVideo(name string) bool {
	return strings.HasPrefix(mediastore.ContentType(name), "video/")
}

// choose 选定一条消息里要恢复的引用：先是 fileData 与外部地址，再是正文里的链接（assistant 正文里的图片链接，目标为 Omni 时还有
// 视频链接与用户正文里的图片、视频链接）
func (restorer *mediaRestorer) choose(index int, content aistudio.Content) {
	assistant := content.Role == aistudio.RoleAssistant
	for partIndex, part := range content.Parts {
		if name, ok := mediaPartName(part); ok && restorer.restorableMedia(assistant, name) {
			restorer.take(name, mediaReference{content: index, part: partIndex, offset: -1})
		}
	}
	for partIndex, part := range content.Parts {
		for _, link := range restorer.textLinks(content.Role, part) {
			restorer.take(link.name, mediaReference{content: index, part: partIndex, offset: link.start})
		}
	}
}

// textLinks 返回文本 part 里按规则可以恢复的链接（按出现顺序，互不重叠）：assistant 正文里的 Markdown 图片链接，目标为 Omni 时
// 还有视频链接；目标为 Omni 时用户正文里的 Markdown 图片、视频链接与裸地址。思考、带说话人元数据的文本与其他 part 没有
func (restorer *mediaRestorer) textLinks(role aistudio.Role, part aistudio.Part) []mediaTextLink {
	if !restorableText(part) {
		return nil
	}
	var links []mediaTextLink
	switch {
	case role == aistudio.RoleAssistant:
		pattern := mediaImageLinkPattern
		if restorer.omni {
			pattern = mediaVisualLinkPattern
		}
		for _, match := range pattern.FindAllStringSubmatchIndex(part.Text, -1) {
			links = append(links, mediaTextLink{start: match[0], end: match[1], name: part.Text[match[2]:match[3]]})
		}
	case role == aistudio.RoleUser && restorer.omni && part.InlineData == nil && part.File == nil && part.ExternalMedia == nil &&
		part.FunctionCall == nil && part.FunctionResult == nil:
		links = userMediaLinks(part.Text)
	}
	return links
}

// userMediaLinks 返回用户正文里指向本服务媒体目录的图片与视频：Markdown 链接，以及不在 Markdown 链接里的裸地址
func userMediaLinks(text string) []mediaTextLink {
	var links []mediaTextLink
	for _, match := range mediaVisualLinkPattern.FindAllStringSubmatchIndex(text, -1) {
		links = append(links, mediaTextLink{start: match[0], end: match[1], name: text[match[2]:match[3]]})
	}
	var bare []mediaTextLink
	for _, match := range mediaBareLinkPattern.FindAllStringSubmatchIndex(text, -1) {
		inside := false
		for _, link := range links {
			if match[0] < link.end && match[1] > link.start {
				inside = true
				break
			}
		}
		if !inside && bareMediaLinkAt(text, match[0], match[1]) {
			bare = append(bare, mediaTextLink{start: match[0], end: match[1], name: text[match[2]:match[3]]})
		}
	}
	if len(bare) == 0 {
		return links
	}
	links = append(links, bare...)
	slices.SortFunc(links, func(left, right mediaTextLink) int { return left.start - right.start })
	return links
}

// bareMediaLinkAt 判断 text[start:end] 是一个完整的地址：前面是开头、空白、中文等非 ASCII 字符或括号引号等分隔符（相对地址不能
// 接在其他地址或单词后面），后面不是还能接在地址里的字符（带查询串、片段或更长路径的不是本服务的媒体链接）
func bareMediaLinkAt(text string, start int, end int) bool {
	if start > 0 {
		if previous := text[start-1]; previous < 0x80 && !strings.ContainsRune(" \t\r\n(<[\"'`:,;*>", rune(previous)) {
			return false
		}
	}
	if end < len(text) {
		next := text[end]
		if next == '?' || next == '#' || next == '/' || next == '&' || next == '=' || next == '%' || next == '_' || next == '-' ||
			next >= '0' && next <= '9' || next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z' {
			return false
		}
	}
	return true
}

// rewrite 按选定的引用改写一条消息的 part
func (restorer *mediaRestorer) rewrite(index int, content aistudio.Content) []aistudio.Part {
	assistant := content.Role == aistudio.RoleAssistant
	linked := mediaLinkedNames(content.Parts)
	parts := make([]aistudio.Part, 0, len(content.Parts))
	for partIndex, part := range content.Parts {
		name, ok := mediaPartName(part)
		if !ok {
			parts = append(parts, restorer.textParts(index, partIndex, content.Role, part)...)
			continue
		}
		if data, restored := restorer.data[mediaReference{content: index, part: partIndex, offset: -1}]; restored {
			// 恢复前的链接文本：Interaction 模型没有可用的 Drive 授权账号时换回它（见 aistudio.Part.RestoredLink），与下面不恢复时相同
			restoredText := ""
			if !linked[name] {
				restoredText = mediaLinkText(mediaPartURL(part), name)
			}
			parts = append(parts, aistudio.Part{
				InlineData:       &aistudio.Blob{MIME: mediastore.ContentType(name), Data: data},
				ThoughtSignature: part.ThoughtSignature,
				RestoredLink:     true, RestoredText: restoredText,
			})
			continue
		}
		// 外部地址可恢复、但文件不在（也可能是别的站点同样路径的地址）或超出总量：保持原样，由上游按地址读取。
		// 目标为 Omni 时不保持（Omni 不接受外部地址），按下面改为文本链接
		if part.ExternalMedia != nil && !restorer.restored[name] && restorer.restorableMedia(assistant, name) && !restorer.omni {
			parts = append(parts, part)
			continue
		}
		// 不恢复为媒体的 fileData 与外部地址（同一文件已在别处恢复、文件已清理或超出总量、assistant 消息里的视频与音频）：
		// 同一条消息里已有文本链接时只留签名，否则改为文本链接
		switch {
		case linked[name] && part.ThoughtSignature != "":
			parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
		case linked[name]:
		default:
			parts = append(parts, aistudio.Part{Text: mediaLinkText(mediaPartURL(part), name), ThoughtSignature: part.ThoughtSignature})
		}
	}
	return parts
}

// mediaAnyLinkPattern 匹配正文里指向本服务媒体目录的任意 Markdown 链接（图片、视频、音频），子匹配为文件名
var mediaAnyLinkPattern = regexp.MustCompile(`\]\((?:https?://[^\s()<>]+)?/media/([0-9a-f]{64}\.[a-z0-9]+)\)`)

// mediaLinkedNames 返回文本 part 里以 Markdown 链接引用的本服务媒体文件名
func mediaLinkedNames(parts []aistudio.Part) map[string]bool {
	names := make(map[string]bool)
	for _, part := range parts {
		if part.Text == "" || part.Thought {
			continue
		}
		for _, match := range mediaAnyLinkPattern.FindAllStringSubmatch(part.Text, -1) {
			names[match[1]] = true
		}
	}
	return names
}

// mediaPartName 返回 fileData 或外部媒体指向的本服务媒体文件名
func mediaPartName(part aistudio.Part) (string, bool) {
	switch {
	case part.File != nil:
		return mediaNameFromURL(part.File.ID)
	case part.ExternalMedia != nil:
		return mediaNameFromURL(part.ExternalMedia.URL)
	}
	return "", false
}

// mediaPartURL 返回 fileData 或外部媒体的地址
func mediaPartURL(part aistudio.Part) string {
	if part.File != nil {
		return part.File.ID
	}
	if part.ExternalMedia != nil {
		return part.ExternalMedia.URL
	}
	return ""
}

// restorableText 判断 part 是否为可以恢复其中链接的正文：思考与带说话人元数据的文本不动
func restorableText(part aistudio.Part) bool {
	return part.Text != "" && !part.Thought && part.SpeechMetadata == nil
}

// textParts 把文本 part 在选定恢复的链接处拆成文本与媒体（链接见 textLinks）；其余链接、思考、带说话人元数据的文本与其他 part
// 原样返回。签名留在拆出的第一个 part 上
func (restorer *mediaRestorer) textParts(index int, partIndex int, role aistudio.Role, part aistudio.Part) []aistudio.Part {
	links := restorer.textLinks(role, part)
	if len(links) == 0 {
		return []aistudio.Part{part}
	}
	text := part.Text
	parts := make([]aistudio.Part, 0)
	position := 0
	for _, link := range links {
		data, ok := restorer.data[mediaReference{content: index, part: partIndex, offset: link.start}]
		if !ok {
			continue
		}
		if link.start > position {
			parts = append(parts, aistudio.Part{Text: text[position:link.start]})
		}
		parts = append(parts, aistudio.Part{
			InlineData:   &aistudio.Blob{MIME: mediastore.ContentType(link.name), Data: data},
			RestoredLink: true, RestoredText: text[link.start:link.end],
		})
		position = link.end
	}
	if position == 0 {
		return []aistudio.Part{part}
	}
	if position < len(text) {
		parts = append(parts, aistudio.Part{Text: text[position:]})
	}
	parts[0].ThoughtSignature = part.ThoughtSignature
	return parts
}

// mediaNameFromURL 从指向本服务媒体目录的地址（http/https 绝对地址或 /media/ 开头的相对地址）取出文件名
func mediaNameFromURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Scheme == "" && parsed.Host != "" {
		return "", false
	}
	directory, name := path.Split(parsed.Path)
	if !strings.HasSuffix(directory, "/media/") || !mediastore.ValidName(name) {
		return "", false
	}
	return name, true
}

// mediaLinkText 返回文件已不在时代替 fileData 的文本链接
func mediaLinkText(rawURL string, name string) string {
	link := mediaLink{url: rawURL, label: mediaLinkLabel(mediastore.ContentType(name))}
	return link.markdown()
}
