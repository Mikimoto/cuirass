//go:build test

package root_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	. "github.com/Mikimoto/cuirass"
	"io"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/repository/providers/stub"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// 端到端 golden:同一段腳本化對話(本機輪 → 工具 → 切 remote → 斷流 → 再問一輪)跑一次,送進 provider 的請求(遮蔽後)
// 與稽核紀錄正規化後,必須與凍結的結果(testdata/golden/e2e.json.gz,已正規化)完全相同。
// 正規化:刪掉 ts;toolCall id 依出現順序換成 call-1、call-2…;物件鍵排序(Go map 序列化會排序鍵);其餘逐字比(數字保留原字面)。
func TestE2EGolden(t *testing.T) {
	f, err := os.Open("../../../testdata/golden/e2e.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	want := string(raw)
	var counts struct{ Requests, Audit []any }
	if err := json.Unmarshal(raw, &counts); err != nil || len(counts.Requests) != 6 || len(counts.Audit) != 4 {
		t.Fatalf("golden 案例數:請求 %d(預期 6)、稽核 %d(預期 4):%v", len(counts.Requests), len(counts.Audit), err)
	}
	got := normalizeE2E(t, runGoScenario(t))
	if got != want {
		t.Fatalf("與 golden 不同:\n%s", firstDiff(want, got))
	}
	// 對照本身有料:6 次請求(第 1 輪 2 次、斷流 1 次、第 3 輪 2 次 + 複核 1 次)、斷流的工具呼叫有配對結果、remote 請求有遮蔽
	for _, s := range []string{`"text": "已中止"`, "▇▇", "[REDACTED:api-key]", `"kind": "redacted"`, "call-3"} {
		if !strings.Contains(got, s) {
			t.Errorf("對照結果少了 %q", s)
		}
	}
	var out struct{ Requests []any }
	if err := json.Unmarshal([]byte(got), &out); err != nil || len(out.Requests) != 6 {
		t.Fatalf("請求數 %d(預期 6):%v", len(out.Requests), err)
	}
}

// breakOnce:第一次 Stream 斷流(先送文字,再以 stopReason error 結束,content 留著做到一半的工具呼叫),之後交給內層 stub。
type breakOnce struct {
	usecase.LLMProvider
	broke   bool
	capture func(usecase.LLMRequest)
}

func (b *breakOnce) Stream(ctx context.Context, req usecase.LLMRequest) iter.Seq[usecase.LLMEvent] {
	if b.broke {
		return b.LLMProvider.Stream(ctx, req)
	}
	b.broke = true
	b.capture(req)
	return func(yield func(usecase.LLMEvent) bool) {
		if !yield(usecase.LLMEvent{Type: "text_delta", Text: "查詢中"}) {
			return
		}
		yield(usecase.LLMEvent{Type: "done", Message: &Message{
			Role:         "assistant",
			Content:      []Part{{Type: "toolCall", ID: "brk-1", Name: "get_price", Args: map[string]any{"symbol": "2330"}}},
			Privacy:      Remote,
			Ts:           time.Now().UnixMilli(),
			Provider:     "cloud",
			StopReason:   "error",
			ErrorMessage: "串流中斷",
		}})
	}
}

type e2eAudit struct {
	mu  *sync.Mutex
	out *[]AuditEvent
}

func (m e2eAudit) Write(e AuditEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	*m.out = append(*m.out, e)
	return true
}

// runGoScenario 與產生 golden 的對話一字不差;回 {requests, audit} 的 JSON。
func runGoScenario(t *testing.T) []byte {
	t.Helper()
	var mu sync.Mutex
	var requests []map[string]any
	var audit []AuditEvent
	capture := func(req usecase.LLMRequest) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, map[string]any{"system": req.System, "messages": req.Messages, "tools": req.Tools, "maxTokens": req.MaxTokens})
	}
	// 兩個 stub 共用同一個 script(共用游標)
	script := []stub.Step{
		{ToolCall: &stub.Call{Name: "get_price", Args: map[string]any{"symbol": "2330"}}}, // 第 1 輪(local)
		{Text: "2330 收盤 1234.5 元,你持有 98765 股。"},
		{ToolCall: &stub.Call{Name: "get_price", Args: map[string]any{"symbol": "0050"}}}, // 第 3 輪(remote)
		{Text: "0050 收盤 1234.5 元,估計市值 4321 萬。"},
		{Text: "[]"}, // NumberVerifier 的複核:4321 對不到
	}
	// OnRequest 與斷流包裝只有 internal 做得到(公開 API 封住了),所以用根套件 testing.go 的 WrapProvider(只在 test tag 建置存在)
	local := WrapProvider(stub.New(stub.Options{Name: "local", Privacy: Local, Script: &script, OnRequest: capture}))
	cloud := WrapProvider(&breakOnce{
		LLMProvider: stub.New(stub.Options{Name: "cloud", Privacy: Remote, Script: &script, OnRequest: capture}),
		capture:     capture,
	})
	agent, err := New(AgentOptions{
		Providers: []*Provider{local, cloud},
		System:    func() string { return "你是台股助理。" },
		Tools: []ToolDef{{
			Name:        "get_price",
			Description: "查股價",
			Parameters: Schema{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"symbol": map[string]any{"type": "string"}}, "required": []any{"symbol"},
			},
			Effects:   EffectsNone,
			AuditArgs: []string{"symbol"},
			Execute: func(_ context.Context, args map[string]any, _ ToolCtx) (ToolOutput, error) {
				// 工具結果會被字串化後送進請求(字串內的鍵不受正規化排序影響)。Go map 序列化會排序鍵,
				// 要保持順序的工具回 struct(同 toolexec 的錯誤內容),這裡照 golden 的鍵順序宣告。
				type quote struct {
					Symbol any     `json:"symbol"`
					Close  float64 `json:"close"`
					Debug  string  `json:"debug"`
				}
				return ToolOutput{
					Content: quote{args["symbol"], 1234.5, "token sk-abcdefghijklmnopqrstuvwx"},
					Details: map[string]any{"close": 1234.5},
				}, nil
			},
		}},
		Audit: e2eAudit{&mu, &audit},
		Masking: &Masking{
			Collect:  func(_ string, d any) ([]float64, error) { return []float64{d.(map[string]any)["close"].(float64)}, nil },
			Snapshot: func() ([]float64, error) { return []float64{98765}, nil },
		},
		Verifiers: []Verifier{NumberVerifier(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, p := range []string{"我有 98765 股,查 2330", "再查一次 2330", "那 0050 呢?"} {
		if i == 1 && !agent.SetProvider("cloud") {
			t.Fatal("SetProvider 失敗")
		}
		if err := agent.Prompt(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	agent.Dispose()
	mu.Lock()
	defer mu.Unlock()
	b, err := json.Marshal(map[string]any{"requests": requests, "audit": audit})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// normalizeE2E:刪 ts、toolCall id 依出現順序重編(物件依排序後的鍵走訪,兩邊順序一致)、鍵排序、縮排輸出。
func normalizeE2E(t *testing.T, b []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // 數字保留原字面,格式不同也看得出來
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("解析失敗:%v\n%s", err, b)
	}
	ids := map[string]string{}
	rename := func(id string) string {
		if _, ok := ids[id]; !ok {
			ids[id] = fmt.Sprintf("call-%d", len(ids)+1)
		}
		return ids[id]
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			delete(x, "ts")
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if s, ok := x[k].(string); ok && (k == "toolCallId" || k == "id" && x["type"] == "toolCall") {
					x[k] = rename(s)
				}
				walk(x[k])
			}
		}
	}
	walk(v)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// firstDiff 指出第一個不同的行與前後文。
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	lo := max(0, i-8)
	return fmt.Sprintf("第 %d 行起不同\n--- golden\n%s\n--- 實際\n%s", i+1,
		strings.Join(w[lo:min(len(w), i+8)], "\n"), strings.Join(g[lo:min(len(g), i+8)], "\n"))
}
