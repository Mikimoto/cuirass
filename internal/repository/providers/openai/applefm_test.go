//go:build test

package openai

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// trimTurns / shrinkToolResults 吃 entity.Message,content 一定是 part 陣列,估算的是它的 JSON。
// 所以預算寫成「一則訊息的估算 c」的倍數(5c、6c、8c、usage 20c),不寫死 token 數。

func fromJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const fmSchemaIn = `{"type":"object","required":["symbol","market"],"additionalProperties":false,"properties":{
	"symbol":{"type":"string","pattern":"^x$","minLength":1},
	"market":{"anyOf":[{"type":"string","const":"TW"},{"type":"string","const":"US"}]},
	"lookback":{"type":"integer","minimum":1,"maximum":250}}}`

const fmSchemaClean = `{"type":"object","required":["symbol","market"],"properties":{
	"symbol":{"type":"string"},"market":{"type":"string","enum":["TW","US"]},"lookback":{"type":"integer"}}}`

func TestFmSchema(t *testing.T) {
	t.Run("schema allowlist", func(t *testing.T) {
		eq(t, fmSchema(fromJSON(t, fmSchemaIn)), fromJSON(t, fmSchemaClean))
	})
	t.Run("items 遞迴、非全 const 的 anyOf 拿掉", func(t *testing.T) {
		eq(t, fmSchema(fromJSON(t, `{"type":"array","items":{"type":"string","pattern":"x"},"maxItems":3}`)), fromJSON(t, `{"type":"array","items":{"type":"string"}}`))
		eq(t, fmSchema(fromJSON(t, `{"anyOf":[{"type":"string"},{"type":"number"}],"description":"d"}`)), fromJSON(t, `{"description":"d"}`))
	})
}

func TestFmPayload(t *testing.T) {
	t.Run("user content 陣列→字串、tools 參數 allowlist、不改輸入", func(t *testing.T) {
		src := `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"role":"assistant","content":"x"}],
			"tools":[{"type":"function","function":{"name":"q","parameters":` + fmSchemaIn + `}}]}`
		input := fromJSON(t, src).(map[string]any)
		before := fromJSON(t, src)
		out := fmPayload(input)
		msgs := out["messages"].([]any)
		eq(t, msgs[0].(map[string]any)["content"], "ab")
		eq(t, msgs[1], map[string]any{"role": "assistant", "content": "x"})
		fn := out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		eq(t, fn["parameters"], fromJSON(t, fmSchemaClean))
		eq(t, fn["name"], "q")
		eq(t, any(input), before)
	})
	// 沒有 messages / tools 的 body 原樣回
	t.Run("非物件原樣回", func(t *testing.T) {
		eq(t, fmPayload(map[string]any{"model": "m", "messages": "x"}), map[string]any{"model": "m", "messages": "x"})
	})
}

func TestEstimateTokens(t *testing.T) {
	t.Run("中文一字一 token、ASCII 四字一 token 進位", func(t *testing.T) {
		eq(t, []int{estimateTokens("台積電"), estimateTokens("abcdefgh"), estimateTokens("abcde"), estimateTokens("")}, []int{3, 2, 2, 0})
	})
	t.Run("數字 JSON 不再以 4 字元/token 低估", func(t *testing.T) {
		type row struct {
			Date   string  `json:"date"`
			Close  float64 `json:"close"`
			Volume int     `json:"volume"`
		}
		var rows []row
		for d := range 40 {
			rows = append(rows, row{fmt.Sprintf("2026-10-%02d", d+1), 2400.5 + float64(d), 12345678 + d})
		}
		b, _ := entity.Stringify(rows)
		if n := estimateTokens(string(b)); n < 1560 {
			t.Fatal(n)
		}
	})
	t.Run("base64 / URL 類長串約 1.5 字元/token", func(t *testing.T) {
		const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		// 照 JS 的 double 運算(乘積超過 2^53 會失真,要算出同一串)
		seed := 7.0
		var b strings.Builder
		for range 400 {
			seed = math.Mod(seed*1103515245+12345, 1<<31)
			b.WriteByte(abc[int(math.Mod(seed, 64))])
		}
		if n := estimateTokens(b.String()); n < 260 {
			t.Fatal(n)
		}
	})
	t.Run("英文句子維持約 4 字元/token", func(t *testing.T) {
		if n := estimateTokens(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40)); n < 400 || n > 700 {
			t.Fatal(n)
		}
	})
}

// 預設 8192 / 1024;沒有選填參數,呼叫端一定傳值(provider 送的是 req.MaxTokens)。
func TestFmBudget(t *testing.T) {
	t.Run("(contextWindow − maxTokens) 再留 10% 餘裕", func(t *testing.T) {
		eq(t, fmBudget(8192, 1024), 6451)
	})
}

var hundred = strings.Repeat("字", 50)

func textMsg(role, text string) entity.Message {
	return entity.Message{Role: role, Content: []entity.Part{{Type: entity.PartText, Text: text}}}
}

func fmTurn() []entity.Message {
	return []entity.Message{textMsg(entity.RoleUser, hundred), textMsg(entity.RoleAssistant, hundred)}
}

func withUsage(m entity.Message, input int) entity.Message {
	m.Usage = &entity.Usage{Input: input}
	return m
}

