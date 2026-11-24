package openai

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// Apple fm(`fm serve`)相容:送出前改 payload(schema allowlist、user content 陣列→字串),
// 以及 8192 context 的長度控制(從最舊一輪丟)。工具參數驗證用原 schema,不受影響。
// 只有 OpenAI 相容 provider 用得到,所以放同一個套件。

var fmKeep = []string{"type", "enum", "description", "required"}

// fmSchema:fm 拒收 anyOf+const(400)、pattern(500)與沒有 type 的參數(400):只留 allowlist 欄位;
// anyOf 全 const → enum;其他 anyOf 取第一個非 null 的選項(例如可空字串 → 字串);
// 仍沒有 type 就依 enum 值推(數字 → number、布林 → boolean),推不出就拿掉 enum、給 string。
// 已有 type 時 enum 只留型別相容的值(integer 算 number),一個都不剩就拿掉 enum。
// 送 fm 的 schema 只是提示,參數驗證仍用原 schema。schema 是 JSON 解碼後的值(map[string]any / []any …)。
func fmSchema(schema any) any {
	s, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	out := map[string]any{}
	if anyOf, ok := s["anyOf"].([]any); ok && len(anyOf) > 0 {
		for _, a := range anyOf {
			if m, ok := a.(map[string]any); ok && m["type"] != "null" {
				if base, ok := fmSchema(m).(map[string]any); ok {
					out = base
				}
				break
			}
		}
		enum := []any{}
		for _, a := range anyOf {
			m, ok := a.(map[string]any)
			if !ok {
				enum = nil
				break
			}
			c, has := m["const"]
			if !has {
				enum = nil
				break
			}
			enum = append(enum, c)
		}
		if enum != nil { // 型別由下面的 fallback 依列舉值決定
			out = map[string]any{"enum": enum}
		}
	}
	for _, k := range fmKeep {
		if v, ok := s[k]; ok {
			out[k] = v
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		p := map[string]any{}
		for k, v := range props {
			p[k] = fmSchema(v)
		}
		out["properties"] = p
	}
	if items, ok := s["items"]; ok {
		out["items"] = fmSchema(items)
	}
	// 合併分支與外層欄位後,type 和 enum 可能來自不同地方:只留和 type 相容的值(integer 配數字算相容,
	// null 等其他值拿掉);一個都不剩就沒有合法值,拿掉 enum。
	if t, ok := out["type"].(string); ok {
		if enum, ok := out["enum"].([]any); ok {
			kept := []any{}
			for _, v := range enum {
				if et := fmEnumType([]any{v}); et != "" && (et == t || t == "integer" && et == "number") {
					kept = append(kept, v)
				}
			}
			if len(kept) == 0 {
				delete(out, "enum")
			} else {
				out["enum"] = kept
			}
		}
	}
	if out["type"] == nil { // fm 一定要 type:先照列舉值推,推不出(任意型別、混合列舉)就給字串
		t := ""
		if enum, ok := out["enum"].([]any); ok {
			t = fmEnumType(enum)
		}
		if t == "" {
			delete(out, "enum") // 字串型別配非字串列舉會沒有合法值
			t = "string"
		}
		out["type"] = t
	}
	return out
}

// fmEnumType:列舉值全是同一種純量時回它的 JSON 型別,否則(空、混合、null、陣列、物件)回 ""。
func fmEnumType(enum []any) string {
	t := ""
	for _, v := range enum {
		var vt string
		switch v.(type) {
		case string:
			vt = "string"
		case bool:
			vt = "boolean"
		case float64, json.Number, int, int64:
			vt = "number"
		default:
			return ""
		}
		if t != "" && t != vt {
			return ""
		}
		t = vt
	}
	return t
}

// fmPayload:OpenAI request body → fm 可接受的版本;不改輸入。
func fmPayload(body map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	if msgs, ok := body["messages"].([]any); ok {
		next := make([]any, len(msgs))
		for i, m := range msgs {
			next[i] = m
			mm, ok := m.(map[string]any)
			if !ok || mm["role"] != "user" {
				continue
			}
			parts, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			var b strings.Builder
			for _, c := range parts {
				if cm, ok := c.(map[string]any); ok {
					if t, ok := cm["text"].(string); ok {
						b.WriteString(t)
					}
				}
			}
			copied := map[string]any{}
			for k, v := range mm {
				copied[k] = v
			}
			copied["content"] = b.String()
			next[i] = copied
		}
		out["messages"] = next
	}
	if tools, ok := body["tools"].([]any); ok {
		next := make([]any, len(tools))
		for i, t := range tools {
			next[i] = t
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tm["function"].(map[string]any)
			if !ok {
				continue
			}
			f2 := map[string]any{}
			for k, v := range fn {
				f2[k] = v
			}
			f2["parameters"] = fmSchema(fn["parameters"])
			t2 := map[string]any{}
			for k, v := range tm {
				t2[k] = v
			}
			t2["function"] = f2
			next[i] = t2
		}
		out["tools"] = next
	}
	return out
}

var longRun = regexp.MustCompile(`[A-Za-z0-9_\-+/=%.]{16,}`)

// estimateTokens:依 fm serve 實測的粗估:URL / base64 類長串 1.5 字元/token;其餘 ASCII 字母與空白
// 4 字元/token;ASCII 數字與標點、非 ASCII(中文)各 1 token。寧可高估:低估會讓 fm 回 400。
func estimateTokens(text string) int {
	n := 0
	rest := longRun.ReplaceAllStringFunc(text, func(run string) string {
		n += int(math.Ceil(float64(len(run)) / 1.5))
		return ""
	})
	words := 0
	for _, r := range rest {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == ' ' {
			words++
		} else {
			n++
		}
	}
	return n + int(math.Ceil(float64(words)/4))
}

// fmBudget:送給 fm 的輸入預算:扣掉輸出上限,再留 10% 給估算誤差。
func fmBudget(contextWindow, maxTokens int) int {
	return int(math.Floor(float64(contextWindow-maxTokens) * 0.9))
}

func msgTokens(m entity.Message) int {
	b, _ := entity.Stringify(m.Content)
	if m.Content == nil {
		b = []byte("[]")
	}
	return estimateTokens(string(b))
}

// trimTurns:一輪 = 某則 user 到下一則 user 之前。估算 = fixed + 各訊息估算;最後一則帶 usage.input 的 assistant
// 之前的部分以真實值取代(取兩者較大)。超過 budget 從最舊一輪丟,最少留最後一輪。
func trimTurns(messages []entity.Message, budget, fixed int) ([]entity.Message, int) {
	cost := make([]int, len(messages))
	for i, m := range messages {
		cost[i] = msgTokens(m)
	}
	sum := func(from, to int) int {
		s := 0
		for _, c := range cost[from:to] {
			s += c
		}
		return s
	}
	a := -1
	for i, m := range messages {
		if m.Role == entity.RoleAssistant && m.Usage != nil {
			a = i
		}
	}
	anchored := 0
	if a >= 0 {
		anchored = messages[a].Usage.Input + sum(a, len(messages))
	}
	total := func(from int) int {
		est := fixed + sum(from, len(messages))
		// 丟掉的訊息在 anchor 之前時,從真實值扣掉它們的估算;anchor 本身被丟就不用。
		if from <= a {
			return max(est, anchored-sum(0, from))
		}
		return est
	}
	var starts []int
	for i, m := range messages {
		if m.Role == entity.RoleUser {
			starts = append(starts, i)
		}
	}
	dropped := 0
	// 第一則 user 之前的訊息算第一輪的一部分
	for dropped+1 < len(starts) {
		from := 0
		if dropped > 0 {
			from = starts[dropped]
		}
		if total(from) <= budget {
			break
		}
		dropped++
	}
	if dropped == 0 {
		return messages, 0
	}
	return messages[starts[dropped]:], dropped
}

const cut = "…(資料過長,已截斷)"

// shrinkToolResults:trimTurns 後仍超過 budget(當前輪單一工具結果就太大):從估算最大的 toolResult 開始,
// 把它的文字截到剛好塞得下並加標記。只換 toolResult 的 text 部分;不改輸入。
// 截斷長度用 UTF-16 code unit(同 JS 的 text.slice)。
func shrinkToolResults(messages []entity.Message, budget, fixed int) []entity.Message {
	cost := make([]int, len(messages))
	excess := fixed - budget
	for i, m := range messages {
		cost[i] = msgTokens(m)
		excess += cost[i]
	}
	if excess <= 0 {
		return messages
	}
	out := slices.Clone(messages)
	var order []int
	for i, m := range messages {
		if m.Role == entity.RoleToolResult {
			order = append(order, i)
		}
	}
	// JS 的 sort 是穩定排序
	slices.SortStableFunc(order, func(x, y int) int { return cost[y] - cost[x] })
	for _, i := range order {
		if excess <= 0 {
			break
		}
		m := messages[i]
		var text strings.Builder
		var others []entity.Part
		for _, c := range m.Content {
			if c.Type == entity.PartText {
				text.WriteString(c.Text)
			} else {
				others = append(others, c)
			}
		}
		units := utf16.Encode([]rune(text.String()))
		make_ := func(k int) entity.Message {
			n := m
			n.Content = append([]entity.Part{{Type: entity.PartText, Text: string(utf16.Decode(units[:k])) + cut}}, others...)
			return n
		}
		target := cost[i] - excess
		// 二分找最長且估算 ≤ target 的前綴;連空字串都塞不下就留標記(k = 0)。
		lo, hi := 0, len(units)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if msgTokens(make_(mid)) <= target {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		next := make_(lo)
		if msgTokens(next) >= cost[i] {
			continue
		}
		out[i] = next
		excess -= cost[i] - msgTokens(next)
	}
	return out
}
