//go:build test

package cuirass_test

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// framework 內對外連線只在 internal/repository/http.go、internal/repository/sse.go 與 internal/repository/providers/**。
// 用 go/types 型別檢查,所以別名 import、點 import、「沒 import 但透過欄位(或內嵌)呼叫 .Do(...)」、方法值都抓得到。

const modulePath = "github.com/Mikimoto/cuirass"

// repoRoot:本目錄(tests/)到 repo 根目錄。掃原始碼的測試先 t.Chdir 到這裡,路徑照舊以根目錄為準。
const repoRoot = ".."

func allowedNet(rel string) bool {
	return rel == "internal/repository/http.go" || rel == "internal/repository/sse.go" || strings.HasPrefix(rel, "internal/repository/providers/")
}

// bannedPkg:能開連線的套件。net/url、net/netip 這類純解析的不算。
// reflect、unsafe 也算:用它們可以繞過型別檢查呼叫連線方法(reflect.ValueOf(c).MethodByName("Do"))。框架目前沒有非測試檔用到。
func bannedPkg(p string) bool {
	return p == "net" || p == "crypto/tls" || p == "net/smtp" || p == "net/http" ||
		strings.HasPrefix(p, "net/http/") || p == "net/rpc" || strings.HasPrefix(p, "net/rpc/") ||
		p == "golang.org/x/net" || strings.HasPrefix(p, "golang.org/x/net/") || p == "reflect" || p == "unsafe"
}

// bannedType:型別(去掉指標與別名)是否定義在連線套件,例如 *http.Client、http.RoundTripper。
func bannedType(t types.Type) bool {
	for {
		t = types.Unalias(t)
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && bannedPkg(n.Obj().Pkg().Path())
}

// fileViolations 回一個檔案裡的連線點。info 為 nil = 只看 import(沒進這次建置的檔案)。
func fileViolations(fset *token.FileSet, rel string, f *ast.File, info *types.Info) []string {
	var out []string
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if bannedPkg(p) {
			out = append(out, fset.Position(im.Pos()).String()+": import "+p)
		}
	}
	if info == nil {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := info.Uses[id]
		if obj == nil || obj.Pkg() == nil || !bannedPkg(obj.Pkg().Path()) {
			return true
		}
		out = append(out, fset.Position(id.Pos()).String()+": "+obj.String())
		return true
	})
	in := func(n ast.Node) bool { return n.Pos() >= f.Pos() && n.Pos() < f.End() }
	// 泛型推導:型別參數是連線套件的型別(例如 get(c *http.Client) 經 interface{ Get(string) } 約束呼叫)。
	for id, inst := range info.Instances {
		if !in(id) {
			continue
		}
		for t := range inst.TypeArgs.Types() {
			if bannedType(t) {
				out = append(out, fset.Position(id.Pos()).String()+": 泛型實例化 "+t.String())
			}
		}
	}
	// 任何值的型別是連線套件的型別(例如沒 import 卻取出 d.HTTP 這個 *http.Client):值一出現就算,
	// 之後經泛型、interface、反射怎麼呼叫都抓得到。
	for e, tv := range info.Types {
		if in(e) && tv.IsValue() && bannedType(tv.Type) {
			out = append(out, fset.Position(e.Pos()).String()+": 值的型別是 "+tv.Type.String())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// checkFiles 型別檢查一個套件;型別錯誤就失敗(否則 Uses 是空的,掃描會默默通過)。
func checkFiles(t *testing.T, imp types.Importer, fset *token.FileSet, importPath string, files []*ast.File) *types.Info {
	t.Helper()
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}, Instances: map[*ast.Ident]types.Instance{}}
	conf := types.Config{Importer: imp}
	if _, err := conf.Check(importPath, fset, files, info); err != nil {
		t.Fatalf("型別檢查 %s 失敗:%v", importPath, err)
	}
	return info
}

// 兩個測試共用一個 importer:標準函式庫只從原始碼型別檢查一次。
var (
	sharedFset = token.NewFileSet()
	sharedImp  = importer.ForCompiler(sharedFset, "source", nil)
)

func newImporter() (*token.FileSet, types.Importer) { return sharedFset, sharedImp }

