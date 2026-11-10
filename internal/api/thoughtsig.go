package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// thoughtSignatureCapacity 是工具调用思考签名的保留条数（含“已签发、没有签名”的调用登记）
const thoughtSignatureCapacity = 4096

// 中转与客户端填进历史函数调用的占位签名。new-api 把 Gemini 回复转成 OpenAI 格式时丢掉 thoughtSignature，下一轮再转回 Gemini 时
// 给每条 assistant 消息的第一个 functionCall 写 placeholderSignatureNewAPI；skipThoughtSignature 为官方文档给出的跳过校验值
// （本服务缺签名时也写它）。两者都不是上一轮真实的思考签名，Gemini 3 拿到它们就丢了之前几步的思考上下文
const (
	placeholderSignatureNewAPI = "context_engineering_is_the_way_to_go"
	skipThoughtSignature       = "skip_thought_signature_validator"
)

// placeholderThoughtSignature 判断函数调用上的签名是不是缺失或占位值：是时可以按本进程签发过的签名还原
func placeholderThoughtSignature(signature string) bool {
	return aistudio.PlaceholderThoughtSignature(signature)
}

// thoughtSignatureStore 保存本进程各协议响应中工具调用的思考签名（OpenAI Chat 与 Gemini 原生入口共用），
// 客户端或中转下一轮带回调用却没有带回签名时按调用 ID 还原。每条记录都带作用域（见 thoughtSignatureScope），作用域不同的请求互不还原。
// 只按调用 ID 还原、不按函数名加参数：经中转时所有终端用户共用本服务的同一把 API key，作用域分不出对话，同名同参的调用
// （不同用户问同一件事、同一段对话里重复检索同一关键词）按函数名加参数会被套上别的对话、别的轮次的签名
type thoughtSignatureStore struct {
	mu sync.Mutex
	// byID 以“作用域 + 调用 ID + 函数名”为键，登记本服务输出过的每个带 ID 的调用：带签名的记下签名，没有签名的（Gemini 只给并行调用的
	// 第一个签名）记为“已签发、没有签名”，还原时保持原样
	byID map[string]thoughtSignatureEntry
	// order 为登记顺序，超过容量时从最早的淘汰
	order    []thoughtSignatureKey
	sequence uint64
}

type thoughtSignatureEntry struct {
	// signature 为空表示这个调用 ID 已签发、没有签名
	signature string
	// arguments 为签发时参数的摘要（thoughtSignatureArguments）：调用 ID 不是本服务补发的格式时，还原前核对参数没有变
	arguments string
	sequence  uint64
}

type thoughtSignatureKey struct {
	id       string
	sequence uint64
}

func newThoughtSignatureStore() *thoughtSignatureStore {
	return &thoughtSignatureStore{byID: make(map[string]thoughtSignatureEntry)}
}

// thoughtSignatureScope 返回签名的作用域：请求的目录模型名（带后缀的别名解析为目录模型名）、号池与本服务 API key 的摘要。
// 签名里是那一段对话那一轮的加密思考上下文：另一个模型的签名可能被上游拒绝（原先发送的占位值或跳过值反而能通过），
// 不同号池、换过 API key 的请求也不共用
func (s *server) thoughtSignatureScope(ctx context.Context, model string) string {
	model = strings.ToLower(s.resolveModelName(ctx, strings.TrimPrefix(strings.TrimSpace(model), "models/")))
	key := sha256.Sum256([]byte(s.config.currentAPIKey()))
	return model + "\x00" + strconv.Itoa(int(aistudio.PoolScopeFromContext(ctx))) + "\x00" + hex.EncodeToString(key[:8])
}

// withThoughtSignatureRestore 给每个公开 API 请求挂上按模型还原签名的函数（aistudio.ThoughtSignatureRestore）：生成服务额度用完改用其他模型时
// 按目标模型的作用域再还原一次（协议层只按请求的模型还原过，改用期间目标模型签发、登记在它作用域下的签名要这样才找得回）
func (s *server) withThoughtSignatureRestore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(aistudio.ContextWithThoughtSignatureRestore(r.Context(), s.restoreThoughtSignatures)))
	})
}

// restoreThoughtSignatures 按 model 的作用域还原 contents 里函数调用的签名（见 Restore）
func (s *server) restoreThoughtSignatures(ctx context.Context, model string, contents []aistudio.Content) int {
	return s.thoughtSignatures.Restore(s.thoughtSignatureScope(ctx, model), contents)
}

