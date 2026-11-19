//go:build test

package entity

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"testing"
)

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %#v\nwant: %#v", got, want)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := Stringify(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fromJSON 模擬「不合型別」的訊息:Go 只能從 JSON 解進來。
func fromJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func msg(role string, privacy Privacy, content []Part) Message {
	return Message{Role: role, Privacy: privacy, Ts: 1, Content: content}
}

func txt(s string) []Part { return []Part{{Type: PartText, Text: s}} }

func TestMask(t *testing.T) {
	setOf := NewMaskSet

	t.Run("maskText:命中集合的數字換成 MASK(千分位、trunc)", func(t *testing.T) {
		eq(t, MaskText("損益 +12,345 元,均價 1000.5", setOf(12345, 1000)), "損益 +"+MASK+" 元,均價 "+MASK)
	})
	t.Run("maskText:同一數字出現多次都遮、不在集合的不動", func(t *testing.T) {
		eq(t, MaskText("600 與 600,現價 1050", setOf(600)), MASK+" 與 "+MASK+",現價 1050")
	})
	t.Run("MaskSet:同時存原值與 trunc,略過非有限值;toJSON 與 from 來回", func(t *testing.T) {
		s := setOf(600.5, math.NaN())
		v := s.Values()
		slices.Sort(v)
		eq(t, v, []float64{600, 600.5})
		eq(t, s.Has(600), true)
		b, _ := json.Marshal(s)
		var back []float64
		_ = json.Unmarshal(b, &back)
		v = NewMaskSet(back...).Values()
		slices.Sort(v)
		eq(t, v, []float64{600, 600.5})
	})
	t.Run("maskText:萬、全形數字、黏英文字、個位整數", func(t *testing.T) {
		eq(t, MaskText("損益 45萬", setOf(450000)), "損益 "+MASK+"萬")
		mustContain(t, MaskText("持有 １，０００ 股", setOf(1000)), MASK)
		eq(t, MaskText("TWD600、600USD、450K、1.05M、1000shares", setOf(600, 450000, 1050000, 1000)),
			"TWD"+MASK+"、"+MASK+"USD、"+MASK+"K、"+MASK+"M、"+MASK+"shares")
		eq(t, MaskText("持有 5 股,第 3 名", setOf(5)), "持有 "+MASK+" 股,第 3 名")
	})

	t.Run("local 訊息:text、thinking、toolCall 參數都遮", func(t *testing.T) {
		m := msg(RoleAssistant, Local, []Part{
			{Type: PartThinking, Text: "損益 12345"},
			{Type: PartText, Text: "損益 12,345"},
			{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{
				"q": "持有 1000 股", "target": 600.0, "symbol": "2330", "n": []any{1000.0, map[string]any{"x": 600.0}}, "lookback": 20.0,
			}},
		})
		out := MaskMessages([]Message{m}, setOf(12345, 1000, 600), nil)
		j := jsonOf(t, out)
		mustNotContain(t, j, "12345")
		mustNotContain(t, j, "12,345")
		eq(t, out[0].Content[2].Args, map[string]any{
			"q": "持有 " + MASK + " 股", "target": MASK, "symbol": "2330", "n": []any{MASK, map[string]any{"x": MASK}}, "lookback": 20.0,
		})
	})
	t.Run("remote 不遮", func(t *testing.T) {
		msgs := []Message{msg(RoleAssistant, Remote, txt("現價 1000"))}
		eq(t, MaskMessages(msgs, setOf(1000), nil), msgs)
	})
	t.Run("沒有隱私標記的訊息當 local,會被遮", func(t *testing.T) {
		out := MaskMessages([]Message{{Role: RoleUser, Content: txt("持有 1234 股"), Ts: 1}}, NewMaskSet(1234), nil)
		mustNotContain(t, TextOf(out[0]), "1234")
	})
	t.Run("toolCall 參數字串也遮", func(t *testing.T) {
		m := msg(RoleAssistant, Local, []Part{{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{"q": "1234"}}})
		mustNotContain(t, jsonOf(t, MaskMessages([]Message{m}, setOf(1234), nil)), "1234")
	})
	t.Run("遮蔽後 toolCall 的 signature 保留(Gemini 重送要用;不是使用者資料)", func(t *testing.T) {
		m := msg(RoleAssistant, Local, []Part{{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{}, Signature: "SIG"}})
		eq(t, MaskMessages([]Message{m}, setOf(1), nil)[0].Content[0].Signature, "SIG")
	})
	t.Run("toolCall 數字參數也遮", func(t *testing.T) {
		m := msg(RoleAssistant, Local, []Part{{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{"target": 1234.5}}})
		eq(t, MaskMessages([]Message{m}, setOf(1234.5), nil)[0].Content[0].Args["target"], any(MASK))
	})
	t.Run("hideResult 回字串 → local toolResult 整則換成那一行;回 undefined 則照一般遮", func(t *testing.T) {
		tr := func(p Privacy, tool string) Message {
			m := msg(RoleToolResult, p, txt(`{"pnl":12345}`))
			m.ToolName = tool
			return m
		}
		hide := func(tool string) (string, bool) { return "(明細已隱藏)", tool == "portfolio" }
		eq(t, MaskMessages([]Message{tr(Local, "portfolio")}, setOf(12345), hide)[0].Content, txt("(明細已隱藏)"))
		eq(t, TextOf(MaskMessages([]Message{tr(Local, "other")}, setOf(12345), hide)[0]), `{"pnl":`+MASK+`}`)
		rr := tr(Remote, "portfolio")
		eq(t, MaskMessages([]Message{rr}, setOf(12345), hide), []Message{rr})
	})
	t.Run("不改輸入", func(t *testing.T) {
		m := msg(RoleAssistant, Local, []Part{{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{"a": 1000.0}}, {Type: PartText, Text: "1000"}})
		before := jsonOf(t, m)
		MaskMessages([]Message{m}, setOf(1000), nil)
		eq(t, jsonOf(t, m), before)
	})
	t.Run("dropLocal:local 與沒標記的丟掉,只留 remote", func(t *testing.T) {
		a, b, c := msg(RoleUser, Local, txt("a")), msg(RoleUser, "", txt("b")), msg(RoleUser, Remote, txt("c"))
		eq(t, DropLocal([]Message{a, b, c}), []Message{c})
	})
	t.Run("local 未知 part、text 不是字串的 part 丟掉", func(t *testing.T) {
		// 調整:Go 的 Part.Text 一定是字串,{"type":"text","text":1000} 在 JSON 解碼時就被拒(見下一段);
		// {type:"thinking"}(沒 text)在 Go 會變成空字串的 thinking 而保留,內容是空的、不外帶資料。
		m := fromJSON[Message](t, `{"role":"assistant","privacy":"local","ts":1,"content":[{"type":"image","data":"1000"},{"type":"text","text":"持有 1000"}]}`)
		eq(t, MaskMessages([]Message{m}, setOf(1000), nil)[0].Content, []Part{{Type: PartText, Text: "持有 " + MASK}})
		var bad Message
		if json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":1000}]}`), &bad) == nil {
			t.Fatal("text 不是字串應該解碼失敗")
		}
	})
	t.Run("local 多餘欄位不外帶,errorMessage 遮數字;hideResult 分支同樣", func(t *testing.T) {
		// 調整:多餘欄位只能從 JSON 帶進來;解碼時就被 struct 丟掉。
		tr := fromJSON[Message](t, `{"role":"toolResult","privacy":"local","ts":1,"content":[{"type":"text","text":"x 1000"}],"toolName":"t","toolCallId":"c","isError":false,"details":{"n":1000},"structuredContent":{"n":1000}}`)
		a := fromJSON[Message](t, `{"role":"assistant","privacy":"local","ts":1,"content":[{"type":"text","text":"ok"}],"provider":"p","stopReason":"error","errorMessage":"失敗 1000","extra":1000}`)
		out := MaskMessages([]Message{tr, a}, setOf(1000), func(tool string) (string, bool) { return "h", tool != "t" })
		mustNotContain(t, jsonOf(t, out), "1000")
		keys := []string{}
		for k := range fromJSON[map[string]any](t, jsonOf(t, out[0])) {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		eq(t, keys, []string{"content", "isError", "privacy", "role", "toolCallId", "toolName", "ts"})
		eq(t, out[1].ErrorMessage, "失敗 "+MASK)
		hid := jsonOf(t, MaskMessages([]Message{tr}, setOf(1000), func(string) (string, bool) { return "h", true })[0])
		mustNotContain(t, hid, "1000")
		mustNotContain(t, hid, "details")
	})
	t.Run("local content 不是陣列 → 丟掉", func(t *testing.T) {
		// N/A:Content 型別是 []Part,不可能不是陣列。最接近的等價:這種 JSON 在解碼邊界就被拒。
		var m Message
		if json.Unmarshal([]byte(`{"role":"user","privacy":"local","ts":1,"content":"持有 1000 股"}`), &m) == nil {
			t.Fatal("content 不是陣列應該解碼失敗")
		}
	})
	t.Run("local part 的多餘欄位不外帶(欄位白名單)", func(t *testing.T) {
		m := fromJSON[Message](t, `{"role":"user","privacy":"local","ts":1,"content":[{"type":"text","text":"hi","secret":"12345"}]}`)
		out := MaskMessages([]Message{m}, NewMaskSet(), nil)
		eq(t, jsonOf(t, out[0].Content), `[{"type":"text","text":"hi"}]`)
	})
}

// Go 才有的:Args 裡放了 JSON 以外的型別(struct、func)也不能漏遮。
func TestMaskValueUnknownTypes(t *testing.T) {
	m := msg(RoleAssistant, Local, []Part{{Type: PartToolCall, ID: "c", Name: "q", Args: map[string]any{
		"s": struct{ Note string }{"持有 1000 股"}, "n": 1000, "f": func() {},
	}}})
	j := jsonOf(t, MaskMessages([]Message{m}, NewMaskSet(1000), nil))
	mustNotContain(t, j, "1000")
	mustContain(t, j, `"f":"`+MASK+`"`)
}
