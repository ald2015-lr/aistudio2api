package aistudio

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// maxStopSequences、maxStopSequenceBytes 为停止序列的数量与单个长度上限（OpenAI 最多 4 个，Gemini 最多 5 个）。
	// 停止序列在本地逐块匹配，每个序列的所有前缀都参与比较，不设上限时耗时随客户端输入按平方增长
	maxStopSequences     = 32
	maxStopSequenceBytes = 1024
)

// ValidateStopSequences 检查停止序列的数量与长度
func ValidateStopSequences(sequences []string) error {
	if len(sequences) > maxStopSequences {
		return fmt.Errorf("stop sequences 最多 %d 个，收到 %d 个", maxStopSequences, len(sequences))
	}
	for _, sequence := range sequences {
		if len(sequence) > maxStopSequenceBytes {
			return fmt.Errorf("单个 stop sequence 最长 %d 字节", maxStopSequenceBytes)
		}
	}
	return nil
}

type stopSequenceMatcher struct {
	sequences []string
	prefixes  []string
	pending   string
}

func newStopSequenceMatcher(sequences []string) *stopSequenceMatcher {
	matcher := &stopSequenceMatcher{}
	seenSequences := make(map[string]struct{}, len(sequences))
	seenPrefixes := make(map[string]struct{})
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if _, exists := seenSequences[sequence]; !exists {
			seenSequences[sequence] = struct{}{}
			matcher.sequences = append(matcher.sequences, sequence)
		}
		for index := range sequence {
			if index == 0 {
				continue
			}
			prefix := sequence[:index]
			if _, exists := seenPrefixes[prefix]; exists {
				continue
			}
			seenPrefixes[prefix] = struct{}{}
			matcher.prefixes = append(matcher.prefixes, prefix)
		}
	}
	if len(matcher.sequences) == 0 {
		return nil
	}
	sort.Slice(matcher.prefixes, func(left int, right int) bool {
		return len(matcher.prefixes[left]) > len(matcher.prefixes[right])
	})
	return matcher
}

func (matcher *stopSequenceMatcher) write(text string) (string, string) {
	combined := matcher.pending + text
	matcher.pending = ""
	matchIndex := -1
	matched := ""
	for _, sequence := range matcher.sequences {
		index := strings.Index(combined, sequence)
		if index < 0 {
			continue
		}
		if matchIndex < 0 || index < matchIndex || (index == matchIndex && len(sequence) < len(matched)) {
			matchIndex = index
			matched = sequence
		}
	}
	if matchIndex >= 0 {
		return combined[:matchIndex], matched
	}
	for _, prefix := range matcher.prefixes {
		if strings.HasSuffix(combined, prefix) {
			matcher.pending = prefix
			return strings.TrimSuffix(combined, prefix), ""
		}
	}
	return combined, ""
}

func (matcher *stopSequenceMatcher) flush() string {
	pending := matcher.pending
	matcher.pending = ""
	return pending
}

func (matcher *stopSequenceMatcher) boundary(kind EventKind) string {
	if matcher == nil || kind == EventText || kind == EventUsage {
		return ""
	}
	return matcher.flush()
}
