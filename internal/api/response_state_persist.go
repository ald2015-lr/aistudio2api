package api

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"slices"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Responses 与 Interactions 的续接状态写入状态库：
//
// previous_response_id 与 previous_interaction_id 指向的续接节点原先只在进程内保存，服务被 start.sh 自动拉起或崩溃自愈后，
// 依赖它们的客户端（Codex 等）一律收到 not found、断了上下文。现在节点的每次变化（保存新节点、淘汰最早的节点并把它的上下文
// 并入子节点）都写进状态库，NewHandler 创建时按保存顺序读回。内存里的规则不变：最多 256 个节点，按保存顺序淘汰并把上下文并入
// 子节点，存入前去掉 GIF 转 PNG 留下的原 GIF（dropSourceGIF）；状态库只是这份内存状态的副本。
//
// 状态库由 app 层接到请求账本的 SQLite（requestdb 的 response_states 表，runtime/requests.db）：节点编码后单独一行，写入经账本的
// 写入协程排队、与用量记录合并成事务，不阻塞请求。表里是明文的对话内容（提示词、回复、函数结果与 4 MiB 以内的内联附件），与
// REQUEST_BODY_LOG 无关；store:false 的请求不保存节点，也就不写库。
//
// 单个节点超过 responseStatePersistLimit（4 MiB，通常是带大图片、视频等内联附件，或被并入了很长的上文）或编码失败时不写入，
// 记入 unpersisted（不入库）并从状态库删除它与它当时的后代（它们重启后都接不上），之后以它为续接起点保存的节点继承这个标记、
// 也不写库：重启后从它们续接返回 not found。一条链只在第一次超限时写一行 INFO（编码失败为 WARN）；编码前先按文本与内联数据的字节数粗估
// （estimatedResponseStateSize，只会偏小），粗估已超限的不编码，不在持锁时为注定丢弃的节点编码几 MiB。
// 读回时编码读不出的、续接链上有节点不在库里的（不论它在库里排在前面还是后面）丢弃并从库里删除；超过 256 个时照常淘汰。
// 库的总量受 256 个节点与单个 4 MiB 限制，最坏约 1 GiB（内存里的同一批节点没有单个上限，只会更大）；SQLite 删除后复用空闲页，
// 文件不会自动缩小。账本没有打开时（Config.ResponseStates 为 nil）与原先一样只在内存里保存。

// ResponseStatePersister 持久保存 Responses 与 Interactions 的续接节点，服务重启后读回
type ResponseStatePersister interface {
	// LoadResponseStates 按保存顺序返回已保存的节点
	LoadResponseStates() ([]SavedResponseState, error)
	// SaveResponseStates 排队写入：saved 按 ID 写入（已有的节点替换内容、保留原来的保存顺序），removed 删除；同一次调用在同一个事务里完成，
	// 不等待落盘；排队已满时丢弃这一次（状态库停在上一次的一致状态）
	SaveResponseStates(saved []SavedResponseState, removed []string)
	// LogResponseStates 写一行续接状态的运行日志（不含提示词与回复）
	LogResponseStates(level string, message string)
}

// SavedResponseState 是写进状态库的一个续接节点：ID 为 resp_ 或 int_ 开头的响应 ID，Data 为 encodeResponseState 的编码
type SavedResponseState struct {
	ID   string
	Data []byte
}

// responseStatePersistLimit 为写进状态库的单个节点编码后的上限
const responseStatePersistLimit = 4 << 20

// responseStateFormat 为节点编码的版本：第一个字节，其后为 gob 编码的 persistedResponseState
const responseStateFormat byte = 1

// persistedResponseState 为写进状态库的节点内容，与 responseState 字段相同（加字段时编译器会提醒这里是否要一起存）。
// 用 gob 编码：Part 的 FromToolResult、RestoredLink、RestoredText 与 Content.FromSystem 在 JSON 里不序列化，续接时整理对话仍要用到；
// 内联数据按原字节保存，不像 JSON 那样多出三分之一的 Base64
type persistedResponseState struct {
	ParentID           string
	Contents           []aistudio.Content
	InlineInstructions []string
}

