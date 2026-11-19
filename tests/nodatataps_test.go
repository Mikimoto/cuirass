//go:build test

package cuirass_test

import (
	"go/ast"
	"go/parser"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 封住「未遮蔽資料送到外部」的三條路:根套件的匯出 API 不得有能看到或改走請求的管道。
// 開發者自己的 callback 在本機看自己的資料不算外送,但每一個都要列在允許清單、寫理由;多一個、少一個都失敗。

// allowedCallbacks:匯出 API 裡型別是 func 或介面(error、any、context.Context 除外)的欄位與參數。
// 鍵的格式見 collectAPI。回傳值(ret#i)是框架交給開發者的值,不是開發者塞進框架的 hook,但同樣要列。
var allowedCallbacks = map[string]string{
	"Agent.On.ret#0":             "取消訂閱的函式:不帶資料",
	"NewFileAudit.ret#0":         "框架建的檔案 AuditSink:開發者再交回 AgentOptions.Audit,寫本機檔案",
	"NullAudit.ret#0":            "什麼都不記的 AuditSink",
	"NumberVerifier.ret#0":       "框架建的 Verifier:開發者放進 AgentOptions.Verifiers,走 VerifyCtx.Ask(送出管線)",
	"Agent.On.l":                 "listener:開發者在本機看事件",
	"AgentOptions.Audit":         "AuditSink:開發者自己的稽核 sink",
	"AgentOptions.LeakSources":   "LeakSources:回外洩比對來源,框架只讀",
	"AgentOptions.Rewrite":       "ContentRewrite:改寫內容的 hook,在送出管線之內",
	"AgentOptions.System":        "System:提供系統提示詞",
	"AgentOptions.TurnTimeout":   "TurnTimeout:依 provider 決定逾時",
	"AgentOptions.Verifiers":     "Verifier:核對回答,在本機執行",
	"CreateDeps.Audit":           "AuditSink:privacy_override 稽核",
	"Masking.Collect":            "Masking hook:回要遮的數字",
	"Masking.HideResult":         "Masking hook:決定隱藏哪些工具結果",
	"Masking.Snapshot":           "Masking hook:回遮蔽集合",
	"NumberBook.Context.mask":    "開發者自己 NewNumberBook 建的 book;mask 只看到開發者自己加的資料(框架內部的 book 不經公開 API 外露)",
	"NewFileAudit.warn":          "NewFileAudit 的 warn:收寫入失敗訊息",
	"Policy.PublicReason":        "PublicReason:給模型看的阻擋原因",
	"Policy.Rules":               "ToolRule:工具政策規則",
	"PolicyOptions.PublicReason": "PublicReason:給模型看的阻擋原因",
	"PolicyOptions.Rules":        "ToolRule:工具政策規則",
	"RuleCtx.TurnCount":          "ToolRule 的參數:框架給規則查本輪呼叫次數",
	"ToolCtx.HTTP":               "ToolCtx.HTTP:框架建的 https + 白名單 client,唯一的對外連線出口",
	"ToolDef.Execute":            "ToolDef.Execute:開發者的工具",
	"VerifyCtx.Ask":              "VerifyCtx.Ask:Verifier 問目前 provider,走送出管線(system 參數不經遮蔽)",
	"VerifyCtx.KnownNumbers":     "VerifyCtx:給 Verifier 的資料",
	"VerifyCtx.Mask":             "VerifyCtx:remote 時遮蔽",
	"VerifyCtx.Remote":           "VerifyCtx:目前 provider 是否 remote",
	"VerifyCtx.ToolData":         "VerifyCtx:給 Verifier 的工具資料",
}

// allowedMessageReaders:回傳訊息內容的方法與函式。
var allowedMessageReaders = map[string]string{
	"Agent.Messages": "開發者讀自己的對話紀錄(本機)",
}

// apiFacts:callback 位置、回傳訊息內容的函式與方法、違規(net 型別、LLMRequest、Stream 方法)。
type apiFacts struct{ callbacks, readers, bad map[string]bool }

// collectAPI 遞迴走 pkg 的匯出 API(型別、函式、套件變數)。鍵:欄位「型別.欄位」、參數「函式.參數名」(沒名字用「#i」)、
// 回傳值「函式.ret#i」、套件變數「變數名」;匿名 struct / 介面的成員接在所在位置後面;內嵌欄位(不論是否匯出)的欄位提升到外層。
// 模組外的泛型型別(iter.Seq[...])照樣走型別參數。
func collectAPI(pkg *types.Package) apiFacts {
	f := apiFacts{map[string]bool{}, map[string]bool{}, map[string]bool{}}
	scope := pkg.Scope()
	names := map[*types.TypeName]string{} // internal 型別在根套件的名字
	for _, n := range scope.Names() {
		if tn, ok := scope.Lookup(n).(*types.TypeName); ok && tn.Exported() {
			if nt, ok := types.Unalias(tn.Type()).(*types.Named); ok {
				names[nt.Origin().Obj()] = n
			}
		}
	}
	inModule := func(p *types.Package) bool { return p != nil && strings.HasPrefix(p.Path(), modulePath) }
	// peel 拿掉別名、指標、切片、陣列、map、chan,回最內層型別。
	peel := func(t types.Type) types.Type {
		for {
			switch x := types.Unalias(t).(type) {
			case *types.Pointer:
				t = x.Elem()
			case *types.Slice:
				t = x.Elem()
			case *types.Array:
				t = x.Elem()
			case *types.Map:
				t = x.Elem()
			case *types.Chan:
				t = x.Elem()
			default:
				return types.Unalias(t)
			}
		}
	}
	isCallback := func(t types.Type) bool {
		t = peel(t)
		if n, ok := t.(*types.Named); ok {
			if o := n.Obj(); o.Pkg() == nil || o.Pkg().Path() == "context" && o.Name() == "Context" {
				return false // error 與 context.Context
			}
		}
		switch u := t.Underlying().(type) {
		case *types.Signature:
			return true
		case *types.Interface:
			return !u.Empty() // any
		}
		return false
	}
	var hasMessage func(t types.Type, seen map[types.Type]bool) bool
	hasMessage = func(t types.Type, seen map[types.Type]bool) bool {
		t = peel(t)
		if seen[t] {
			return false
		}
		seen[t] = true
		if n, ok := t.(*types.Named); ok {
			if inModule(n.Obj().Pkg()) && (n.Obj().Name() == "Message" || n.Obj().Name() == "Part") {
				return true
			}
			for a := range n.TypeArgs().Types() {
				if hasMessage(a, seen) {
					return true
				}
			}
		}
		switch u := t.Underlying().(type) {
		case *types.Struct:
			for fld := range u.Fields() {
				if (fld.Exported() || fld.Embedded()) && hasMessage(fld.Type(), seen) {
					return true
				}
			}
		case *types.Signature: // 回傳的函式再回訊息也算
			for v := range u.Results().Variables() {
				if hasMessage(v.Type(), seen) {
					return true
				}
			}
		case *types.Interface:
			for m := range u.Methods() {
				if m.Exported() && hasMessage(m.Type(), seen) {
					return true
				}
			}
		}
		return false
	}
	visited := map[*types.Named]bool{}
	var visit, descend func(where string, t types.Type)
	var sig func(where string, s *types.Signature, api bool)
	var fields func(where string, st *types.Struct)
	var methods func(where string, recv types.Type)
	visit = func(where string, t types.Type) {
		if isCallback(t) {
			f.callbacks[where] = true
		}
		descend(where, t)
	}
	sig = func(where string, s *types.Signature, api bool) {
		for i := range s.Params().Len() {
			v := s.Params().At(i)
			key := v.Name()
			if key == "" || key == "_" {
				key = "#" + strconv.Itoa(i)
			}
			visit(where+"."+key, v.Type())
		}
		for i := range s.Results().Len() {
			v := s.Results().At(i)
			if api && hasMessage(v.Type(), map[types.Type]bool{}) {
				f.readers[where] = true
			}
			visit(where+".ret#"+strconv.Itoa(i), v.Type())
		}
	}
	methods = func(where string, recv types.Type) {
		for sel := range types.NewMethodSet(recv).Methods() {
			if sel.Obj().Name() == "Stream" {
				f.bad[where+" 有 Stream 方法"] = true
			}
			if sel.Obj().Exported() {
				sig(where+"."+sel.Obj().Name(), sel.Type().(*types.Signature), true)
			}
		}
	}
	// ponytail: 內嵌自己的指標(type X struct{ *X })會無限遞迴;公開 API 沒有這種型別,出現時再加防護。
	fields = func(where string, st *types.Struct) {
		for fld := range st.Fields() {
			if fld.Exported() {
				visit(where+"."+fld.Name(), fld.Type())
			}
			if !fld.Embedded() {
				continue
			}
			// 內嵌欄位不論是否匯出,它的欄位都會提升到外層(方法已在外層的 method set 裡)
			et := peel(fld.Type())
			if n, ok := et.(*types.Named); ok && !inModule(n.Obj().Pkg()) {
				descend(where+"."+fld.Name(), et) // 模組外:只做 net 等檢查
			} else if est, ok := et.Underlying().(*types.Struct); ok {
				fields(where, est)
			}
		}
	}
	descend = func(where string, t types.Type) {
		switch x := types.Unalias(t).(type) {
		case *types.Pointer:
			visit(where, x.Elem())
		case *types.Slice:
			visit(where, x.Elem())
		case *types.Array:
			visit(where, x.Elem())
		case *types.Chan:
			visit(where, x.Elem())
		case *types.Map:
			visit(where, x.Key())
			visit(where, x.Elem())
		case *types.Signature:
			sig(where, x, false)
		case *types.Struct:
			fields(where, x)
		case *types.Interface:
			methods(where, x)
		case *types.Named:
			obj := x.Origin().Obj()
			if obj.Pkg() == nil {
				return // error
			}
			if p := obj.Pkg().Path(); p == "net" || p == "net/http" || strings.HasPrefix(p, "net/http/") {
				f.bad["net 型別 "+p+"."+obj.Name()+"(在 "+where+")"] = true
			}
			if obj.Name() == "LLMRequest" {
				f.bad["LLMRequest(在 "+where+")"] = true
			}
			for a := range x.TypeArgs().Types() { // 模組外的泛型也要走(iter.Seq[*http.Client])
				visit(where, a)
			}
			if !inModule(obj.Pkg()) || visited[x] {
				return // 標準函式庫(net 已在上面擋)
			}
			visited[x] = true
			name, ok := names[obj]
			if !ok {
				name = obj.Name()
			}
			if types.IsInterface(x) {
				methods(name, x) // 介面的指標沒有方法,介面要用本身的 method set
				return
			}
			methods(name, types.NewPointer(x))
			switch u := x.Underlying().(type) {
			case *types.Struct:
				fields(name, u)
			case *types.Signature:
				sig(name, u, false)
			}
		}
	}
	for _, n := range scope.Names() {
		switch o := scope.Lookup(n).(type) {
		case *types.Func:
			if o.Exported() {
				sig(n, o.Type().(*types.Signature), true)
			}
		case *types.Var:
			if o.Exported() {
				visit(n, o.Type())
			}
		case *types.TypeName:
			if o.Exported() {
				descend(n, o.Type()) // 型別名稱本身不是 callback 位置
			}
		}
	}
	return f
}

func TestPublicAPINoDataTaps(t *testing.T) {
	got := collectAPI(rootAPI(t))
	callbacks, readers := got.callbacks, got.readers
	for k := range got.bad {
		t.Errorf("公開 API 不可有:%s", k)
	}
	diff := func(what string, got map[string]bool, want map[string]string) {
		for _, k := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[k]; !ok {
				t.Errorf("%s 多了 %s:新增 callback 要寫進允許清單並寫理由", what, k)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if !got[k] {
				t.Errorf("%s 少了 %s", what, k)
			}
		}
	}
	diff("callback 清單", callbacks, allowedCallbacks)
	diff("回傳訊息的方法", readers, allowedMessageReaders)
}

// collectAPI 本身的 bite check:每種曾經漏掉的寫法都要抓得到(審查 fix round 1),乾淨的 API 什麼都不報。
func TestCollectAPICatchesBypasses(t *testing.T) {
	const base = `package synth
import ("iter"; "net/http")
type Message struct{ Text string }
type LLMRequest struct{}
var _ = http.Client{}
var _ iter.Seq[int]
`
	type want struct{ kind, key string } // kind:callback、reader、bad(bad 比對子字串)
	cases := []struct {
		name, src string
		want      []want
	}{
		{"乾淨", `type Ok struct{ N int }; func Fine(s string) int { return 0 }`, nil},
		{"內嵌未匯出 struct 的 func 欄位", `type inner struct{ Tap func([]Message) }; type X struct{ inner }`, []want{{"callback", "X.Tap"}}},
		{"內嵌未匯出 struct 的 net 欄位", `type inner struct{ C *http.Client }; type X struct{ inner }`, []want{{"bad", "net/http.Client"}}},
		{"內嵌 *http.Client", `type X struct{ *http.Client }`, []want{{"bad", "net/http.Client"}}},
		{"匿名 struct 欄位的 func", `type X struct{ A struct{ Tap func([]Message) } }`, []want{{"callback", "X.A.Tap"}}},
		{"匿名 struct 欄位的 net", `type X struct{ A struct{ C *http.Client } }`, []want{{"bad", "net/http.Client"}}},
		{"模組外泛型的 net 型別參數", `func F() iter.Seq[*http.Client] { return nil }`, []want{{"bad", "net/http.Client"}}},
		{"模組外泛型的 LLMRequest", `func F() iter.Seq[LLMRequest] { return nil }`, []want{{"bad", "LLMRequest(在 F.ret#0)"}}},
		{"回傳 iter.Seq[Message]", `type A struct{}; func (*A) All() iter.Seq[Message] { return nil }`, []want{{"reader", "A.All"}}},
		{"套件變數", `var Hook func([]Message)`, []want{{"callback", "Hook"}}},
		{"回傳函式", `func Snap() func() []Message { return nil }`, []want{{"callback", "Snap.ret#0"}, {"reader", "Snap"}}},
		{"沒名字的參數各自一個鍵", `func U(func([]Message), func(string)) {}`, []want{{"callback", "U.#0"}, {"callback", "U.#1"}}},
		{"介面的 Stream 方法", `type S interface{ Stream() }`, []want{{"bad", "S 有 Stream 方法"}}},
		{"介面方法的 func 參數", `type I interface{ Tap(f func([]Message)) }`, []want{{"callback", "I.Tap.f"}}},
	}
	fset, imp := newImporter()
	for _, c := range cases {
		file, err := parser.ParseFile(fset, "synth.go", base+c.src, 0)
		if err != nil {
			t.Fatal(c.name, err)
		}
		pkg, err := (&types.Config{Importer: imp}).Check(modulePath+"/synth", fset, []*ast.File{file}, nil)
		if err != nil {
			t.Fatal(c.name, err)
		}
		got := collectAPI(pkg)
		for _, w := range c.want {
			switch w.kind {
			case "callback":
				if !got.callbacks[w.key] {
					t.Errorf("%s:沒抓到 callback %s(%v)", c.name, w.key, got.callbacks)
				}
			case "reader":
				if !got.readers[w.key] {
					t.Errorf("%s:沒抓到回傳訊息的 %s(%v)", c.name, w.key, got.readers)
				}
			case "bad":
				if !slices.ContainsFunc(slices.Collect(maps.Keys(got.bad)), func(k string) bool { return strings.Contains(k, w.key) }) {
					t.Errorf("%s:沒抓到 %s(%v)", c.name, w.key, got.bad)
				}
			}
		}
		// base 自己匯出 LLMRequest(名稱檢查要用),所以乾淨的情況剛好只報這一項
		if c.want == nil && (len(got.callbacks)+len(got.readers) != 0 || !maps.Equal(got.bad, map[string]bool{"LLMRequest(在 LLMRequest)": true})) {
			t.Errorf("%s:乾淨的 API 卻報了 %v %v %v", c.name, got.callbacks, got.readers, got.bad)
		}
	}
}
