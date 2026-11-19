//go:build test

package entity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
)

// 小工具:根層一律是封閉物件,x 是被測的子 schema。
func objWith(x map[string]any) Schema {
	return Schema{"type": "object", "additionalProperties": false, "required": []any{"x"}, "properties": map[string]any{"x": x}}
}

func mustCompile(t *testing.T, s Schema) *CompiledSchema {
	t.Helper()
	c, err := CompileSchema(s)
	if err != nil {
		t.Fatalf("CompileSchema(%v) 失敗:%v", s, err)
	}
	return c
}

// decode 模擬實際來源:工具參數是 encoding/json 解出來的。
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("測試資料不是 JSON:%s", s)
	}
	return v
}

// schemaKeywordCases:每個支援關鍵字的正反例(ok/ng 是 x 的 JSON 值),由 TestSchemaKeywords 驗證。
var schemaKeywordCases = []struct {
	name   string
	x      map[string]any
	ok, ng []string // x 的 JSON 值
}{
	{"type string", map[string]any{"type": "string"}, []string{`""`, `"a"`}, []string{`1`, `null`, `true`, `[]`, `{}`}},
	{"type number", map[string]any{"type": "number"}, []string{`0`, `-1.5`, `1e308`}, []string{`"1"`, `null`, `true`, `[]`}},
	{"type integer:1.0 算、1.5 不算", map[string]any{"type": "integer"}, []string{`1`, `1.0`, `-3`, `1e20`}, []string{`1.5`, `0.1`, `"1"`}},
	{"type boolean", map[string]any{"type": "boolean"}, []string{`true`, `false`}, []string{`0`, `"true"`, `null`}},
	{"type null", map[string]any{"type": "null"}, []string{`null`}, []string{`0`, `""`, `false`, `{}`}},
	{"type array", map[string]any{"type": "array"}, []string{`[]`, `[1,"a"]`}, []string{`{}`, `"[]"`, `null`}},
	{"type object", map[string]any{"type": "object"}, []string{`{}`, `{"a":1}`}, []string{`[]`, `null`, `"{}"`}},
	{"properties + required", map[string]any{"type": "object", "required": []any{"a"}, "properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "integer"}}},
		[]string{`{"a":"x"}`, `{"a":"x","b":2}`, `{"a":"x","c":true}`}, []string{`{}`, `{"b":2}`, `{"a":1}`, `{"a":"x","b":"2"}`, `{"a":"x","b":null}`}},
	{"additionalProperties false", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		[]string{`{}`, `{"a":"x"}`}, []string{`{"b":1}`, `{"a":"x","__proto__":1}`, `{"toString":1}`, `{"constructor":{}}`}},
	{"additionalProperties true", map[string]any{"type": "object", "additionalProperties": true}, []string{`{"z":1}`}, []string{`[]`}},
	{"items", map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, []string{`[]`, `[1,2]`}, []string{`[1,"2"]`, `[1.5]`, `[null]`}},
	{"minItems / maxItems", map[string]any{"type": "array", "minItems": 1, "maxItems": 2}, []string{`[1]`, `[1,2]`}, []string{`[]`, `[1,2,3]`}},
	{"enum", map[string]any{"enum": []any{"a", 1, nil, []any{1, "x"}, map[string]any{"k": true}}},
		[]string{`"a"`, `1`, `1.0`, `null`, `[1,"x"]`, `{"k":true}`}, []string{`"b"`, `2`, `[1]`, `[1,"x",2]`, `["x",1]`, `{"k":false}`, `{"k":true,"j":1}`, `{}`, `false`}},
	{"const", map[string]any{"type": "string", "const": "TW"}, []string{`"TW"`}, []string{`"tw"`, `"TW "`, `1`}},
	{"const 物件", map[string]any{"const": map[string]any{"a": []any{1, nil}}}, []string{`{"a":[1,null]}`}, []string{`{"a":[1]}`, `{"a":[1,null],"b":1}`, `[1,null]`, `null`}},
	{"anyOf", map[string]any{"anyOf": []any{map[string]any{"type": "string", "const": "TW"}, map[string]any{"type": "string", "const": "US"}}},
		[]string{`"TW"`, `"US"`}, []string{`"JP"`, `null`, `1`}},
	{"minimum / maximum", map[string]any{"type": "integer", "minimum": 1, "maximum": 250}, []string{`1`, `250`, `100`}, []string{`0`, `251`, `-1`, `250.5`}},
	{"exclusiveMinimum", map[string]any{"type": "number", "exclusiveMinimum": 0}, []string{`5e-324`, `0.1`, `100`}, []string{`0`, `-0`, `-5e-324`, `-1`}},
	{"minLength / maxLength 以 code point 計(typebox 1.x)", map[string]any{"type": "string", "minLength": 2, "maxLength": 3},
		[]string{`"ab"`, `"abc"`, `"🙂🙂"`, `"🙂🙂🙂"`, `"中文字"`, `"\u3000\u3000"`}, []string{`"a"`, `"🙂"`, `"abcd"`, `"🙂🙂🙂🙂"`, `""`}},
	{"pattern 代號字元集", map[string]any{"type": "string", "pattern": "^[A-Za-z0-9.^=_-]+$"},
		[]string{`"2330"`, `"BRK.B"`, `"^TWII"`, `"A=B_c-1"`}, []string{`""`, `"2330\n"`, `"23 30"`, `"２３３０"`, `"🙂"`, `"a/b"`, `"中"`}},
	{"pattern 的 . 不吃行結束字元", map[string]any{"type": "string", "pattern": "^a.c$"}, []string{`"abc"`, `"a🙂c"`, `"a\u3000c"`}, []string{`"a\nc"`, `"a\rc"`, `"a\u2028c"`, `"a\u2029c"`}},
	{"pattern 的 \\s 是 JS 空白", map[string]any{"type": "string", "pattern": `^\s+$`}, []string{`" "`, `"\u3000"`, `"\ufeff"`, `"\u00a0"`}, []string{`"a"`, `"\u0085"`, `""`}},
	{"description / title / default 不影響驗證", map[string]any{"type": "string", "description": "d", "title": "t", "default": 1}, []string{`"x"`}, []string{`1`}},
	{"沒有 type 時關鍵字只套用到對應型別", map[string]any{"minLength": 2, "minimum": 5, "minItems": 1}, []string{`"ab"`, `7`, `[1]`, `null`, `true`, `{}`}, []string{`"a"`, `4`, `[]`}},
}

func TestSchemaKeywords(t *testing.T) {
	for _, c := range schemaKeywordCases {
		t.Run(c.name, func(t *testing.T) {
			cs := mustCompile(t, objWith(c.x))
			for _, s := range c.ok {
				if !cs.Check(decode(t, `{"x":`+s+`}`)) {
					t.Errorf("%s 應該通過", s)
				}
			}
			for _, s := range c.ng {
				if cs.Check(decode(t, `{"x":`+s+`}`)) {
					t.Errorf("%s 不該通過", s)
				}
			}
		})
	}
}

func TestSchemaJSONNumber(t *testing.T) {
	cs := mustCompile(t, objWith(map[string]any{"type": "integer", "minimum": 1, "maximum": 250}))
	for v, want := range map[json.Number]bool{
		"1": true, "250": true, "1.0": true, "2.5e1": true, "0": false, "1.5": false, "abc": false, "1e400": false,
		// 審查:json.Unmarshal 把 "null" 解成 0、會吃前後空白;這些都不是 JSON 數字
		"null": false, " 1 ": false, "+1": false, "01": false, ".5": false, "0x1p4": false, "Inf": false, "NaN": false, "": false,
	} {
		if got := cs.Check(map[string]any{"x": v}); got != want {
			t.Errorf("json.Number(%s) = %v,預期 %v", v, got, want)
		}
	}
	for _, x := range []map[string]any{{"type": "number"}, {"minimum": 0}, {"enum": []any{0}}, {"const": 0}} {
		if mustCompile(t, objWith(x)).Check(map[string]any{"x": json.Number("null")}) {
			t.Errorf("%v:json.Number(\"null\") 不該通過", x)
		}
	}
	en := mustCompile(t, objWith(map[string]any{"enum": []any{2}}))
	if !en.Check(map[string]any{"x": json.Number("2.0")}) || en.Check(map[string]any{"x": json.Number("3")}) {
		t.Error("enum 對 json.Number 要比數值")
	}
}

func TestSchemaRejectsAtCompile(t *testing.T) {
	bad := map[string]map[string]any{
		"$ref":                    {"$ref": "#/x"},
		"oneOf":                   {"oneOf": []any{map[string]any{"type": "string"}}},
		"patternProperties":       {"type": "object", "patternProperties": map[string]any{"^a": map[string]any{}}},
		"format":                  {"type": "string", "format": "email"},
		"allOf":                   {"allOf": []any{}},
		"not":                     {"not": map[string]any{}},
		"exclusiveMaximum":        {"type": "number", "exclusiveMaximum": 1},
		"exclusiveMinimum 布林":     {"type": "number", "exclusiveMinimum": true},
		"multipleOf":              {"type": "number", "multipleOf": 2},
		"prefixItems":             {"type": "array", "prefixItems": []any{}},
		"uniqueItems":             {"type": "array", "uniqueItems": true},
		"未知 type":                 {"type": "strnig"},
		"type bigint(typebox 擴充)": {"type": "bigint"},
		"type 陣列":                 {"type": []any{"string", "null"}},
		"布林子 schema":              {"type": "object", "properties": map[string]any{"a": true}},
		"additionalProperties 物件": {"type": "object", "additionalProperties": map[string]any{}},
		"additionalProperties 字串": {"type": "object", "additionalProperties": "false"},
		"minLength 小數":            {"type": "string", "minLength": 1.5},
		"minLength 負數":            {"type": "string", "minLength": -1},
		"maxItems 字串":             {"type": "array", "maxItems": "3"},
		"minimum 字串":              {"type": "number", "minimum": "1"},
		"required 字串":             {"type": "object", "required": "a"},
		"required 非字串元素":          {"type": "object", "required": []any{1}},
		"properties 陣列":           {"type": "object", "properties": []any{}},
		"items 陣列(tuple)":         {"type": "array", "items": []any{map[string]any{}}},
		"anyOf 空":                 {"anyOf": []any{}},
		"anyOf 元素非物件":             {"anyOf": []any{"string"}},
		"enum 非陣列":                {"enum": "a"},
		"pattern 非字串":             {"type": "string", "pattern": 1},
		"description 非字串":         {"type": "string", "description": 1},
		"巢狀不支援關鍵字":                {"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "format": "uri"}}},
		"anyOf 裡不支援關鍵字":           {"anyOf": []any{map[string]any{"oneOf": []any{}}}},
		"items 裡不支援關鍵字":           {"type": "array", "items": map[string]any{"$ref": "#"}},
		"typebox 內部鍵":             {"type": "string", "~kind": "String"},
	}
	for name, x := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileSchema(objWith(x)); err == nil {
				t.Errorf("%v 應該註冊失敗", x)
			}
		})
	}
	t.Run("根層不支援關鍵字", func(t *testing.T) {
		if _, err := CompileSchema(Schema{"type": "object", "additionalProperties": false, "oneOf": []any{}}); err == nil {
			t.Error("應該失敗")
		}
	})
	t.Run("無法 JSON 化", func(t *testing.T) {
		if _, err := CompileSchema(Schema{"type": "object", "additionalProperties": false, "default": func() {}}); err == nil {
			t.Error("應該失敗")
		}
		if _, err := CompileSchema(Schema{"type": "object", "additionalProperties": false, "minimum": math.NaN()}); err == nil {
			t.Error("應該失敗")
		}
	})
}