// encodeResponseState 编码一个续接节点
func encodeResponseState(state responseState) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte(responseStateFormat)
	if err := gob.NewEncoder(&buffer).Encode(persistedResponseState(state)); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// decodeResponseState 解码 encodeResponseState 写出的续接节点
func decodeResponseState(data []byte) (responseState, error) {
	if len(data) == 0 || data[0] != responseStateFormat {
		return responseState{}, errors.New("未知的续接状态编码")
	}
	var state persistedResponseState
	if err := gob.NewDecoder(bytes.NewReader(data[1:])).Decode(&state); err != nil {
		return responseState{}, err
	}
	return responseState(state), nil
}

// restore 读回状态库里的节点并开始把之后的变化写进它。先解码所有行，再按保存顺序逐个确认续接链上的每一级都在库里：
// 续接起点通常在子节点之前保存，但起点的某次写入被丢弃（排队已满）后、又在淘汰时作为改动过的子节点重新写入的，排在它的子节点之后，
// 只看前面已读入的行会把这些子节点误判为孤儿。编码读不出、续接链断开（或成环）的节点丢弃并从库里删除；超过容量时按内存的规则淘汰
// （evictOldest 不要求续接起点排在子节点之前）。只在创建处理器时、开始服务之前调用一次
func (store *responseStateStore) restore(persist ResponseStatePersister) {
	saved, err := persist.LoadResponseStates()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.persist = persist
	if err != nil {
		persist.LogResponseStates("WARN", "读取续接状态失败，本次运行从空状态开始，previous_response_id 与 previous_interaction_id 指向重启前的响应时返回 not found | 错误="+err.Error())
		return
	}
	var removed []string
	invalid, orphaned := 0, 0
	decoded := make(map[string]responseState, len(saved))
	ids := make([]string, 0, len(saved))
	for index, item := range saved {
		if _, exists := decoded[item.ID]; exists {
			continue
		}
		state, err := decodeResponseState(item.Data)
		// 解码后不再需要原始编码，尽早释放，读回时不同时常驻两份
		saved[index].Data = nil
		if err != nil {
			invalid++
			removed = append(removed, item.ID)
			continue
		}
		decoded[item.ID] = state
		ids = append(ids, item.ID)
	}
	complete := make(map[string]bool, len(decoded))
	for _, id := range ids {
		if !responseChainComplete(decoded, complete, id) {
			orphaned++
			removed = append(removed, id)
			continue
		}
		store.states[id] = decoded[id]
		store.order = append(store.order, id)
	}
	var touched []string
	for len(store.order) > responseStateCapacity {
		evicted, children := store.evictOldest()
		removed = append(removed, evicted)
		touched = append(touched, children...)
	}
	store.persistLocked(touched, removed)
	if len(saved) > 0 {
		message := fmt.Sprintf("已读回 Responses 与 Interactions 的续接状态（状态库以明文保存这些节点的对话内容）| 节点=%d", len(store.states))
		if invalid > 0 || orphaned > 0 {
			message += fmt.Sprintf(" | 丢弃读不出的节点=%d | 丢弃续接起点已不在的节点=%d", invalid, orphaned)
		}
		persist.LogResponseStates("INFO", message)
	}
}

// responseChainComplete 返回从 id 沿 ParentID 向上的每一级是否都在 decoded 里（成环的也算不完整），沿途的结果记进 known 复用
func responseChainComplete(decoded map[string]responseState, known map[string]bool, id string) bool {
	var path []string
	visiting := make(map[string]bool)
	complete := true
	for current := id; current != ""; {
		if result, ok := known[current]; ok {
			complete = result
			break
		}
		state, exists := decoded[current]
		if !exists || visiting[current] {
			complete = false
			break
		}
		visiting[current] = true
		path = append(path, current)
		current = state.ParentID
	}
	for _, node := range path {
		known[node] = complete
	}
	return complete
}

