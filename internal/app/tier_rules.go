package app

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 权益规则（ULTRA_ONLY_MODELS、PAID_ONLY_MODELS，判断在账户池，见 aistudio/tier_rules.go）的运行日志与管理页提示。
// 规则只收窄、不放宽，Google 以后给 Pro 开放某个模型时规则仍会挡住它，所以对照账户目录，分两种：
//   - 目录明确标注了被排除的权益可用（访问模式允许它，例如 Pro 账户的目录给 Ultra 专用规则里的模型标了 [3]）：多半是 Google 已开放，
//     写一行 WARN“权益规则比账户目录更严”，管理页设置项旁用醒目的提示建议确认后移除；
//   - 目录没有标注权益限制（访问模式为空）：规则本来就是为这种目录设的，从第一天起就会一直存在，不说明 Google 已开放，
//     按配置项与模型合并成一行 INFO“权益规则在起作用”，管理页用不醒目的说明列出这些权益，不建议移除。
// 规则里的名称不在任何账户的 Playground 或 Build 目录里（写错、带了后缀，或模型还没上线）时写一行 WARN。对照在后台周期（keepWarm）
// 与配置热更新后进行，只读账户池内存（按版本缓存），不在请求路径上，不发上游请求；同样的内容只写一次，消失后再出现时重新写

// tierRuleMonitor 记下已写进运行日志的提示、没有匹配的名称与 Pro 号池能服务的只限 Ultra 的模型
type tierRuleMonitor struct {
	mu        sync.Mutex
	hints     map[string]struct{}
	unmatched map[string]struct{}
	proUltra  map[string]struct{}
}

// checkTierRules 对照规则与账户目录，有新的内容时写日志（不含账户）：目录明确标注被排除的权益可用的每条写一行 WARN（账户数只数
// 标注了的账户），只是没有标注的按配置项与模型合并写一行 INFO，不在任何目录里的名称写一行 WARN。号池里还没有账户读到目录时不对照
func (service *trackedService) checkTierRules() {
	if service == nil || service.pool == nil || service.requests == nil {
		return
	}
	report := service.pool.TierRuleReport()
	if !report.Checked {
		return
	}
	// PRO_POOL=true 时“Pro 不能用只限 Ultra 的模型”只靠 ULTRA_ONLY_MODELS：默认列表里的模型（gemini-4-carbon）与某个目录明确标注只有 Ultra
	// 能用的模型，Pro 号池却能服务（不在 ULTRA_ONLY_MODELS 里、Pro 账户的目录又没有标注）时提示加进去。模型通道按目录版本记忆，锁外逐个查
	proUltra := service.proServableUltraModels(report.UltraMarked)
	hints := make(map[string]struct{}, len(report.Hints))
	unmatched := make(map[string]struct{}, len(report.Unmatched))
	type logLine struct {
		level   string
		message string
	}
	var lines []logLine
	monitor := &service.tierRuleNotes
	monitor.mu.Lock()
	// 目录没有标注的按配置项与模型合并成一行，记住的键带上全部权益（不带账户数：账户陆续载入时不重复写）
	type ruleModel struct{ key, model string }
	var unmarkedOrder []ruleModel
	unmarked := make(map[ruleModel][]aistudio.TierRuleHint)
	for _, hint := range report.Hints {
		if !hint.Marked {
			group := ruleModel{key: hint.Key, model: hint.Model}
			if _, exists := unmarked[group]; !exists {
				unmarkedOrder = append(unmarkedOrder, group)
			}
			unmarked[group] = append(unmarked[group], hint)
			continue
		}
		key := fmt.Sprintf("%s|%s|%d|marked", hint.Key, strings.ToLower(hint.Model), hint.Tier)
		hints[key] = struct{}{}
		if _, logged := monitor.hints[key]; !logged {
			lines = append(lines, logLine{level: "WARN", message: tierRuleHintLine(hint)})
		}
	}
	for _, group := range unmarkedOrder {
		tiers := make([]string, 0, len(unmarked[group]))
		for _, hint := range unmarked[group] {
			tiers = append(tiers, hint.Tier.String())
		}
		key := group.key + "|" + strings.ToLower(group.model) + "|unmarked|" + strings.Join(tiers, ",")
		hints[key] = struct{}{}
		if _, logged := monitor.hints[key]; !logged {
			lines = append(lines, logLine{level: "INFO", message: tierRuleUnmarkedLine(unmarked[group])})
		}
	}
	for _, name := range report.Unmatched {
		key := name.Key + "|" + strings.ToLower(name.Name)
		unmatched[key] = struct{}{}
		if _, logged := monitor.unmatched[key]; !logged {
			lines = append(lines, logLine{level: "WARN", message: fmt.Sprintf(
				"规则模型目前不在任何账户目录里（带后缀的名称要写成基础模型 ID） | 配置项=%s | 模型=%s", name.Key, name.Name)})
		}
	}
	proUltraLogged := make(map[string]struct{}, len(proUltra))
	for _, model := range proUltra {
		key := strings.ToLower(model)
		proUltraLogged[key] = struct{}{}
		if _, logged := monitor.proUltra[key]; !logged {
			lines = append(lines, logLine{level: "WARN", message: fmt.Sprintf(
				"PRO_POOL=true 时 Pro 号池能服务只限 Ultra 的模型 | 模型=%s | 它不在 %s 里，Pro 账户的目录也没有标注不能用，/pro 会选 Pro 账户服务它并失败；"+
					"Pro 不能用这类模型只靠 %s，请把它加回去", model, aistudio.TierRuleUltraOnlyKey, aistudio.TierRuleUltraOnlyKey)})
		}
	}
	// 只记住这次仍在的：消失后再出现时重新写
	monitor.hints, monitor.unmatched, monitor.proUltra = hints, unmatched, proUltraLogged
	monitor.mu.Unlock()
	for _, line := range lines {
		service.requests.log("service", line.level, line.message)
	}
}

