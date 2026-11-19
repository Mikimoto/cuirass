//go:build test

package cuirass_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestLayerDependencies 鎖住分層方向 entity ← usecase ← repository ← 根套件(只看非測試檔的 import)。
// 另外根套件(package cuirass_test)與 tests/ 套件的外部測試不得 import internal:它們代表使用者,只能用公開 API。
func TestLayerDependencies(t *testing.T) {
	t.Chdir(repoRoot)
	out, err := exec.Command("go", "list", "-tags", "test", "-f", `{{.ImportPath}}|{{join .Imports " "}}|{{join .XTestImports " "}}`, "./...").Output()
	if err != nil {
		t.Fatalf("go list:%v", err)
	}
	const (
		entity     = modulePath + "/internal/entity"
		usecase    = modulePath + "/internal/usecase"
		repository = modulePath + "/internal/repository"
	)
	seen := map[string]bool{}
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "|")
		pkg, imports, xtest := f[0], strings.Fields(f[1]), strings.Fields(f[2])
		seen[pkg] = true
		internal := func(p string) bool { return strings.HasPrefix(p, modulePath+"/") || p == modulePath }
		switch pkg {
		case entity:
			if i := slices.IndexFunc(imports, internal); i >= 0 {
				t.Errorf("entity 不可 import 模組內套件:%s", imports[i])
			}
		case usecase:
			for _, p := range imports {
				if internal(p) && p != entity {
					t.Errorf("usecase 只可 import entity:%s", p)
				}
			}
		case modulePath:
			for _, p := range xtest {
				if strings.HasPrefix(p, modulePath+"/internal/") {
					t.Errorf("根套件的外部測試不可 import internal:%s", p)
				}
			}
		case modulePath + "/tests":
			for _, p := range xtest {
				if strings.HasPrefix(p, modulePath+"/internal/") {
					t.Errorf("tests 套件的外部測試不可 import internal:%s", p)
				}
			}
		}
		if pkg == entity || pkg == usecase {
			for _, p := range imports {
				if p == repository || strings.HasPrefix(p, repository+"/") {
					t.Errorf("%s 不可 import repository:%s", pkg, p)
				}
			}
		}
	}
	for _, want := range []string{modulePath, modulePath + "/tests", entity, usecase, repository} {
		if !seen[want] {
			t.Errorf("go list 沒列出 %s,檢查範圍不對", want)
		}
	}
}
