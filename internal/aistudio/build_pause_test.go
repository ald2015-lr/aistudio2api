package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	buildPauseStreamMessage = "PROXY_GENERATION_DISABLED: streamGenerateContent is not supported"
	buildPauseUnaryMessage  = "PROXY_GENERATION_DISABLED: generateContent is not supported"
	buildPauseSharedModel   = "pause-shared"
	buildPauseOnlyModel     = "pause-build-only"
	buildPauseA             = "pause-a@example.com"
	buildPauseB             = "pause-b@example.com"
)

// TestBuildGenerationDisabledClassification 只有 Build 代理调用（ProxyStreamedCall、ProxyUnaryCall）返回、文案以 PROXY_GENERATION_DISABLED
// 开头的错误归为 ErrBuildGenerationDisabled：流尾状态与直接返回的错误响应两种来源都识别，%w 与 errors.Join 包装后仍识别、原文保留；
// 普通 400（包括回显了客户端字段名 proxy_generation_disabled 的参数错误）、额度 429、403、组织停用与 Playground 的同名文案都不算
func TestBuildGenerationDisabledClassification(t *testing.T) {
	stream := &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: buildPauseStreamMessage}
	unary := &RPCError{Method: buildProxyUnaryMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: buildPauseUnaryMessage}
	for _, err := range []error{
		stream, unary,
		fmt.Errorf("换号重试: %w", stream),
		errors.Join(errors.New("删除临时 Drive 副本失败"), unary),
		fmt.Errorf("外层: %w", errors.Join(stream, io.EOF)),
		&RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: "  " + buildPauseStreamMessage + "\n"},
		&RPCError{Method: buildProxyUnaryMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: "PROXY_GENERATION_DISABLED"},
		// 原文带上游内部标记（API 层对外返回前会去掉，客户端看到的同样是 “PROXY_GENERATION_DISABLED: …”）
		&RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: "[original: beyond::dependency::FAILED_PRECONDITION] " + buildPauseStreamMessage},
		&RPCError{Method: buildProxyUnaryMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: "generic::failed_precondition: " + buildPauseUnaryMessage},
		&RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: "beyond::invalid_argument: " + buildPauseStreamMessage + " (qos=CRITICAL_PLUS)"},
	} {
		if !errors.Is(err, ErrBuildGenerationDisabled) || !BuildGenerationDisabledFailure(err) {
			t.Fatalf("应识别为官网停用 Build 生成: %v", err)
		}
		var rpcError *RPCError
		if !errors.As(err, &rpcError) || !strings.Contains(rpcError.Message, "PROXY_GENERATION_DISABLED") {
			t.Fatalf("错误链应保留上游原文: %v", err)
		}
	}

	// 流尾状态：响应列表为空，其后是 [3, "PROXY_GENERATION_DISABLED: …"]
	trailer := `[[],[3,"` + buildPauseStreamMessage + `"]]`
	err := DecodeBuildStream(strings.NewReader(trailer), &BuildStreamDecoder{}, func(Event) error { return nil })
	if !BuildGenerationDisabledFailure(err) {
		t.Fatalf("流尾状态 err = %v，期望识别为官网停用 Build 生成", err)
	}
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest || rpcError.Method != buildProxyStreamedMethod {
		t.Fatalf("流尾状态应解码为 ProxyStreamedCall 的 400: %#v", err)
	}
	// 单次代理调用直接返回的 HTTP 400 错误响应（需要订阅权益的模型）
	_, err = validateRPCResponse(buildProxyUnaryMethod, &RPCResponse{
		StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`[3,"` + buildPauseUnaryMessage + `"]`)),
	})
	if !BuildGenerationDisabledFailure(err) {
		t.Fatalf("单次代理调用的错误响应 err = %v，期望识别为官网停用 Build 生成", err)
	}
	// 流式代理调用直接返回的错误响应（流式封装 [null, [code, message]]）
	_, err = validateRPCResponse(buildProxyStreamedMethod, &RPCResponse{
		StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`[null,[3,"` + buildPauseStreamMessage + `"]]`)),
	})
	if !BuildGenerationDisabledFailure(err) {
		t.Fatalf("流式代理调用的错误响应 err = %v，期望识别为官网停用 Build 生成", err)
	}

	for name, err := range map[string]error{
		"普通 400":          &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: "Request contains an invalid argument."},
		"额度 429":          &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"},
		"403":             &RPCError{Method: buildProxyUnaryMethod, StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"},
		"组织停用":            &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled."},
		"Playground 同名文案": &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: buildPauseStreamMessage},
		"只在中文说明里":         errors.New("ProxyStreamedCall 失败：PROXY_GENERATION_DISABLED"),
		// 参数错误回显客户端请求里的字段名（工具参数的属性名），出现在文案中间，大小写不定
		"回显的小写字段名": &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: `Invalid JSON payload received. Unknown name "proxy_generation_disabled" at 'tools[0].function_declarations[0].parameters': Cannot find field.`},
		"回显的大写字段名": &RPCError{Method: buildProxyUnaryMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: "* GenerateContentRequest.tools[0].function_declarations[0].parameters.properties[PROXY_GENERATION_DISABLED: x].type: must be specified"},
		"开头是小写":    &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: "proxy_generation_disabled: streamGenerateContent is not supported"},
		"开头是更长的名字": &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: "PROXY_GENERATION_DISABLED_FIELD is not a valid enum value"},
		// 去掉内部标记后仍只认开头：标记之后是参数错误的原文、字段名在中间时不算
		"标记后回显的字段名": &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3,
			Message: "[original: beyond::dependency::INVALID_ARGUMENT] Invalid value at 'tools[0]' (PROXY_GENERATION_DISABLED): unknown field"},
		"标记后的 Playground 同名文案": &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3,
			Message: "generic::failed_precondition: " + buildPauseStreamMessage},
		"Build 不可用的结论（已处理）": &BuildGenerationUnavailableError{Reason: BuildUnavailableModel, Model: "m", Cause: stream},
		"nil": nil,
	} {
		if BuildGenerationDisabledFailure(err) {
			t.Fatalf("%s 不应识别为 Build 代理返回的 PROXY_GENERATION_DISABLED: %v", name, err)
		}
	}
	if errors.Is(stream, ErrOrgDisabled) || errors.Is(&RPCError{
		Method: buildProxyStreamedMethod, StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled.",
	}, ErrBuildGenerationDisabled) {
		t.Fatal("组织停用与官网停用 Build 生成不能互相混淆")
	}
	if !errors.Is(&RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled."}, ErrOrgDisabled) {
		t.Fatal("组织停用的识别应保持不变")
	}
}

