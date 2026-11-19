package entity

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// 切到雲端時遮敏感數字(spec 6.4):遮蔽集合裡的數字換成 MASK。
// 純函式;數字抽取與比對沿用 output.go。寧可多遮:小數字撞到集合也遮。

// MASK 是遮蔽後的替代字串。
const MASK = "▇▇"

// MaskSet 是遮蔽集合:只增不減,每個數字同時存原值與 trunc。順序照加入順序(與 JS Set 相同)。
type MaskSet struct {
	seen map[float64]bool
	nums []float64
}

// NewMaskSet 對應 MaskSet.from。
func NewMaskSet(nums ...float64) *MaskSet {
	s := &MaskSet{}
	s.Add(nums)
	return s
}

// Add 略過 NaN 與 ±Inf。
func (s *MaskSet) Add(nums []float64) {
	if s.seen == nil {
		s.seen = map[float64]bool{}
	}
	for _, n := range nums {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			continue
		}
		for _, v := range []float64{n, math.Trunc(n)} {
			if v == 0 {
				v = 0 // -0 與 0 視為同一個(JS Set 的 SameValueZero)
			}
			if !s.seen[v] {
				s.seen[v] = true
				s.nums = append(s.nums, v)
			}
		}
	}
}

func (s *MaskSet) Has(n float64) bool { return s.seen[n] }

func (s *MaskSet) Values() []float64 { return slices.Clone(s.nums) }

func (s *MaskSet) MarshalJSON() ([]byte, error) { return json.Marshal(s.Values()) }

func (s *MaskSet) UnmarshalJSON(b []byte) error {
	var nums []float64
	if err := json.Unmarshal(b, &nums); err != nil {
		return err
	}
	*s = MaskSet{}
	s.Add(nums)
	return nil
}

// hit:個位整數(1–9)只在集合裡剛好有它才算命中(Matches 的四捨五入會讓 4.5–5.5 都撞到 5);其餘沿用 Matches。
func hit(n float64, known []float64, isInt bool) bool {
	if isInt && math.Abs(n) < 10 {
		return n != 0 && slices.Contains(known, math.Abs(n))
	}
	return Matches(n, known)
}

// ExtractNumbers 刻意略過的(output guard 要少誤報,這裡要多遮):黏英文字(TWD600、600USD、1000shares)、
// K/M/B 後綴(450K、1.05M)、日期/序號裡的數字、個位整數。另跑一輪。
// 原式:/(?<![\d.])(?<!\d,)(\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?)(?!\d)(?:([KkMmBb])(?![A-Za-z]))?/g
// 後綴是可選的、而且後綴字母不是數字,不影響本體怎麼停,所以在 Go 裡手動接。
var looseRule = newLookRule(`(\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?)(?P<la>[^\d]|\z)`, func(s string, i int) bool {
	r, w, ok := prevRune(s, i)
	if !ok {
		return true
	}
	if isDigit(r) || r == '.' {
		return false
	}
	if r == ',' {
		return notDigitBefore(s, i-w)
	}
	return true
}, isDigit)

func init() { looseRule.skipAt = skipDigitCommaRun }

// skipDigitCommaRun:looseRule 的 lookbehind 在 start 不成立時,整段「數字,或前一字元是數字的逗號」都跳過。
// 這段裡每個位置的前一字元都是數字、或是「前面是數字」的逗號,lookbehind 一定不成立,跳過不會漏遮。
// 只跳數字的話,12345,234,234,… 每個 ,234 都會讓 RE2 從那裡把整串逗號數字再掃一次 → 二次方。
// 「,,」的第二個逗號前面不是數字,停在那裡:它後面的數字 lookbehind 成立,要照常比對。
func skipDigitCommaRun(s string, i int) int {
	for i < len(s) {
		c := s[i]
		if isDigit(rune(c)) || c == ',' && i > 0 && isDigit(rune(s[i-1])) {
			i++
			continue
		}
		break
	}
	return i
}

var suffixMult = map[byte]float64{'k': 1e3, 'K': 1e3, 'm': 1e6, 'M': 1e6, 'b': 1e9, 'B': 1e9}

func isASCIILetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

type span struct{ at, n int }

// spliceAll 把遞增、不重疊的 span(rune 索引)一次換成 MASK。
// 每命中一次就 slice 重組整串會變成 O(n) × 命中數 = 二次方;這裡單趟重建。
func spliceAll(rs []rune, spans []span) []rune {
	if len(spans) == 0 {
		return rs
	}
	out := make([]rune, 0, len(rs))
	last := 0
	for _, sp := range spans {
		out = append(out, rs[last:sp.at]...)
		out = append(out, []rune(MASK)...)
		last = sp.at + sp.n
	}
	return append(out, rs[last:]...)
}

