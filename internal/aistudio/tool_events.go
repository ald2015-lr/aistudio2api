package aistudio

import (
	"encoding/json"
	"fmt"
	"strings"
)

// executableCodeLanguages 为 ExecutableCode.language 的枚举表
var executableCodeLanguages = map[int64]string{0: "LANGUAGE_UNSPECIFIED", 1: "PYTHON"}

// codeExecutionOutcomes 为 CodeExecutionResult.outcome 的枚举表
var codeExecutionOutcomes = map[int64]string{
	0: "OUTCOME_UNSPECIFIED",
	1: "OUTCOME_OK",
	2: "OUTCOME_FAILED",
	3: "OUTCOME_DEADLINE_EXCEEDED",
}

// decodeToolEnum 读取代码执行的枚举：整数按表换算（known 为真）；不在表里的保留为 <prefix>_<n>；null 与缺位取表里 0 的缺省值；
// 写成名称时按名称（大写），名称在表里时 known 为真；其他写法取缺省值。除表内整数外都写一行容错日志
func decodeToolEnum(raw json.RawMessage, table map[int64]string, prefix string, label string, path string, tolerance *decodeTolerance) (string, bool) {
	fallback := table[0]
	if isJSONNull(raw) {
		tolerance.note(gcMethod, path, label+"缺位或为 null，取缺省值 "+fallback, raw)
		return fallback, false
	}
	code, err := rawInt64(raw, path, raw)
	if err != nil {
		var name string
		if converted, ok := lenientInt64(raw); ok {
			code = converted
		} else if json.Unmarshal(raw, &name) == nil && strings.TrimSpace(name) != "" {
			name = strings.ToUpper(strings.TrimSpace(name))
			tolerance.note(gcMethod, path, label+"写成名称，按名称处理", raw)
			for _, known := range table {
				if known == name {
					return name, true
				}
			}
			return name, false
		} else {
			tolerance.note(gcMethod, path, label+"无法识别，取缺省值 "+fallback, raw)
			return fallback, false
		}
	}
	if name, ok := table[code]; ok {
		if err != nil {
			tolerance.note(gcMethod, path, label+"不是整数，按编号换算", raw)
		}
		return name, true
	}
	name := fmt.Sprintf("%s_%d", prefix, code)
	tolerance.note(gcMethod, path, label+"不在枚举表里，保留为 "+prefix+"_<n>", raw)
	return name, false
}

// decodeToolText 读取代码执行里的文本：缺位与 null 为空串，数字与布尔值按原文转成文本（缺位与转换各写一行容错日志）；对象与数组读不出
// 文本，与正文一样按协议证据错误返回（代码与执行输出都是交给客户端的内容，不清空后照常结束）
func decodeToolText(raw json.RawMessage, label string, path string, tolerance *decodeTolerance) (string, error) {
	if raw == nil {
		tolerance.note(gcMethod, path, label+"缺位，按空串", raw)
		return "", nil
	}
	text, converted, ok := lenientText(raw)
	switch {
	case !ok:
		return "", &ProtocolEvidenceError{Method: gcMethod, Path: path, Detail: label + "是对象或数组，读不出内容", Raw: cloneRaw(raw)}
	case converted:
		tolerance.note(gcMethod, path, label+"不是字符串，按原文转成文本", raw)
	}
	return text, nil
}

// decodeExecutableCode 解码 Part field 8 的 executable code [language, code]：language 为 null、缺位或不在表里时按 decodeToolEnum 取值，
// code 缺位或为 null 时按空串（都写容错日志，不中止）。executable code 不是数组、code 是对象或数组时读不出代码，按协议证据错误返回
func decodeExecutableCode(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance) (ExecutableCode, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return ExecutableCode{}, withMethod(err, gcMethod)
	}
	language, _ := decodeToolEnum(rawAt(values, 0), executableCodeLanguages, "LANGUAGE", "executable code 的 language ", path+"[0]", tolerance)
	code, err := decodeToolText(rawAt(values, 1), "executable code 的代码", path+"[1]", tolerance)
	if err != nil {
		return ExecutableCode{}, err
	}
	return ExecutableCode{Language: language, Code: code}, nil
}