// TestBuildGenerationUnavailableError 给客户端的说明：输入超过 Playground 上限为 400 并写出上限，其余为 503；
// 对外文案不含 PROXY_GENERATION_DISABLED 原文，内部文案与错误链保留原因
func TestBuildGenerationUnavailableError(t *testing.T) {
	capErr := &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "The input token count exceeds the maximum number of tokens allowed 131072"}
	if got := InputTokenLimitValue(capErr); got != 131072 {
		t.Fatalf("上限 = %d，期望 131072", got)
	}
	if got := InputTokenLimitValue(errors.New("The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).")); got != 1048576 {
		t.Fatalf("带括号写法的上限 = %d，期望 1048576", got)
	}
	if InputTokenLimitValue(errors.New("其他错误")) != 0 || InputTokenLimitValue(nil) != 0 {
		t.Fatal("没有上限时应为 0")
	}
	disabled := &RPCError{Method: buildProxyStreamedMethod, StatusCode: http.StatusBadRequest, Code: 3, Message: buildPauseStreamMessage}
	input := &BuildGenerationUnavailableError{Reason: BuildUnavailableInputLimit, Model: "m", InputLimit: InputTokenLimitValue(capErr), Cause: errors.Join(capErr, disabled)}
	if input.HTTPStatus() != http.StatusBadRequest || !strings.Contains(input.PublicMessage(), "131072") ||
		!strings.Contains(input.PublicMessage(), "Playground") || !strings.Contains(input.PublicMessage(), "Build channel") {
		t.Fatalf("输入超限：状态 %d、文案 %q", input.HTTPStatus(), input.PublicMessage())
	}
	model := &BuildGenerationUnavailableError{Reason: BuildUnavailableModel, Model: "gemini-4-argon"}
	none := &BuildGenerationUnavailableError{Reason: BuildUnavailableNoPlayground, Model: "m"}
	if model.HTTPStatus() != http.StatusServiceUnavailable || none.HTTPStatus() != http.StatusServiceUnavailable ||
		!strings.Contains(model.PublicMessage(), "gemini-4-argon") || !strings.Contains(model.PublicMessage(), "only available through") {
		t.Fatalf("只能经 Build 调用：状态 %d、文案 %q", model.HTTPStatus(), model.PublicMessage())
	}
	for _, err := range []*BuildGenerationUnavailableError{input, model, none} {
		if strings.Contains(err.PublicMessage(), "PROXY_GENERATION_DISABLED") {
			t.Fatalf("对外文案不应含原文: %q", err.PublicMessage())
		}
		if !errors.Is(err, ErrBuildGenerationDisabled) || !strings.Contains(err.Error(), "PROXY_GENERATION_DISABLED") {
			t.Fatalf("内部错误应满足 ErrBuildGenerationDisabled 并写明原因: %v", err)
		}
	}
	var rpcError *RPCError
	if !errors.As(input, &rpcError) || !InputTokenLimitError(input) {
		t.Fatal("内部原因应保留在错误链里")
	}
}

