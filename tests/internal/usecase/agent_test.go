//go:build test

package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/repository/providers"
	stubp "github.com/Mikimoto/cuirass/internal/repository/providers/stub"
	. "github.com/Mikimoto/cuirass/internal/usecase"
)

func agentTool(over func(*entity.ToolDef)) entity.ToolDef {
	t := entity.ToolDef{
		Name: "t", Description: "d",
		Parameters: entity.Schema{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		Effects:    entity.EffectsNone,
		Execute: func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			return entity.ToolOutput{Content: map[string]any{"price": 512.5}, Details: map[string]any{"price": 512.5}}, nil
		},
	}
	if over != nil {
		over(&t)
	}
	return t
}

func stubOf(name string, privacy entity.Privacy, script []stubp.Step, onReq func(LLMRequest)) *stubp.Provider {
	return stubp.New(stubp.Options{Name: name, Privacy: privacy, Script: &script, OnRequest: onReq})
}

func sys() string { return "sys" }

// newClient 是根套件交給 NewAgent 的工廠(同 cuirass.New)。
func newClient(hosts []string, audit entity.AuditSink) entity.HTTPClient {
	return repository.NewHTTPClient(repository.HTTPClientOptions{Hosts: hosts, Audit: audit})
}

func mustAgent(t *testing.T, o AgentOptions) *Agent {
	t.Helper()
	if o.System == nil {
		o.System = sys
	}
	a, err := NewAgent(o, newClient)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// glog:多個 gated provider 共用的紀錄。
type glog struct {
	mu    sync.Mutex
	lines []string
}

func (l *glog) add(s string) { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *glog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// gatedLLM:每次 stream 先等 gate 放行(或 ctx 結束),再回一句文字。
type gatedLLM struct {
	name  string
	log   *glog
	mu    sync.Mutex
	gates []chan struct{}
}

func (g *gatedLLM) Name() string            { return g.name }
func (g *gatedLLM) Model() string           { return g.name }
func (g *gatedLLM) Privacy() entity.Privacy { return entity.Local }
func (g *gatedLLM) ContextWindow() int      { return 1000 }
func (g *gatedLLM) MaxTokens() int          { return 4096 }
func (g *gatedLLM) Stream(ctx context.Context, req LLMRequest) iter.Seq[LLMEvent] {
	return func(yield func(LLMEvent) bool) {
		text := ""
		for _, p := range req.Messages[len(req.Messages)-1].Content {
			text += p.Text
		}
		g.log.add(g.name + ":start:" + text)
		ch := make(chan struct{})
		g.mu.Lock()
		g.gates = append(g.gates, ch)
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done(): // 收掉,免得 goroutine 留到測試結束
			return
		}
		g.log.add(g.name + ":end:" + text)
		yield(LLMEvent{Type: LLMEventDone, Message: &entity.Message{
			Role: entity.RoleAssistant, Content: []entity.Part{textPart("回 " + text)}, Privacy: entity.Local, Ts: 1, Provider: g.name, StopReason: "stop",
		}})
	}
}

func (g *gatedLLM) started(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		k := len(g.gates)
		g.mu.Unlock()
		if k >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s:等不到第 %d 個 stream(現在 %d)", g.name, n, k)
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *gatedLLM) release(i int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gates[i])
}

func gated(name string, log *glog) *gatedLLM { return &gatedLLM{name: name, log: log} }

func promptAsync(a *Agent, text string) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- a.Prompt(context.Background(), text) }()
	return ch
}

func await(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt 沒有返回")
		return nil
	}
}

func awaitOK(t *testing.T, ch <-chan error) {
	t.Helper()
	if err := await(t, ch); err != nil {
		t.Fatal(err)
	}
}

// waitQueue 等佇列(排隊中 + 執行中)長度到 n。
func waitQueue(t *testing.T, a *Agent, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		k := a.QueueLen()
		if k == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("佇列長度等不到 %d(現在 %d)", n, k)
		}
		time.Sleep(time.Millisecond)
	}
}

func recOf(a *Agent) *turnRec {
	r := &turnRec{}
	a.On(r.on)
	return r
}

func roles(ms []entity.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Role)
	}
	return out
}

func ip(n int) *int { return &n }