func sameSlice(a, b []entity.Message) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func TestTrimTurns(t *testing.T) {
	msgs := slices.Concat(fmTurn(), fmTurn(), fmTurn())
	c := msgTokens(msgs[0])

	t.Run("超過預算從最舊一輪丟", func(t *testing.T) {
		r, dropped := trimTurns(msgs, 5*c, 0)
		eq(t, dropped, 1)
		if len(r) != 4 || &r[0] != &msgs[2] {
			t.Fatalf("len %d", len(r))
		}
	})
	t.Run("最少保留最後一輪", func(t *testing.T) {
		r, dropped := trimTurns(msgs, 10, 0)
		eq(t, dropped, 2)
		eq(t, r, msgs[4:])
	})
	t.Run("塞得下就不動;fixed 算進去", func(t *testing.T) {
		r, dropped := trimTurns(msgs, 6*c, 0)
		if dropped != 0 || !sameSlice(r, msgs) {
			t.Fatal(dropped)
		}
		_, dropped = trimTurns(msgs, 6*c, 1)
		eq(t, dropped, 1)
	})
	t.Run("assistant 的 usage.input 取代其之前的估算(比估算大時)", func(t *testing.T) {
		anchored := slices.Concat(fmTurn(), fmTurn(), []entity.Message{textMsg(entity.RoleUser, hundred), withUsage(textMsg(entity.RoleAssistant, hundred), 20*c)})
		// 估算 6c 塞得下 8c,但真實值 20c + c 超過 → 丟
		if _, dropped := trimTurns(anchored, 8*c, 0); dropped == 0 {
			t.Fatal("應該丟")
		}
		// 真實值比估算小時以估算為準(上一次請求可能已被裁過)
		small := append(slices.Clone(msgs[:5]), withUsage(textMsg(entity.RoleAssistant, hundred), 10))
		_, dropped := trimTurns(small, 5*c, 0)
		eq(t, dropped, 1)
	})
	t.Run("usage.input 為 0(Ollama)時,大的數字 JSON 工具結果仍被估準並裁到預算內", func(t *testing.T) {
		type row struct {
			Date   string  `json:"date"`
			Close  float64 `json:"close"`
			Volume int     `json:"volume"`
		}
		var rows []row
		for d := range 200 {
			rows = append(rows, row{"2026-10-01", 2400.5 + float64(d), 12345678 + d})
		}
		b, _ := entity.Stringify(rows)
		hist := []entity.Message{
			textMsg(entity.RoleUser, "2330 狀況?"),
			withUsage(entity.Message{Role: entity.RoleAssistant, Content: []entity.Part{{Type: entity.PartToolCall, Name: "quote"}}}, 0),
			textMsg(entity.RoleToolResult, string(b)),
			withUsage(textMsg(entity.RoleAssistant, "好"), 0),
			textMsg(entity.RoleUser, "0050 最近怎樣?"),
		}
		r, dropped := trimTurns(hist, fmBudget(8192, 1024), 0)
		eq(t, dropped, 1)
		eq(t, r, hist[4:])
	})
}

func TestShrinkToolResults(t *testing.T) {
	est := func(ms []entity.Message, fixed int) int {
		for _, m := range ms {
			fixed += msgTokens(m)
		}
		return fixed
	}
	type bar struct {
		D string  `json:"d"`
		O float64 `json:"o"`
		H float64 `json:"h"`
		L float64 `json:"l"`
		C float64 `json:"c"`
		V int     `json:"v"`
	}
	var rows []bar
	for i := range 300 {
		f := float64(i)
		rows = append(rows, bar{fmt.Sprintf("2026-%02d-01", i%12+1), 180.5 + f, 182.75 + f, 179.5 + f, 181.25 + f, 51234567 + i})
	}
	bb, _ := entity.Stringify(rows)
	bars := string(bb)

	t.Run("當前輪單一過大的工具結果截到預算內並留標記,user / assistant / 小結果不動", func(t *testing.T) {
		if len(bars) <= 22000 {
			t.Fatal(len(bars))
		}
		tr := func(id, text string) entity.Message {
			m := textMsg(entity.RoleToolResult, text)
			m.ToolCallID = id
			return m
		}
		msgs := []entity.Message{
			textMsg(entity.RoleUser, "AAPL 最近走勢?"),
			{Role: entity.RoleAssistant, Content: []entity.Part{{Type: entity.PartToolCall, ID: "c1", Name: "history"}, {Type: entity.PartToolCall, ID: "c2", Name: "quote"}}},
			tr("c1", bars),
			tr("c2", `{"price":181}`),
		}
		before := slices.Clone(msgs)
		budget := fmBudget(8192, 1024)
		if est(msgs, 500) <= budget {
			t.Fatal("測試前提:超過預算")
		}
		out := shrinkToolResults(msgs, budget, 500)
		if n := est(out, 500); n > budget {
			t.Fatalf("截完仍超過:%d > %d", n, budget)
		}
		eq(t, len(out), 4)
		eq(t, out[0], msgs[0])
		eq(t, out[1], msgs[1])
		eq(t, out[3], msgs[3])
		eq(t, out[2].ToolCallID, "c1")
		text := out[2].Content[0].Text
		if !strings.HasSuffix(text, cut) || !strings.HasPrefix(bars, strings.TrimSuffix(text, cut)) {
			t.Fatal(text[max(0, len(text)-80):])
		}
		eq(t, msgs, before) // 不改輸入
		eq(t, msgs[2].Content, []entity.Part{{Type: entity.PartText, Text: bars}})
	})
	t.Run("預算內原樣回傳", func(t *testing.T) {
		msgs := []entity.Message{textMsg(entity.RoleUser, "hi"), textMsg(entity.RoleToolResult, "ok")}
		if out := shrinkToolResults(msgs, 6451, 0); !sameSlice(out, msgs) {
			t.Fatal(out)
		}
	})
}
