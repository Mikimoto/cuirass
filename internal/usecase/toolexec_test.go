//go:build test

package usecase

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

type recAudit struct {
	events  []entity.AuditEvent
	fail    bool // Write 回 false
	panic   bool // Write panic
	onWrite func(entity.AuditEvent)
}

func (a *recAudit) Write(e entity.AuditEvent) bool {
	if a.panic {
		panic("disk")
	}
	if a.onWrite != nil {
		a.onWrite(e)
	}
	a.events = append(a.events, e)
	return !a.fail
}

func (a *recAudit) kinds() []string {
	var out []string
	for _, e := range a.events {
		out = append(out, e.Kind)
	}
	return out
}

func (a *recAudit) find(kind string) *entity.AuditEvent {
	for i := range a.events {
		if a.events[i].Kind == kind {
			return &a.events[i]
		}
	}
	return nil
}

type testTool struct {
	entity.ToolDef
	calls []map[string]any
}

// newTool:參數 {q: string, secret?: string},effects none,auditArgs [q]。
func newTool(over func(*entity.ToolDef)) *testTool {
	t := &testTool{}
	t.ToolDef = entity.ToolDef{
		Name: "t", Description: "d",
		Parameters: entity.Schema{
			"type": "object", "additionalProperties": false, "required": []any{"q"},
			"properties": map[string]any{
				"q":      map[string]any{"type": "string"},
				"secret": map[string]any{"type": "string"},
				"nested": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"v": map[string]any{"type": "string"}}},
			},
		},
		Effects:   entity.EffectsNone,
		AuditArgs: []string{"q"},
		Execute: func(_ context.Context, args map[string]any, _ entity.ToolCtx) (entity.ToolOutput, error) {
			t.calls = append(t.calls, args)
			return entity.ToolOutput{Content: map[string]any{"ok": true}, Details: map[string]any{"n": 1.0}}, nil
		},
	}
	if over != nil {
		over(&t.ToolDef)
	}
	return t
}

type harness struct {
	deps   ToolExecDeps
	audit  *recAudit
	events *[]Event
}

func setup(t *testTool, policy func(*entity.Policy), audit *recAudit) harness {
	p := entity.Policy{Tools: []string{t.Name}, Limits: entity.DefaultLimits()}
	if policy != nil {
		policy(&p)
	}
	if audit == nil {
		audit = &recAudit{}
	}
	em := &Emitter{}
	events := &[]Event{}
	em.On(func(e Event) { *events = append(*events, e) })
	return harness{
		deps: ToolExecDeps{
			AgentID: "main",
			Tools:   map[string]entity.ToolDef{t.Name: t.ToolDef},
			Policy:  p,
			Budget:  entity.NewBudget(p.Limits, nil),
			Privacy: entity.Local,
			Audit:   audit,
			Emit:    em,
			HTTP: func(context.Context, string, entity.HTTPOptions) (entity.HTTPResponse, error) {
				return entity.HTTPResponse{Status: 200}, nil
			},
			MaskSet: entity.NewMaskSet(),
		},
		audit:  audit,
		events: events,
	}
}

func (h harness) event(typ string) *Event {
	for i := range *h.events {
		if (*h.events)[i].Type == typ {
			return &(*h.events)[i]
		}
	}
	return nil
}

func call(args map[string]any) entity.Part {
	if args == nil {
		args = map[string]any{"q": "x"}
	}
	return entity.Part{Type: entity.PartToolCall, ID: "c1", Name: "t", Args: args}
}

func textOf(r ToolExecResult) string { return r.Message.Content[0].Text }

