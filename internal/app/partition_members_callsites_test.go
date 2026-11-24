package app

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"
)

// packageSources 为本包的源码，供结构测试扫描。用 embed 而不是运行时读目录：go test -overlay 换掉的源文件同样换进这里，变异检查能覆盖结构测试
//
//go:embed *.go
var packageSources embed.FS

// partitionMembersCallers 为可以直接调用账户池 PartitionMembers() 的函数（文件:函数 → 调用次数）：它们问的是“账户属于哪个号池”（家分区），
// 补充池标记不改变它。其余问“Worker 计在哪个分区”的地方一律用叠加视图 slotMembers（见 extra_pool.go），否则补充池 Worker 会被计入家分区
var partitionMembersCallers = map[string]int{
	"first_warm.go:firstWarmBlocks":         1,
	"hot_config.go:applyLiveFields":         1,
	"worker_proxy.go:syncProxyPoolCapacity": 1,
	"pro_pool.go:checkProPoolNotice":        1,
	"permission_denied.go:poolMembership":   1,
	"runtime.go:Worker":                     1,
	"worker_partition.go:poolRuleChanged":   2,
	"worker_retire.go:finishRetirement":     1,
	"worker_retire.go:finishRelease":        1,
	"balanced_rotation.go:finishRotation":   1,
	"extra_pool.go:slotMembers":             1,
	// 补充池 Worker 归还时要知道它的家分区（标记不改变家分区）
	"extra_pool.go:extraReturn": 1,
	// 借用时按家分区判断是否跨号池借用、写“账号所在号池”
	"extra_select.go:take": 1,
}

// TestPartitionMembersCallSites 结构测试：app 包的非测试文件里 PartitionMembers() 只出现在 partitionMembersCallers 列出的函数里，
// 次数也相同。以后新增调用点必须先表态：问家分区就加进列表，问 Worker 计数就改用 slotMembers
func TestPartitionMembersCallSites(t *testing.T) {
	entries, err := packageSources.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	got := make(map[string]int)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := packageSources.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fileSet, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "PartitionMembers" {
					got[name+":"+function.Name.Name]++
				}
				return true
			})
		}
	}
	if !maps.Equal(got, partitionMembersCallers) {
		keys := slices.Sorted(maps.Keys(got))
		t.Fatalf("PartitionMembers() 的调用点与清单不符（新增调用点要先判断问的是家分区还是 Worker 计数）：\n实际 %v\n清单 %v", keysWithCounts(keys, got), partitionMembersCallers)
	}
}

func keysWithCounts(keys []string, counts map[string]int) []string {
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+strings.Repeat("I", counts[key]))
	}
	return result
}