// estimatedResponseStateSize 粗估节点编码后的字节数：文本、内联数据、函数参数与结果、代码等的字节数之和，不含 gob 的类型、字段与长度开销，
// 只会小于编码后的实际大小。超过上限时不必编码就能确定超限
func estimatedResponseStateSize(state responseState) int {
	size := len(state.ParentID)
	for _, instruction := range state.InlineInstructions {
		size += len(instruction)
	}
	for _, content := range state.Contents {
		for _, part := range content.Parts {
			size += len(part.Text) + len(part.ThoughtSignature) + len(part.RestoredText)
			if blob := part.InlineData; blob != nil {
				size += len(blob.MIME) + len(blob.Data) + len(blob.SourceGIF)
			}
			if call := part.FunctionCall; call != nil {
				size += len(call.Name) + len(call.Arguments) + len(call.ThoughtSignature)
			}
			if result := part.FunctionResult; result != nil {
				size += len(result.Name) + len(result.Content)
			}
			if code := part.ExecutableCode; code != nil {
				size += len(code.Code)
			}
			if result := part.CodeExecutionResult; result != nil {
				size += len(result.Output) + len(result.Error)
			}
		}
	}
	return size
}

// persistLocked 把改动过的节点（touched 中仍存在、没有标记为不入库的）写进状态库并删除 removed；超过上限或编码失败的节点改为不入库
// （markUnpersisted），状态库里不留它与它后代的旧版本。调用方持有 store.mu：写入只是排队，保证状态库收到的改动与内存的顺序一致
func (store *responseStateStore) persistLocked(touched []string, removed []string) {
	if store.persist == nil || len(touched) == 0 && len(removed) == 0 {
		return
	}
	seen := make(map[string]bool, len(touched))
	saved := make([]SavedResponseState, 0, len(touched))
	for _, id := range touched {
		state, exists := store.states[id]
		if seen[id] || !exists || store.unpersisted[id] {
			continue
		}
		seen[id] = true
		if estimate := estimatedResponseStateSize(state); estimate > responseStatePersistLimit {
			removed = append(removed, store.markUnpersisted(id, "INFO", fmt.Sprintf("内容至少 %d 字节，超过 %d MiB", estimate, responseStatePersistLimit>>20))...)
			continue
		}
		data, err := encodeResponseState(state)
		switch {
		case err != nil:
			removed = append(removed, store.markUnpersisted(id, "WARN", fmt.Sprintf("编码失败（%v）", err))...)
		case len(data) > responseStatePersistLimit:
			removed = append(removed, store.markUnpersisted(id, "INFO", fmt.Sprintf("编码后 %d 字节，超过 %d MiB", len(data), responseStatePersistLimit>>20))...)
		default:
			saved = append(saved, SavedResponseState{ID: id, Data: data})
		}
	}
	// 同一次改动里先写入、后来又随续接起点标记为不入库的节点不再写入（删除已在 removed 里）
	saved = slices.DeleteFunc(saved, func(state SavedResponseState) bool { return store.unpersisted[state.ID] })
	store.persist.SaveResponseStates(saved, removed)
}

// markUnpersisted 把节点与它现在的后代标记为不入库并返回要从状态库删除的 ID：它们重启后都接不上，库里留着也没用。
// 之后以它们为续接起点保存的节点在 Store 时继承标记；节点被淘汰时去掉标记（它的上下文已并入子节点，子节点仍带标记）。
// 每条链只在这里写一行日志（已标记的节点再次改动时不编码、不再写）：超过上限是带大附件或长上文的常规情形，按 INFO；编码失败按 WARN。
// 调用方持有 store.mu
func (store *responseStateStore) markUnpersisted(id string, level string, reason string) []string {
	if store.unpersisted == nil {
		store.unpersisted = make(map[string]bool)
	}
	marked := []string{id}
	store.unpersisted[id] = true
	for index := 0; index < len(marked); index++ {
		for childID, child := range store.states {
			if child.ParentID == marked[index] && !store.unpersisted[childID] {
				store.unpersisted[childID] = true
				marked = append(marked, childID)
			}
		}
	}
	message := fmt.Sprintf("续接状态节点%s，不写入状态库（只留在内存，重启后不能从它续接）| 节点=%s", reason, id)
	if descendants := len(marked) - 1; descendants > 0 {
		message += fmt.Sprintf(" | 一并从状态库删除以它为续接起点的节点=%d", descendants)
	}
	store.persist.LogResponseStates(level, message)
	return marked
}
