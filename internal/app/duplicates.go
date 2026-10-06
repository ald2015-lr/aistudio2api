package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 重复回复检测（主路由也开启，只保存指纹，不保存任何内容）：
// 同一段回复正文在窗口期内再次出现时写一条 WARN 日志，并列出两次请求实际发给上游的
// seed、温度、top_p、top_k、思考强度、账号与通道，以及两次的提示词是否相同，用来判断重复从哪里来
const (
	duplicateWindow     = 2 * time.Hour
	duplicateMaxEntries = 20000
	duplicateMinBytes   = 200
	duplicateRecentKeep = 200
	progressReasonLimit = 300
)

// generationDiagnostics 为一次生成实际发给上游的关键参数
type generationDiagnostics struct {
	seed        string
	temperature string
	topP        string
	topK        string
	thinking    string
	promptHash  string
	nonce       bool
	// downgrade 为降级判定（只有被拦截的模型才有，见 downgrade_guard.go）
	downgrade *downgradeGate
}

// replyRecord 为一次完成生成的指纹记录
type replyRecord struct {
	RequestID    string    `json:"request_id"`
	At           time.Time `json:"at"`
	Model        string    `json:"model"`
	Account      string    `json:"account"`
	Channel      string    `json:"channel"`
	Seed         string    `json:"seed"`
	Temperature  string    `json:"temperature"`
	TopP         string    `json:"top_p"`
	TopK         string    `json:"top_k"`
	Thinking     string    `json:"thinking"`
	PromptHash   string    `json:"prompt_hash"`
	ReplyHash    string    `json:"reply_hash"`
	ReplyBytes   int       `json:"reply_bytes"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	Traced       bool      `json:"traced,omitempty"`
	Nonce        bool      `json:"nonce,omitempty"`
}

// duplicatePair 为一对内容完全相同的回复
type duplicatePair struct {
	Previous   replyRecord `json:"previous"`
	Current    replyRecord `json:"current"`
	SamePrompt bool        `json:"same_prompt"`
	SameSeed   bool        `json:"same_seed"`
	Gap        string      `json:"gap"`
}

type duplicateStats struct {
	Since         time.Time `json:"since"`
	Observed      int64     `json:"observed"`
	Duplicates    int64     `json:"duplicates"`
	SamePrompt    int64     `json:"same_prompt"`
	SameSeed      int64     `json:"same_seed"`
	DifferentSeed int64     `json:"different_seed"`
	RandomSeed    int64     `json:"random_seed"`
	// Repeats 为提示词与窗口期内某次完成的请求相同的次数（重新生成），RepeatsSameReply 为其中回复也相同的次数
	Repeats          int64 `json:"repeats"`
	RepeatsSameReply int64 `json:"repeats_same_reply"`
	// RepeatsWithNonce 为加了随机后缀的重新生成次数，NonceSameReply 为加了后缀回复仍相同的次数（应接近 0）
	RepeatsWithNonce int64 `json:"repeats_with_nonce"`
	NonceSameReply   int64 `json:"nonce_same_reply"`
}

type duplicateKey struct {
	hash   string
	prompt string
	at     time.Time
}

type duplicateDetector struct {
	mu      sync.Mutex
	byReply map[string]replyRecord
	// byPrompt 为每个提示词指纹最近一次完成的记录，用于统计重新生成时回复是否相同
	byPrompt map[string]replyRecord
	order   []duplicateKey
	recent  []duplicatePair
	stats   duplicateStats
}

// replyDuplicates 在进程内共享，服务重启（管理页重启生成服务）后统计不清零
var replyDuplicates = newDuplicateDetector()

func newDuplicateDetector() *duplicateDetector {
	return &duplicateDetector{
		byReply: make(map[string]replyRecord), byPrompt: make(map[string]replyRecord),
		stats: duplicateStats{Since: time.Now()},
	}
}

func newGenerationDiagnostics(request aistudio.GenerateRequest, seed string) *generationDiagnostics {
	config := request.Config
	diag := &generationDiagnostics{
		seed: seed, temperature: "默认", topP: "默认", topK: "默认", thinking: "默认",
		promptHash: promptFingerprint(request),
	}
	if config.Temperature != nil {
		diag.temperature = fmt.Sprintf("%g", *config.Temperature)
	}
	if config.TopP != nil {
		diag.topP = fmt.Sprintf("%g", *config.TopP)
	}
	if config.TopK != nil {
		diag.topK = fmt.Sprintf("%d", *config.TopK)
	}
	if effort := strings.TrimSpace(config.ReasoningEffort); effort != "" {
		diag.thinking = effort
	} else if config.ThinkingBudget != nil {
		diag.thinking = fmt.Sprintf("预算%d", *config.ThinkingBudget)
	}
	return diag
}

// promptFingerprint 计算提示词指纹：系统指令与全部消息的角色、文本、附件、工具调用与结果
func promptFingerprint(request aistudio.GenerateRequest) string {
	digest := sha256.New()
	write := func(tag string, value string) {
		digest.Write([]byte(tag))
		digest.Write([]byte{0})
		digest.Write([]byte(value))
		digest.Write([]byte{0})
	}
	write("system", request.System)
	for _, content := range request.Contents {
		write("role", string(content.Role))
		for _, part := range content.Parts {
			write("text", part.Text)
			if part.InlineData != nil {
				write("inline", part.InlineData.MIME)
				digest.Write(part.InlineData.Data)
			}
			if part.ExternalMedia != nil || part.File != nil || part.FunctionCall != nil || part.FunctionResult != nil ||
				part.ExecutableCode != nil || part.CodeExecutionResult != nil {
				stripped := part
				stripped.Text = ""
				stripped.InlineData = nil
				if encoded, err := json.Marshal(stripped); err == nil {
					digest.Write(encoded)
				}
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil))[:12]
}

// observe 记录一次完成的回复；与窗口期内某次回复完全相同时返回那次记录
func (detector *duplicateDetector) observe(record replyRecord) (replyRecord, bool) {
	if record.ReplyHash == "" || record.ReplyBytes < duplicateMinBytes {
		return replyRecord{}, false
	}
	detector.mu.Lock()
	defer detector.mu.Unlock()
	detector.evict(record.At)
	detector.stats.Observed++
	if record.PromptHash != "" {
		if last, ok := detector.byPrompt[record.PromptHash]; ok {
			sameReply := last.ReplyHash == record.ReplyHash
			detector.stats.Repeats++
			if sameReply {
				detector.stats.RepeatsSameReply++
			}
			if record.Nonce {
				detector.stats.RepeatsWithNonce++
				if sameReply {
					detector.stats.NonceSameReply++
				}
			}
		}
		detector.byPrompt[record.PromptHash] = record
	}
	previous, found := detector.byReply[record.ReplyHash]
	detector.byReply[record.ReplyHash] = record
	detector.order = append(detector.order, duplicateKey{hash: record.ReplyHash, prompt: record.PromptHash, at: record.At})
	if !found {
		return replyRecord{}, false
	}
	pair := duplicatePair{
		Previous: previous, Current: record,
		SamePrompt: previous.PromptHash == record.PromptHash,
		SameSeed:   previous.Seed == record.Seed,
		Gap:        record.At.Sub(previous.At).Round(time.Second).String(),
	}
	detector.stats.Duplicates++
	if pair.SamePrompt {
		detector.stats.SamePrompt++
	}
	if pair.SameSeed {
		detector.stats.SameSeed++
	} else {
		detector.stats.DifferentSeed++
	}
	if strings.HasPrefix(record.Seed, "随机") {
		detector.stats.RandomSeed++
	}
	detector.recent = append(detector.recent, pair)
	if len(detector.recent) > duplicateRecentKeep {
		detector.recent = append([]duplicatePair(nil), detector.recent[len(detector.recent)-duplicateRecentKeep:]...)
	}
	return previous, true
}

// evict 删除窗口期外或超出数量上限的旧指纹
func (detector *duplicateDetector) evict(now time.Time) {
	drop := 0
	for drop < len(detector.order) {
		key := detector.order[drop]
		if now.Sub(key.at) <= duplicateWindow && len(detector.order)-drop < duplicateMaxEntries {
			break
		}
		if current, ok := detector.byReply[key.hash]; ok && current.At.Equal(key.at) {
			delete(detector.byReply, key.hash)
		}
		if current, ok := detector.byPrompt[key.prompt]; ok && current.At.Equal(key.at) {
			delete(detector.byPrompt, key.prompt)
		}
		drop++
	}
	if drop > 0 {
		detector.order = detector.order[drop:]
	}
}

func (detector *duplicateDetector) snapshot() map[string]any {
	detector.mu.Lock()
	defer detector.mu.Unlock()
	recent := make([]duplicatePair, len(detector.recent))
	for index := range detector.recent {
		// 最新的在前
		recent[index] = detector.recent[len(detector.recent)-1-index]
	}
	return map[string]any{
		"window": duplicateWindow.String(), "min_reply_bytes": duplicateMinBytes,
		"tracked": len(detector.byReply), "tracked_prompts": len(detector.byPrompt), "stats": detector.stats, "recent": recent,
	}
}

// DuplicateReplies 返回重复回复统计与最近的重复记录（管理接口 GET /api/debug/duplicates）
func (manager *runtimeManager) DuplicateReplies() any {
	return replyDuplicates.snapshot()
}

// recordReplyFingerprint 用回复指纹做重复检测：与之前某次回复完全相同时写一条 WARN，并写入该请求的时间线
func (service *trackedService) recordReplyFingerprint(
	requestID string,
	accountLabel string,
	channel string,
	modelID string,
	replyHash string,
	replyBytes int,
	usage *aistudio.Usage,
	diag *generationDiagnostics,
	traced bool,
) {
	if diag == nil {
		return
	}
	record := replyRecord{
		RequestID: requestID, At: time.Now(), Model: modelID, Account: accountLabel, Channel: channel,
		Seed: diag.seed, Temperature: diag.temperature, TopP: diag.topP, TopK: diag.topK, Thinking: diag.thinking,
		PromptHash: diag.promptHash, ReplyHash: replyHash, ReplyBytes: replyBytes, Traced: traced, Nonce: diag.nonce,
	}
	if usage != nil {
		record.OutputTokens = usage.OutputTokens + usage.ReasoningTokens
	}
	previous, duplicate := replyDuplicates.observe(record)
	if !duplicate {
		return
	}
	samePrompt := "不同"
	if previous.PromptHash == record.PromptHash {
		samePrompt = "相同"
	}
	gap := record.At.Sub(previous.At).Round(time.Second)
	service.requests.log(accountLabel, "WARN", fmt.Sprintf(
		"重复回复 | 回复指纹=%s | 间隔=%s | 提示词=%s | 模型=%s\n"+
			"本次: 请求=%s seed=%s 温度=%s top_p=%s top_k=%s 思考=%s 账号=%s 通道=%s 随机后缀=%s\n"+
			"上次: 请求=%s seed=%s 温度=%s top_p=%s top_k=%s 思考=%s 账号=%s 通道=%s 随机后缀=%s",
		replyHash, gap, samePrompt, modelID,
		requestID, record.Seed, record.Temperature, record.TopP, record.TopK, record.Thinking, accountLabel, channel,
		nonceLabel(record.Nonce),
		previous.RequestID, previous.Seed, previous.Temperature, previous.TopP, previous.TopK, previous.Thinking,
		previous.Account, previous.Channel, nonceLabel(previous.Nonce),
	))
	service.requests.logRequestProgress(requestID, accountLabel, "WARN", fmt.Sprintf(
		"回复与 %s 前的请求 %s 完全相同 | 提示词%s | seed 本次=%s 上次=%s",
		gap, previous.RequestID, samePrompt, record.Seed, previous.Seed,
	))
}

// progressReason 把错误压成一行写进请求时间线，过长时截断
func progressReason(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if len(reason) <= progressReasonLimit {
		return reason
	}
	cut := progressReasonLimit
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + "…"
}

// 随机后缀。排查记录证实：上游对完全相同的请求会返回完全相同的回复——两次请求只有 seed 不同、温度 1.5、账号也不同，
// 回复正文、输出 token 数与思考 token 数仍然完全一致；只差几百个字的提示词也出现过回复一字不差的情况。上游不理会 seed，
// 采样结果由输入决定。两次的上游原始响应各自独立（思考摘要、签名、响应 ID 都不同），不是本服务重放了回复。
// 本服务能做的只有让每次发给上游的输入都不同：在最后一条用户消息末尾加入不可见的零宽字符随机后缀（加在末尾，
// 不影响上游对前面内容的隐式缓存）。只对完全相同的提示词加后缀之后，这类重复回复已经消失；只差几个字的提示词
// 仍会撞上同一回复，所以改为每个请求都加
const promptNonceBits = 32

// newPromptNonce 生成不可见的随机后缀：32 个零宽字符（U+200B / U+200C）编码 32 位随机数
func newPromptNonce() string {
	bits := rand.Uint32()
	var builder strings.Builder
	builder.Grow(promptNonceBits * 3)
	for index := 0; index < promptNonceBits; index++ {
		if (bits>>index)&1 == 1 {
			builder.WriteString("\u200c")
		} else {
			builder.WriteString("\u200b")
		}
	}
	return builder.String()
}

// applyPromptNonce 把随机后缀加到最后一条用户消息末尾；工具结果所在的轮次不改动。返回副本，不修改调用方的数据
func applyPromptNonce(request aistudio.GenerateRequest, nonce string) (aistudio.GenerateRequest, bool) {
	for index := len(request.Contents) - 1; index >= 0; index-- {
		content := request.Contents[index]
		if content.Role != aistudio.RoleUser {
			continue
		}
		for _, part := range content.Parts {
			if part.FunctionResult != nil {
				return request, false
			}
		}
		parts := append([]aistudio.Part(nil), content.Parts...)
		if last := len(parts) - 1; last >= 0 && isPlainTextPart(parts[last]) {
			parts[last].Text += nonce
		} else {
			parts = append(parts, aistudio.Part{Text: nonce})
		}
		contents := append([]aistudio.Content(nil), request.Contents...)
		contents[index] = aistudio.Content{Role: content.Role, Parts: parts}
		request.Contents = contents
		return request, true
	}
	return request, false
}

func isPlainTextPart(part aistudio.Part) bool {
	return part.Text != "" && !part.Thought && part.InlineData == nil && part.ExternalMedia == nil && part.File == nil &&
		part.FunctionCall == nil && part.FunctionResult == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil
}

// maybeAddPromptNonce 开启了该功能、客户端没有指定 seed、温度不为 0 时给请求加入随机后缀；返回是否加入。
// 客户端指定 seed 或把温度设为 0，通常是需要可复现的结果，这两种情况不做改动
func (service *trackedService) maybeAddPromptNonce(request *aistudio.GenerateRequest, diag *generationDiagnostics, clientSeed bool) bool {
	greedy := request.Config.Temperature != nil && *request.Config.Temperature == 0
	if clientSeed || greedy || !service.repeatNonce.Load() || !randomSeedApplicable(request.Model, request.Config) {
		return false
	}
	modified, ok := applyPromptNonce(*request, newPromptNonce())
	if !ok {
		return false
	}
	*request = modified
	diag.nonce = true
	return true
}

func nonceLabel(added bool) string {
	if added {
		return "已加"
	}
	return "未加"
}

