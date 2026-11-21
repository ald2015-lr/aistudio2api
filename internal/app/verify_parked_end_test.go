package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 需要验证账号的冷却（整户暂停、单个模型需要验证）在请求结束处的对外结果：改用其他模型、额度失败之后、发出前被拦下

// markModelVerify 在这些账户的 model 上写单个模型需要验证的冷却（30 天）
func markModelVerify(t *testing.T, pool *aistudio.AccountPool, model string, accounts ...string) {
	t.Helper()
	now := time.Now()
	for _, accountID := range accounts {
		if written, write, err := pool.MarkModelVerifyCooldownIfGeneration(accountID, model, pool.ModelAccessGeneration(accountID), now,
			now.Add(verifyRequiredPause)); err != nil || written != 2 {
			t.Fatalf("写入 %s 的单个模型需要验证: written=%d write=%v err=%v", accountID, written, write, err)
		}
	}
}

// verifyParkedProtocols 为三种协议经 /ultra 请求源模型的路径与正文
var verifyParkedProtocols = []struct{ name, path, body string }{
	{name: "OpenAI", path: "/ultra/v1/chat/completions", body: `{"model":"` + fallbackSource + `","messages":[{"role":"user","content":"hi"}]}`},
	{name: "Anthropic", path: "/ultra/v1/messages", body: `{"model":"` + fallbackSource + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
	{name: "Gemini", path: "/ultra/v1beta/models/" + fallbackSource + ":generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
}

// TestModelFallbackVerifyParkedNoRetryAfter 源模型的候选全部只被需要验证账号挡住（Parked）、按 MODEL_FALLBACKS 改用其他模型也没有成功：
// 目标同样 Parked、目标被跳过（不在目录里）时三种协议都是 503、不带 Retry-After、消息里没有 “Please try again in”，日志不写 720h 的最早恢复；
// 目标只是会自行恢复的冷却（3 小时）时照常取它的恢复时间（3 小时后改用过去就能用上）。原先按 Parked 错误的 Until（30 天兜底上限）取最早恢复，
// 最终错误带 Retry-After≈2592000 与 “Please try again in 2592001s”
func TestModelFallbackVerifyParkedNoRetryAfter(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, service *trackedService, pool *aistudio.AccountPool, accounts []string)
		// retry 为期望的 Retry-After（0 为不带）
		retry time.Duration
	}{
		{name: "目标同样只被需要验证挡住", setup: func(t *testing.T, _ *trackedService, pool *aistudio.AccountPool, accounts []string) {
			markModelVerify(t, pool, fallbackTarget, accounts...)
		}},
		{name: "目标被跳过", setup: func(t *testing.T, service *trackedService, _ *aistudio.AccountPool, _ []string) {
			service.setModelFallbacks(fallbackSource+"=missing-model", config.ModelFallbackScopeUltra)
		}},
		{name: "目标会自行恢复", setup: func(t *testing.T, _ *trackedService, pool *aistudio.AccountPool, accounts []string) {
			coolAll(t, pool, fallbackTarget, 3*time.Hour, accounts...)
		}, retry: 3 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			markModelVerify(t, pool, fallbackSource, accounts...)
			test.setup(t, service, pool, accounts)
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
			var cooling *aistudio.AllCoolingError
			if !errors.As(result.err, &cooling) || !cooling.Parked || cooling.ModelID != fallbackSource {
				t.Fatalf("err=%v，期望按源模型 Parked 的全部冷却返回", result.err)
			}
			var provider interface{ RetryAt() time.Time }
			if !errors.As(result.err, &provider) {
				t.Fatal("最终错误应能取建议重试时间")
			}
			if retryAt := provider.RetryAt(); test.retry == 0 && !retryAt.IsZero() ||
				test.retry > 0 && (time.Until(retryAt) < test.retry-time.Minute || time.Until(retryAt) > test.retry+time.Minute) {
				t.Fatalf("建议重试 = %v（%s 后），期望 %s", retryAt, time.Until(retryAt).Round(time.Hour), test.retry)
			}
			// 改用时那一行写明源模型的候选都在等人工验证账号（目标被跳过时没有改用、没有这一行）；结束行的最早恢复不计源模型
			if switched := test.name != "目标被跳过"; switched != progressLoggedAll(service, "模型 "+fallbackSource+" 额度用完（"+verifyParkedEndReason+"），按 MODEL_FALLBACKS 改用 "+fallbackTarget) {
				t.Fatalf("改用日志不符（期望改用=%t）: %v", switched, serviceLogMessages(service.requests))
			}
			if test.retry == 0 && !progressLoggedAll(service, "按 MODEL_FALLBACKS 改用其他模型也没有成功，按源模型的错误返回", "| 最早恢复=-") {
				t.Fatalf("结束日志的最早恢复应为 -: %v", serviceLogMessages(service.requests))
			}
			if progressLoggedAll(service, "720h") || progressLoggedAll(service, "719h") {
				t.Fatalf("日志不应写 30 天兜底上限的最早恢复: %v", serviceLogMessages(service.requests))
			}
			for _, protocol := range verifyParkedProtocols {
				recorder, _ := postFallback(t, service, protocol.path, protocol.body)
				body := recorder.Body.String()
				header := recorder.Header().Get("Retry-After")
				if recorder.Code != http.StatusServiceUnavailable || strings.Contains(body, "2592") {
					t.Fatalf("%s：状态 %d Retry-After=%q 正文=%s，期望 503", protocol.name, recorder.Code, header, body)
				}
				if test.retry == 0 {
					if header != "" || strings.Contains(body, "Please try again in") {
						t.Fatalf("%s：Retry-After=%q 正文=%s，期望不带重试时间", protocol.name, header, body)
					}
					continue
				}
				seconds, err := strconv.Atoi(header)
				if want := int(test.retry.Seconds()); err != nil || seconds < want-60 || seconds > want+5 ||
					!strings.Contains(body, fmt.Sprintf("Please try again in %ds", seconds)) {
					t.Fatalf("%s：Retry-After=%q 正文=%s，期望约 %d 秒", protocol.name, header, body, want)
				}
			}
		})
	}
}