func TestExecuteToolCall(t *testing.T) {
	ctx := context.Background()

	t.Run("工具丟 Error('token sk-ant-api03-AAAAAAAAAAAAAAAAAAAA') → 模型看到 REDACTED,audit 有 redacted", func(t *testing.T) {
		def := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{}, errors.New("token sk-ant-api03-AAAAAAAAAAAAAAAAAAAA")
			}
		})
		h := setup(def, nil, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if !r.Message.IsError || !strings.Contains(textOf(r), "REDACTED") || strings.Contains(textOf(r), "sk-ant-api03") {
			t.Fatalf("%+v", r)
		}
		if e := h.audit.find("redacted"); e == nil || e.Count == nil || *e.Count <= 0 {
			t.Fatalf("%+v", h.audit.events)
		}
	})

	t.Run("gate 規則丟例外 → 工具不執行、audit block", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, func(p *entity.Policy) {
			p.Rules = []entity.ToolRule{func(string, map[string]any, entity.RuleCtx) (string, bool) { panic("boom") }}
		}, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if len(def.calls) != 0 || !r.Message.IsError || !slices.Contains(h.audit.kinds(), "block") {
			t.Fatalf("%+v %v", r, h.audit.kinds())
		}
	})

	// 調整:Go 的 nil Tools 只是「不在允許清單」,不會丟例外;用 nil *Budget 讓 CheckToolCall 本身 panic。
	t.Run("checkToolCall 本身丟例外(policy.tools 壞掉)→ 當成擋", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, nil, nil)
		h.deps.Budget = nil
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if len(def.calls) != 0 || !r.Message.IsError {
			t.Fatalf("%+v", r)
		}
		if e := h.audit.find("block"); e == nil || e.Reason != "規則檢查失敗" {
			t.Fatalf("%+v", h.audit.events)
		}
	})

	t.Run("給模型的原因用 publicReason,tool_blocked 事件是真原因", func(t *testing.T) {
		h := setup(newTool(nil), func(p *entity.Policy) {
			p.Rules = []entity.ToolRule{func(string, map[string]any, entity.RuleCtx) (string, bool) { return "真原因", true }}
			p.PublicReason = func(string, string, bool) string { return "不能用" }
		}, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if textOf(r) != "不能用" {
			t.Fatal(textOf(r))
		}
		if e := h.event("tool_blocked"); e == nil || e.Reason != "真原因" {
			t.Fatalf("%+v", *h.events)
		}
	})

	t.Run("額度用完 → cap:true,audit block + cap", func(t *testing.T) {
		h := setup(newTool(nil), func(p *entity.Policy) { p.Limits.PerTurn = 0 }, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		k := h.audit.kinds()
		if !r.Cap || !slices.Contains(k, "block") || !slices.Contains(k, "cap") {
			t.Fatalf("%+v %v", r, k)
		}
	})

	t.Run("auditFailure halt:sink 回 false → 工具不執行", func(t *testing.T) {
		def := newTool(func(d *entity.ToolDef) { d.Effects = entity.EffectsFS })
		h := setup(def, nil, &recAudit{fail: true})
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if len(def.calls) != 0 || !r.Message.IsError {
			t.Fatalf("%+v", r)
		}
		if e := h.event("tool_blocked"); e == nil || e.Reason != "稽核寫入失敗" {
			t.Fatalf("%+v", *h.events)
		}
	})

	t.Run("auditFailure halt:sink 丟例外 → 工具不執行", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, func(p *entity.Policy) { p.AuditFailure = "halt" }, &recAudit{panic: true})
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if len(def.calls) != 0 || textOf(r) != "稽核寫入失敗" {
			t.Fatal(def.calls, textOf(r))
		}
		// warn 時 sink panic 也照樣執行(panic 當寫入失敗,不是讓整個呼叫失敗)
		w := newTool(nil)
		hw := setup(w, nil, &recAudit{panic: true})
		if r := ExecuteToolCall(ctx, call(nil), hw.deps); len(w.calls) != 1 || r.Message.IsError {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("auditFailure warn(effects none 預設):sink 回 false → 照樣執行", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, nil, &recAudit{fail: true})
		ExecuteToolCall(ctx, call(nil), h.deps)
		if len(def.calls) != 1 {
			t.Fatal(def.calls)
		}
	})

	t.Run("schema 不符 → 不計數、不執行", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, nil, nil)
		r := ExecuteToolCall(ctx, call(map[string]any{"q": 1.0}), h.deps)
		if textOf(r) != "參數不符 schema" || !r.Message.IsError || len(def.calls) != 0 {
			t.Fatalf("%+v", r)
		}
		if h.deps.Budget.Snapshot().Session != 0 || len(h.audit.events) != 0 {
			t.Fatalf("%+v %+v", h.deps.Budget.Snapshot(), h.audit.events)
		}
	})

	t.Run("找不到工具 → 參數不符 schema、不計數", func(t *testing.T) {
		h := setup(newTool(nil), nil, nil)
		c := call(nil)
		c.Name = "nope"
		r := ExecuteToolCall(ctx, c, h.deps)
		if textOf(r) != "參數不符 schema" || h.deps.Budget.Snapshot().Session != 0 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("結果超過 toolResultBytes 被截斷並標記", func(t *testing.T) {
		def := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: strings.Repeat("字", 1000)}, nil
			}
		})
		h := setup(def, func(p *entity.Policy) { p.Limits.ToolResultBytes = 100 }, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		s := textOf(r)
		if !strings.Contains(s, "\n…(已截斷)") || strings.Contains(s, "\uFFFD") || entity.UTF16Len(s) >= 200 {
			t.Fatal(s)
		}
	})

	t.Run("結果包在 <untrusted-data tool=...>", func(t *testing.T) {
		h := setup(newTool(nil), nil, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if !regexp.MustCompile(`^<untrusted-data tool="t">\n[\s\S]*\n</untrusted-data>$`).MatchString(textOf(r)) {
			t.Fatal(textOf(r))
		}
		m := r.Message
		if m.Role != entity.RoleToolResult || m.ToolCallID != "c1" || m.ToolName != "t" || m.IsError || m.Privacy != entity.Local {
			t.Fatalf("%+v", m)
		}
	})

	t.Run("auditArgs 外的參數不進 audit", func(t *testing.T) {
		h := setup(newTool(nil), nil, nil)
		ExecuteToolCall(ctx, call(map[string]any{"q": "x", "secret": "s3cr3t"}), h.deps)
		if e := h.audit.find("tool"); e == nil || !reflect.DeepEqual(e.Args, map[string]any{"q": "x"}) {
			t.Fatalf("%+v", h.audit.events)
		}
	})

	t.Run("listener 改 tool_start 的 args → 工具收到的仍是原參數", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, nil, nil)
		h.deps.Emit.On(func(e Event) {
			if e.Type == "tool_start" {
				e.Args["q"] = "hacked"
			}
		})
		h.deps.Emit.On(func(Event) { panic("listener 壞掉") })
		ExecuteToolCall(ctx, call(nil), h.deps)
		if !reflect.DeepEqual(def.calls[0], map[string]any{"q": "x"}) {
			t.Fatal(def.calls)
		}
	})

	t.Run("工具回 { key: '-----BEGIN RSA PRIVATE KEY-----\\nAAA\\n-----END RSA PRIVATE KEY-----' } → 模型看到 REDACTED", func(t *testing.T) {
		def := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: map[string]any{"key": "-----BEGIN RSA PRIVATE KEY-----\nAAA\n-----END RSA PRIVATE KEY-----"}}, nil
			}
		})
		h := setup(def, nil, nil)
		r := ExecuteToolCall(ctx, call(nil), h.deps)
		if !strings.Contains(textOf(r), "REDACTED") || strings.Contains(textOf(r), "BEGIN RSA") {
			t.Fatal(textOf(r))
		}
	})

	t.Run("ToolExecResult.details:成功時是 redact 過的,isError 時 undefined", func(t *testing.T) {
		ok := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: "c", Details: map[string]any{"tok": "sk-ant-api03-AAAAAAAAAAAAAAAAAAAA", "n": 5}}, nil
			}
		})
		a := setup(ok, nil, nil)
		r := ExecuteToolCall(ctx, call(nil), a.deps)
		d, _ := r.Details.(map[string]any)
		if d["n"] != 5.0 || !strings.Contains(d["tok"].(string), "REDACTED") {
			t.Fatalf("%#v", r.Details)
		}
		if e := a.event("tool_end"); e == nil || e.IsError || !reflect.DeepEqual(e.Details, r.Details) {
			t.Fatalf("%+v", *a.events)
		}

		bad := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: "c", IsError: true, Details: map[string]any{"n": 5}}, nil
			}
		})
		b := setup(bad, nil, nil)
		r2 := ExecuteToolCall(ctx, call(nil), b.deps)
		if r2.Details != nil || !r2.Message.IsError {
			t.Fatalf("%+v", r2)
		}
	})

	t.Run("privacy local:masking.collect 的數字進 maskSet;collect 丟例外忽略", func(t *testing.T) {
		h := setup(newTool(nil), nil, nil)
		h.deps.Masking = Masking{Collect: func(string, any) ([]float64, error) { return []float64{123.4}, nil }}
		ExecuteToolCall(ctx, call(nil), h.deps)
		if !h.deps.MaskSet.Has(123.4) {
			t.Fatal(h.deps.MaskSet.Values())
		}
		s2 := setup(newTool(nil), nil, nil)
		s2.deps.Masking = Masking{Collect: func(string, any) ([]float64, error) { panic("x") }}
		if r := ExecuteToolCall(ctx, call(nil), s2.deps); r.Message.IsError {
			t.Fatalf("%+v", r)
		}
		s3 := setup(newTool(nil), nil, nil)
		s3.deps.Masking = Masking{Collect: func(string, any) ([]float64, error) { return []float64{7}, errors.New("x") }}
		if r := ExecuteToolCall(ctx, call(nil), s3.deps); r.Message.IsError || s3.deps.MaskSet.Has(7) {
			t.Fatalf("%+v %v", r, s3.deps.MaskSet.Values())
		}
	})

	t.Run("工具結果 isError(回報或丟例外)→ 不跑 masking.collect(錯誤文字可能回顯模型給的參數)", func(t *testing.T) {
		calls := 0
		collect := func(string, any) ([]float64, error) { calls++; return []float64{99}, nil }
		errDef := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{Content: "bad 99", IsError: true, Details: map[string]any{"n": 99}}, nil
			}
		})
		a := setup(errDef, nil, nil)
		a.deps.Masking = Masking{Collect: collect}
		ExecuteToolCall(ctx, call(nil), a.deps)
		throwDef := newTool(func(d *entity.ToolDef) {
			d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
				return entity.ToolOutput{}, errors.New("bad 99")
			}
		})
		b := setup(throwDef, nil, nil)
		b.deps.Masking = Masking{Collect: collect}
		ExecuteToolCall(ctx, call(nil), b.deps)
		if calls != 0 || a.deps.MaskSet.Has(99) || b.deps.MaskSet.Has(99) {
			t.Fatal(calls)
		}
	})

	t.Run("規則改 call.args → 不影響 schema 驗過的參數:工具收到原值,原 call 不變", func(t *testing.T) {
		def := newTool(nil)
		h := setup(def, func(p *entity.Policy) {
			p.Rules = []entity.ToolRule{func(_ string, args map[string]any, _ entity.RuleCtx) (string, bool) {
				args["q"] = "evil"
				args["secret"] = "x"
				return "", false
			}}
		}, nil)
		c := call(nil)
		ExecuteToolCall(ctx, c, h.deps)
		if !reflect.DeepEqual(def.calls[0], map[string]any{"q": "x"}) || !reflect.DeepEqual(c.Args, map[string]any{"q": "x"}) {
			t.Fatal(def.calls, c.Args)
		}
	})
}

