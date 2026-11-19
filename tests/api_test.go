//go:build test

package cuirass_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 根套件的公開 API 形狀:用 go/types 型別檢查根套件的非測試檔,只看匯出名稱。

// rootAPI 型別檢查根套件(非測試檔)。
func rootAPI(t *testing.T) *types.Package {
	t.Helper()
	t.Chdir(repoRoot)
	bp, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset, imp := newImporter()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	pkg, err := (&types.Config{Importer: imp}).Check(modulePath, fset, files, nil)
	if err != nil {
		t.Fatalf("型別檢查根套件失敗:%v", err)
	}
	return pkg
}

// 根套件匯出的函式、方法、型別欄位裡出現的具名型別,都要能從根套件叫出名字(或是標準函式庫的型別)。
// 內部型別要有根套件的別名;遞迴走訪別名指到的型別的匯出欄位與匯出方法。
func TestAPISelfContained(t *testing.T) {
	pkg := rootAPI(t)
	scope := pkg.Scope()
	named := map[*types.TypeName]bool{}
	for _, n := range scope.Names() {
		if tn, ok := scope.Lookup(n).(*types.TypeName); ok && tn.Exported() {
			if nt, ok := types.Unalias(tn.Type()).(*types.Named); ok {
				named[nt.Origin().Obj()] = true
			}
		}
	}
	seen := map[*types.Named]bool{}
	bad := map[string]bool{}
	var walk func(where string, t types.Type)
	walk = func(where string, t types.Type) {
		switch x := t.(type) {
		case *types.Alias:
			walk(where, types.Unalias(x))
		case *types.Named:
			obj := x.Origin().Obj()
			if obj.Pkg() == nil || !strings.Contains(strings.SplitN(obj.Pkg().Path(), "/", 2)[0], ".") {
				return // error 這類內建型別、標準函式庫
			}
			if !named[obj] {
				bad[obj.Pkg().Path()+"."+obj.Name()+"(在 "+where+")"] = true
			}
			for a := range x.TypeArgs().Types() {
				walk(where, a)
			}
			if seen[x] {
				return
			}
			seen[x] = true
			walk(obj.Name(), x.Underlying())
			ms := types.NewMethodSet(types.NewPointer(x))
			for sel := range ms.Methods() {
				if sel.Obj().Exported() {
					walk(obj.Name()+"."+sel.Obj().Name(), sel.Type())
				}
			}
		case *types.Pointer:
			walk(where, x.Elem())
		case *types.Slice:
			walk(where, x.Elem())
		case *types.Array:
			walk(where, x.Elem())
		case *types.Chan:
			walk(where, x.Elem())
		case *types.Map:
			walk(where, x.Key())
			walk(where, x.Elem())
		case *types.Signature:
			for v := range x.Params().Variables() {
				walk(where, v.Type())
			}
			for v := range x.Results().Variables() {
				walk(where, v.Type())
			}
		case *types.Struct:
			for f := range x.Fields() {
				if f.Exported() {
					walk(where+"."+f.Name(), f.Type())
				}
			}
		case *types.Interface:
			for m := range x.Methods() {
				if m.Exported() {
					walk(where+"."+m.Name(), m.Type())
				}
			}
		}
	}
	for _, n := range scope.Names() {
		if obj := scope.Lookup(n); obj.Exported() {
			walk(n, obj.Type())
		}
	}
	if len(bad) > 0 {
		t.Fatalf("根套件 API 用到沒有根套件名稱的型別:\n%s", strings.Join(slices.Sorted(func(yield func(string) bool) {
			for k := range bad {
				if !yield(k) {
					return
				}
			}
		}), "\n"))
	}
}

// Provider 是不透明的具體型別:沒有匯出欄位,方法剛好五個讀取方法(沒有 Stream);AgentOptions.Providers 是 []*Provider。
func TestProviderOpaque(t *testing.T) {
	scope := rootAPI(t).Scope()
	tn, ok := scope.Lookup("Provider").(*types.TypeName)
	if !ok {
		t.Fatal("根套件沒有 Provider 型別")
	}
	if _, isAlias := tn.Type().(*types.Alias); isAlias {
		t.Fatal("Provider 不可是別名")
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatalf("Provider 應是 struct:%s", tn.Type().Underlying())
	}
	for f := range st.Fields() {
		if f.Exported() {
			t.Errorf("Provider 有匯出欄位 %s", f.Name())
		}
	}
	var methods []string
	for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
		methods = append(methods, sel.Obj().Name())
	}
	slices.Sort(methods)
	if want := []string{"ContextWindow", "MaxTokens", "Model", "Name", "Privacy"}; !slices.Equal(methods, want) {
		t.Errorf("Provider 方法集合 %v,應剛好是 %v", methods, want)
	}
	ao, ok := scope.Lookup("AgentOptions").(*types.TypeName)
	if !ok {
		t.Fatal("根套件沒有 AgentOptions")
	}
	if _, isAlias := ao.Type().(*types.Alias); isAlias {
		t.Fatal("AgentOptions 應是根套件自己定義的 struct,不是別名")
	}
	aos, ok := ao.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatal("AgentOptions 應是 struct")
	}
	var found bool
	for f := range aos.Fields() {
		switch f.Name() {
		case "Providers":
			found = true
			if want := types.NewSlice(types.NewPointer(tn.Type())); !types.Identical(f.Type(), want) {
				t.Errorf("AgentOptions.Providers 是 %s,應是 []*Provider", f.Type())
			}
		case "Transport":
			t.Error("AgentOptions 不可有 Transport")
		}
	}
	if !found {
		t.Error("AgentOptions 沒有 Providers")
	}
}

// 根套件不匯出 provider 介面與請求型別(留著就封不住 provider)。
func TestNoLLMTypesExported(t *testing.T) {
	scope := rootAPI(t).Scope()
	for _, n := range []string{"LLMProvider", "LLMRequest", "LLMEvent", "ToolSpec", "LlmProvider", "LlmRequest", "LlmEvent"} {
		if scope.Lookup(n) != nil {
			t.Errorf("根套件不可匯出 %s", n)
		}
	}
}
