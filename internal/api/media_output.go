package api

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"regexp"
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

// mediaReference 为回传历史里一处指向本服务媒体的引用：content 与 part 为所在位置，offset 为 Markdown 图片链接在文本 part 里的
// 起始位置，fileData 与外部地址的 part 为 -1
type mediaReference struct {
	content int
	part    int
	offset  int
}

// mediaRestorer 在一次请求内选定要恢复的引用并读取文件：同一文件只恢复一处，恢复的总量不超过 limit
type mediaRestorer struct {
	store *mediastore.Store
	limit int64
	used  int64
	// restored 为已选定恢复的文件；skipped 为不存在、读取失败或超出剩余总量的文件（剩余总量只减不增，之后的引用不再重读）
	restored map[string]bool
	skipped  map[string]bool
	data     map[mediaReference][]byte
}

// take 读取 name 并选定在 reference 处恢复；文件已在别处恢复或已确定不能恢复时什么也不做
func (restorer *mediaRestorer) take(name string, reference mediaReference) {
	if restorer.restored[name] || restorer.skipped[name] {
		return
	}
	data, err := restorer.store.Read(name, restorer.limit-restorer.used)
	if err != nil || len(data) == 0 {
		restorer.skipped[name] = true
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
//   - 同一文件一次请求只恢复一处：从最新的消息往前选（同一条消息里 fileData 与外部地址先于文本链接），其余引用不恢复，文本链接保持文本，
//     fileData 与外部地址按上一条去掉或改为文本链接；恢复的总量不超过 maxRestoredMediaBytes，超出时较早的引用不恢复。
//     每处引用各展开一份的话，同一个视频在几 KB 的请求里引用上百次就能展开成几百 MB
//   - 文件已被清理或超出总量时：fileData 改为文本链接（不让整个请求失败），其余保持原样
//
// 不区分是否为链接模式：切回 inline 后，历史里已有的链接在文件保留期间照常恢复
func (s *server) restoreMediaLinks(contents []aistudio.Content) {
	if s.mediaStore == nil {
		return
	}
	restoreMediaLinksWithin(s.mediaStore, contents, maxRestoredMediaBytes)
}

// restoreMediaLinksWithin 按 restoreMediaLinks 的规则恢复 store 里的媒体，恢复的总量不超过 limit
func restoreMediaLinksWithin(store *mediastore.Store, contents []aistudio.Content, limit int64) {
	restorer := &mediaRestorer{
		store: store, limit: limit,
		restored: make(map[string]bool), skipped: make(map[string]bool), data: make(map[mediaReference][]byte),
	}
	for index := len(contents) - 1; index >= 0; index-- {
		restorer.choose(index, contents[index])
	}
	for index := range contents {
		contents[index].Parts = restorer.rewrite(index, contents[index])
	}
}

// choose 选定一条消息里要恢复的引用：先是 fileData 与外部地址（assistant 消息只恢复图片），再是 assistant 正文里的图片链接
func (restorer *mediaRestorer) choose(index int, content aistudio.Content) {
	assistant := content.Role == aistudio.RoleAssistant
	for partIndex, part := range content.Parts {
		if name, ok := mediaPartName(part); ok && (!assistant || mediastore.IsImage(name)) {
			restorer.take(name, mediaReference{content: index, part: partIndex, offset: -1})
		}
	}
	if !assistant {
		return
	}
	for partIndex, part := range content.Parts {
		if !restorableText(part) {
			continue
		}
		for _, match := range mediaImageLinkPattern.FindAllStringSubmatchIndex(part.Text, -1) {
			restorer.take(part.Text[match[2]:match[3]], mediaReference{content: index, part: partIndex, offset: match[0]})
		}
	}
}

// rewrite 按选定的引用改写一条消息的 part
func (restorer *mediaRestorer) rewrite(index int, content aistudio.Content) []aistudio.Part {
	assistant := content.Role == aistudio.RoleAssistant
	linked := mediaLinkedNames(content.Parts)
	parts := make([]aistudio.Part, 0, len(content.Parts))
	for partIndex, part := range content.Parts {
		name, ok := mediaPartName(part)
		if !ok {
			if assistant {
				parts = append(parts, restorer.textParts(index, partIndex, part)...)
			} else {
				parts = append(parts, part)
			}
			continue
		}
		if data, restored := restorer.data[mediaReference{content: index, part: partIndex, offset: -1}]; restored {
			parts = append(parts, aistudio.Part{
				InlineData:       &aistudio.Blob{MIME: mediastore.ContentType(name), Data: data},
				ThoughtSignature: part.ThoughtSignature,
			})
			continue
		}
		// 外部地址可恢复、但文件不在（也可能是别的站点同样路径的地址）或超出总量：保持原样，由上游按地址读取
		if part.ExternalMedia != nil && !restorer.restored[name] && (!assistant || mediastore.IsImage(name)) {
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

// restorableText 判断 part 是否为可以恢复其中图片链接的正文：思考与带说话人元数据的文本不动
func restorableText(part aistudio.Part) bool {
	return part.Text != "" && !part.Thought && part.SpeechMetadata == nil
}

// textParts 把 assistant 文本 part 在选定恢复的图片链接处拆成文本与图片；其余链接、思考、带说话人元数据的文本与其他 part 原样返回。
// 签名留在拆出的第一个 part 上
func (restorer *mediaRestorer) textParts(index int, partIndex int, part aistudio.Part) []aistudio.Part {
	if !restorableText(part) {
		return []aistudio.Part{part}
	}
	text := part.Text
	parts := make([]aistudio.Part, 0)
	position := 0
	for _, match := range mediaImageLinkPattern.FindAllStringSubmatchIndex(text, -1) {
		data, ok := restorer.data[mediaReference{content: index, part: partIndex, offset: match[0]}]
		if !ok {
			continue
		}
		if match[0] > position {
			parts = append(parts, aistudio.Part{Text: text[position:match[0]]})
		}
		name := text[match[2]:match[3]]
		parts = append(parts, aistudio.Part{InlineData: &aistudio.Blob{MIME: mediastore.ContentType(name), Data: data}})
		position = match[1]
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
