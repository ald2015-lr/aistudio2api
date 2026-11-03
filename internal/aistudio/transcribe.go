package aistudio

import (
	"encoding/json"
	"fmt"
)

func validateTranscriptionConfig(config *TranscriptionConfig, model Model) error {
	if config == nil {
		return nil
	}
	checks := []struct {
		requested  bool
		capability string
		setting    string
	}{
		{config.WordTimestamps, "transcription_word_timestamps", "word timestamps"},
		{config.SpeakerLabels, "transcription_speaker_labels", "speaker labels"},
		{len(config.LanguageCodes) > 0, "transcription_language_codes", "language codes"},
		{len(config.CustomVocabulary) > 0, "transcription_custom_vocabulary", "custom vocabulary"},
		{config.SmartTranscription, "transcription_smart", "smart transcription"},
	}
	for _, check := range checks {
		if check.requested && !model.Capabilities[check.capability] {
			return fmt.Errorf("模型 %s 不支持 %s", model.ID, check.setting)
		}
	}
	return nil
}

func encodeTranscriptionConfig(config *TranscriptionConfig) ([]any, error) {
	if config == nil {
		return nil, nil
	}
	if config.SmartTranscription && (config.WordTimestamps || config.SpeakerLabels) {
		return nil, fmt.Errorf("smart transcription 不能同时启用 word timestamps 或 speaker labels")
	}
	length := 0
	if config.WordTimestamps || config.SpeakerLabels {
		length = 6
	}
	if len(config.CustomVocabulary) > 0 {
		length = 7
	}
	if len(config.LanguageCodes) > 0 {
		length = 8
	}
	if config.SmartTranscription {
		length = 9
	}
	if length == 0 {
		return nil, nil
	}
	wire := make([]any, length)
	if config.WordTimestamps {
		wire[4] = int64(1)
	}
	if config.SpeakerLabels {
		wire[5] = int64(1)
	}
	if len(config.CustomVocabulary) > 0 {
		wire[6] = append([]string(nil), config.CustomVocabulary...)
	}
	if len(config.LanguageCodes) > 0 {
		wire[7] = append([]string(nil), config.LanguageCodes...)
	}
	if config.SmartTranscription {
		wire[8] = int64(2)
	}
	return wire, nil
}

// decodeTranscriptMetadata 解码 Part field 23 的转写元数据 [text, speaker, timestamps]。text 按正文规则读取：null 为空串，数字与布尔值
// 按原文转成文本，对象与数组读不出正文，按协议证据错误返回；speaker 与时间戳是辅助元数据：speaker 不是字符串时忽略，timestamps 不是数组
// 时丢掉，某个时间戳读不出时丢掉该项（原先都按协议证据错误中止）。容错各写一行日志
func decodeTranscriptMetadata(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance) (TranscriptMetadata, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return TranscriptMetadata{}, withMethod(err, gcMethod)
	}
	metadata := TranscriptMetadata{}
	if textRaw := rawAt(values, 0); !isJSONNull(textRaw) {
		text, converted, ok := lenientText(textRaw)
		if !ok {
			return TranscriptMetadata{}, &ProtocolEvidenceError{
				Method: gcMethod, Path: path + "[0]", Detail: "转写正文不是字符串，读不出正文", Raw: cloneEvidence(textRaw, evidence),
			}
		}
		if converted {
			tolerance.note(gcMethod, path+"[0]", "转写正文不是字符串，按原文转成文本", textRaw)
		}
		metadata.Text = text
	}
	if speakerRaw := rawAt(values, 1); !isJSONNull(speakerRaw) {
		if json.Unmarshal(speakerRaw, &metadata.Speaker) != nil {
			metadata.Speaker = ""
			tolerance.note(gcMethod, path+"[1]", "转写的说话人不是字符串，已忽略", speakerRaw)
		}
	}
	timestampsRaw := rawAt(values, 2)
	if isJSONNull(timestampsRaw) {
		return metadata, nil
	}
	timestamps, err := rawArray(timestampsRaw, path+"[2]", evidence)
	if err != nil {
		tolerance.note(gcMethod, path+"[2]", "转写时间戳列表不是数组，已丢弃", timestampsRaw)
		return metadata, nil
	}
	metadata.Timestamps = make([]TranscriptTimestamp, 0, len(timestamps))
	for index, timestampRaw := range timestamps {
		itemPath := fmt.Sprintf("%s[2][%d]", path, index)
		timestamp, err := decodeTranscriptTimestamp(timestampRaw, itemPath, evidence)
		if err != nil {
			tolerance.note(gcMethod, itemPath, "转写时间戳读不出，已丢弃该项", timestampRaw)
			continue
		}
		metadata.Timestamps = append(metadata.Timestamps, timestamp)
	}
	return metadata, nil
}

func decodeTranscriptTimestamp(raw json.RawMessage, path string, evidence json.RawMessage) (TranscriptTimestamp, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return TranscriptTimestamp{}, withMethod(err, "GenerateContent")
	}
	timestamp := TranscriptTimestamp{}
	if startRaw := rawAt(values, 1); !isJSONNull(startRaw) {
		timestamp.Start, err = decodeTranscriptDuration(startRaw, path+"[1]", evidence)
		if err != nil {
			return TranscriptTimestamp{}, err
		}
	}
	if endRaw := rawAt(values, 2); !isJSONNull(endRaw) {
		timestamp.End, err = decodeTranscriptDuration(endRaw, path+"[2]", evidence)
		if err != nil {
			return TranscriptTimestamp{}, err
		}
	}
	return timestamp, nil
}

func decodeTranscriptDuration(raw json.RawMessage, path string, evidence json.RawMessage) (TranscriptDuration, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return TranscriptDuration{}, withMethod(err, "GenerateContent")
	}
	duration := TranscriptDuration{}
	if secondsRaw := rawAt(values, 0); !isJSONNull(secondsRaw) {
		duration.Seconds, err = rawInt64(secondsRaw, path+"[0]", evidence)
		if err != nil {
			return TranscriptDuration{}, withMethod(err, "GenerateContent")
		}
	}
	if nanosRaw := rawAt(values, 1); !isJSONNull(nanosRaw) {
		duration.Nanos, err = rawInt64(nanosRaw, path+"[1]", evidence)
		if err != nil {
			return TranscriptDuration{}, withMethod(err, "GenerateContent")
		}
	}
	return duration, nil
}