func TestToolExecPanicIsBlock(t *testing.T) {
	def := newTool(func(d *entity.ToolDef) {
		d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			panic("token sk-ant-api03-AAAAAAAAAAAAAAAAAAAA")
		}
	})
	h := setup(def, nil, nil)
	r := ExecuteToolCall(context.Background(), call(nil), h.deps)
	s := textOf(r)
	if !r.Message.IsError || !strings.Contains(s, `{"ok":false,"error":`) || strings.Contains(s, "sk-ant-api03") || r.Details != nil {
		t.Fatalf("%+v", r)
	}
	if e := h.event("tool_end"); e == nil || !e.IsError {
		t.Fatalf("%+v", *h.events)
	}
	// Execute 是 nil 也一樣
	nilDef := newTool(func(d *entity.ToolDef) { d.Execute = nil })
	if r := ExecuteToolCall(context.Background(), call(nil), setup(nilDef, nil, nil).deps); !r.Message.IsError {
		t.Fatalf("%+v", r)
	}
}

// 規則與 Execute 各拿一份深複本:改了(含巢狀值)不影響稽核、事件、原 call,也不影響彼此。
func TestToolExecArgsCloned(t *testing.T) {
	var seen []map[string]any
	def := newTool(func(d *entity.ToolDef) {
		d.AuditArgs = []string{"q"}
		d.Execute = func(_ context.Context, args map[string]any, _ entity.ToolCtx) (entity.ToolOutput, error) {
			seen = append(seen, map[string]any{"q": args["q"], "v": args["nested"].(map[string]any)["v"]})
			args["q"] = "exec-evil"
			args["nested"].(map[string]any)["v"] = "exec-evil"
			return entity.ToolOutput{Content: "ok"}, nil
		}
	})
	h := setup(def, func(p *entity.Policy) {
		p.Rules = []entity.ToolRule{func(_ string, args map[string]any, _ entity.RuleCtx) (string, bool) {
			args["q"] = "rule-evil"
			args["nested"].(map[string]any)["v"] = "rule-evil"
			return "", false
		}}
	}, nil)
	c := call(map[string]any{"q": "x", "nested": map[string]any{"v": "orig"}})
	ExecuteToolCall(context.Background(), c, h.deps)
	if !reflect.DeepEqual(seen, []map[string]any{{"q": "x", "v": "orig"}}) {
		t.Fatalf("Execute 看到規則改過的參數:%v", seen)
	}
	want := map[string]any{"q": "x", "nested": map[string]any{"v": "orig"}}
	if !reflect.DeepEqual(c.Args, want) {
		t.Fatalf("原 call 被改:%v", c.Args)
	}
	if e := h.audit.find("tool"); e == nil || !reflect.DeepEqual(e.Args, map[string]any{"q": "x"}) {
		t.Fatalf("%+v", h.audit.events)
	}
	if e := h.event("tool_end"); e == nil || !reflect.DeepEqual(e.Args, want) {
		t.Fatalf("%+v", *h.events)
	}
}

