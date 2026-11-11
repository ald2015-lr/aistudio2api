package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type openAIImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	ResponseFormat string `json:"response_format"`
	// OutputFormat 为 gpt-image 的输出格式（png、jpeg、webp）：本服务不转码，按模型输出的原样返回，客户端写了这个字段时响应的
	// output_format 写实际格式。output_compression、background、moderation、style、user 等其他 gpt-image 字段接受并忽略
	OutputFormat string `json:"output_format"`
}

type openAISpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
	Instructions   string  `json:"instructions"`
}

func (s *server) handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	var request openAIImageRequest
	if err := decodeJSON(r, &request); err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	if request.Model == "" || strings.TrimSpace(request.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model and prompt are required")
		return
	}
	if request.N == 0 {
		request.N = 1
	}
	if request.N != 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "AI Studio image models generate one image per request")
		return
	}
	imageConfig, notes, err := openAIImageConfig(request.Size, request.Quality, s.imageModelOptions(r.Context(), request.Model))
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	if format := strings.ToLower(strings.TrimSpace(request.OutputFormat)); format != "" && format != "png" {
		notes = append(notes, fmt.Sprintf("图片 output_format=%s：本服务不转码，按模型输出的格式原样返回（响应的 output_format 为实际格式）", format))
	}
	events, err := s.service.Generate(r.Context(), aistudio.GenerateRequest{
		ID:    newID("image"),
		Model: request.Model,
		Contents: []aistudio.Content{{
			Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: request.Prompt}},
		}},
		Config: aistudio.GenerationConfig{
			ResponseModalities: []aistudio.ResponseModality{aistudio.ResponseModalityImage},
			ImageConfig:        imageConfig,
		},
		Notes: notes,
	})
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	// 每张图同时带 b64_json 与 url（data URI，图片总是内联返回、不存成本服务链接）：gpt-image 客户端默认读 b64_json，
	// 旧的 DALL·E 写法读 url；response_format 明确要求其中一种时只给那一种（其他取值与原先一样只给 url）
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	data := make([]map[string]any, 0, len(result.media))
	actualFormat := ""
	for _, media := range result.media {
		if !strings.HasPrefix(media.MIME, "image/") || len(media.Data) == 0 {
			continue
		}
		encoded := base64.StdEncoding.EncodeToString(media.Data)
		item := map[string]any{}
		if format == "" || format == "b64_json" {
			item["b64_json"] = encoded
		}
		if format != "b64_json" {
			item["url"] = "data:" + media.MIME + ";base64," + encoded
		}
		if result.text.Len() > 0 {
			item["revised_prompt"] = result.text.String()
		}
		if actualFormat == "" {
			actualFormat = imageOutputFormat(media.MIME)
		}
		data = append(data, item)
	}
	if len(data) == 0 {
		message := "The model did not return an image"
		if reason := result.finishReason; reason != "" && reason != "stop" {
			message += ": finish reason " + reason
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", message)
		return
	}
	response := map[string]any{"created": time.Now().Unix(), "data": data}
	if strings.TrimSpace(request.OutputFormat) != "" && actualFormat != "" {
		response["output_format"] = actualFormat
	}
	if usage := openAIImageUsage(result.usage); usage != nil {
		response["usage"] = usage
	}
	writeJSON(w, http.StatusOK, response)
}

// imageOutputFormat 返回图片 MIME 对应的 gpt-image output_format 写法（png、jpeg、webp 等，即 image/ 之后的子类型）
func imageOutputFormat(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(mediaType, "image/")
}

// openAIImageUsage 按 gpt-image 的写法返回用量：输入只有文字提示词（input_tokens_details 的 image_tokens 为 0），输出为图片
// （含思考）的 token；上游没有给用量时返回 nil，响应不带 usage
func openAIImageUsage(usage *aistudio.Usage) map[string]any {
	if usage == nil {
		return nil
	}
	input, output := inputTokens(usage), outputTokens(usage)
	total := usage.TotalTokens
	if total <= 0 {
		total = input + output
	}
	return map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": total,
		"input_tokens_details": map[string]int64{"text_tokens": input, "image_tokens": 0},
	}
}