// decodeCodeExecutionResult 解码 Part field 9 的 code execution result [outcome, output]：outcome 为表内整数时与原先相同（OUTCOME_OK 放进
// Output，其余放进 Error）；为 null、缺位（含 []）或不在表里时结果不确定，文本同时放进 Output 与 Error（各协议按 outcome 取其中之一，
// 文本都不丢）。code execution result 不是数组、输出是对象或数组时读不出输出，按协议证据错误返回
func decodeCodeExecutionResult(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance) (CodeExecutionResult, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return CodeExecutionResult{}, withMethod(err, gcMethod)
	}
	outcome, known := decodeToolEnum(rawAt(values, 0), codeExecutionOutcomes, "OUTCOME", "code execution result 的 outcome ", path+"[0]", tolerance)
	value := ""
	if valueRaw := rawAt(values, 1); !isJSONNull(valueRaw) {
		if value, err = decodeToolText(valueRaw, "code execution result 的输出", path+"[1]", tolerance); err != nil {
			return CodeExecutionResult{}, err
		}
	}
	result := CodeExecutionResult{Outcome: outcome}
	switch {
	case known && outcome == "OUTCOME_OK":
		result.Output = value
	case known:
		result.Error = value
	default:
		result.Output, result.Error = value, value
	}
	return result, nil
}

// decodeGroundingMetadata 解码候选 field 8 的 grounding metadata。grounding 是辅助元数据：不是数组时整段丢弃（ok 为假，只写容错日志、
// 不发这个事件）；其中某一项（搜索入口、chunk 列表、support、检索分数、查询词、Maps token）读不出时只丢掉这一项，chunk 与 support 的
// 规则见 decodeGroundingChunk 与 decodeGroundingSupports
func decodeGroundingMetadata(raw json.RawMessage, path string, evidence json.RawMessage, tolerance *decodeTolerance) (GroundingMetadata, bool) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding 不是数组，已丢弃这次 grounding", raw)
		return GroundingMetadata{}, false
	}
	metadata := GroundingMetadata{}
	if entryRaw := rawAt(values, 0); !isJSONNull(entryRaw) {
		if entry, ok := decodeSearchEntryPoint(entryRaw, path+"[0]", tolerance); ok {
			metadata.SearchEntryPoint = &entry
		}
	}
	chunksOK := true
	if chunksRaw := rawAt(values, 1); !isJSONNull(chunksRaw) {
		metadata.Chunks, chunksOK = decodeGroundingChunks(chunksRaw, path+"[1]", tolerance)
	}
	if supportsRaw := rawAt(values, 2); !isJSONNull(supportsRaw) {
		if chunksOK {
			metadata.Supports = decodeGroundingSupports(supportsRaw, path+"[2]", tolerance)
		} else {
			tolerance.note(gcMethod, path+"[2]", "grounding chunk 列表已丢弃，support 无从对应，一并丢弃", supportsRaw)
		}
	}
	if retrievalRaw := rawAt(values, 3); !isJSONNull(retrievalRaw) {
		if retrieval, err := rawArray(retrievalRaw, path+"[3]", evidence); err != nil {
			tolerance.note(gcMethod, path+"[3]", "grounding 检索元数据不是数组，已忽略", retrievalRaw)
		} else if scoreRaw := rawAt(retrieval, 1); !isJSONNull(scoreRaw) {
			if score, ok := lenientFloat64(scoreRaw); ok {
				metadata.DynamicRetrievalScore = &score
			} else {
				tolerance.note(gcMethod, path+"[3][1]", "grounding 动态检索分数不是数字，已忽略", scoreRaw)
			}
		}
	}
	var seenQueries map[string]struct{}
	for _, fieldIndex := range []int{4, 5} {
		queriesRaw := rawAt(values, fieldIndex)
		if isJSONNull(queriesRaw) {
			continue
		}
		queryPath := fmt.Sprintf("%s[%d]", path, fieldIndex)
		queries, err := rawArray(queriesRaw, queryPath, evidence)
		if err != nil {
			tolerance.note(gcMethod, queryPath, "grounding 查询词列表不是数组，已忽略", queriesRaw)
			continue
		}
		if metadata.WebSearchQueries == nil {
			metadata.WebSearchQueries = make([]string, 0, len(queries))
			seenQueries = make(map[string]struct{}, len(queries))
		}
		for index, queryRaw := range queries {
			var query string
			if json.Unmarshal(queryRaw, &query) != nil {
				tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", queryPath, index), "grounding 查询词不是字符串，已忽略", queryRaw)
				continue
			}
			if _, exists := seenQueries[query]; exists {
				continue
			}
			seenQueries[query] = struct{}{}
			metadata.WebSearchQueries = append(metadata.WebSearchQueries, query)
		}
	}
	if tokenRaw := rawAt(values, 6); !isJSONNull(tokenRaw) {
		if json.Unmarshal(tokenRaw, &metadata.MapsWidgetContextToken) != nil {
			metadata.MapsWidgetContextToken = ""
			tolerance.note(gcMethod, path+"[6]", "grounding Maps token 不是字符串，已忽略", tokenRaw)
		}
	}
	return metadata, true
}

// lenientFloat64 读取一个数：数字与数字字符串都接受，null 与其他类型返回假
func lenientFloat64(raw json.RawMessage) (float64, bool) {
	if value, err := rawFloat64(raw, "", raw); err == nil && jsonKind(raw) != "null" {
		return value, true
	}
	return 0, false
}