func TestToolExecIsErrorSkipsCollect(t *testing.T) {
	collected := 0
	def := newTool(func(d *entity.ToolDef) {
		d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			panic("bad 42")
		}
	})
	h := setup(def, nil, nil)
	h.deps.Masking = Masking{Collect: func(string, any) ([]float64, error) { collected++; return []float64{42}, nil }}
	ExecuteToolCall(context.Background(), call(nil), h.deps)
	if collected != 0 || h.deps.MaskSet.Has(42) {
		t.Fatal(collected)
	}
	// 對照組:成功結果會收,工具名照傳
	var tool string
	ok := setup(newTool(nil), nil, nil)
	ok.deps.Masking = Masking{Collect: func(name string, _ any) ([]float64, error) { tool = name; return []float64{42}, nil }}
	ExecuteToolCall(context.Background(), call(nil), ok.deps)
	if tool != "t" || !ok.deps.MaskSet.Has(42) {
		t.Fatal(tool)
	}
	// remote 輪不收
	remote := setup(newTool(nil), nil, nil)
	remote.deps.Privacy = entity.Remote
	remote.deps.Masking = Masking{Collect: func(string, any) ([]float64, error) { collected++; return []float64{42}, nil }}
	ExecuteToolCall(context.Background(), call(nil), remote.deps)
	if collected != 0 {
		t.Fatal(collected)
	}
}