// TestPrimaryAllCoolingParkedMessage Ultra 主用模型选号就遇到候选全部只被需要验证挡住：日志写“候选都在等人工验证账号……立即返回 503”，
// 不写“立即返回 429 | 最早恢复=720h”（对外是 503）；交给 MODEL_FALLBACKS 时结尾照常；普通的全部冷却不变
func TestPrimaryAllCoolingParkedMessage(t *testing.T) {
	parked := &aistudio.AllCoolingError{ModelID: fallbackSource, Until: time.Now().Add(verifyRequiredPause), Parked: true}
	if got := primaryAllCoolingMessage(parked, false); got != "Ultra 主用模型候选都在等人工验证账号（需要验证账号的暂停，不会自行恢复），立即返回 503" {
		t.Fatalf("Parked 的日志 = %q", got)
	}
	if got := primaryAllCoolingMessage(parked, true); !strings.HasSuffix(got, "，"+modelFallbackEnding) || strings.Contains(got, "最早恢复") {
		t.Fatalf("交给 MODEL_FALLBACKS 时 = %q", got)
	}
	cooling := &aistudio.AllCoolingError{ModelID: fallbackSource, Until: time.Now().Add(42 * time.Second)}
	if got := primaryAllCoolingMessage(cooling, false); !strings.HasPrefix(got, "Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=") {
		t.Fatalf("普通的全部冷却 = %q", got)
	}
}