// decodeSearchEntryPoint 解码搜索入口 [renderedContent?, sdkBlob?]：不是数组时丢掉（ok 为假），字段不是字符串时忽略该字段
func decodeSearchEntryPoint(raw json.RawMessage, path string, tolerance *decodeTolerance) (SearchEntryPoint, bool) {
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding 搜索入口不是数组，已忽略", raw)
		return SearchEntryPoint{}, false
	}
	entry := SearchEntryPoint{}
	for index, target := range []*string{&entry.RenderedContent, &entry.SDKBlob} {
		if value := rawAt(values, index); !isJSONNull(value) && json.Unmarshal(value, target) != nil {
			*target = ""
			tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", path, index), "grounding 搜索入口的字段不是字符串，已忽略", value)
		}
	}
	return entry, true
}

// decodeGroundingChunks 解码 grounding chunk 列表：每一项都留一个位置（读不出的换成占位项），support 的 chunk 索引照常对齐；
// 列表本身不是数组时丢掉（ok 为假）
func decodeGroundingChunks(raw json.RawMessage, path string, tolerance *decodeTolerance) ([]GroundingChunk, bool) {
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding chunk 列表不是数组，已丢弃", raw)
		return nil, false
	}
	chunks := make([]GroundingChunk, 0, len(values))
	for index, rawChunk := range values {
		chunks = append(chunks, decodeGroundingChunk(rawChunk, fmt.Sprintf("%s[%d]", path, index), tolerance))
	}
	return chunks, true
}

// groundingChunkUnknown 为读不出来源的 grounding chunk 的占位来源：各协议输出时不带网址，只保持 support 的 chunk 索引对齐
const groundingChunkUnknown = "unknown"

// groundingChunkSources 为 grounding chunk oneof 下标 0/1/2 的来源。下标 1 为图片来源（字段依次为 source_uri、domain），按上游
// Mag1cFall/AIStudio2API bd6c40f 的现场确认改认（原先当作 retrieved_context）；web 与 maps 的字段依次为 URI、title、text、place ID
var groundingChunkSources = []string{"web", "image", "maps"}

// decodeGroundingChunk 解码一个 grounding chunk：取下标 0/1/2 里第一个设置了的来源（同时设置多个时取第一个）；为空、为 null、
// 不是数组或只有未知下标（3 及以上，来源类型确实会新增）时换成占位项；字段不是字符串时忽略该字段。都写容错日志，不中止
func decodeGroundingChunk(raw json.RawMessage, path string, tolerance *decodeTolerance) GroundingChunk {
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding chunk 不是数组，换成占位项", raw)
		return GroundingChunk{Source: groundingChunkUnknown}
	}
	variant, known, unknown := -1, 0, -1
	for index, value := range values {
		if isJSONNull(value) {
			continue
		}
		if index >= len(groundingChunkSources) {
			if unknown < 0 {
				unknown = index
			}
			continue
		}
		if variant < 0 {
			variant = index
		}
		known++
	}
	switch {
	case variant < 0 && unknown >= 0:
		tolerance.note(gcMethod, path, fmt.Sprintf("grounding chunk 的来源类型未知（下标 %d），换成占位项", unknown), raw)
		return GroundingChunk{Source: groundingChunkUnknown}
	case variant < 0:
		tolerance.note(gcMethod, path, "grounding chunk 为空，换成占位项", raw)
		return GroundingChunk{Source: groundingChunkUnknown}
	case known > 1:
		tolerance.note(gcMethod, path, "grounding chunk 同时设置多个来源，取第一个", raw)
	}
	chunk := GroundingChunk{Source: groundingChunkSources[variant]}
	sourcePath := fmt.Sprintf("%s[%d]", path, variant)
	fields, err := rawArray(values[variant], sourcePath, raw)
	if err != nil {
		tolerance.note(gcMethod, sourcePath, "grounding chunk 的来源不是数组，只保留来源类型", values[variant])
		return chunk
	}
	stringFields := []*string{&chunk.URI, &chunk.Title, &chunk.Text, &chunk.PlaceID}
	if chunk.Source == "image" {
		// GroundingChunk.Image：source_uri、domain
		stringFields = []*string{&chunk.URI, &chunk.Domain}
	}
	for index := 0; index < len(stringFields) && index < len(fields); index++ {
		if isJSONNull(fields[index]) {
			continue
		}
		if json.Unmarshal(fields[index], stringFields[index]) != nil {
			*stringFields[index] = ""
			tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", sourcePath, index), "grounding chunk 的字段不是字符串，已忽略", fields[index])
		}
	}
	return chunk
}