func TestSchemaRoot(t *testing.T) {
	for name, s := range map[string]Schema{
		"缺 additionalProperties":    {"type": "object", "properties": map[string]any{}},
		"additionalProperties true": {"type": "object", "additionalProperties": true},
		"additionalProperties 物件":   {"type": "object", "additionalProperties": map[string]any{}},
		"nil":                       nil,
		"空":                         {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileSchema(s)
			if !errors.Is(err, ErrSchemaNotClosed) {
				t.Fatalf("預期 ErrSchemaNotClosed,得到 %v", err)
			}
			// 註冊端前面接「工具 <名稱> 的 」(名稱 = q)
			if got := fmt.Sprintf("工具 %s 的 %v", "q", err); got != "工具 q 的 schema 必須是 additionalProperties:false" {
				t.Errorf("錯誤訊息不對:%q", got)
			}
		})
	}
	t.Run("根層不是 type:object", func(t *testing.T) {
		for _, s := range []Schema{{"additionalProperties": false}, {"type": "string", "additionalProperties": false}} {
			if _, err := CompileSchema(s); err == nil || errors.Is(err, ErrSchemaNotClosed) {
				t.Errorf("%v:預期根層 type 錯誤,得到 %v", s, err)
			}
		}
	})
	t.Run("Go literal 寫法(Schema 巢狀、[]string、int)", func(t *testing.T) {
		cs := mustCompile(t, Schema{"type": "object", "additionalProperties": false, "required": []string{"a"},
			"properties": map[string]Schema{"a": {"type": "integer", "maximum": 3}}})
		if !cs.Check(map[string]any{"a": 3.0}) || cs.Check(map[string]any{"a": 4.0}) || cs.Check(map[string]any{}) {
			t.Error("Go literal 寫法的 schema 驗證結果不對")
		}
	})
	t.Run("nil CompiledSchema 一律不過", func(t *testing.T) {
		var c *CompiledSchema
		if c.Check(map[string]any{}) {
			t.Error("nil 應該回 false")
		}
	})
}