// publicReason panic 時給固定泛化字串(真原因可能含不該給模型看的細節)。
func TestToolExecPublicReasonPanicGeneric(t *testing.T) {
	h := setup(newTool(nil), func(p *entity.Policy) {
		p.Rules = []entity.ToolRule{func(string, map[string]any, entity.RuleCtx) (string, bool) { return "真原因:內部細節", true }}
		p.PublicReason = func(string, string, bool) string { panic("hook 壞掉") }
	}, nil)
	r := ExecuteToolCall(context.Background(), call(nil), h.deps)
	if textOf(r) != "工具呼叫被擋下" || !r.Message.IsError {
		t.Fatalf("%+v", r)
	}
	if e := h.event("tool_blocked"); e == nil || e.Reason != "真原因:內部細節" {
		t.Fatalf("%+v", *h.events)
	}
}

// 工具結果 MarshalJSON panic(序列化時才炸)→ 固定錯誤,不 crash、不帶原文。
type panicMarshal struct{}

func (panicMarshal) MarshalJSON() ([]byte, error) { panic("sk-ant-api03-AAAAAAAAAAAAAAAAAAAA") }

func TestToolExecContentMarshalPanic(t *testing.T) {
	def := newTool(func(d *entity.ToolDef) {
		d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			return entity.ToolOutput{Content: panicMarshal{}, Details: panicMarshal{}}, nil
		}
	})
	h := setup(def, nil, nil)
	r := ExecuteToolCall(context.Background(), call(nil), h.deps)
	if !r.Message.IsError || !strings.Contains(textOf(r), "工具結果無法序列化") || r.Details != nil {
		t.Fatalf("%+v", r)
	}
}

