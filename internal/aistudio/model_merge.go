package aistudio

import (
	"slices"
	"sort"
)

// mergeModelSets 一次合并多组模型目录，结果与按顺序依次调用 mergeModels 完全相同
// （各字段取值规则、名称取首个非空、最终去重排序都一致）。
//
// 依次调用 mergeModels 时，每一步都要把已合并的全部模型和新一组模型各深拷贝一遍，
// 并对每个重复模型做多次"建哈希表再排序"的集合运算；几百个账户、每个几十个模型时，
// 一次合并要几十万次集合运算。这里每个模型只在首次出现时拷贝一次，字段内容相同时跳过集合运算，
// 开销随模型总数线性增长
func mergeModelSets(sets [][]Model) []Model {
	merged := make([]Model, 0)
	positions := make(map[string]int)
	repeated := make([]bool, 0)
	for _, set := range sets {
		for _, model := range set {
			position, exists := positions[model.ID]
			if !exists {
				positions[model.ID] = len(merged)
				merged = append(merged, cloneModels([]Model{model})[0])
				repeated = append(repeated, false)
				continue
			}
			mergeModelInto(&merged[position], model)
			repeated[position] = true
		}
	}
	for index := range merged {
		merged[index].Methods = unionStrings(nil, merged[index].Methods)
		for name, values := range merged[index].CapabilityOptions {
			merged[index].CapabilityOptions[name] = unionStrings(nil, values)
		}
		// mergeModels 只在模型重复出现时才对 AccessModes 做并集（排序去重），这里保持一致
		if repeated[index] {
			merged[index].AccessModes = unionInt64(nil, merged[index].AccessModes)
		}
	}
	sort.Slice(merged, func(left int, right int) bool {
		return merged[left].ID < merged[right].ID
	})
	return merged
}

// mergeModelInto 把 model 合并进 current，规则与 mergeModels 相同；current 必须是独占的拷贝，
// model 只读（并集函数总是返回新切片，不会与 model 共享底层数组）
func mergeModelInto(current *Model, model Model) {
	if !slices.Equal(current.Methods, model.Methods) {
		current.Methods = unionStrings(current.Methods, model.Methods)
	}
	if current.Name == "" {
		current.Name = model.Name
	}
	if current.Description == "" {
		current.Description = model.Description
	}
	current.InputTokenLimit = minimumPositive(current.InputTokenLimit, model.InputTokenLimit)
	current.OutputTokenLimit = minimumPositive(current.OutputTokenLimit, model.OutputTokenLimit)
	if current.Capabilities == nil && len(model.Capabilities) > 0 {
		current.Capabilities = make(map[string]bool, len(model.Capabilities))
	}
	for name, enabled := range model.Capabilities {
		current.Capabilities[name] = current.Capabilities[name] || enabled
	}
	if current.CapabilityOptions == nil && len(model.CapabilityOptions) > 0 {
		current.CapabilityOptions = make(map[string][]string, len(model.CapabilityOptions))
	}
	for name, values := range model.CapabilityOptions {
		existing, exists := current.CapabilityOptions[name]
		if !exists || !slices.Equal(existing, values) {
			current.CapabilityOptions[name] = unionStrings(existing, values)
		}
	}
	if !slices.Equal(current.AccessModes, model.AccessModes) {
		current.AccessModes = unionInt64(current.AccessModes, model.AccessModes)
	}
	current.Paid = current.Paid || model.Paid
}