// buildPauseTestPool 建立两个账号：都能经 Playground 与 Build 调用 pause-shared，只有 Build 目录有 pause-build-only；
// buildOnlyAccounts 为另加的账号，Playground 目录没有 pause-shared（只能经 Build 调用它）
func buildPauseTestPool(t *testing.T, channels []Channel, buildOnlyAccounts ...string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	shared := Model{ID: buildPauseSharedModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	only := Model{ID: buildPauseOnlyModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	other := Model{ID: "pause-other", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, id := range append([]string{buildPauseA, buildPauseB}, buildOnlyAccounts...) {
		account, publish, err := store.Create(DefaultAccountConfig(id), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		catalog := []Model{shared}
		if slices.Contains(buildOnlyAccounts, id) {
			catalog = []Model{other}
		}
		if err := pool.SetCatalog(id, BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(id, []Model{shared, only}); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetUpstreamChannels(channels)
	pool.SetRoutingStrategy("fill-first")
	return pool
}

// acquireChannel 取得租约并返回“账号/通道”，随即释放
func acquireChannel(t *testing.T, pool *AccountPool, selection AccountSelection) (string, error) {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), selection)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	return lease.Account().ID + "/" + string(lease.Channel()), nil
}

// TestBuildPauseSelection 账户池的 Build 暂停：按账号暂停时该账号不经 Build 接生成请求（Playground 照常、其他账号的 Build 照常），
// 全局暂停时 Build 视为没有启用，试探（BuildProbe）不受影响；只能经 Build 处理的选择给出 Build 不可用的错误；解除后与原来一致
func TestBuildPauseSelection(t *testing.T) {
	pool := buildPauseTestPool(t, []Channel{ChannelBuild, ChannelPlayground})
	shared := AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent"}
	only := AccountSelection{ModelID: buildPauseOnlyModel, Method: "generateContent"}
	expect := func(step string, selection AccountSelection, want string) {
		t.Helper()
		got, err := acquireChannel(t, pool, selection)
		if err != nil || got != want {
			t.Fatalf("%s：选号 = %q err=%v，期望 %q", step, got, err, want)
		}
	}
	expect("没有暂停", shared, buildPauseA+"/build")

	if !pool.SetAccountBuildPaused(buildPauseA, true) || pool.SetAccountBuildPaused(buildPauseA, true) {
		t.Fatal("按账号暂停只在状态变化时返回 true")
	}
	expect("A 暂停：A 的 Playground", shared, buildPauseA+"/playground")
	expect("A 暂停：Build 独有模型落到 B 的 Build", only, buildPauseB+"/build")
	if !pool.AccountChannelAvailable(buildPauseA, shared) {
		t.Fatal("A 的 Playground 仍可用")
	}
	if pool.AccountChannelAvailable(buildPauseA, only) {
		t.Fatal("A 只剩 Build 能接 Build 独有模型，暂停后不可用")
	}

	if !pool.SetBuildGenerationPaused(true) || !pool.BuildGenerationPaused() {
		t.Fatal("全局暂停应生效")
	}
	pool.SetAccountBuildPaused(buildPauseA, false)
	expect("全局暂停：只走 Playground", shared, buildPauseA+"/playground")
	expect("全局暂停：试探只走 Build", AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", BuildOnly: true, BuildProbe: true}, buildPauseA+"/build")
	_, err := acquireChannel(t, pool, only)
	var unavailable *BuildGenerationUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableModel || unavailable.Model != buildPauseOnlyModel {
		t.Fatalf("全局暂停时 Build 独有模型 err = %v，期望只能经 Build 调用的错误", err)
	}
	_, err = acquireChannel(t, pool, AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", BuildOnly: true})
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableInputLimit {
		t.Fatalf("全局暂停时只走 Build 的选择 err = %v，期望输入超限的 Build 不可用错误", err)
	}
	// 模型列表只看目录：暂停期间 Build 独有模型照常列出 build 通道
	listed := pool.CatalogModels([]Model{{ID: buildPauseOnlyModel, Methods: []string{"generateContent"}}})
	if len(listed) != 1 || !slices.Equal(listed[0].Channels, []string{"build"}) {
		t.Fatalf("暂停期间模型列表 = %+v，期望 Build 独有模型照常列出", listed)
	}

	pool.SetAccountBuildPaused(buildPauseB, true)
	if cleared := pool.ClearBuildPauses(); cleared != 1 || pool.BuildGenerationPaused() {
		t.Fatalf("解除暂停 = %d 个账号、全局 %t", cleared, pool.BuildGenerationPaused())
	}
	expect("解除后与原来一致", shared, buildPauseA+"/build")
	expect("解除后 Build 独有模型", only, buildPauseA+"/build")
}

// TestBuildPauseOnlyBuildChannel 只启用 Build 时全局暂停让生成请求没有候选，错误说明 Playground 接不了；
// 已经取得的 Build 租约在暂停后仍通过目录校验（正在发送的请求不因暂停失败）
func TestBuildPauseOnlyBuildChannel(t *testing.T) {
	pool := buildPauseTestPool(t, []Channel{ChannelBuild})
	selection := AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent"}
	lease, err := pool.AcquireFor(context.Background(), selection)
	if err != nil || lease.Channel() != ChannelBuild {
		t.Fatalf("暂停前选号 err=%v", err)
	}
	defer lease.Release()
	pool.SetBuildGenerationPaused(true)
	pool.SetAccountBuildPaused(lease.Account().ID, true)
	if err := validateLeaseSelection(lease, selection); err != nil {
		t.Fatalf("暂停前取得的 Build 租约应仍通过校验: %v", err)
	}
	// 热更新去掉 Build 通道时，在途的 Build 租约同样照常发送；恢复后再看暂停的选号
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	if err := validateLeaseSelection(lease, selection); err != nil {
		t.Fatalf("去掉 Build 通道后在途的 Build 租约应仍通过校验: %v", err)
	}
	pool.SetUpstreamChannels([]Channel{ChannelBuild})
	_, err = acquireChannel(t, pool, AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", AccountID: buildPauseB})
	var unavailable *BuildGenerationUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableNoPlayground {
		t.Fatalf("只启用 Build 且全局暂停 err = %v，期望 Playground 接不了的错误", err)
	}
	if unavailable.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", unavailable.HTTPStatus())
	}
}

const buildPauseC = "pause-c@example.com"

// TestBuildPauseNoCandidateError 没有候选时只在 Build 暂停确实是原因时才给出 Build 不可用的错误：Playground 能接该模型的账号只是被换号排除
// （额度用完等）时仍为 ErrNoEligibleAccount，与没有暂停时相同（换号重试据此按额度不足结束、保留上一次的错误）；只能经 Build 调用的模型
// 给出 Build 不可用的错误，同时满足 ErrNoEligibleAccount；降级判定的 Playground 优先不经 Build，暂停不是原因
func TestBuildPauseNoCandidateError(t *testing.T) {
	pool := buildPauseTestPool(t, []Channel{ChannelPlayground, ChannelBuild}, buildPauseC)
	shared := AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", ExcludedAccountIDs: []string{buildPauseA, buildPauseB}}
	var unavailable *BuildGenerationUnavailableError
	for _, paused := range []bool{false, true} {
		pool.SetBuildGenerationPaused(paused)
		err := pool.NoEligibleError(shared)
		if !errors.Is(err, ErrNoEligibleAccount) || errors.As(err, &unavailable) {
			t.Fatalf("暂停=%t：排除了 Playground 能接的账号后 err = %v，期望 ErrNoEligibleAccount", paused, err)
		}
	}
	// 全局暂停中排除 A、B 后选号：C 只有暂停中的 Build，A、B 的 Playground 能接该模型，按没有可换的账号返回
	if _, err := acquireChannel(t, pool, shared); !errors.Is(err, ErrNoEligibleAccount) || errors.As(err, &unavailable) {
		t.Fatalf("全局暂停中排除 A、B 后选号 err = %v，期望 ErrNoEligibleAccount", err)
	}
	// 只能经 Build 调用的模型：Build 不可用的错误，同时满足 ErrNoEligibleAccount
	_, err := acquireChannel(t, pool, AccountSelection{ModelID: buildPauseOnlyModel, Method: "generateContent", ExcludedAccountIDs: []string{buildPauseA}})
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableModel || !unavailable.NoCandidate || !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("Build 独有模型 err = %v，期望只能经 Build 调用、同时满足 ErrNoEligibleAccount", err)
	}
	// 固定到只有 Build 能接的账号：Playground 目录列出了该模型，只是这个账号经 Playground 接不了
	_, err = acquireChannel(t, pool, AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", AccountID: buildPauseC})
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableNoPlayground {
		t.Fatalf("固定到只有 Build 的账号 err = %v，期望 Playground 接不了", err)
	}
	// 限定账号集合（更高权益账号的再试）只有 C：范围外 A、B 的 Playground 不算，没有候选是 Build 暂停造成的
	err = pool.NoEligibleError(AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", AllowedAccountIDs: []string{buildPauseC}})
	if !errors.As(err, &unavailable) || unavailable.Reason != BuildUnavailableNoPlayground || !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("限定到只有 Build 的账号 err = %v，期望 Playground 接不了", err)
	}
	// 限定集合含 A：A 的 Playground 能接，照旧 ErrNoEligibleAccount
	err = pool.NoEligibleError(AccountSelection{ModelID: buildPauseSharedModel, Method: "generateContent", AllowedAccountIDs: []string{buildPauseA, buildPauseC}})
	if !errors.Is(err, ErrNoEligibleAccount) || errors.As(err, &unavailable) {
		t.Fatalf("限定集合含 Playground 能接的账号 err = %v，期望 ErrNoEligibleAccount", err)
	}
	// 按账号暂停：降级判定的 Playground 优先本来就不经 Build，没有候选不是暂停造成的
	pool.SetBuildGenerationPaused(false)
	pool.SetAccountBuildPaused(buildPauseA, true)
	err = pool.NoEligibleError(AccountSelection{ModelID: buildPauseOnlyModel, Method: "generateContent", PlaygroundFirst: true})
	if !errors.Is(err, ErrNoEligibleAccount) || errors.As(err, &unavailable) {
		t.Fatalf("Playground 优先 err = %v，期望 ErrNoEligibleAccount（随后退回其他通道）", err)
	}
	// 其他账号的 Build 照常：Build 独有模型落到 B
	if got, err := acquireChannel(t, pool, AccountSelection{ModelID: buildPauseOnlyModel, Method: "generateContent"}); err != nil || got != buildPauseB+"/build" {
		t.Fatalf("按账号暂停时 Build 独有模型选号 = %q err = %v", got, err)
	}
}

// TestBuildPauseLongCooling Build 生成暂停（全局或该账号）期间，Playground 长时间冷却的账号与只启用 Playground 时一样算死槽位、
// 状态为冷却；解除暂停后恢复原判断。只启用 Build 时暂停不算（没有别的通道，否则全部账号都成死槽位）
func TestBuildPauseLongCooling(t *testing.T) {
	pool := buildPauseTestPool(t, []Channel{ChannelPlayground, ChannelBuild})
	until := time.Now().Add(10 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(buildPauseA, buildPauseSharedModel, pool.ModelAccessGeneration(buildPauseA), time.Now(), until, "每日限额: test"); err != nil {
		t.Fatal(err)
	}
	expect := func(step string, want bool) {
		t.Helper()
		long := pool.AccountLongCooling(buildPauseA, buildPauseSharedModel, time.Hour)
		status, _ := pool.StatusOf(buildPauseA)
		dead := pool.WarmWorkerStates([]string{buildPauseA}, buildPauseSharedModel, time.Hour)[buildPauseA].Dead
		_, listed := pool.LongCoolingSet([]string{buildPauseA, buildPauseB}, []string{buildPauseSharedModel}, time.Hour, false)[buildPauseA]
		cooling := status.State == AccountCooldown
		if long != want || dead != want || listed != want || cooling != want {
			t.Fatalf("%s：长时间冷却=%t 死槽位=%t 列入=%t 状态=%s，期望 %t", step, long, dead, listed, status.State, want)
		}
		if _, other := pool.LongCoolingSet([]string{buildPauseB}, []string{buildPauseSharedModel}, time.Hour, false)[buildPauseB]; other {
			t.Fatalf("%s：没有冷却的账号不应算死槽位", step)
		}
		// 状态计数按候选索引里的冷却期限统计（见 accountCoolingUntilLocked），与 StatusOf 同口径
		if counts, _ := pool.PoolStateCounts(); want && counts.Cooldown != 1 || !want && counts.Cooldown != 0 {
			t.Fatalf("%s：状态计数 = %+v", step, counts)
		}
	}
	expect("没有暂停：Build 还能接", false)
	pool.SetBuildGenerationPaused(true)
	expect("全局暂停", true)
	pool.ClearBuildPauses()
	expect("解除全局暂停", false)
	pool.SetAccountBuildPaused(buildPauseA, true)
	expect("该账号暂停", true)
	pool.SetAccountBuildPaused(buildPauseA, false)
	expect("解除该账号暂停", false)

	only := buildPauseTestPool(t, []Channel{ChannelBuild})
	only.SetBuildGenerationPaused(true)
	if only.AccountLongCooling(buildPauseA, buildPauseSharedModel, time.Hour) {
		t.Fatal("只启用 Build 时暂停不应让账号成死槽位")
	}
}

// TestBuildPauseStatusCache 账户多时 Status() 按缓存返回：全局暂停与解除后缓存期内的账户列表也要立刻反映冷却状态的变化
// （暂停期间 Playground 长时间冷却的账户显示冷却），与 StatusOf 一致
func TestBuildPauseStatusCache(t *testing.T) {
	pool, victim := statusCacheTestPool(t)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	if err := pool.SetBuildCatalog(victim, indexTestCatalog()[:1]); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkCooldownIfGeneration(victim, indexTestModel, pool.ModelAccessGeneration(victim), time.Now(), time.Now().Add(10*time.Hour), "每日限额: test"); err != nil {
		t.Fatal(err)
	}
	expect := func(step string, want AccountState) {
		t.Helper()
		direct, _ := pool.StatusOf(victim)
		if listed := listedStatus(t, pool, victim); listed.State != want || direct.State != want {
			t.Fatalf("%s：Status() 列表中的状态 = %s，StatusOf = %s，期望 %s", step, listed.State, direct.State, want)
		}
	}
	expect("没有暂停：Build 还能接", AccountReady)
	pool.SetBuildGenerationPaused(true)
	expect("全局暂停", AccountCooldown)
	pool.ClearBuildPauses()
	expect("解除暂停", AccountReady)
}