// 切到半個字元時丟掉殘字。
func TestTruncateBytes(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{`"字字`, 7, `"字字`},
		{`"字字`, 5, `"字` + truncated},
		{`"字字`, 6, `"字` + truncated},
		{`"字字`, 4, `"字` + truncated},
		{`"字字`, -1, truncated},
		{"aa\uFFFD字", 5, "aa" + truncated}, // 切在字元邊界,但原本就有的 U+FFFD 也去掉
	} {
		if got := truncateBytes(c.in, c.n); got != c.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// 秘密先遮再截(Review Focus 5):秘密跨過 ToolResultBytes 邊界時,截斷後也不能留下秘密片段;包裝在截斷之後,結尾標記一定在。
func TestToolExecRedactBeforeTruncate(t *testing.T) {
	def := newTool(func(d *entity.ToolDef) {
		d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
			return entity.ToolOutput{Content: strings.Repeat("a", 89) + "sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"}, nil
		}
	})
	h := setup(def, func(p *entity.Policy) { p.Limits.ToolResultBytes = 100 }, nil)
	s := textOf(ExecuteToolCall(context.Background(), call(nil), h.deps))
	if strings.Contains(s, "sk-ant") || !strings.Contains(s, truncated) || !strings.HasSuffix(s, "\n</untrusted-data>") {
		t.Fatal(s)
	}
}