// proServableUltraModels 返回 PRO_POOL=true 时 Pro 号池能服务的只限 Ultra 的模型：ULTRA_ONLY_MODELS 的默认列表与 marked（某个目录明确标注
// 只有 Ultra 能用的模型）里，Pro 号池有启用账户能经启用通道调用的，按名称排序去重；PRO_POOL=false 时为空
func (service *trackedService) proServableUltraModels(marked []string) []string {
	if !service.pool.ProPool() {
		return nil
	}
	seen := make(map[string]struct{})
	var servable []string
	for _, model := range append(config.Default().UltraOnlyModels, marked...) {
		key := strings.ToLower(model)
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		if service.pool.ModelServableIn(aistudio.PoolScopePro, model) {
			servable = append(servable, model)
		}
	}
	slices.Sort(servable)
	return servable
}

// tierRuleHintLine 返回目录明确标注被排除的权益可用时的运行日志（WARN）
func tierRuleHintLine(hint aistudio.TierRuleHint) string {
	return fmt.Sprintf("权益规则比账户目录更严 | 配置项=%s | 模型=%s | 权益=%s | 账户数=%d | 目录标注了 %s 可用 | 确认 %s 账户能用后可从 %s 移除",
		hint.Key, hint.Model, hint.Tier, hint.Accounts, hint.Tier, hint.Tier, hint.Key)
}

// tierRuleUnmarkedLine 返回同一配置项、同一模型上目录没有标注权益限制的各权益合并成的运行日志（INFO）
func tierRuleUnmarkedLine(hints []aistudio.TierRuleHint) string {
	tiers := make([]string, 0, len(hints))
	for _, hint := range hints {
		tiers = append(tiers, fmt.Sprintf("%s %d 个账户", hint.Tier, hint.Accounts))
	}
	return fmt.Sprintf("权益规则在起作用 | 配置项=%s | 模型=%s | 目录没有标注权益限制=%s | 这些账户只靠规则不被选中，目录没有标注不代表它们能用",
		hints[0].Key, hints[0].Model, strings.Join(tiers, "、"))
}

// tierRuleHintsDTO 返回管理页设置项旁的规则提示
func tierRuleHintsDTO(report aistudio.TierRuleReport) []api.TierRuleHint {
	if len(report.Hints) == 0 {
		return nil
	}
	hints := make([]api.TierRuleHint, 0, len(report.Hints))
	for _, hint := range report.Hints {
		hints = append(hints, api.TierRuleHint{
			Key: hint.Key, Model: hint.Model, Tier: hint.Tier.String(), Marked: hint.Marked, Accounts: hint.Accounts,
		})
	}
	return hints
}

// tierRulesSummary 为运行日志里的权益规则：两份列表分别写出，空列表写“无”
func tierRulesSummary(ultraOnly []string, paidOnly []string) string {
	list := func(models []string) string {
		if len(models) == 0 {
			return "无"
		}
		return strings.Join(models, ",")
	}
	return "仅 Ultra " + list(ultraOnly) + "；仅付费 " + list(paidOnly)
}