// MaskText 把命中集合的數字 token 換成 MASK。
func MaskText(text string, set *MaskSet) string {
	known := set.Values()
	var spans []span
	for _, t := range ExtractNumbers(text) {
		if Matches(t.Value, known) || Matches(t.Base, known) {
			spans = append(spans, span{t.Index, utf8.RuneCountInString(t.Raw)})
		}
	}
	out := spliceAll([]rune(text), spans)
	h := Half(string(out))
	idx := runeIndexer{s: h}
	spans = spans[:0]
	looseRule.each(h, func(m []int) {
		digits := h[m[2]:m[3]]
		base := JSNumber(strings.ReplaceAll(digits, ",", ""))
		mult, hasMult := 0.0, false
		if e := m[1]; e < len(h) {
			if v, ok := suffixMult[h[e]]; ok && (e+1 == len(h) || !isASCIILetter(h[e+1])) {
				mult, hasMult = v, true
			}
		}
		if hit(base, known, !strings.Contains(digits, ".")) || hasMult && Matches(base*mult, known) {
			spans = append(spans, span{idx.index(m[0]), len(digits)})
		}
	})
	return string(spliceAll(out, spans))
}

// maskValue:toolCall args 裡的字串 MaskText,命中的數字換成 MASK 字串(歷史訊息,不再驗 schema)。
// Go 的 any 可能裝任意型別;不認得的先 JSON 正規化再遮,連 JSON 都轉不了就整個換成 MASK(fail-closed)。
func maskValue(v any, set *MaskSet) any {
	switch x := v.(type) {
	case nil, bool:
		return x
	case string:
		return MaskText(x, set)
	case float64:
		if hit(x, set.Values(), x == math.Trunc(x) && !math.IsInf(x, 0)) {
			return MASK
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = maskValue(e, set)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = maskValue(e, set)
		}
		return out
	}
	n, err := CloneJSON[any](v)
	if err != nil {
		return MASK
	}
	return maskValue(n, set)
}

// MaskMessages 只動 PrivacyOf(m) == local 的訊息(沒標記也是 local);回新切片,不改輸入。
//   - local 訊息依角色用欄位白名單重建(struct 本身就是白名單;多餘欄位進不來);ErrorMessage 用 MaskText。
//   - local toolResult:hideResult(toolName) 回 ok → 整則 content 換成那一行。
//   - text、thinking → MaskText;toolCall.Args → maskValue。thinking 也遮:換模型時會被當一般文字送出。
//   - 無法辨識的 part、無法辨識的 role → 丟掉(fail-closed)。
//
// hideResult 可為 nil。它若 panic 會往上傳,由呼叫端(送出管線)當成遮蔽失敗處理。
func MaskMessages(messages []Message, set *MaskSet, hideResult func(tool string) (string, bool)) []Message {
	maskParts := func(ps []Part) []Part {
		out := []Part{}
		for _, p := range ps {
			switch p.Type {
			case PartText:
				out = append(out, Part{Type: PartText, Text: MaskText(p.Text, set)})
			case PartThinking:
				out = append(out, Part{Type: PartThinking, Text: MaskText(p.Text, set), Signature: p.Signature})
			case PartToolCall:
				args, _ := maskValue(p.Args, set).(map[string]any)
				out = append(out, Part{Type: PartToolCall, ID: p.ID, Name: p.Name, Args: args, Signature: p.Signature})
			}
		}
		return out
	}
	out := []Message{}
	for _, m := range messages {
		if PrivacyOf(m) == Remote {
			out = append(out, m)
			continue
		}
		base := Message{Role: m.Role, Privacy: m.Privacy, Ts: m.Ts}
		switch m.Role {
		case RoleToolResult:
			base.ToolCallID, base.ToolName, base.IsError = m.ToolCallID, m.ToolName, m.IsError
			if hideResult != nil {
				if line, ok := hideResult(m.ToolName); ok {
					base.Content = []Part{{Type: PartText, Text: line}}
					out = append(out, base)
					continue
				}
			}
			base.Content = maskParts(m.Content)
		case RoleUser:
			base.Content = maskParts(m.Content)
		case RoleAssistant:
			base.Content = maskParts(m.Content)
			base.Provider, base.StopReason = m.Provider, m.StopReason
			if m.Usage != nil {
				u := *m.Usage
				base.Usage = &u
			}
			base.ErrorMessage = MaskText(m.ErrorMessage, set)
		default:
			continue
		}
		out = append(out, base)
	}
	return out
}

// DropLocal 是 fail-closed 退路:local(含沒標記)的訊息整則丟掉。
func DropLocal(messages []Message) []Message {
	out := []Message{}
	for _, m := range messages {
		if PrivacyOf(m) == Remote {
			out = append(out, m)
		}
	}
	return out
}