func TestNetworkSitesRules(t *testing.T) {
	src := map[string]string{
		"a.go": `package p
import h "net/http"
type S struct{ C *h.Client; T h.RoundTripper }
type Em struct{ *h.Client }
func A(s S) { s.C.Get("u") }`,
		// 沒 import:透過欄位、內嵌、方法值呼叫
		"field.go":     `package p; func B(s S) { s.C.Do(nil) }`,
		"embed.go":     `package p; func B2(e Em) { e.Get("u") }`,
		"methodval.go": `package p; func C(s S) { f := s.T.RoundTrip; _ = f }`,
		"dial.go": `package p
import ("net"; "crypto/tls")
func D() { net.Dial("tcp", "x"); tls.Dial("tcp", "x", nil) }`,
		"dot.go": `package p
import . "net/http"
func E() { Get("u") }`,
		// 注入的白名單 client:不算
		"inject.go": `package p
import "github.com/Mikimoto/cuirass/internal/entity"
func F(tc entity.ToolCtx) { tc.HTTP(nil, "u", entity.HTTPOptions{}) }`,
		// 只往下傳 RoundTripper 也算(usecase 不得 import net/http)
		"pass.go": `package p
import "net/http"
type G struct{ T http.RoundTripper }
func NewG(t http.RoundTripper) *http.Client { return &http.Client{Transport: t} }`,
		// 沒 import:泛型推導經 interface 約束呼叫 Get(呼叫到的 Get 是約束的方法,不是 net/http 的)
		"generic.go": `package p
func get[R any, C interface{ Get(string) (R, error) }](c C, u string) (R, error) { return c.Get(u) }
func GenCall(s S) { _, _ = get(s.C, "u") }`,
		// 反射
		"reflect.go": `package p
import "reflect"
func R(s S) { reflect.ValueOf(s.C).MethodByName("Get").Call(nil) }`,
		// golang.org/x/net(go.mod 沒有,只看 import;不進型別檢查)
		"passcall.go": `package p
import "net/http"
func H(t http.RoundTripper, r *http.Request) { t.RoundTrip(r) }`,
	}
	fset, imp := newImporter()
	var files []*ast.File
	byName := map[string]*ast.File{}
	for _, name := range slices.Sorted(maps.Keys(src)) {
		f, err := parser.ParseFile(fset, name, src[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		byName[name] = f
	}
	info := checkFiles(t, imp, fset, modulePath+"/zz_rules", files)

	hit := func(name, rel string) bool { return len(fileViolations(fset, rel, byName[name], info)) > 0 }
	for _, name := range []string{"a.go", "field.go", "embed.go", "methodval.go", "dial.go", "dot.go", "pass.go", "passcall.go", "generic.go", "reflect.go"} {
		if !hit(name, "usecase/x.go") {
			t.Errorf("%s 在一般檔案應該被抓到", name)
		}
	}
	if v := fileViolations(fset, "usecase/x.go", byName["inject.go"], info); len(v) != 0 {
		t.Errorf("注入的白名單 client 不該算:%v", v)
	}
	// 被抓到的是那個呼叫,不只是 import
	if v := fileViolations(fset, "usecase/x.go", byName["field.go"], info); !slices.ContainsFunc(v, func(s string) bool { return strings.Contains(s, "Do") }) {
		t.Errorf("field.go:%v", v)
	}
	// golang.org/x/net 的 import(只做語法檢查:go.mod 沒有這個依賴)
	xf, err := parser.ParseFile(fset, "xnet.go", `package p; import _ "golang.org/x/net/proxy"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileViolations(fset, "usecase/x.go", xf, nil)) == 0 {
		t.Error("golang.org/x/net 的 import 應該被抓到")
	}
}

func TestNetworkSites(t *testing.T) {
	t.Chdir(repoRoot)
	fset, imp := newImporter()
	dirs := map[string][]string{} // rel dir → 非測試 .go 檔
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			dir := filepath.ToSlash(filepath.Dir(p))
			dirs[dir] = append(dirs[dir], filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 若哪天這裡因只在 windows 編的檔用到 windows 專屬 API(如 syscall)而型別檢查失敗,是本機標準函式庫的限制,不是違規:
	// 改成那個檔另外處理(或用該 GOOS 的 importer)。
	// 兩種建置設定各型別檢查一次(本機 + 另一個 GOOS),build tag 排除的檔(例如 auditfile_other.go)也要過型別檢查;
	// 兩種都沒選到的檔直接失敗,新加的 tag 檔不會默默只剩 import 檢查。另一個 GOOS 的檔用本機的標準函式庫型別檢查。
	// 兩種設定都帶 test tag,否則 testing.go 這類只在測試建置才編的檔兩種都選不到。
	cur := build.Default
	cur.BuildTags = append(slices.Clone(cur.BuildTags), "test")
	alt := cur
	alt.GOOS = "windows"
	if cur.GOOS == "windows" {
		alt.GOOS = "linux"
	}
	contexts := []*build.Context{&cur, &alt}

	var hits, bad []string
	nfiles := 0
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		parsed := map[string]*ast.File{}
		for _, rel := range dirs[dir] {
			f, err := parser.ParseFile(fset, rel, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			parsed[rel] = f
		}
		importPath := modulePath
		if dir != "." {
			importPath += "/" + dir
		}
		found := map[string]map[string]bool{} // rel → 違規(兩種設定的聯集)
		for _, ctx := range contexts {
			var rels []string
			var files []*ast.File
			for _, rel := range dirs[dir] {
				ok, err := ctx.MatchFile(dir, path.Base(rel))
				if err != nil {
					t.Fatal(err)
				}
				if ok {
					rels = append(rels, rel)
					files = append(files, parsed[rel])
				}
			}
			if len(files) == 0 {
				continue
			}
			info := checkFiles(t, imp, fset, importPath+" ("+ctx.GOOS+")", files)
			for _, rel := range rels {
				if found[rel] == nil {
					found[rel] = map[string]bool{}
				}
				for _, v := range fileViolations(fset, rel, parsed[rel], info) {
					found[rel][v] = true
				}
			}
		}
		for _, rel := range dirs[dir] {
			nfiles++
			vs, checked := found[rel]
			if !checked {
				t.Errorf("%s 在 %s/%s 都沒進建置,沒做型別檢查", rel, cur.GOOS, alt.GOOS)
				continue
			}
			if len(vs) == 0 {
				continue
			}
			hits = append(hits, rel)
			if !allowedNet(rel) {
				bad = append(bad, slices.Sorted(maps.Keys(vs))...)
			}
		}
	}
	if len(bad) != 0 {
		t.Errorf("允許範圍外有對外連線:\n%s", strings.Join(bad, "\n"))
	}
	// 掃描本身有效:已知的連線點要掃得到
	for _, want := range []string{"internal/repository/http.go", "internal/repository/sse.go", "internal/repository/providers/openai/openai.go", "internal/repository/providers/providers.go"} {
		if !slices.Contains(hits, want) {
			t.Errorf("掃描沒抓到已知連線點 %s(hits=%v)", want, hits)
		}
	}
	if !slices.Contains(dirs["internal/usecase"], "internal/usecase/agent.go") || nfiles < 30 {
		t.Errorf("掃描範圍不對:%d 個檔,usecase=%v", nfiles, dirs["internal/usecase"])
	}
}