// Remember 登记一次响应中带 ID 的工具调用：带真实签名的按调用 ID 记下签名；签名为空或占位值的登记为“已签发、没有签名”
// （同一个 ID 已有签名时不覆盖）；没有函数名或没有调用 ID 的不登记（还原只按 ID）
func (store *thoughtSignatureStore) Remember(scope string, calls []aistudio.FunctionCall) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, call := range calls {
		if call.Name == "" || call.ID == "" {
			continue
		}
		signed := !placeholderThoughtSignature(call.ThoughtSignature)
		key := thoughtSignatureIDKey(scope, call)
		if existing, ok := store.byID[key]; !signed && ok && existing.signature != "" {
			continue
		}
		store.sequence++
		entry := thoughtSignatureEntry{arguments: thoughtSignatureArguments(call.Arguments), sequence: store.sequence}
		if signed {
			entry.signature = call.ThoughtSignature
		}
		store.byID[key] = entry
		store.order = append(store.order, thoughtSignatureKey{id: key, sequence: store.sequence})
	}
	for len(store.order) > thoughtSignatureCapacity {
		oldest := store.order[0]
		store.order = store.order[1:]
		// 同一个键之后又被更新时保留新的记录
		if entry, ok := store.byID[oldest.id]; ok && entry.sequence == oldest.sequence {
			delete(store.byID, oldest.id)
		}
	}
}

// Restore 为历史函数调用补回本进程在同一作用域签发过的真实签名，返回还原的个数。只处理签名缺失或为占位值、带调用 ID 的调用
// （客户端带回的真实签名优先）：按 ID 查到真实签名才还原，ID 不是本服务补发的 call_local_* 时还要求参数一致；登记为“已签发、没有签名”
// （并行调用的第二个及以后）、查不到（服务重启、已淘汰、不是本服务签发的 ID）或不带 ID 的调用保持原样，不按函数名加参数去找，
// 否则会把别的对话、别的轮次里同名同参调用的签名套上来。
//
// 还原时同时改写 Part.ThoughtSignature 与 FunctionCall.ThoughtSignature（编码时 Part 上的签名优先），按下标改写原切片，
// FunctionCall 换成副本（不改调用方持有的结构）。还原不到时保持原样（占位值照原样发送，缺失时由编码补跳过校验值）
func (store *thoughtSignatureStore) Restore(scope string, contents []aistudio.Content) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	restored := 0
	for contentIndex := range contents {
		parts := contents[contentIndex].Parts
		for partIndex := range parts {
			part := &parts[partIndex]
			call := part.FunctionCall
			if call == nil || call.ID == "" {
				continue
			}
			if !placeholderThoughtSignature(part.ThoughtSignature) || !placeholderThoughtSignature(call.ThoughtSignature) {
				continue
			}
			entry, ok := store.byID[thoughtSignatureIDKey(scope, *call)]
			if !ok || entry.signature == "" {
				continue
			}
			if !aistudio.LocalCallID(call.ID) && entry.arguments != thoughtSignatureArguments(call.Arguments) {
				continue
			}
			copied := *call
			copied.ThoughtSignature = entry.signature
			part.FunctionCall = &copied
			part.ThoughtSignature = entry.signature
			restored++
		}
	}
	return restored
}

// thoughtSignatureIDKey 以作用域、调用 ID 与函数名标识一次工具调用
func thoughtSignatureIDKey(scope string, call aistudio.FunctionCall) string {
	return scope + "\x00" + call.ID + "\x00" + call.Name
}

// thoughtSignatureArguments 返回参数规范化后的摘要：键排序、数值按 Go 的默认格式重写（中转把参数解析成 map 再序列化时写法相同），
// 空参数、null 与 {} 相同；不是合法 JSON 时按原文。只存摘要，大参数不占内存
func thoughtSignatureArguments(raw json.RawMessage) string {
	arguments := []byte(strings.TrimSpace(string(raw)))
	var value any
	if len(arguments) == 0 {
		arguments = []byte(`{}`)
	} else if json.Unmarshal(arguments, &value) == nil {
		if value == nil {
			value = map[string]any{}
		}
		if canonical, err := json.Marshal(value); err == nil {
			arguments = canonical
		}
	}
	digest := sha256.Sum256(arguments)
	return hex.EncodeToString(digest[:16])
}
