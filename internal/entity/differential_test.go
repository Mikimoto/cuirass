//go:build test

package entity

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"
)

// golden 測試:同一批隨機 / 對抗輸入,結果要與凍結的 golden 逐字相同。
// 答案凍結在 testdata/golden(見 golden_test.go 的 loadGolden)。

var pieces = []string{
	"0", "1", "5", "9", "12", "600", "1000", "12345", "2024", "2330", "19", "20", "０", "１", "５", "，", "．",
	",", ".", " ", "  ", "\u3000", "\u00a0", "\n", "\r", "\r\n", "\t", "\u2028",
	"A", "Z", "_", "a", "x", "K", "k", "M", "B", "b", "e", "USD", "TWD", "shares",
	"=", "\"", "'", "\\", "\\\"", ":", "/", "@", "-", "+", "%", "％", "(", ")", "、",
	"年", "月", "日", "天", "週", "周", "個月", "萬", "億", "千元", "股", "持有",
	"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "DB_", "API_KEY=", "PATH=",
	"sk-", "sk-ant-", "sk-proj-", "abcdefghijklmnop", "eyJ", "xoxb-", "AKIA", "ghp_", "AIza", "AAAAAAAAAAAAAAAA",
	"http://", "https://", "postgres://", "u:p@", "Authorization", "authorization: ", "Proxy-Authorization",
	"-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----", "a@b.com", "🙂", "1.05", "450", "0.5", "2.5",
	// 審查 Minor 5:其餘 JS 空白、大小寫摺疊陷阱;I3:U+2029、控制字元、<>&、字面的 \u2028
	"\v", "\f", "\b", "\x00", "\x1b", "\x7f", "\u0085", "\u009f", "\ufeff", "\u1680", "\u2000", "\u200a", "\u202f", "\u205f", "\u2029", "\u180e", "\u200b",
	"AUTHORİZATION", "authorızation", "\u212a", "𝟏", "Ｋ", "<", ">", "&", `\u2028`, "API_KEY= ", "Authorization: Bearer ", "DB_PASSWORD = ",
}

var setPool = []float64{0.5, 1, 2.5, 3, 5, 12.5, 600, 600.5, 1000, 1000.5, 1050, 2024, 12345, 450000, 1050000, 0.005, 1.005, 1e21, 2.675}

type goldenCase struct {
	Fn    string          `json:"fn"`
	Text  string          `json:"text"`
	Set   []float64       `json:"set,omitempty"`
	Value float64         `json:"value"`
	Spec  json.RawMessage `json:"spec,omitempty"`
}

func randText(r *rand.Rand) string {
	var b strings.Builder
	for range r.IntN(40) {
		b.WriteString(pieces[r.IntN(len(pieces))])
	}
	return b.String()
}

func randSet(r *rand.Rand) []float64 {
	s := []float64{}
	for range 1 + r.IntN(4) {
		s = append(s, setPool[r.IntN(len(setPool))])
	}
	return s
}

func goResult(c goldenCase) any {
	switch c.Fn {
	case "redact":
		text, n := RedactCount(c.Text, nil)
		return map[string]any{"text": text, "count": n}
	case "mask":
		return MaskText(c.Text, NewMaskSet(c.Set...))
	case "extract":
		runes := []rune(c.Text)
		out := [][]any{}
		for _, tk := range ExtractNumbers(c.Text) {
			u16 := len(utf16.Encode(runes[:tk.Index])) // 換成 JS 的 UTF-16 索引再比
			out = append(out, []any{tk.Raw, tk.Value, tk.Base, tk.Unit, tk.Line, u16})
		}
		return out
	case "matches":
		return Matches(c.Value, c.Set)
	case "privacy":
		return privacyCase(c.Text)
	case "stringify":
		b, _ := Stringify(map[string]string{"s": c.Text})
		return string(b)
	case "redactJSON":
		b, _ := Stringify(map[string]string{"out": c.Text})
		text, n := RedactCount(string(b), nil)
		return map[string]any{"text": text, "count": n}
	}
	return nil
}

// normalize 把兩邊都轉成 any 再比(數字都是 float64)。
func normalize(t *testing.T, b []byte) any {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDifferentialGolden(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var cases []goldenCase
	for range 3000 {
		text := randText(r)
		cases = append(cases, goldenCase{Fn: "redact", Text: text}, goldenCase{Fn: "mask", Text: text, Set: randSet(r)}, goldenCase{Fn: "extract", Text: text},
			goldenCase{Fn: "stringify", Text: text}, goldenCase{Fn: "redactJSON", Text: text})
	}
	for range 3000 {
		cases = append(cases, goldenCase{Fn: "matches", Value: setPool[r.IntN(len(setPool))] * float64(1+r.IntN(3)) / float64(1+r.IntN(2)), Set: randSet(r)})
	}
	// 手挑的邊界:toFixed 剛好一半、指數表示、各條 lookaround
	for _, v := range []float64{3, 2, 0.5, 1, 1.01, 2.68, 2.67, 1e-7, 1.5e-7, 1e21} {
		cases = append(cases, goldenCase{Fn: "matches", Value: v, Set: []float64{2.5, 0.5, 1.005, 2.675, 1.5e-7, 1e21}})
	}
	for _, s := range []string{
		"1,2345", "1,234,5678", "1.,5", "a1,000", "TWD1,000K", "1000Kg", "5萬", "2024-11-10", "2024-1-123", "第 3 名\n 1. 項", "x\r2. y",
		"PATH=FOO_KEY=bar", "KEY='x'ABC_KEY=1", "KEY= ", "1abc://u:p@h", "a://u:p@b://u:p@c", "Authorization: Bearer a b c",
		"sk-aaaaaaaaaaaaaaaa-", "https://x.y/1000\u30001000", "１，０００．５ 萬", "🙂1000🙂", "12:30 1000",
	} {
		cases = append(cases, goldenCase{Fn: "redact", Text: s}, goldenCase{Fn: "mask", Text: s, Set: []float64{1000, 5, 1, 1234, 50000, 3}}, goldenCase{Fn: "extract", Text: s})
	}
	want := loadGolden(t, cases)
	fails := 0
	for i, c := range cases {
		got, _ := json.Marshal(goResult(c))
		if !reflectEqual(normalize(t, got), normalize(t, want[i])) {
			fails++
			if fails <= 10 {
				t.Errorf("%s(%q, %v):\n  go: %s\n  golden: %s", c.Fn, c.Text, c.Set, got, want[i])
			}
		}
	}
	t.Logf("差異測試 %d 筆,不一致 %d 筆", len(cases), fails)
}

func reflectEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