// legacyImageSizes 为原先就接受的三种 size 与宽高比：照原样发送（只写宽高比、不写分辨率），与改动之前逐字节相同
var legacyImageSizes = map[string]string{"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3"}

// geminiImageAspectRatios 为模型目录没有列出宽高比选项（或取不到目录）时可选的宽高比，即 Gemini 图片模型通用的 10 种
var geminiImageAspectRatios = []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}

// imageResolutionEdges 为图片分辨率档的标称边长（1:1 时的像素），按面积比较远近
var imageResolutionEdges = map[string]float64{"512": 512, "1K": 1024, "2K": 2048, "4K": 4096}

// imageModelOptions 返回请求模型在别名目录里的图片选项（image_aspect_ratios、image_output_resolutions）；目录取不到或没有这个模型时为 nil
func (s *server) imageModelOptions(ctx context.Context, name string) map[string][]string {
	models, err := s.aliasCatalog(ctx)
	if err != nil {
		return nil
	}
	if model, _, _, _, ok := resolveLongRequestModel(models, name); ok {
		return model.CapabilityOptions
	}
	return nil
}

// parseImageSize 解析 WIDTHxHEIGHT 写法的 size（不区分 x 的大小写），宽高都要在 1–16384 之间
func parseImageSize(size string) (int, int, bool) {
	widthText, heightText, ok := strings.Cut(strings.ToLower(size), "x")
	width, widthErr := strconv.Atoi(strings.TrimSpace(widthText))
	height, heightErr := strconv.Atoi(strings.TrimSpace(heightText))
	if !ok || widthErr != nil || heightErr != nil || width < 1 || height < 1 || width > 16384 || height > 16384 {
		return 0, 0, false
	}
	return width, height, true
}