// decodeGroundingSupports 解码 grounding support 列表 [segment, chunkIndices, confidenceScores]：列表不是数组时丢掉；某一项不是数组时
// 跳过该项；chunk 索引为 null 或读不出时跳过该索引（与它等长的置信度同时去掉同一位置，保持一一对应）；读不出的置信度使整组置信度丢弃
func decodeGroundingSupports(raw json.RawMessage, path string, tolerance *decodeTolerance) []GroundingSupport {
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding support 列表不是数组，已丢弃", raw)
		return nil
	}
	supports := make([]GroundingSupport, 0, len(values))
	for index, supportRaw := range values {
		supportPath := fmt.Sprintf("%s[%d]", path, index)
		fields, err := rawArray(supportRaw, supportPath, raw)
		if err != nil {
			tolerance.note(gcMethod, supportPath, "grounding support 不是数组，已跳过", supportRaw)
			continue
		}
		support := GroundingSupport{}
		if segmentRaw := rawAt(fields, 0); !isJSONNull(segmentRaw) {
			support.Segment = decodeGroundingSegment(segmentRaw, supportPath+"[0]", tolerance)
		}
		var indices, scores []json.RawMessage
		if indicesRaw := rawAt(fields, 1); !isJSONNull(indicesRaw) {
			if indices, err = rawArray(indicesRaw, supportPath+"[1]", raw); err != nil {
				tolerance.note(gcMethod, supportPath+"[1]", "grounding support 的 chunk 索引不是数组，已忽略", indicesRaw)
			} else {
				support.ChunkIndices = make([]int, 0, len(indices))
			}
		}
		scoresRaw := rawAt(fields, 2)
		if !isJSONNull(scoresRaw) {
			if scores, err = rawArray(scoresRaw, supportPath+"[2]", raw); err != nil {
				tolerance.note(gcMethod, supportPath+"[2]", "grounding support 的置信度不是数组，已忽略", scoresRaw)
				scores = nil
			} else {
				support.ConfidenceScores = make([]float64, 0, len(scores))
			}
		}
		// paired 为置信度与 chunk 索引一一对应：跳过的索引同时去掉同一位置的置信度
		paired := len(indices) == len(scores)
		skipped := make(map[int]bool)
		for valueIndex, indexRaw := range indices {
			value, err := rawInt64(indexRaw, "", raw)
			if err != nil {
				converted, ok := lenientInt64(indexRaw)
				if !ok {
					tolerance.note(gcMethod, fmt.Sprintf("%s[1][%d]", supportPath, valueIndex), "grounding support 的 chunk 索引为 null 或不是整数，已跳过该索引", indexRaw)
					skipped[valueIndex] = true
					continue
				}
				tolerance.note(gcMethod, fmt.Sprintf("%s[1][%d]", supportPath, valueIndex), "grounding support 的 chunk 索引不是整数，按整数换算", indexRaw)
				value = converted
			}
			support.ChunkIndices = append(support.ChunkIndices, int(value))
		}
		for valueIndex, scoreRaw := range scores {
			if paired && skipped[valueIndex] {
				continue
			}
			score, ok := lenientFloat64(scoreRaw)
			if !ok {
				tolerance.note(gcMethod, fmt.Sprintf("%s[2][%d]", supportPath, valueIndex), "grounding support 的置信度不是数字，丢弃这一组置信度", scoreRaw)
				support.ConfidenceScores = nil
				break
			}
			support.ConfidenceScores = append(support.ConfidenceScores, score)
		}
		supports = append(supports, support)
	}
	return supports
}

// decodeGroundingSegment 解码 support 的 segment [partIndex, startIndex, endIndex, text]：不是数组时按空片段，整数字段读不出时忽略该字段
// （带小数的数与数字字符串按整数换算），text 不是字符串时忽略
func decodeGroundingSegment(raw json.RawMessage, path string, tolerance *decodeTolerance) GroundingSegment {
	segment := GroundingSegment{}
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "grounding support 的 segment 不是数组，按空片段", raw)
		return segment
	}
	for index, destination := range []*int{&segment.PartIndex, &segment.StartIndex, &segment.EndIndex} {
		value := rawAt(values, index)
		if isJSONNull(value) {
			continue
		}
		parsed, err := rawInt64(value, "", raw)
		if err != nil {
			var ok bool
			if parsed, ok = lenientInt64(value); !ok {
				tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", path, index), "grounding support 的 segment 位置不是整数，已忽略", value)
				continue
			}
			tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", path, index), "grounding support 的 segment 位置不是整数，按整数换算", value)
		}
		*destination = int(parsed)
	}
	if textRaw := rawAt(values, 3); !isJSONNull(textRaw) && json.Unmarshal(textRaw, &segment.Text) != nil {
		segment.Text = ""
		tolerance.note(gcMethod, path+"[3]", "grounding support 的 segment 文本不是字符串，已忽略", textRaw)
	}
	return segment
}
