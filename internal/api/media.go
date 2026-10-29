package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
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
	imageConfig, err := openAIImageConfig(request.Size, request.Quality)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
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
	data := make([]map[string]any, 0, len(result.media))
	for _, media := range result.media {
		if !strings.HasPrefix(media.MIME, "image/") || len(media.Data) == 0 {
			continue
		}
		encoded := base64.StdEncoding.EncodeToString(media.Data)
		item := map[string]any{}
		if request.ResponseFormat == "b64_json" {
			item["b64_json"] = encoded
		} else {
			item["url"] = "data:" + media.MIME + ";base64," + encoded
		}
		if result.text.Len() > 0 {
			item["revised_prompt"] = result.text.String()
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
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func openAIImageConfig(size string, quality string) (*aistudio.ImageConfig, error) {
	config := &aistudio.ImageConfig{}
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "", "auto":
	case "1024x1024":
		config.AspectRatio = "1:1"
	case "1536x1024":
		config.AspectRatio = "3:2"
	case "1024x1536":
		config.AspectRatio = "2:3"
	default:
		return nil, fmt.Errorf("size must be auto, 1024x1024, 1536x1024 or 1024x1536")
	}
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "", "auto":
	case "low", "standard":
		config.ImageSize = "1K"
	case "medium", "hd":
		config.ImageSize = "2K"
	case "high":
		config.ImageSize = "4K"
	default:
		return nil, fmt.Errorf("quality must be auto, low, medium or high")
	}
	if config.AspectRatio == "" && config.ImageSize == "" {
		return nil, nil
	}
	return config, nil
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