// openAIImageConfig 把 OpenAI 图片请求的 size 与 quality 换成 AI Studio 的图片设置，返回要写进请求日志的说明。
// size：auto 与空不指定；1024x1024、1536x1024、1024x1536 照原样只写宽高比；其他 WIDTHxHEIGHT（gpt-image、DALL·E 的 1792x1024、
// 1024x1792、512x512 等）按宽高比取模型目录里最接近的一种（目录没有列出时取 Gemini 通用的 10 种，按比值的对数比较），分辨率按面积取
// 目录 image_output_resolutions 里最接近的一档（512、1K、2K、4K；1K 是模型默认，不写；目录没有列出分辨率时不写）。只换成最接近的
// 宽高比与分辨率档，不缩放、不裁切、不转码：返回的图片尺寸是模型这一档的实际尺寸，不一定等于请求的像素。宽高比不是精确对应、
// 或按 size 写了分辨率时记一行说明。quality 照原样映射分辨率（low/standard 1K、medium/hd 2K、high 4K），写了时优先于按 size 推断的分辨率
func openAIImageConfig(size string, quality string, options map[string][]string) (*aistudio.ImageConfig, []string, error) {
	config := &aistudio.ImageConfig{}
	sized := false
	var notes []string
	size = strings.ToLower(strings.TrimSpace(size))
	if ratio, ok := legacyImageSizes[size]; ok {
		config.AspectRatio = ratio
	} else if size != "" && size != "auto" {
		width, height, ok := parseImageSize(size)
		if !ok {
			return nil, nil, fmt.Errorf("size must be auto or WIDTHxHEIGHT, for example 1024x1024, 1536x1024 or 1024x1792")
		}
		ratios := options["image_aspect_ratios"]
		if len(ratios) == 0 {
			ratios = geminiImageAspectRatios
		}
		divisor := gcd(width, height)
		exact := fmt.Sprintf("%d:%d", width/divisor, height/divisor)
		target := math.Log(float64(width)) - math.Log(float64(height))
		config.AspectRatio = aistudio.NearestOption(target, ratios, aistudio.AspectRatioLog)
		if config.AspectRatio == "" {
			// 目录列出的宽高比都读不出时按通用的 10 种
			config.AspectRatio = aistudio.NearestOption(target, geminiImageAspectRatios, aistudio.AspectRatioLog)
		}
		if config.AspectRatio != exact {
			notes = append(notes, fmt.Sprintf("图片 size %s 按最接近的宽高比 %s 发送（不缩放、不裁切）", size, config.AspectRatio))
		}
		if resolutions := options["image_output_resolutions"]; len(resolutions) > 0 {
			area := math.Log(float64(width) * float64(height))
			nearest := aistudio.NearestOption(area, resolutions, func(option string) (float64, bool) {
				edge, ok := imageResolutionEdges[option]
				return math.Log(edge * edge), ok
			})
			if nearest != "" && nearest != "1K" {
				config.ImageSize, sized = nearest, true
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "", "auto":
		if sized {
			notes = append(notes, fmt.Sprintf("图片 size %s 按最接近的分辨率 %s 发送", size, config.ImageSize))
		}
	case "low", "standard":
		config.ImageSize = "1K"
	case "medium", "hd":
		config.ImageSize = "2K"
	case "high":
		config.ImageSize = "4K"
	default:
		return nil, nil, fmt.Errorf("quality must be auto, low, medium or high")
	}
	if config.AspectRatio == "" && config.ImageSize == "" {
		return nil, notes, nil
	}
	return config, notes, nil
}

// gcd 返回两个正整数的最大公约数
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (s *server) handleOpenAISpeech(w http.ResponseWriter, r *http.Request) {
	var request openAISpeechRequest
	if err := decodeJSON(r, &request); err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	if request.Model == "" || strings.TrimSpace(request.Input) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model and input are required")
		return
	}
	if request.Speed != 0 && request.Speed != 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "AI Studio TTS does not expose speech speed")
		return
	}
	voice := strings.TrimSpace(request.Voice)
	if voice == "" {
		voice = "Zephyr"
	}
	part := aistudio.Part{Text: strings.TrimSpace(request.Input)}
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		part.SpeechMetadata = &aistudio.SpeechMetadata{Style: instructions}
	}
	events, err := s.service.Generate(r.Context(), aistudio.GenerateRequest{
		ID:    newID("speech"),
		Model: request.Model,
		Contents: []aistudio.Content{{
			Role: aistudio.RoleUser, Parts: []aistudio.Part{part},
		}},
		Config: aistudio.GenerationConfig{
			ResponseModalities: []aistudio.ResponseModality{aistudio.ResponseModalityAudio},
			SpeechConfig:       &aistudio.SpeechConfig{VoiceName: voice},
		},
	})
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	media, err := joinedAudio(result.media)
	if err != nil {
		writeOpenAIRequestError(w, err)
		return
	}
	data, contentType, err := encodeSpeechResponse(media, request.ResponseFormat)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func joinedAudio(values []aistudio.Media) (aistudio.Media, error) {
	var joined aistudio.Media
	for _, media := range values {
		if !strings.HasPrefix(media.MIME, "audio/") || len(media.Data) == 0 {
			continue
		}
		if joined.MIME == "" {
			joined.MIME = media.MIME
		}
		if joined.MIME != media.MIME {
			return aistudio.Media{}, fmt.Errorf("AI Studio returned multiple audio formats")
		}
		joined.Data = append(joined.Data, media.Data...)
	}
	if len(joined.Data) == 0 {
		return aistudio.Media{}, fmt.Errorf("AI Studio did not return audio")
	}
	return joined, nil
}

// isPCMAudio 判断 MIME 是否为 16 位 PCM（audio/l16）：类型名不区分大小写，可以带 rate、channels 等参数
func isPCMAudio(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "audio/l16"
}