// I1:ctx 在開始執行前就結束(呼叫前已取消,或在規則、稽核寫入這段同步步驟裡被取消)→ 回「已中止」,Execute 一次都不呼叫。
// 有副作用的工具不能在中止之後才開始。
func TestToolExecAbortBeforeExecute(t *testing.T) {
	cases := []struct {
		name string
		mk   func(cancel context.CancelFunc) (func(*entity.Policy), *recAudit)
	}{
		{"呼叫前已取消", func(context.CancelFunc) (func(*entity.Policy), *recAudit) { return nil, nil }},
		{"規則執行中取消", func(cancel context.CancelFunc) (func(*entity.Policy), *recAudit) {
			return func(p *entity.Policy) {
				p.Rules = []entity.ToolRule{func(string, map[string]any, entity.RuleCtx) (string, bool) { cancel(); return "", false }}
			}, nil
		}},
		{"稽核寫入中取消", func(cancel context.CancelFunc) (func(*entity.Policy), *recAudit) {
			return nil, &recAudit{onWrite: func(e entity.AuditEvent) {
				if e.Kind == "tool" {
					cancel()
				}
			}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.name == "呼叫前已取消" {
				cancel()
			}
			var ran atomic.Bool
			def := newTool(func(d *entity.ToolDef) {
				d.Execute = func(context.Context, map[string]any, entity.ToolCtx) (entity.ToolOutput, error) {
					ran.Store(true)
					return entity.ToolOutput{Content: "side effect"}, nil
				}
			})
			policy, audit := c.mk(cancel)
			h := setup(def, policy, audit)
			r := ExecuteToolCall(ctx, call(nil), h.deps)
			time.Sleep(50 * time.Millisecond) // 修正前 Execute 在另一個 goroutine 啟動,給它時間跑,讓 RED 穩定
			if ran.Load() {
				t.Fatal("中止後還開始執行工具")
			}
			if !r.Message.IsError || !strings.Contains(textOf(r), `"error":"`+abortedText+`"`) {
				t.Fatalf("%+v", r)
			}
			if e := h.event("tool_end"); e == nil || !e.IsError {
				t.Fatalf("%+v", *h.events)
			}
		})
	}
}

// 已註冊、但不在 Policy.Tools 的工具,走完整條 ExecuteToolCall:擋下、模型看到允許清單原因、寫 block、不計數、不是 cap。
func TestToolExecNotInAllowlist(t *testing.T) {
	def := newTool(nil)
	other := newTool(func(d *entity.ToolDef) { d.Name = "u" })
	h := setup(def, nil, nil) // Policy.Tools 只有 "t"
	h.deps.Tools["u"] = other.ToolDef
	c := call(nil)
	c.Name = "u"
	r := ExecuteToolCall(context.Background(), c, h.deps)
	if len(other.calls) != 0 || !r.Message.IsError || r.Cap || textOf(r) != "工具不在允許清單:u" {
		t.Fatalf("calls=%v %+v %q", other.calls, r, textOf(r))
	}
	if b := h.audit.find("block"); b == nil || b.Tool != "u" || b.Reason != "工具不在允許清單:u" {
		t.Fatalf("block 稽核:%+v", h.audit.events)
	}
	if h.audit.find("cap") != nil || h.audit.find("tool") != nil {
		t.Fatalf("不該有 cap 或 tool 稽核:%v", h.audit.kinds())
	}
	if e := h.event("tool_blocked"); e == nil || e.Reason != "工具不在允許清單:u" {
		t.Fatalf("%+v", *h.events)
	}
	if h.event("tool_start") != nil {
		t.Fatal("工具不該開始")
	}
	if s := h.deps.Budget.Snapshot(); s.Session != 0 || h.deps.Budget.TurnCount("main", "u") != 0 {
		t.Fatalf("不該計數:%+v", s)
	}
}

// halt 模式稽核寫不進去:除了不執行工具,還要嘗試寫一筆 audit_halt(Tool、Reason 都要對)。
func TestToolExecAuditHaltEvent(t *testing.T) {
	def := newTool(nil)
	h := setup(def, func(p *entity.Policy) { p.AuditFailure = "halt" }, &recAudit{fail: true})
	r := ExecuteToolCall(context.Background(), call(nil), h.deps)
	if len(def.calls) != 0 || textOf(r) != "稽核寫入失敗" {
		t.Fatalf("calls=%v %q", def.calls, textOf(r))
	}
	if got := h.audit.kinds(); !slices.Equal(got, []string{"tool", "audit_halt"}) {
		t.Fatalf("稽核順序 %v,預期 [tool audit_halt]", got)
	}
	if e := h.audit.find("audit_halt"); e.Tool != "t" || e.Reason != "稽核寫入失敗" || e.Agent != "main" {
		t.Fatalf("%+v", e)
	}
	// warn 模式寫不進去時不寫 audit_halt
	hw := setup(newTool(nil), func(p *entity.Policy) { p.AuditFailure = "warn" }, &recAudit{fail: true})
	ExecuteToolCall(context.Background(), call(nil), hw.deps)
	if hw.audit.find("audit_halt") != nil {
		t.Fatal("warn 不該寫 audit_halt")
	}
}
