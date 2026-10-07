package camoufoxnative

import (
	"strings"
	"testing"
)

// TestVerifyCamoufoxArchive 已知平台的发行包必须与固定的 SHA-256 一致
func TestVerifyCamoufoxArchive(t *testing.T) {
	asset := "camoufox-" + camoufoxRelease + "-lin.x86_64.zip"
	expected, ok := camoufoxSHA256[asset]
	if !ok {
		t.Fatalf("缺少 %s 的固定校验值", asset)
	}
	if err := verifyCamoufoxArchive(asset, strings.ToUpper(expected)); err != nil {
		t.Fatalf("校验值一致时应通过: %v", err)
	}
	if err := verifyCamoufoxArchive(asset, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("校验值不一致时应拒绝，得到 %v", err)
	}
	if err := verifyCamoufoxArchive("camoufox-"+camoufoxRelease+"-lin.i686.zip", "abc"); err != nil {
		t.Fatalf("没有固定校验值的平台应记录警告后继续，得到 %v", err)
	}
}

// TestCamoufoxSHA256CoversReleaseTargets 发布构建的每个平台都有固定校验值，且与当前版本号一致
func TestCamoufoxSHA256CoversReleaseTargets(t *testing.T) {
	for _, platform := range []string{"win.x86_64", "lin.x86_64", "lin.arm64", "mac.x86_64", "mac.arm64"} {
		asset := "camoufox-" + camoufoxRelease + "-" + platform + ".zip"
		if len(camoufoxSHA256[asset]) != 64 {
			t.Fatalf("%s 缺少固定校验值（升级 camoufoxRelease 时需要同步更新）", asset)
		}
	}
}