func encodeSpeechResponse(media aistudio.Media, format string) ([]byte, string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "wav"
	}
	baseType, parameters, err := mime.ParseMediaType(media.MIME)
	if err != nil {
		return nil, "", fmt.Errorf("AI Studio returned invalid audio MIME %q", media.MIME)
	}
	if format == "pcm" {
		return media.Data, media.MIME, nil
	}
	if format == "mp3" && baseType == "audio/mpeg" {
		return media.Data, "audio/mpeg", nil
	}
	if format != "wav" {
		return nil, "", fmt.Errorf("response_format must be wav or pcm for AI Studio TTS")
	}
	if baseType != "audio/l16" {
		return nil, "", fmt.Errorf("AI Studio returned %s, which cannot be wrapped as WAV", media.MIME)
	}
	sampleRate, err := strconv.Atoi(parameters["rate"])
	if err != nil || sampleRate <= 0 {
		return nil, "", fmt.Errorf("AI Studio audio MIME is missing a valid rate")
	}
	channels := 1
	if value := parameters["channels"]; value != "" {
		channels, err = strconv.Atoi(value)
		if err != nil || channels <= 0 {
			return nil, "", fmt.Errorf("AI Studio audio MIME has invalid channels")
		}
	}
	return pcmWAV(media.Data, sampleRate, channels), "audio/wav", nil
}

func pcmWAV(pcm []byte, sampleRate int, channels int) []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, 44+len(pcm)))
	buffer.WriteString("RIFF")
	_ = binary.Write(buffer, binary.LittleEndian, uint32(36+len(pcm)))
	buffer.WriteString("WAVEfmt ")
	_ = binary.Write(buffer, binary.LittleEndian, uint32(16))
	_ = binary.Write(buffer, binary.LittleEndian, uint16(1))
	_ = binary.Write(buffer, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buffer, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buffer, binary.LittleEndian, uint32(sampleRate*channels*2))
	_ = binary.Write(buffer, binary.LittleEndian, uint16(channels*2))
	_ = binary.Write(buffer, binary.LittleEndian, uint16(16))
	buffer.WriteString("data")
	_ = binary.Write(buffer, binary.LittleEndian, uint32(len(pcm)))
	buffer.Write(pcm)
	return buffer.Bytes()
}

// decodeBase64Flexible 根据字母表和填充形式解码 Base64 与 Data URL
func decodeBase64Flexible(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, ","); idx != -1 && strings.HasPrefix(s, "data:") {
		s = s[idx+1:]
	}
	encoding := base64.StdEncoding
	if index := strings.IndexAny(s, "+/-_"); index >= 0 && (s[index] == '-' || s[index] == '_') {
		encoding = base64.URLEncoding
	}
	if !strings.HasSuffix(s, "=") {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	return encoding.DecodeString(s)
}

// normalizeImagePayload 将 GIF 首帧按逻辑画布转换为 PNG 图片（见 aistudio.GIFFirstFramePNG：保留帧位置与透明背景，画布超过
// 4096×4096 时不解码）；不能转换时原样返回，由上游处理
func normalizeImagePayload(mimeType string, data []byte) (string, []byte) {
	blob := normalizedImageBlob(mimeType, data)
	return blob.MIME, blob.Data
}

// normalizedImageBlob 返回按 normalizeImagePayload 整理后的内联附件：GIF 转成 PNG 时在 SourceGIF 留下原 GIF（不序列化），
// Interaction 模型据此把这张 PNG 计入单次请求的 GIF 转换额度，超出时换回原 GIF 上传 Drive（见 aistudio.PrepareInteractionImages）；
// 其他模型只用 MIME 与 Data，与原先相同；存入续接状态时去掉 SourceGIF（见 dropSourceGIF），不多常驻一份原 GIF
func normalizedImageBlob(mimeType string, data []byte) *aistudio.Blob {
	lowerMIME := strings.ToLower(strings.TrimSpace(mimeType))
	if lowerMIME == "image/gif" || (len(data) >= 3 && string(data[:3]) == "GIF") {
		if converted, ok := aistudio.GIFFirstFramePNG(data); ok {
			return &aistudio.Blob{MIME: "image/png", Data: converted, SourceGIF: data}
		}
	}
	return &aistudio.Blob{MIME: mimeType, Data: data}
}
