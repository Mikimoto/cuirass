//go:build test

package entity

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// auditKey 是測試用的假 key。
const auditKey = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"

func TestFilterAuditArgs(t *testing.T) {
	t.Run("只留 keys 內的欄位", func(t *testing.T) {
		got := FilterAuditArgs(map[string]any{"cmd": "rm -rf", "q": "x"}, []string{"q"})
		if !reflect.DeepEqual(got, map[string]any{"q": "x"}) {
			t.Fatal(got)
		}
	})
	t.Run("keys 未給回 {}", func(t *testing.T) {
		got := FilterAuditArgs(map[string]any{"q": "x"}, nil)
		if got == nil || len(got) != 0 {
			t.Fatal(got)
		}
	})
	t.Run("字串先 redact", func(t *testing.T) {
		if q := FilterAuditArgs(map[string]any{"q": auditKey}, []string{"q"})["q"].(string); !strings.Contains(q, "[REDACTED") {
			t.Fatal(q)
		}
	})
	t.Run("非原始型別丟棄、字串截 200", func(t *testing.T) {
		r := FilterAuditArgs(map[string]any{"a": map[string]any{"x": 1}, "b": []any{"t"}, "c": 5.0, "d": true, "e": "a\n" + strings.Repeat("x", 500)},
			[]string{"a", "b", "c", "d", "e"})
		e, _ := r["e"].(string)
		if len(r) != 3 || r["c"] != 5.0 || r["d"] != true || len(e) != 200 || strings.Contains(e, "\n") {
			t.Fatal(r)
		}
	})
	t.Run("Go 整數型別與 json.Number 也算數字", func(t *testing.T) {
		r := FilterAuditArgs(map[string]any{"i": 3, "u": uint8(4), "n": json.Number("5"), "nil": nil, "f": func() {}}, []string{"i", "u", "n", "nil", "f"})
		if !reflect.DeepEqual(r, map[string]any{"i": 3, "u": uint8(4), "n": json.Number("5")}) {
			t.Fatal(r)
		}
	})
}

func TestOneLine(t *testing.T) {
	if got := OneLine(" \x1b[31ma\r\n\x7f\u0085b\u00a0\t", 0); got != "[31ma b" {
		t.Fatalf("%q", got)
	}
	if got := OneLine("a b c", 2); got != "a " {
		t.Fatalf("先 trim 再截,結尾空白保留:%q", got)
	}
}

// filterAuditArgs、oneLine 的差異測試。字串截 200 時若切在代理對中間,golden 留下孤立高代理
// (經 JSON 到 Go 變 U+FFFD),Go 的 SliceUTF16 直接不收那個字元;只有這一種差異允許。
func TestAuditGolden(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	var texts []string
	for range 2000 {
		texts = append(texts, randText(r))
	}
	for range 300 {
		texts = append(texts, strings.Repeat(randText(r)+"🙂", 1+r.IntN(20)))
	}
	texts = append(texts, strings.Repeat("x", 199)+"🙂", auditKey, "a\n"+strings.Repeat("x", 500), "\x00\x1f\x7f\u009f", "\ufeff a \u3000")
	var cases []goldenCase
	for i, s := range texts {
		cases = append(cases, goldenCase{Fn: "oneLine", Text: s}, goldenCase{Fn: "audit", Text: s, Value: setPool[i%len(setPool)]})
	}
	want := loadGolden(t, cases)
	fails := 0
	for i, c := range cases {
		var got any
		if c.Fn == "oneLine" {
			got = OneLine(c.Text, 0)
		} else {
			got = FilterAuditArgs(map[string]any{"q": c.Text, "n": c.Value, "o": map[string]any{"q": c.Text}, "a": []any{c.Text}, "b": false},
				[]string{"q", "n", "o", "a", "b", "missing", "toString"})
		}
		g, _ := json.Marshal(got)
		gv, tv := normalize(t, g), normalize(t, want[i])
		if c.Fn == "audit" {
			gm, tm := gv.(map[string]any), tv.(map[string]any)
			if tq := tm["q"].(string); strings.HasSuffix(tq, "\ufffd") && tq == gm["q"].(string)+"\ufffd" {
				tm["q"] = gm["q"] // 只容許「切在代理對中間」這一種差異
			}
		}
		if !reflectEqual(gv, tv) {
			fails++
			if fails <= 10 {
				t.Errorf("%s(%q):\n  go: %s\n  golden: %s", c.Fn, c.Text, g, want[i])
			}
		}
	}
	t.Logf("audit 差異測試 %d 筆,不一致 %d 筆", len(cases), fails)
}