func TestAgent(t *testing.T) {
	t.Run("輪中呼叫 prompt → 排隊,依序執行", func(t *testing.T) {
		log := &glog{}
		g := gated("a", log)
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{g}})
		p1 := promptAsync(a, "一")
		g.started(t, 1)
		p2 := promptAsync(a, "二")
		waitQueue(t, a, 2)
		time.Sleep(10 * time.Millisecond)
		eqStrs(t, log.get(), []string{"a:start:一"})
		g.release(0)
		awaitOK(t, p1)
		g.started(t, 2)
		g.release(1)
		awaitOK(t, p2)
		eqStrs(t, log.get(), []string{"a:start:一", "a:end:一", "a:start:二", "a:end:二"})
		eqStrs(t, roles(a.Messages()), []string{"user", "assistant", "user", "assistant"})
	})

	t.Run("abort 後 session 可再 prompt", func(t *testing.T) {
		g := gated("a", &glog{})
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{g}})
		r := recOf(a)
		p1 := promptAsync(a, "一")
		g.started(t, 1)
		a.Abort()
		awaitOK(t, p1)
		eqStrs(t, r.ends(), []string{"aborted"})
		p2 := promptAsync(a, "二")
		g.started(t, 2)
		g.release(1)
		awaitOK(t, p2)
		eqStrs(t, r.ends(), []string{"aborted", "done"})
	})

	t.Run("abort 只中止目前這輪,排隊中的輪照跑", func(t *testing.T) {
		g := gated("a", &glog{})
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{g}})
		r := recOf(a)
		p1 := promptAsync(a, "一")
		g.started(t, 1)
		p2 := promptAsync(a, "二")
		waitQueue(t, a, 2)
		a.Abort()
		awaitOK(t, p1)
		g.started(t, 2)
		g.release(1)
		awaitOK(t, p2)
		eqStrs(t, r.ends(), []string{"aborted", "done"})
	})

	t.Run("setProvider 在輪中呼叫 → 下一輪才生效", func(t *testing.T) {
		log := &glog{}
		ga, gb := gated("a", log), gated("b", log)
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{ga, gb}})
		p1 := promptAsync(a, "一")
		ga.started(t, 1)
		if !a.SetProvider("b") || a.Provider().Name != "a" || a.SetProvider("nope") {
			t.Fatal(a.Provider())
		}
		ga.release(0)
		awaitOK(t, p1)
		p2 := promptAsync(a, "二")
		gb.started(t, 1)
		gb.release(0)
		awaitOK(t, p2)
		if a.Provider().Name != "b" {
			t.Fatal(a.Provider())
		}
		eqStrs(t, log.get(), []string{"a:start:一", "a:end:一", "b:start:二", "b:end:二"})
	})

	t.Run("閒置時 setProvider 立刻生效", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("a", "", nil, nil), stubOf("b", "", nil, nil)}})
		if !a.SetProvider("b") || a.Provider().Name != "b" || a.Provider().ContextWindow != 200000 || a.Provider().MaxTokens != 4096 {
			t.Fatal(a.Provider())
		}
	})

	t.Run("一輪丟例外不卡住佇列", func(t *testing.T) {
		var n atomic.Int32
		hold := make(chan struct{})
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{stubOf("", "", []stubp.Step{{Text: "好"}}, nil)},
			TurnTimeout: func(ProviderInfo) time.Duration {
				if n.Add(1) == 1 {
					<-hold // 等 p2 排進來再丟(p1、p2 都排好後 p1 才失敗)
					panic("壞掉")
				}
				return 0
			},
		})
		p1 := promptAsync(a, "一")
		waitQueue(t, a, 1)
		p2 := promptAsync(a, "二")
		waitQueue(t, a, 2)
		close(hold)
		if err := await(t, p1); err == nil || !strings.Contains(err.Error(), "壞掉") {
			t.Fatal(err)
		}
		awaitOK(t, p2)
		eqStrs(t, roles(a.Messages()), []string{"user", "assistant"})
	})

	t.Run("dispose 後 prompt 被拒", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}})
		a.Dispose()
		if err := a.Prompt(context.Background(), "一"); err == nil || err.Error() != "agent 已停用" {
			t.Fatal(err)
		}
	})

	reg := func(tools []entity.ToolDef, mod func(*AgentOptions)) error {
		o := AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, System: sys, Tools: tools}
		if mod != nil {
			mod(&o)
		}
		_, err := NewAgent(o, newClient)
		return err
	}
	wantErr := func(t *testing.T, err error, msg string) {
		t.Helper()
		if err == nil || err.Error() != msg {
			t.Fatalf("got %v, want %q", err, msg)
		}
	}

	t.Run("註冊 exec 工具 → 丟錯", func(t *testing.T) {
		for _, e := range []entity.Effects{entity.EffectsExec, entity.EffectsFS} {
			wantErr(t, reg([]entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Effects = e })}, nil), "專案 1 不支援 fs/exec 工具")
		}
	})

	t.Run("schema 沒有 additionalProperties:false → 丟錯", func(t *testing.T) {
		err := reg([]entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Parameters = entity.Schema{"type": "object"} })}, nil)
		wantErr(t, err, "工具 t 的 schema 必須是 additionalProperties:false")
		if !errors.Is(err, entity.ErrSchemaNotClosed) {
			t.Fatal(err)
		}
	})

	t.Run("工具名重複、名稱不合法、policy.tools 有未註冊工具 → 丟錯", func(t *testing.T) {
		wantErr(t, reg([]entity.ToolDef{agentTool(nil), agentTool(nil)}, nil), "工具名重複:t")
		wantErr(t, reg([]entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Name = `a" x="` })}, nil), `工具名不合法:"a\" x=\""`)
		wantErr(t, reg([]entity.ToolDef{agentTool(nil)}, func(o *AgentOptions) { o.Policy.Tools = []string{"t", "u"} }), "policy.tools 含未註冊的工具:u")
		wantErr(t, reg(nil, func(o *AgentOptions) { o.InitialProvider = "nope" }), "找不到 provider:nope")
	})

	t.Run("limits 明寫 undefined → 用預設值,上限不失效", func(t *testing.T) {
		var execs atomic.Int32
		script := make([]stubp.Step, 30)
		for i := range script {
			script[i] = stubp.Step{ToolCall: &stubp.Call{Name: "t"}}
		}
		tool := agentTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				execs.Add(1)
				return entity.ToolOutput{Content: "ok"}, nil
			}
		})
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{stubOf("", "", script, nil)}, Tools: []entity.ToolDef{tool},
			Policy: PolicyOptions{Limits: LimitsOptions{PerTurn: nil, MaxModelCallsPerTurn: nil, ToolResultBytes: nil}},
		})
		if err := a.Prompt(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if execs.Load() != 20 {
			t.Fatal(execs.Load())
		}
	})

	// 「limits 給 NaN、負數、非整數、Infinity、非數字 → 拒絕建立」見 TestAgentLimitsValidation

	t.Run("hideResult 的工具資料不經數字複核送到 remote(hideResult 丟例外也一樣)", func(t *testing.T) {
		hides := []func(string) (string, bool){
			func(tool string) (string, bool) { return "(持股已隱藏)", tool == "portfolio" },
			func(string) (string, bool) { panic("x") },
		}
		for i, hide := range hides {
			var mu sync.Mutex
			var reqs []LLMRequest
			script := []stubp.Step{{ToolCall: &stubp.Call{Name: "portfolio"}}, {Text: "好"}, {Text: "你的報酬 12.34%"}, {Text: "[]"}}
			local := stubp.New(stubp.Options{Name: "local", Privacy: entity.Local, Script: &script})
			remote := stubp.New(stubp.Options{Name: "remote", Privacy: entity.Remote, Script: &script, OnRequest: func(r LLMRequest) {
				mu.Lock()
				reqs = append(reqs, r)
				mu.Unlock()
			}})
			pf := agentTool(func(d *entity.ToolDef) {
				d.Name = "portfolio"
				d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
					return entity.ToolOutput{Content: "x", Details: map[string]any{"holdings": []any{map[string]any{"name": "SECRETNAME", "shares": 1000}}}}, nil
				}
			})
			a := mustAgent(t, AgentOptions{
				Providers: []LLMProvider{local, remote}, Tools: []entity.ToolDef{pf},
				Masking: &Masking{HideResult: hide}, Verifiers: []Verifier{NumberVerifier(0)},
			})
			if err := a.Prompt(context.Background(), "持股?"); err != nil {
				t.Fatal(err)
			}
			a.SetProvider("remote")
			if err := a.Prompt(context.Background(), "報酬?"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if len(reqs) != 2 { // 回答 + 複核
				t.Fatal(i, len(reqs))
			}
			if s := jsonStr(t, reqs); strings.Contains(s, "SECRETNAME") {
				t.Fatal(i, s)
			}
			mu.Unlock()
		}
	})

	t.Run("Anthropic 串流在 tool_use 結束後斷掉 → 下一輪請求的每個 tool_use 都有 tool_result", func(t *testing.T) {
		sse := func(evs ...string) string {
			var b strings.Builder
			for _, e := range evs {
				b.WriteString("data: " + e + "\n\n")
			}
			return b.String()
		}
		type block struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		type body struct {
			Messages []struct {
				Role    string  `json:"role"`
				Content []block `json:"content"`
			} `json:"messages"`
		}
		var mu sync.Mutex
		var bodies []body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var b body
			_ = json.Unmarshal(raw, &b)
			mu.Lock()
			bodies = append(bodies, b)
			first := len(bodies) == 1
			mu.Unlock()
			if first { // 沒有 message_delta(stop_reason):串流提前結束
				_, _ = io.WriteString(w, sse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"t"}}`,
					`{"type":"content_block_stop","index":0}`))
				return
			}
			_, _ = io.WriteString(w, sse(`{"type":"message_start","message":{}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, `{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
		}))
		defer srv.Close()
		p, err := providers.CreateProvider(repository.ProviderConfig{Name: "a", API: "anthropic-messages", Model: "m", BaseURL: srv.URL, APIKey: "k"},
			providers.CreateDeps{HTTP: srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		var execs atomic.Int32
		tool := agentTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				execs.Add(1)
				return entity.ToolOutput{Content: "ok"}, nil
			}
		})
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{p}, Tools: []entity.ToolDef{tool}})
		r := recOf(a)
		if err := a.Prompt(context.Background(), "一"); err != nil {
			t.Fatal(err)
		}
		eqStrs(t, r.ends(), []string{"error"})
		if execs.Load() != 0 { // 沒完成的回應不執行工具
			t.Fatal(execs.Load())
		}
		if err := a.Prompt(context.Background(), "二"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		msgs := bodies[1].Messages
		var uses []string
		for i, m := range msgs {
			for _, b := range m.Content {
				if b.Type != "tool_use" {
					continue
				}
				uses = append(uses, b.ID)
				if i+1 >= len(msgs) || !slices.ContainsFunc(msgs[i+1].Content, func(x block) bool { return x.Type == "tool_result" && x.ToolUseID == b.ID }) {
					t.Fatal("沒有配對的 tool_result:", b.ID)
				}
			}
		}
		eqStrs(t, uses, []string{"tu_1"})
	})

	t.Run("policy.tools 未填 = 全部工具;填了用子集", func(t *testing.T) {
		u := agentTool(func(d *entity.ToolDef) { d.Name = "u" })
		tools := []entity.ToolDef{agentTool(nil), u}
		eqStrs(t, mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, Tools: tools}).ActiveTools(), []string{"t", "u"})
		eqStrs(t, mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, Tools: tools, Policy: PolicyOptions{Tools: []string{"u"}}}).ActiveTools(), []string{"u"})
		// 非 nil 的空切片 = 不開任何工具(不是「全部」)
		if got := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, Tools: tools, Policy: PolicyOptions{Tools: []string{}}}).ActiveTools(); len(got) != 0 {
			t.Fatal(got)
		}
	})

	t.Run("使用者訊息的 privacy = 送出當下 provider 的 privacy", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{
			stubOf("l", "", []stubp.Step{{Text: "a"}}, nil), stubOf("r", entity.Remote, []stubp.Step{{Text: "b"}}, nil),
		}})
		_ = a.Prompt(context.Background(), "一")
		a.SetProvider("r")
		_ = a.Prompt(context.Background(), "二")
		var got []string
		for _, m := range a.Messages() {
			if m.Role == entity.RoleUser {
				got = append(got, string(m.Privacy))
			}
		}
		eqStrs(t, got, []string{"local", "remote"})
	})

	t.Run("工具透過 ctx.http 連線 → 白名單外的主機被擋", func(t *testing.T) {
		errText := ""
		tool := agentTool(func(d *entity.ToolDef) {
			d.Effects = entity.EffectsNetwork
			d.Execute = func(ctx context.Context, _ map[string]any, tc entity.ToolCtx) (entity.ToolOutput, error) {
				if _, err := tc.HTTP(ctx, "https://evil.example/x", entity.HTTPOptions{}); err != nil {
					errText = err.Error()
				}
				return entity.ToolOutput{Content: "ok"}, nil
			}
		})
		rec := &turnRec{}
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{stubOf("", "", []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "完"}}, nil)},
			Tools:     []entity.ToolDef{tool}, Policy: PolicyOptions{Hosts: []string{"api.example.com"}}, Audit: rec,
		})
		_ = a.Prompt(context.Background(), "一")
		if errText != "blocked host" || !slices.Contains(rec.kinds(), "egress_block") {
			t.Fatal(errText, rec.kinds())
		}
	})

	t.Run("turnTimeoutMs 依 provider 決定", func(t *testing.T) {
		hang := &fakeLLM{privacy: entity.Local, stream: func(ctx context.Context, _ LLMRequest, _ func(LLMEvent) bool) { <-ctx.Done() }}
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{hang},
			TurnTimeout: func(p ProviderInfo) time.Duration {
				if p.Name == "scripted" {
					return 20 * time.Millisecond
				}
				return 0
			},
		})
		r := recOf(a)
		_ = a.Prompt(context.Background(), "一")
		eqStrs(t, r.ends(), []string{"timeout"})
	})

	t.Run("remote → local 切換後,先前 local 輪的數字送 remote 時被遮", func(t *testing.T) {
		var mu sync.Mutex
		var last LLMRequest
		remote := stubOf("r", entity.Remote, []stubp.Step{{Text: "嗨"}, {Text: "好"}}, func(r LLMRequest) { mu.Lock(); last = r; mu.Unlock() })
		local := stubOf("l", entity.Local, []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "價格 512.5 元"}}, nil)
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{remote, local}, Tools: []entity.ToolDef{agentTool(nil)},
			Masking: &Masking{Collect: func(_ string, details any) ([]float64, error) { return entity.CollectNumbers(details), nil }},
		})
		_ = a.Prompt(context.Background(), "你好")
		a.SetProvider("l")
		_ = a.Prompt(context.Background(), "查價")
		ms := a.Messages()
		if s := jsonStr(t, ms[len(ms)-1].Content); !strings.Contains(s, "512.5") {
			t.Fatal(s)
		}
		a.SetProvider("r")
		_ = a.Prompt(context.Background(), "再說一次")
		mu.Lock()
		sent := jsonStr(t, last.Messages)
		mu.Unlock()
		if strings.Contains(sent, "512.5") || !strings.Contains(sent, entity.MASK) {
			t.Fatal(sent)
		}
	})
}

// roundTrip 記錄經過的請求;回 200。
type recTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (r *recTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Host)
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
}

// 注入 Transport 後白名單照樣生效:白名單外的主機根本不會到 Transport;白名單內的走注入的 Transport。
func TestAgentEgressWithInjectedTransport(t *testing.T) {
	tr := &recTransport{}
	results := map[string]string{}
	tool := agentTool(func(d *entity.ToolDef) {
		d.Effects = entity.EffectsNetwork
		d.Execute = func(ctx context.Context, _ map[string]any, tc entity.ToolCtx) (entity.ToolOutput, error) {
			for _, u := range []string{"https://evil.example/x", "https://api.example.com/ok", "http://api.example.com/plain"} {
				res, err := tc.HTTP(ctx, u, entity.HTTPOptions{})
				if err != nil {
					results[u] = err.Error()
				} else {
					results[u] = res.Body
				}
			}
			return entity.ToolOutput{Content: "ok"}, nil
		}
	})
	rec := &turnRec{}
	a, err := NewAgent(AgentOptions{
		Providers: []LLMProvider{stubOf("", "", []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "完"}}, nil)},
		System:    sys, Tools: []entity.ToolDef{tool}, Policy: PolicyOptions{Hosts: []string{"api.example.com"}}, Audit: rec,
	}, func(hosts []string, audit entity.AuditSink) entity.HTTPClient {
		return repository.NewHTTPClient(repository.HTTPClientOptions{Hosts: hosts, HTTP: &http.Client{Transport: tr}, Audit: audit})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "一"); err != nil {
		t.Fatal(err)
	}
	if results["https://evil.example/x"] != "blocked host" || results["http://api.example.com/plain"] != "blocked host" || results["https://api.example.com/ok"] != "ok" {
		t.Fatal(results)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	eqStrs(t, tr.hosts, []string{"api.example.com"})
	n := 0
	for _, k := range rec.kinds() {
		if k == "egress_block" {
			n++
		}
	}
	if n != 2 {
		t.Fatal(rec.kinds())
	}
}

// quickLLM:稍等一下就回一句文字;ctx 結束就收。
type quickLLM struct{ name string }

func (c *quickLLM) Name() string            { return c.name }
func (c *quickLLM) Model() string           { return c.name }
func (c *quickLLM) Privacy() entity.Privacy { return entity.Local }
func (c *quickLLM) ContextWindow() int      { return 1000 }
func (c *quickLLM) MaxTokens() int          { return 4096 }
func (c *quickLLM) Stream(ctx context.Context, _ LLMRequest) iter.Seq[LLMEvent] {
	return func(yield func(LLMEvent) bool) {
		select {
		case <-time.After(200 * time.Microsecond):
		case <-ctx.Done():
			return
		}
		yield(LLMEvent{Type: LLMEventDone, Message: &entity.Message{
			Role: entity.RoleAssistant, Content: []entity.Part{textPart("回")}, Privacy: entity.Local, Ts: 1, Provider: c.name, StopReason: "stop",
		}})
	}
}

// Review Focus 3:輪中再 Prompt → 依呼叫順序排隊(FIFO),任何時刻只有一輪;併發呼叫其他方法 -race 乾淨。
func TestAgentConcurrentSendQueues(t *testing.T) {
	t.Run("排隊中的輪依呼叫順序執行", func(t *testing.T) {
		log := &glog{}
		g := gated("a", log)
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{g}})
		chs := []<-chan error{promptAsync(a, "0")}
		g.started(t, 1)
		for i := 1; i < 6; i++ {
			chs = append(chs, promptAsync(a, fmt.Sprint(i)))
			waitQueue(t, a, i+1)
		}
		for i := range chs {
			g.started(t, i+1)
			g.release(i)
			awaitOK(t, chs[i])
		}
		var want []string
		for i := range 6 {
			want = append(want, fmt.Sprintf("a:start:%d", i), fmt.Sprintf("a:end:%d", i))
		}
		eqStrs(t, log.get(), want)
	})

	t.Run("多個 goroutine 同時 Prompt、SetProvider、Abort、讀狀態", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{&quickLLM{"a"}, &quickLLM{"b"}}})
		r := recOf(a)
		var wg sync.WaitGroup
		var errs atomic.Int32
		for g := range 8 {
			wg.Go(func() {
				for i := range 5 {
					if a.Prompt(context.Background(), fmt.Sprintf("%d-%d", g, i)) != nil {
						errs.Add(1)
					}
				}
			})
		}
		stop := make(chan struct{})
		var side sync.WaitGroup
		side.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				a.SetProvider([]string{"a", "b"}[i%2])
				_ = a.Provider()
				_ = a.Messages()
				_ = a.Budget()
				_ = a.ActiveTools()
				if i%7 == 0 {
					a.Abort()
				}
				time.Sleep(50 * time.Microsecond)
			}
		})
		wg.Wait()
		close(stop)
		side.Wait()
		if errs.Load() != 0 {
			t.Fatal(errs.Load())
		}
		// 任何時刻只有一輪:turn_start、turn_end 嚴格交錯(事件在 turn 自己的 goroutine 同步送出)。
		// 不量 stream 的同時數:被中止的輪,它放棄的 stream goroutine 可能晚一點才結束。
		var seq []string
		for _, e := range r.evs() {
			if e.Type == "turn_start" || e.Type == "turn_end" {
				seq = append(seq, e.Type)
			}
		}
		if len(seq) != 80 {
			t.Fatal(len(seq))
		}
		for i, s := range seq {
			if want := []string{"turn_start", "turn_end"}[i%2]; s != want {
				t.Fatal(i, seq)
			}
		}
		// 每輪的使用者訊息都在紀錄裡,且只出現一次
		users := map[string]int{}
		for _, m := range a.Messages() {
			if m.Role == entity.RoleUser {
				users[m.Content[0].Text]++
			}
		}
		if len(users) != 40 {
			t.Fatal(len(users))
		}
		for k, n := range users {
			if n != 1 {
				t.Fatal(k, n)
			}
		}
	})
}

// Review Focus 3:輪中切換,這輪仍用舊的、Provider() 也回舊的;切換排在它之前已排隊的輪之後(佇列語意)。
func TestAgentSetProviderMidTurnQueued(t *testing.T) {
	log := &glog{}
	ga, gb := gated("a", log), gated("b", log)
	a := mustAgent(t, AgentOptions{Providers: []LLMProvider{ga, gb}})
	p1 := promptAsync(a, "一")
	ga.started(t, 1)
	p2 := promptAsync(a, "二") // 切換之前已排隊:仍用 a
	waitQueue(t, a, 2)
	if !a.SetProvider("b") {
		t.Fatal("SetProvider")
	}
	p3 := promptAsync(a, "三") // 切換之後才排隊:用 b
	waitQueue(t, a, 4)
	if p := a.Provider(); p.Name != "a" || p.Privacy != entity.Local {
		t.Fatal(p)
	}
	ga.release(0)
	awaitOK(t, p1)
	ga.started(t, 2)
	if a.Provider().Name != "a" {
		t.Fatal(a.Provider())
	}
	ga.release(1)
	awaitOK(t, p2)
	// 切換在 p2 返回之前就套用
	if a.Provider().Name != "b" {
		t.Fatal(a.Provider())
	}
	gb.started(t, 1)
	gb.release(0)
	awaitOK(t, p3)
	eqStrs(t, log.get(), []string{"a:start:一", "a:end:一", "a:start:二", "a:end:二", "b:start:三", "b:end:三"})
	var provs []string
	for _, m := range a.Messages() {
		if m.Role == entity.RoleAssistant {
			provs = append(provs, m.Provider)
		}
	}
	eqStrs(t, provs, []string{"a", "a", "b"})
}

// limits 不合法 → 拒絕建立。Go 的 int 擋掉 NaN、小數、Infinity、字串、null,
// 剩下負數與超過 2^53-1(Number.isSafeInteger 的上限)。
func TestAgentLimitsValidation(t *testing.T) {
	mk := func(l LimitsOptions) error {
		_, err := NewAgent(AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, System: sys, Tools: []entity.ToolDef{agentTool(nil)}, Policy: PolicyOptions{Limits: l}}, newClient)
		return err
	}
	for _, bad := range []int{-1, -1 << 40, 1 << 53} {
		fields := map[string]LimitsOptions{
			"perTurn": {PerTurn: ip(bad)}, "perSession": {PerSession: ip(bad)}, "turnTimeoutMs": {TurnTimeoutMs: ip(bad)},
			"maxOutputTokens": {MaxOutputTokens: ip(bad)}, "toolResultBytes": {ToolResultBytes: ip(bad)},
			"maxModelCallsPerTurn": {MaxModelCallsPerTurn: ip(bad)}, "perToolPerTurn.t": {PerTool: map[string]int{"t": bad}},
		}
		for k, l := range fields {
			err := mk(l)
			if want := fmt.Sprintf("policy.limits.%s 必須是非負整數:%d", k, bad); err == nil || err.Error() != want {
				t.Fatalf("%s=%d:%v", k, bad, err)
			}
		}
	}
	if err := mk(LimitsOptions{PerTurn: ip(0), PerTool: map[string]int{"t": 3}}); err != nil {
		t.Fatal(err)
	}
	// 最大的合法值也能建,逾時不溢位(飽和)
	a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", []stubp.Step{{Text: "好"}}, nil)}, Policy: PolicyOptions{Limits: LimitsOptions{TurnTimeoutMs: ip(1<<53 - 1)}}})
	r := recOf(a)
	_ = a.Prompt(context.Background(), "一")
	eqStrs(t, r.ends(), []string{"done"})
	if d := MsDuration(1<<53 - 1); d != math.MaxInt64 {
		t.Fatal(d)
	}
	if d := MsDuration(300_000); d != 5*time.Minute {
		t.Fatal(d)
	}
	// PerTurn 0 生效:不准呼叫工具
	a = mustAgent(t, AgentOptions{
		Providers: []LLMProvider{stubOf("", "", []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "完"}}, nil)},
		Tools:     []entity.ToolDef{agentTool(nil)}, Policy: PolicyOptions{Limits: LimitsOptions{PerTurn: ip(0)}},
	})
	r = recOf(a)
	_ = a.Prompt(context.Background(), "一")
	eqStrs(t, r.ends(), []string{"cap"})
}

// 建立時的其他檢查(含 providers 名稱重複)。
func TestAgentRegistrationChecks(t *testing.T) {
	base := func() AgentOptions {
		return AgentOptions{Providers: []LLMProvider{stubOf("a", "", nil, nil)}, System: sys}
	}
	cases := map[string]struct {
		mod  func(*AgentOptions)
		want string
	}{
		"auditFailure 打錯字": {func(o *AgentOptions) { o.Policy.AuditFailure = "Halt" }, `policy.auditFailure 只能是 "halt" 或 "warn"(或不填):"Halt"`},
		"leakAllow 太短":     {func(o *AgentOptions) { o.LeakAllow = []string{"ok"} }, "leakAllow[0] 太短:正規化後只有 2 字,至少要 20 字(空字串或短句會讓外洩偵測失效)"},
		"leakAllow 空字串":    {func(o *AgentOptions) { o.LeakAllow = []string{""} }, "leakAllow[0] 太短:正規化後只有 0 字,至少要 20 字(空字串或短句會讓外洩偵測失效)"},
		"provider 名稱重複":    {func(o *AgentOptions) { o.Providers = append(o.Providers, stubOf("a", "", nil, nil)) }, "provider 名稱重複:a"},
		"provider 是 nil":   {func(o *AgentOptions) { o.Providers = append(o.Providers, nil) }, "providers[1] 是 nil"},
		"沒有 provider":      {func(o *AgentOptions) { o.Providers = nil }, "至少要有一個 provider"},
		"system 是 nil":     {func(o *AgentOptions) { o.System = nil }, "system 必填"},
		"effects 無法辨識": {func(o *AgentOptions) {
			o.Tools = []entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Effects = "" })}
		}, "工具 t 的 effects 無法辨識"},
		"schema 根層不是 object": {func(o *AgentOptions) {
			o.Tools = []entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Parameters = entity.Schema{"additionalProperties": false} })}
		}, "工具 t 的 schema 根層必須是 type:object"},
		"工具名太長": {func(o *AgentOptions) {
			o.Tools = []entity.ToolDef{agentTool(func(d *entity.ToolDef) { d.Name = strings.Repeat("a", 65) })}
		}, `工具名不合法:"` + strings.Repeat("a", 65) + `"`},
	}
	for name, c := range cases {
		o := base()
		c.mod(&o)
		if _, err := NewAgent(o, newClient); err == nil || err.Error() != c.want {
			t.Errorf("%s:%v", name, err)
		}
	}
	for _, ok := range []string{"", "halt", "warn"} {
		o := base()
		o.Policy.AuditFailure = ok
		if _, err := NewAgent(o, newClient); err != nil {
			t.Fatal(ok, err)
		}
	}
}

// carried note 4:還原的額度壞掉 = 用完,絕不從 0 開始。
func TestAgentRestoreBudget(t *testing.T) {
	run := func(restore *entity.BudgetState) (string, entity.BudgetState) {
		a := mustAgent(t, AgentOptions{
			Providers: []LLMProvider{stubOf("", "", []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "完"}}, nil)},
			Tools:     []entity.ToolDef{agentTool(nil)}, Restore: restore,
		})
		r := recOf(a)
		_ = a.Prompt(context.Background(), "一")
		var blocked string
		for _, e := range r.evs() {
			if e.Type == "tool_blocked" {
				blocked = e.Reason
			}
		}
		return blocked, a.Budget()
	}
	var fromBadJSON entity.BudgetState
	_ = json.Unmarshal([]byte(`{"session":-1,"turn":{}}`), &fromBadJSON)
	var fromBrokenJSON entity.BudgetState
	_ = json.Unmarshal([]byte(`{"session":`), &fromBrokenJSON) // 語法錯:保持零值
	for name, st := range map[string]*entity.BudgetState{"零值": {}, "壞值": &fromBadJSON, "語法錯": &fromBrokenJSON} {
		blocked, b := run(st)
		if blocked != "本次對話工具呼叫已達上限(200 次),請重啟" || !b.Exhausted {
			t.Fatal(name, blocked, b)
		}
	}
	var good entity.BudgetState
	if err := json.Unmarshal([]byte(`{"session":7,"turn":{}}`), &good); err != nil {
		t.Fatal(err)
	}
	blocked, b := run(&good)
	if blocked != "" || b.Session != 8 || b.Exhausted {
		t.Fatal(blocked, b)
	}
	blocked, b = run(nil)
	if blocked != "" || b.Session != 1 {
		t.Fatal(blocked, b)
	}
}

// Messages 回深複本:改了不影響 agent。
func TestAgentMessagesIsCopy(t *testing.T) {
	a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", []stubp.Step{{ToolCall: &stubp.Call{Name: "t", Args: map[string]any{}}}, {Text: "完"}}, nil)}, Tools: []entity.ToolDef{agentTool(nil)}})
	_ = a.Prompt(context.Background(), "一")
	ms := a.Messages()
	ms[0].Content[0].Text = "改掉"
	ms[1].Content[0].Args["x"] = 1
	again := a.Messages()
	if again[0].Content[0].Text != "一" || len(again[1].Content[0].Args) != 0 {
		t.Fatal(again)
	}
}

// 裁決 G21:排隊中的 Prompt 在 ctx 取消或 Dispose 時離開佇列,不跑;listener 同步呼叫 Prompt 放得開。
func TestAgentQueuedPromptExits(t *testing.T) {
	t.Run("排隊中 ctx 取消 → 馬上回 ctx.Err()、不跑,後面排的切換照樣生效", func(t *testing.T) {
		ga, gb := gated("a", &glog{}), gated("b", &glog{})
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{ga, gb}})
		p1 := promptAsync(a, "一")
		ga.started(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		p2 := make(chan error, 1)
		go func() { p2 <- a.Prompt(ctx, "二") }()
		waitQueue(t, a, 2)
		a.SetProvider("b")
		waitQueue(t, a, 3)
		cancel()
		if err := await(t, p2); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		waitQueue(t, a, 2) // 執行中的輪 + 切換
		ga.release(0)
		awaitOK(t, p1)
		if a.Provider().Name != "b" {
			t.Fatal(a.Provider())
		}
		for _, m := range a.Messages() {
			if m.Role == entity.RoleUser && m.Content[0].Text == "二" {
				t.Fatal("被取消的排隊 Prompt 不該跑")
			}
		}
		eqStrs(t, roles(a.Messages()), []string{"user", "assistant"})
	})

	t.Run("Dispose 放掉排隊中的 Prompt", func(t *testing.T) {
		g := gated("a", &glog{})
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{g}})
		r := recOf(a)
		p1 := promptAsync(a, "一")
		g.started(t, 1)
		p2, p3 := promptAsync(a, "二"), promptAsync(a, "三")
		waitQueue(t, a, 3)
		a.Dispose()
		a.Dispose() // 可重複呼叫
		for _, ch := range []<-chan error{p2, p3} {
			if err := await(t, ch); err == nil || err.Error() != "agent 已停用" {
				t.Fatal(err)
			}
		}
		awaitOK(t, p1)
		waitQueue(t, a, 0)
		eqStrs(t, r.ends(), []string{"aborted"})
	})

	t.Run("listener 用 go a.Prompt 接著問", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", []stubp.Step{{Text: "一好"}, {Text: "二好"}}, nil)}})
		var once sync.Once
		follow := make(chan error, 1)
		a.On(func(e Event) {
			if e.Type == "turn_end" {
				once.Do(func() { go func() { follow <- a.Prompt(context.Background(), "續") }() })
			}
		})
		if err := a.Prompt(context.Background(), "一"); err != nil {
			t.Fatal(err)
		}
		awaitOK(t, follow)
		ms := a.Messages()
		eqStrs(t, roles(ms), []string{"user", "assistant", "user", "assistant"})
		if ms[2].Content[0].Text != "續" {
			t.Fatal(ms[2])
		}
	})

	// 同步呼叫是誤用(godoc、README 寫明);這裡確認它不會永遠卡死:Dispose 或那個 Prompt 的 ctx 放得開。
	t.Run("listener 同步呼叫 Prompt → 另一個 goroutine Dispose 放得開", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", []stubp.Step{{Text: "好"}}, nil)}})
		var once sync.Once
		nested := make(chan error, 1)
		a.On(func(e Event) {
			if e.Type == "turn_end" {
				once.Do(func() { nested <- a.Prompt(context.Background(), "巢狀") })
			}
		})
		p1 := promptAsync(a, "一")
		waitQueue(t, a, 2) // 巢狀的排在自己這輪後面
		a.Dispose()
		awaitOK(t, p1)
		if err := await(t, nested); err == nil || err.Error() != "agent 已停用" {
			t.Fatal(err)
		}
	})

	t.Run("listener 同步呼叫 Prompt → 它自己的 ctx 逾時放得開", func(t *testing.T) {
		a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", []stubp.Step{{Text: "好"}}, nil)}})
		var once sync.Once
		nested := make(chan error, 1)
		a.On(func(e Event) {
			if e.Type == "turn_end" {
				once.Do(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
					defer cancel()
					nested <- a.Prompt(ctx, "巢狀")
				})
			}
		})
		awaitOK(t, promptAsync(a, "一"))
		if err := await(t, nested); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		awaitOK(t, promptAsync(a, "二")) // 之後照常可用
	})
}

// 註冊時深複製 ToolDef.Parameters 與 AuditArgs:之後改呼叫端的 map / 切片改不到驗證與稽核。
func TestAgentToolDefCopied(t *testing.T) {
	props := map[string]any{"q": map[string]any{"type": "string"}}
	auditArgs := []string{"q"}
	var execs atomic.Int32
	tool := agentTool(func(d *entity.ToolDef) {
		d.Parameters = entity.Schema{"type": "object", "additionalProperties": false, "properties": props}
		d.AuditArgs = auditArgs
		d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			execs.Add(1)
			return entity.ToolOutput{Content: "ok"}, nil
		}
	})
	rec := &turnRec{}
	script := []stubp.Step{
		{ToolCall: &stubp.Call{Name: "t", Args: map[string]any{"q": "x", "extra": "y"}}},
		{ToolCall: &stubp.Call{Name: "t", Args: map[string]any{"q": "x"}}},
		{Text: "完"},
	}
	a := mustAgent(t, AgentOptions{Providers: []LLMProvider{stubOf("", "", script, nil)}, Tools: []entity.ToolDef{tool}, Audit: rec})
	props["extra"] = map[string]any{"type": "string"} // 註冊後放寬
	auditArgs[0] = "extra"
	if err := a.Prompt(context.Background(), "一"); err != nil {
		t.Fatal(err)
	}
	if execs.Load() != 1 { // 帶 extra 的那次仍被 schema 擋
		t.Fatal(execs.Load())
	}
	ev := rec.audit("tool")
	if ev == nil || ev.Args["q"] != "x" || ev.Args["extra"] != nil {
		t.Fatal(ev)
	}
}

// 工廠是 nil 時回錯,不可退化成沒有白名單的 client。
func TestNewAgentNilClientFactory(t *testing.T) {
	_, err := NewAgent(AgentOptions{Providers: []LLMProvider{stubOf("", "", nil, nil)}, System: sys}, nil)
	if err == nil || err.Error() != "缺少 HTTP client 工廠" {
		t.Fatal(err)
	}
}
