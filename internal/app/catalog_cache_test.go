package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/gofrs/flock"
)

// useTempCatalogCacheDir 把模型目录缓存改到临时目录（调用 newRuntime 的测试用，免得在源码目录下写 runtime/），测试结束后恢复
func useTempCatalogCacheDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	previous := catalogCacheDir
	catalogCacheDir = dir
	t.Cleanup(func() { catalogCacheDir = previous })
	return dir
}

// catalogTestRow 返回 ListModels 的一行：ID、显示名、描述、输入输出上限、generateContent、温度与 topP/topK 默认值、
// 聊天与思考能力、访问模式与付费标记
func catalogTestRow(id string, temperature float64) []any {
	row := make([]any, 83)
	row[0] = "models/" + id
	row[3] = strings.ToUpper(id)
	row[4] = id + " 描述"
	row[5] = 1048576
	row[6] = 65536
	row[7] = []string{"generateContent", "countTokens"}
	row[8] = temperature
	row[9] = 0.95
	row[10] = 64
	row[64] = []int{1, 25}
	row[77] = 2
	row[82] = []int{3}
	return row
}

// catalogTestRaw 返回 ListModels 的响应正文
func catalogTestRaw(t testing.TB, rows ...[]any) string {
	t.Helper()
	if rows == nil {
		rows = [][]any{}
	}
	data, err := json.Marshal([]any{rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// catalogTestData 解析一份目录
func catalogTestData(t testing.TB, ids ...string) aistudio.ModelCatalogData {
	t.Helper()
	rows := make([][]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, catalogTestRow(id, 0.7))
	}
	data, err := aistudio.ParseModelCatalogRaw([]byte(catalogTestRaw(t, rows...)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// catalogRPCTransport 为目录测试的假上游：ListModels 返回 listModels，GetAiStudioBenefitTier 返回 tier
type catalogRPCTransport struct {
	mu         sync.Mutex
	listModels string
	tier       string
	calls      map[string]int
}

func (transport *catalogRPCTransport) Do(_ context.Context, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.calls == nil {
		transport.calls = make(map[string]int)
	}
	transport.calls[rpc.Method]++
	body := "[]"
	switch rpc.Method {
	case "ListModels":
		body = transport.listModels
	case "GetAiStudioBenefitTier":
		if transport.tier != "" {
			body = transport.tier
		}
	}
	return &aistudio.RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {aistudio.JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (transport *catalogRPCTransport) count(method string) int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.calls[method]
}

// catalogTestAccounts 在 root 下创建账户（没有目录）
func catalogTestAccounts(t testing.TB, root string, ids ...string) {
	t.Helper()
	store := aistudio.NewAccountStore(root)
	for _, id := range ids {
		cookies := make([]aistudio.StateCookie, 0, 3)
		for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
			cookies = append(cookies, aistudio.StateCookie{
				Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
			})
		}
		_, publish, err := store.Create(aistudio.DefaultAccountConfig(id), aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// catalogGeneration 为一代运行时里与目录有关的部分：从 root 载入账户（模拟进程重启后的新一代）
type catalogGeneration struct {
	pool      *aistudio.AccountPool
	client    *aistudio.Client
	pooled    *aistudio.PooledService
	transport *catalogRPCTransport
}

func newCatalogGeneration(t testing.TB, root string, transport *catalogRPCTransport) *catalogGeneration {
	t.Helper()
	accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(errors.Join(failures...))
	}
	pool := aistudio.NewAccountPool(accounts, 2)
	t.Cleanup(pool.WaitBackground)
	if transport == nil {
		transport = &catalogRPCTransport{}
	}
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: transport,
		Protected: aistudio.ProtectedTransportFunc(func(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			return nil, errors.New("unused")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return &catalogGeneration{pool: pool, client: client, pooled: pooled, transport: transport}
}

func catalogBlobFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		if catalogBlobName.MatchString(file.Name()) {
			names = append(names, file.Name())
		}
	}
	return names
}

func readCatalogIndex(t *testing.T, dir string) catalogIndex {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index catalogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

// TestCatalogCacheRoundTrip 同步→写盘→新一代载入：模型全部字段、生成默认值（内容键一致）、Build 目录、权益、
// ar/am/en/ez 都带回；认证文件没变过的需要登录记录列为疑似失效；恢复不发目录请求
func TestCatalogCacheRoundTrip(t *testing.T) {
	ctx := context.Background()
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	const accountID = "pro@example.com"
	catalogTestAccounts(t, root, accountID)
	requests := newRequestRegistry(ctx)
	first := newCatalogGeneration(t, root, &catalogRPCTransport{
		listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7), catalogTestRow("beta", 1)), tier: "[1]",
	})
	models, err := first.pooled.RefreshAccountModels(ctx, accountID)
	if err != nil || len(models) != 2 {
		t.Fatalf("首次同步 models=%v err=%v", models, err)
	}
	build := []aistudio.Model{{
		ID: "build-only", Name: "Build", Description: "d", Methods: []string{"generateContent"}, InputTokenLimit: 1, OutputTokenLimit: 2,
		Capabilities: map[string]bool{"chat_model": true}, CapabilityOptions: map[string][]string{"aliases": {"b"}}, AccessModes: []int64{3}, Paid: true,
	}}
	if err := first.pool.SetBuildCatalog(accountID, build); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := first.pooled.CatalogSnapshot(accountID)
	if !ok || snapshot.Tier != aistudio.BenefitTierPro || !snapshot.BuildSynced {
		t.Fatalf("快照 %+v ok=%v", snapshot, ok)
	}
	cache := openCatalogCache(dir, requests)
	syncedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	cache.put(accountID, snapshot, syncedAt)
	since := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	cache.noteEmpty(accountID, 2, since)
	account, err := first.pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	modTime, _ := fileModTime(account.StoragePath)
	cache.noteAuthRequired(accountID, time.Now(), modTime)
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	firstModels, _ := first.client.ModelsForAccount(ctx, accountID)
	firstData, _, _ := first.client.AccountCatalog(accountID)

	second := newCatalogGeneration(t, root, nil)
	restored := openCatalogCache(dir, requests)
	summary := restored.restore(second.pool, second.client, true)
	if summary.FromDisk != 1 || summary.Blobs != 1 || len(summary.Stale) != 0 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	if _, suspect := summary.Suspects[accountID]; !suspect {
		t.Fatal("认证文件没变过的需要登录记录应列为疑似失效")
	}
	data, tier, ok := second.client.AccountCatalog(accountID)
	if !ok || tier != aistudio.BenefitTierPro || data.ContentKey() != firstData.ContentKey() {
		t.Fatal("Client 侧的目录（含默认值）与权益应恢复")
	}
	if !reflect.DeepEqual(data.Models(), firstModels) {
		t.Fatalf("恢复的模型与同步的不同：\n%+v\n%+v", data.Models(), firstModels)
	}
	if status, _ := second.pool.StatusOf(accountID); status.BenefitTier != "Pro" || len(status.Models) != 2 {
		t.Fatalf("账户池状态 %+v", status)
	}
	if restoredBuild, ok := second.pool.BuildCatalogOf(accountID); !ok || !reflect.DeepEqual(restoredBuild, build) {
		t.Fatalf("Build 目录 %+v", restoredBuild)
	}
	if again, ok := second.pooled.CatalogSnapshot(accountID); !ok || again.EmptyCount != 2 || !again.EmptySince.Equal(since) {
		t.Fatalf("连续空结果应带回账户池：%+v", again)
	}
	entry, _ := restored.entryOf(accountID)
	if entry.At != syncedAt.Unix() || entry.Tier != int64(aistudio.BenefitTierPro) || entry.AuthModTime != modTime.UnixNano() || entry.Empties != 2 {
		t.Fatalf("缓存记录 %+v", entry)
	}
	if calls := second.transport.count("ListModels") + second.transport.count("GetAiStudioBenefitTier"); calls != 0 {
		t.Fatalf("恢复不应发请求，实际 %d 次", calls)
	}
	if !hasLogContaining(requests, "模型目录缓存载入 | 账户=1 | 目录=1 | 耗时=", "| 跳过=权益不符 0、文件缺失 0、版本不符 0、已删除账户 0") {
		t.Fatal("缺少“模型目录缓存载入”日志")
	}
}

// TestCatalogCacheDedupBlobs 100 个账户同一目录只写 1 个 blob
func TestCatalogCacheDedupBlobs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	cache := openCatalogCache(dir, nil)
	data := catalogTestData(t, "alpha", "beta")
	for index := range 100 {
		cache.put(fmt.Sprintf("a%03d@example.com", index), aistudio.CatalogSnapshot{Playground: data}, time.Now())
	}
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if blobs := catalogBlobFiles(t, dir); len(blobs) != 1 || blobs[0] != data.ContentKey()+".json" {
		t.Fatalf("blob = %v，期望只有 1 个", blobs)
	}
	if index := readCatalogIndex(t, dir); len(index.Accounts) != 100 || index.Version != catalogCacheVersion {
		t.Fatalf("index 有 %d 个账户", len(index.Accounts))
	}
}

// TestCatalogCacheCorruptIndexRenamed index 损坏时改名保留、按空缓存继续，并写“模型目录缓存损坏”
func TestCatalogCacheCorruptIndexRenamed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"version":1,"accounts":{`), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	cache := openCatalogCache(dir, requests)
	if len(cache.entries) != 0 {
		t.Fatal("损坏的 index 应按空缓存继续")
	}
	renamed, _ := filepath.Glob(filepath.Join(dir, "index.json.corrupt-*"))
	if len(renamed) != 1 {
		t.Fatalf("损坏的 index 应改名保留，实际 %v", renamed)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("原 index 应已改名")
	}
	if !hasLogContaining(requests, "模型目录缓存损坏 | 文件=", "| 已改名=", ".corrupt-", "| 错误=") {
		t.Fatal("缺少“模型目录缓存损坏”日志")
	}
}

// TestCatalogCacheMissingBlobSkipped blob 缺失的账户不恢复（按缺失同步），其他账户照常
func TestCatalogCacheMissingBlobSkipped(t *testing.T) {
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, "a@example.com", "b@example.com")
	cache := openCatalogCache(dir, nil)
	alpha, beta := catalogTestData(t, "alpha"), catalogTestData(t, "beta")
	cache.put("a@example.com", aistudio.CatalogSnapshot{Playground: alpha}, time.Now())
	cache.put("b@example.com", aistudio.CatalogSnapshot{Playground: beta}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cache.blobPath(beta.ContentKey())); err != nil {
		t.Fatal(err)
	}
	generation := newCatalogGeneration(t, root, nil)
	summary := openCatalogCache(dir, nil).restore(generation.pool, generation.client, false)
	if summary.FromDisk != 1 || summary.Missing != 1 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	if status, _ := generation.pool.StatusOf("b@example.com"); len(status.Models) != 0 {
		t.Fatal("blob 缺失的账户不应恢复")
	}
}

// TestCatalogCacheVersionMismatchIgnored index 版本不符时忽略且不删；blob 版本不符时跳过该账户、不删 blob
func TestCatalogCacheVersionMismatchIgnored(t *testing.T) {
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, "a@example.com")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"version":2,"accounts":{"a@example.com":{"t":0,"p":"x"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), future, 0o600); err != nil {
		t.Fatal(err)
	}
	if cache := openCatalogCache(dir, nil); len(cache.entries) != 0 {
		t.Fatal("版本不符的 index 应忽略")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "index.json")); string(data) != string(future) {
		t.Fatal("版本不符的 index 不应删除或改写")
	}
	key := strings.Repeat("ab", 16)
	blob := []byte(`{"version":9,"kind":"playground","raw":[[]]}`)
	if err := os.WriteFile(filepath.Join(dir, "blobs", key+".json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	index, _ := json.Marshal(catalogIndex{Version: catalogCacheVersion, Accounts: map[string]catalogEntry{"a@example.com": {Playground: key, At: 1}}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0o600); err != nil {
		t.Fatal(err)
	}
	generation := newCatalogGeneration(t, root, nil)
	summary := openCatalogCache(dir, nil).restore(generation.pool, generation.client, false)
	if summary.FromDisk != 0 || summary.Version != 1 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", key+".json")); err != nil {
		t.Fatal("版本不符的 blob 不应删除")
	}
}

// TestCatalogCacheForeignIndexNotOverwritten 磁盘上是另一个版本的 index 时写盘不覆盖它（便于回退旧版本），本进程的变化只留在内存
func TestCatalogCacheForeignIndexNotOverwritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"version":2,"accounts":{"a@example.com":{"t":0,"p":"x"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), future, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := openCatalogCache(dir, nil)
	data := catalogTestData(t, "alpha")
	cache.put("b@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatalf("版本不符时写盘应跳过而不是报错: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "index.json")); string(got) != string(future) {
		t.Fatalf("版本不符的 index 被改写为 %s", got)
	}
	if entry, ok := cache.entryOf("b@example.com"); !ok || entry.Playground != data.ContentKey() {
		t.Fatal("本进程的变化应留在内存")
	}
}

// TestCatalogCacheCollectedBlobRewritten 清理删掉的 blob 不再当作在盘上：之后有账户的目录又回到这个键时重新写 blob；
// 另一个进程删掉了本进程确认过的 blob 时，下次 put 发现文件不在同样重写；复用旧 blob 时刷新修改时间
func TestCatalogCacheCollectedBlobRewritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	alpha, beta := catalogTestData(t, "alpha"), catalogTestData(t, "beta")
	cache := openCatalogCache(dir, nil)
	cache.put("a@example.com", aistudio.CatalogSnapshot{Playground: alpha}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	// 账户换到另一份目录，旧 blob 不再被引用且已超过 24 小时：本进程的清理删掉它
	cache.put("a@example.com", aistudio.CatalogSnapshot{Playground: beta}, time.Now())
	old := time.Now().Add(-catalogBlobGCAge - time.Hour)
	if err := os.Chtimes(cache.blobPath(alpha.ContentKey()), old, old); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	cache.lastGC = time.Time{}
	cache.mu.Unlock()
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.blobPath(alpha.ContentKey())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不再引用的旧 blob 应被清理")
	}
	// 目录又回到旧内容：重新写 blob
	cache.put("a@example.com", aistudio.CatalogSnapshot{Playground: alpha}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.blobPath(alpha.ContentKey())); err != nil {
		t.Fatalf("清理后又被引用的 blob 应重新写入: %v", err)
	}
	// 另一个进程删掉了 blob（本进程仍记着在盘上）：下次 put 发现文件不在，重写
	if err := os.Remove(cache.blobPath(alpha.ContentKey())); err != nil {
		t.Fatal(err)
	}
	cache.put("b@example.com", aistudio.CatalogSnapshot{Playground: alpha}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.blobPath(alpha.ContentKey())); err != nil {
		t.Fatalf("被另一个进程删掉的 blob 应重新写入: %v", err)
	}
	// 复用很久没改过的 blob：刷新修改时间，index 写出前不会被清理
	if err := os.Chtimes(cache.blobPath(alpha.ContentKey()), old, old); err != nil {
		t.Fatal(err)
	}
	cache.put("c@example.com", aistudio.CatalogSnapshot{Playground: alpha}, time.Now())
	info, err := os.Stat(cache.blobPath(alpha.ContentKey()))
	if err != nil || time.Since(info.ModTime()) > time.Hour {
		t.Fatalf("复用旧 blob 应刷新修改时间: %v %v", info, err)
	}
}

// TestCatalogCacheTruncatedBlobRewritten 写一半的 blob 读回校验失败：删除，账户按缺失；下次 put 时重写。
// 磁盘上已有同键却损坏的 blob 写盘时读回校验失败，覆盖重写
func TestCatalogCacheTruncatedBlobRewritten(t *testing.T) {
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, "a@example.com")
	data := catalogTestData(t, "alpha")
	cache := openCatalogCache(dir, nil)
	cache.put("a@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	path := cache.blobPath(data.ContentKey())
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, whole[:len(whole)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	generation := newCatalogGeneration(t, root, nil)
	reopened := openCatalogCache(dir, nil)
	summary := reopened.restore(generation.pool, generation.client, false)
	if summary.FromDisk != 0 || summary.Missing != 1 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("损坏的 blob 应删除")
	}
	reopened.put("a@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	if err := reopened.flush(); err != nil {
		t.Fatal(err)
	}
	if rewritten, err := os.ReadFile(path); err != nil || blobKeyOf(rewritten, catalogBlobPlayground) != data.ContentKey() {
		t.Fatalf("下次 put 应重写完整的 blob: %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := openCatalogCache(dir, nil)
	other.put("b@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	if err := other.flush(); err != nil {
		t.Fatal(err)
	}
	if rewritten, _ := os.ReadFile(path); blobKeyOf(rewritten, catalogBlobPlayground) != data.ContentKey() {
		t.Fatal("同键 blob 校验失败时应覆盖重写")
	}
}

// TestCatalogCacheConcurrentWriters 两个实例交错保存：按账户 at 新者胜合并、不丢条目；损坏改名在锁内进行
func TestCatalogCacheConcurrentWriters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	data := catalogTestData(t, "alpha")
	first, second := openCatalogCache(dir, nil), openCatalogCache(dir, nil)
	older, newer := time.Unix(1_700_000_000, 0), time.Unix(1_700_000_100, 0)
	first.put("shared@example.com", aistudio.CatalogSnapshot{Playground: data}, older)
	second.put("shared@example.com", aistudio.CatalogSnapshot{Playground: data, Tier: aistudio.BenefitTierPro}, newer)
	var writers sync.WaitGroup
	for round := range 20 {
		writers.Add(2)
		go func() {
			defer writers.Done()
			first.put(fmt.Sprintf("first-%02d@example.com", round), aistudio.CatalogSnapshot{Playground: data}, newer)
			if err := first.flush(); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer writers.Done()
			second.put(fmt.Sprintf("second-%02d@example.com", round), aistudio.CatalogSnapshot{Playground: data}, newer)
			if err := second.flush(); err != nil {
				t.Error(err)
			}
		}()
	}
	writers.Wait()
	// 晚写的旧记录也不覆盖磁盘上更新的记录，内存改用磁盘上的
	first.put("shared@example.com", aistudio.CatalogSnapshot{Playground: data}, older)
	if err := first.flush(); err != nil {
		t.Fatal(err)
	}
	index := readCatalogIndex(t, dir)
	if len(index.Accounts) != 41 {
		t.Fatalf("合并后有 %d 个账户，期望 41 个", len(index.Accounts))
	}
	if shared := index.Accounts["shared@example.com"]; shared.At != newer.Unix() || shared.Tier != int64(aistudio.BenefitTierPro) {
		t.Fatalf("共同账户应取 at 更新的记录：%+v", shared)
	}
	if entry, _ := first.entryOf("shared@example.com"); entry.At != newer.Unix() {
		t.Fatal("磁盘一侧胜出时内存应改用磁盘上的记录")
	}

	// 损坏改名在锁内：另一进程持锁期间不改名，锁释放后才改名
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, ".lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *catalogCache, 1)
	go func() { opened <- openCatalogCache(dir, nil) }()
	time.Sleep(100 * time.Millisecond)
	if renamed, _ := filepath.Glob(filepath.Join(dir, "index.json.corrupt-*")); len(renamed) != 0 {
		t.Fatal("持锁期间不应改名")
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	<-opened
	if renamed, _ := filepath.Glob(filepath.Join(dir, "index.json.corrupt-*")); len(renamed) != 1 {
		t.Fatal("锁释放后应改名")
	}
}

// TestCatalogCacheBlobGC index 不再引用、修改时间超过 24 小时的 blob 删除，新写的与仍引用的保留；
// 号池里没有、7 天没有同步的账户条目删除
func TestCatalogCacheBlobGC(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	data := catalogTestData(t, "alpha")
	cache := openCatalogCache(dir, nil)
	cache.known = func(accountID string) bool { return accountID != "gone@example.com" }
	cache.put("kept@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	cache.put("gone@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now().Add(-8*24*time.Hour))
	cache.put("old-known@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now().Add(-8*24*time.Hour))
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	staleKey, freshKey := strings.Repeat("0a", 16), strings.Repeat("0b", 16)
	for _, key := range []string{staleKey, freshKey} {
		if err := os.WriteFile(cache.blobPath(key), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-catalogBlobGCAge - time.Hour)
	for _, path := range []string{cache.blobPath(staleKey), cache.blobPath(data.ContentKey())} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	cache.lastGC = time.Time{}
	cache.markDirtyLocked()
	cache.mu.Unlock()
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.blobPath(staleKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不再引用的旧 blob 应删除")
	}
	for _, key := range []string{freshKey, data.ContentKey()} {
		if _, err := os.Stat(cache.blobPath(key)); err != nil {
			t.Fatalf("blob %s 应保留: %v", key, err)
		}
	}
	index := readCatalogIndex(t, dir)
	if _, exists := index.Accounts["gone@example.com"]; exists {
		t.Fatal("号池里没有、7 天没有同步的条目应删除")
	}
	if _, exists := index.Accounts["old-known@example.com"]; !exists {
		t.Fatal("号池里还有的账户条目应保留")
	}
}

// BenchmarkCatalogCacheLoad12k 1.2 万账户的 index 载入（读文件与解码，3 种目录）
func BenchmarkCatalogCacheLoad12k(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "model-catalogs")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		b.Fatal(err)
	}
	keys := []string{strings.Repeat("1a", 16), strings.Repeat("2b", 16), strings.Repeat("3c", 16)}
	entries := make(map[string]catalogEntry, 12000)
	for index := range 12000 {
		entries[fmt.Sprintf("account-%05d@example.com", index)] = catalogEntry{
			Tier: int64(index % 3), Playground: keys[index%3], At: 1_700_000_000 + int64(index),
		}
	}
	data, err := json.Marshal(catalogIndex{Version: catalogCacheVersion, Accounts: entries})
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, 0o600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		if cache := openCatalogCache(dir, nil); len(cache.entries) != 12000 {
			b.Fatalf("载入 %d 个账户", len(cache.entries))
		}
	}
}

// TestCatalogCacheEntryGCSticks 清理删掉的条目（已删除账户超过 7 天）同时从内存里删掉：之后任意一次写盘不再把它写回 index；
// 清理与写盘之间又改过的条目保留在内存，下次照常写
func TestCatalogCacheEntryGCSticks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	data := catalogTestData(t, "alpha")
	cache := openCatalogCache(dir, nil)
	cache.known = func(accountID string) bool { return accountID == "kept@example.com" }
	cache.put("kept@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now())
	cache.put("gone@example.com", aistudio.CatalogSnapshot{Playground: data}, time.Now().Add(-8*24*time.Hour))
	cache.mu.Lock()
	cache.lastGC = time.Time{}
	cache.mu.Unlock()
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCatalogIndex(t, dir).Accounts["gone@example.com"]; ok {
		t.Fatal("带清理的写盘应删掉已删除账户超过 7 天的条目")
	}
	if _, ok := cache.entryOf("gone@example.com"); ok {
		t.Fatal("清理删掉的条目应同时从内存删掉")
	}
	// 不到一小时、不清理的下一次写盘
	cache.noteFailure("kept@example.com", 1, time.Now().Add(time.Minute))
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCatalogIndex(t, dir).Accounts["gone@example.com"]; ok {
		t.Fatal("清理删掉的条目被下一次写盘写回 index")
	}
	if _, ok := readCatalogIndex(t, dir).Accounts["kept@example.com"]; !ok {
		t.Fatal("仍在本池的账户应保留")
	}
}

// TestCatalogCacheRestoresBuildWhenDisabled 启动时没有启用 Build 也恢复缓存里的 Build 目录（账户池不使用它），运行中经 UPSTREAM_CHANNELS
// 热更新启用 Build 后立即可用；没有启用 Build 时缺 Build 目录的账户不排进后台同步
func TestCatalogCacheRestoresBuildWhenDisabled(t *testing.T) {
	ctx := context.Background()
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	const accountID = "pro@example.com"
	catalogTestAccounts(t, root, accountID)
	requests := newRequestRegistry(ctx)
	first := newCatalogGeneration(t, root, &catalogRPCTransport{
		listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7)), tier: "[1]",
	})
	if _, err := first.pooled.RefreshAccountModels(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	build := []aistudio.Model{{ID: "build-only", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	if err := first.pool.SetBuildCatalog(accountID, build); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := first.pooled.CatalogSnapshot(accountID)
	cache := openCatalogCache(dir, requests)
	cache.put(accountID, snapshot, time.Now())
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}

	second := newCatalogGeneration(t, root, nil)
	summary := openCatalogCache(dir, requests).restore(second.pool, second.client, false)
	if summary.FromDisk != 1 || len(summary.Stale) != 0 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	if restored, ok := second.pool.BuildCatalogOf(accountID); !ok || !reflect.DeepEqual(restored, build) {
		t.Fatalf("没有启用 Build 时也应恢复 Build 目录，得到 %+v", restored)
	}
	if missing := second.pool.AccountsWithoutBuildCatalog(); len(missing) != 0 {
		t.Fatalf("恢复后没有 Build 目录的账户 = %v", missing)
	}
}