func TestSchemaPatternSubset(t *testing.T) {
	// JS(u 旗標)與 RE2 不同或 JS 會丟錯的語法:註冊時就拒絕
	for _, p := range []string{
		`(?i)a`, `\pL`, `\p{L}`, `[[:alpha:]]`, `\Qa\E`, `\z`, `\A`, `(?P<n>a)`, `(?<n>a)`, `(?=a)`, `(?!a)`, `(?<=a)`, `(?<!a)`,
		`(a)\1`, `\1`, `\k<n>`, `\b`, `\B`, `\cA`, `\u0041`, `\x41`, `\0`, `\n`, `\e`, `\-`, `a{`, `a{1`, `a{x}`, `{1}`, `*a`, `a**`, `^*`, `a|*`,
		`[]`, `[^]`, `a)`, `(a`, `\`, `[a`, `]`, `}`, `[\d-z]`, `[a-\d]`, `[z-a]`, `[\S]`, `[\D]`, `[\W]`, `a{2,1}`, `a{1001}`, `[\b]`, `[\q]`,
	} {
		if _, err := CompileSchema(objWith(map[string]any{"type": "string", "pattern": p})); err == nil {
			t.Errorf("pattern %q 應該註冊失敗", p)
		}
	}
}

// 內部檢查真的 panic 時,公開的 Check 要回 false(fail closed)。
// 正常編譯產生不出 nil 的子節點,這裡手工做一個,讓 root.check 走到 nil 指標。
func TestSchemaCheckRecoversToFalse(t *testing.T) {
	cs := &CompiledSchema{root: &node{props: map[string]*node{"a": nil}, minLen: -1, maxLen: -1, minItems: -1, maxItems: -1}}
	arg := map[string]any{"a": 1.0}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("前提變了:root.check 應該 panic")
			}
		}()
		cs.root.check(arg)
	}()
	if cs.Check(arg) {
		t.Fatal("內部 panic 時 Check 應該回 false")
	}
}

func TestSchemaCheckNeverPanics(t *testing.T) {
	cs := mustCompile(t, Schema{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"a": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"a": map[string]any{"type": "array", "items": map[string]any{"type": "number"}}}},
			"b": map[string]any{"enum": []any{[]any{1, []any{2}}, map[string]any{"k": map[string]any{"j": nil}}}},
			"c": map[string]any{"const": []any{map[string]any{}}},
			"d": map[string]any{"anyOf": []any{map[string]any{"type": "integer", "minimum": 0}, map[string]any{"type": "string", "pattern": "^a.c$", "maxLength": 3}}},
			"e": map[string]any{"type": "number", "minimum": 1, "exclusiveMinimum": 0},
			// 沒有 type:各關鍵字只套到對應型別,怪型別會一路放行;這裡只要求不 panic,結果不斷言
			"f": map[string]any{"minLength": 1, "minimum": 1, "minItems": 1, "exclusiveMinimum": 0, "pattern": "a"},
		},
	})
	cyc := []any{nil}
	cyc[0] = cyc
	cm := map[string]any{}
	cm["a"] = cm
	type st struct{ A int }
	var nilMap map[string]any
	var nilSlice []any
	var nilPtr *st
	odd := []any{
		nil, math.NaN(), math.Inf(1), math.Inf(-1), cyc, cm, func() {}, make(chan int), map[any]any{1: 2}, nilMap, nilSlice, nilPtr,
		st{1}, &st{1}, 1, int64(1), float32(1), json.Number("abc"), json.Number("1e400"), "\xff\xfe", []byte("x"), [2]int{}, complex(1, 2),
		[]any{math.NaN(), math.Inf(1), cyc, cm}, map[string]any{"k": map[string]any{"j": cm}},
	}
	// 直接呼叫 root.check,中間沒有 Check 的 recover:任何 panic 都會讓測試失敗。
	// a–e 的設計讓每個怪值在每種包法下都不合法,所以也斷言回 false。
	for i, v := range odd {
		for _, k := range []string{"a", "b", "c", "d", "e"} {
			for j, arg := range []any{
				v,
				map[string]any{k: v},
				map[string]any{"a": map[string]any{"a": v}},
				map[string]any{"a": map[string]any{"a": []any{v, v}}},
				map[string]any{k: []any{v}},
				map[string]any{k: map[string]any{"k": v}},
			} {
				if cs.root.check(arg) {
					t.Errorf("#%d %s 包法 %d:應該不過", i, k, j)
				}
			}
		}
		cs.root.check(map[string]any{"f": v})
		cs.root.check(map[string]any{"f": []any{v}})
	}
	// NaN、±Inf 不是 number(typebox 的 IsNumber 是 Number.isFinite)
	num := mustCompile(t, objWith(map[string]any{"type": "number"}))
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if num.Check(map[string]any{"x": v}) {
			t.Errorf("%v 不該算 number", v)
		}
	}
	// 沒有 type 時也不放行非有限數(JSON 本來就產生不出來,Go 直接擋)
	loose := mustCompile(t, objWith(map[string]any{"minimum": 1}))
	if loose.Check(map[string]any{"x": math.NaN()}) || loose.Check(map[string]any{"x": json.Number("x")}) {
		t.Error("非有限數 / 壞掉的 json.Number 應該不過")
	}
}