// TestQuotaThenVerifyParkedReturns429 本请求先在会自行恢复的账号上遇到每日限额 429（冷却 2 小时、被排除），换号时其余候选全部只被
// 需要验证挡住：与其余候选全部冷却一样按额度不足的 429 结束，建议重试为那个额度冷却的恢复时间（同一时刻的新请求候选里有它，同样得到 429
// 与它的恢复时间）。原先原样上抛 Parked 的错误，这次请求得到 503、不带 Retry-After。对照：其余候选是阶梯 403 冷却时同样 429
func TestQuotaThenVerifyParkedReturns429(t *testing.T) {
	for _, kind := range []string{"需要验证", "阶梯 403"} {
		t.Run(kind, func(t *testing.T) {
			upstream := &quotaOnceUpstream{failed: forbiddenGood}
			service, pool, _ := verifyModelService(t, upstream)
			// preverifyRuntimeModelAccess 把 good 的检查时间写到了一小时后（测试替身）：额度冷却要能写入
			if err := pool.ResetModelAccess(forbiddenGood); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if kind == "需要验证" {
				markModelVerify(t, pool, testRuntimeModel, forbiddenBad)
			} else {
				for _, scope := range []string{testRuntimeModel, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, testRuntimeModel)} {
					if err := pool.MarkCooldownIfGeneration(forbiddenBad, scope, pool.ModelAccessGeneration(forbiddenBad), now,
						now.Add(6*time.Hour), forbiddenModelReasonPrefix+"测试"); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err, _ := runAccountRequest(t, service, "req-quota-parked", "")
			var quota *aistudio.PrimaryQuotaError
			if !errors.As(err, &quota) || quota.HTTPStatus() != http.StatusTooManyRequests || quota.Reason != primaryQuotaReasonCooling {
				t.Fatalf("err=%v 调用=%v，期望额度不足的 429（其余候选全部冷却）", err, upstream.called())
			}
			// good 先在 Playground、再在 Build 上（同账号换通道）遇到额度 429；bad 一次也没有发出
			if calls := upstream.called(); len(calls) == 0 || slices.Contains(calls, forbiddenBad) {
				t.Fatalf("只应在 good 上发出，实际 %v", calls)
			}
			goodUntil, _, ok := modelCooldownUntil(t, pool, forbiddenGood, testRuntimeModel)
			if !ok {
				t.Fatal("good 应写入额度冷却")
			}
			// 需要验证：只取 good 的额度冷却（不是 30 天、也不是 5 秒）；阶梯 403：bad 6 小时后恢复，早于 good 的次日零点
			want := goodUntil
			if kind != "需要验证" {
				want = now.Add(6 * time.Hour)
			}
			if retryAt := quota.RetryAt(); retryAt.Sub(want).Abs() > time.Second {
				t.Fatalf("建议重试 = %v，期望 %v", retryAt, want)
			}
		})
	}
}

// verifyBlockedUpstream 每次生成都按租约账号在发出前被单个模型需要验证的冷却拦下返回（AccountCoolingDownError，发出前的检查不知道号池）
type verifyBlockedUpstream struct{ fallbackUpstream }

func (upstream *verifyBlockedUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	return nil, &aistudio.AccountCoolingDownError{
		AccountID: request.AccountID, ModelID: request.Model, Until: time.Now().Add(verifyRequiredPause), Reason: aistudio.ModelVerifyRequiredReason,
	}
}

// TestVerifyBlockedCarriesPool 请求以“租约账号发出前被需要验证账号的冷却拦下”结束时带上请求的号池：/ultra 的对外说明与选号就判成 Parked
// 的全部冷却相同（officialUltraUnavailableMessage 的说法），503、不带 Retry-After。按 MODEL_FALLBACKS 改用、目标同样被拦下时同样如此：
// 拦下它的冷却不计最早恢复（原先取它的 Until，即 30 天后）
func TestVerifyBlockedCarriesPool(t *testing.T) {
	for _, scope := range []string{config.ModelFallbackScopeOff, config.ModelFallbackScopeUltra} {
		t.Run("MODEL_FALLBACK_SCOPE="+scope, func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &verifyBlockedUpstream{}
			service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, scope, accounts...)
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
			var blocked *aistudio.AccountCoolingDownError
			if !errors.As(result.err, &blocked) || !blocked.VerifyParked() || blocked.Pool != aistudio.PoolScopeUltra {
				t.Fatalf("err=%v，期望带 Ultra 号池的发出前被拦下", result.err)
			}
			var provider interface{ RetryAt() time.Time }
			if !errors.As(result.err, &provider) || !provider.RetryAt().IsZero() {
				t.Fatalf("最终错误不应带建议重试时间: %v", result.err)
			}
			if scope == config.ModelFallbackScopeUltra && !progressLoggedAll(service,
				"模型 "+fallbackSource+" 额度用完（"+verifyBlockedEndReason+"），按 MODEL_FALLBACKS 改用 "+fallbackTarget) {
				t.Fatalf("改用日志应写明发出前被需要验证账号的暂停拦下: %v", serviceLogMessages(service.requests))
			}
			for _, protocol := range verifyParkedProtocols {
				recorder, _ := postFallback(t, service, protocol.path, protocol.body)
				body := recorder.Body.String()
				if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "" ||
					!strings.Contains(body, "Ultra pool") || strings.Contains(body, "Please try again in") {
					t.Fatalf("%s：状态 %d Retry-After=%q 正文=%s，期望 503 与 Ultra 号池的说法", protocol.name, recorder.Code,
						recorder.Header().Get("Retry-After"), body)
				}
			}
		})
	}
}
