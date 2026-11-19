//go:build test

package entity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

var gatePolicy = Policy{Tools: []string{"t"}, Limits: DefaultLimits()}

func ruleReturning(reason string, block bool) ToolRule {
	return func(string, map[string]any, RuleCtx) (string, bool) { return reason, block }
}

func TestGate(t *testing.T) {
	args := map[string]any{}
	fresh := func() *Budget { return NewBudget(DefaultLimits(), nil) }

	t.Run("放行", func(t *testing.T) {
		if r := CheckToolCall("main", "t", args, gatePolicy, fresh(), Local); r != (GateResult{OK: true}) {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("不在 tools 清單 → 擋(且不計數)", func(t *testing.T) {
		b := fresh()
		if r := CheckToolCall("main", "x", args, gatePolicy, b, Local); r != (GateResult{Reason: "工具不在允許清單:x"}) {
			t.Fatalf("%+v", r)
		}
		if s := b.Snapshot().Session; s != 0 {
			t.Fatalf("session=%d", s)
		}
	})

	t.Run("超額 → cap", func(t *testing.T) {
		b := NewBudget(limitsWith(func(l *Limits) { l.PerTurn = 1 }), nil)
		CheckToolCall("main", "t", args, gatePolicy, b, Local)
		if r := CheckToolCall("main", "t", args, gatePolicy, b, Local); r.OK || !r.Cap {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("規則丟例外 → 擋,不放行", func(t *testing.T) {
		p := gatePolicy
		p.Rules = []ToolRule{func(string, map[string]any, RuleCtx) (string, bool) { panic(errors.New("boom")) }}
		if r := CheckToolCall("main", "t", args, p, fresh(), Local); r != (GateResult{Reason: "規則檢查失敗"}) {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("第一個回字串的規則擋;規則看得到 privacy 與本輪次數", func(t *testing.T) {
		var seen [][]any
		p := gatePolicy
		p.Rules = []ToolRule{
			func(_ string, _ map[string]any, ctx RuleCtx) (string, bool) {
				seen = append(seen, []any{ctx.Privacy, ctx.TurnCount("t")})
				return "", false
			},
			ruleReturning("不行", true),
			ruleReturning("不會到", true),
		}
		if r := CheckToolCall("main", "t", args, p, fresh(), Remote); r != (GateResult{Reason: "不行"}) {
			t.Fatalf("%+v", r)
		}
		if !reflect.DeepEqual(seen, [][]any{{Remote, 1}}) {
			t.Fatalf("seen=%v", seen)
		}
	})

	// ToolRule 回 (string, bool),所以「block=true 一律擋,reason 空字串給預設原因」。
	t.Run("規則回 undefined 以外的任何值都擋(空字串、null、物件)", func(t *testing.T) {
		for _, c := range []struct{ ret, want string }{{"", "規則擋下"}, {"null", "null"}, {"0", "0"}, {" ", " "}} {
			p := gatePolicy
			p.Rules = []ToolRule{ruleReturning(c.ret, true)}
			if r := CheckToolCall("main", "t", args, p, fresh(), Local); r != (GateResult{Reason: c.want}) {
				t.Errorf("%q:%+v", c.ret, r)
			}
		}
		// block=false 才放行,不管 reason 寫什麼
		p := gatePolicy
		p.Rules = []ToolRule{ruleReturning("有原因但不擋", false)}
		if r := CheckToolCall("main", "t", args, p, fresh(), Local); r != (GateResult{OK: true}) {
			t.Errorf("block=false:%+v", r)
		}
	})

	t.Run("允許清單或額度檢查丟例外 → 回擋,不往外丟", func(t *testing.T) {
		// nil 清單就是空清單,照樣擋
		p := gatePolicy
		p.Tools = nil
		if r := CheckToolCall("main", "t", args, p, fresh(), Local); r.OK {
			t.Fatalf("nil tools:%+v", r)
		}
		// 額度無法使用:nil *Budget
		if r := CheckToolCall("main", "t", args, gatePolicy, nil, Local); r != (GateResult{Reason: "規則檢查失敗"}) {
			t.Fatalf("nil budget:%+v", r)
		}
	})

	t.Run("被規則擋下的呼叫也計數", func(t *testing.T) {
		b := fresh()
		p := gatePolicy
		p.Rules = []ToolRule{ruleReturning("不行", true)}
		CheckToolCall("main", "t", args, p, b, Local)
		if s := b.Snapshot().Session; s != 1 {
			t.Fatalf("session=%d", s)
		}
	})
}

// 規則 panic(任何值,含 nil)、nil 規則、規則裡再 panic 的 TurnCount → 一律擋,不往外丟,也不卡住 budget 的鎖。
func TestGateRulePanicIsBlock(t *testing.T) {
	b := NewBudget(DefaultLimits(), nil)
	panics := []ToolRule{
		func(string, map[string]any, RuleCtx) (string, bool) { panic("x") },
		func(string, map[string]any, RuleCtx) (string, bool) { panic(nil) },
		func(string, map[string]any, RuleCtx) (string, bool) { panic(errors.New("boom")) },
		func(_ string, a map[string]any, _ RuleCtx) (string, bool) { return a["missing"].(string), false }, // 型別斷言失敗
		func(string, map[string]any, RuleCtx) (string, bool) {
			var m map[string]int
			m["x"] = 1
			return "", false
		},
		nil,
	}
	for i, rule := range panics {
		p := gatePolicy
		p.Rules = []ToolRule{ruleReturning("", false), rule, ruleReturning("", false)}
		if r := CheckToolCall("main", "t", map[string]any{}, p, b, Local); r != (GateResult{Reason: "規則檢查失敗"}) {
			t.Errorf("#%d:%+v", i, r)
		}
	}
	// 鎖沒卡住、計數照算
	if n := b.TurnCount("main", "t"); n != len(panics) {
		t.Fatalf("turnCount=%d", n)
	}
	if !CheckToolCall("main", "t", map[string]any{}, gatePolicy, b, Local).OK {
		t.Fatal("panic 之後的正常呼叫應放行")
	}
}

// 差異測試:規則與步驟用小規格描述,兩邊各自解譯,比對每一步結果與最後的 snapshot。

type gateRuleSpec struct {
	Kind    string `json:"kind"` // pass / block / turnGt / privacy / name
	Reason  string `json:"reason"`
	Tool    string `json:"tool,omitempty"`
	N       int    `json:"n,omitempty"`
	Privacy string `json:"privacy,omitempty"`
}

type gateStep struct {
	Op    string `json:"op"` // call / reset / spend / count
	Agent string `json:"agent"`
	Tool  string `json:"tool,omitempty"`
}

type gateSpec struct {
	Limits struct {
		PerTurn    int            `json:"perTurn"`
		PerSession int            `json:"perSession"`
		PerTool    map[string]int `json:"perTool"`
	} `json:"limits"`
	Tools   []string        `json:"tools"`
	Privacy Privacy         `json:"privacy"`
	Rules   []gateRuleSpec  `json:"rules"`
	Steps   []gateStep      `json:"steps"`
	Restore json.RawMessage `json:"restore,omitempty"`
}

func (s gateSpec) rules() []ToolRule {
	var out []ToolRule
	for _, r := range s.Rules {
		out = append(out, func(name string, _ map[string]any, ctx RuleCtx) (string, bool) {
			switch r.Kind {
			case "block":
				return r.Reason, true
			case "turnGt":
				return r.Reason, ctx.TurnCount(r.Tool) > r.N
			case "privacy":
				return r.Reason, string(ctx.Privacy) == r.Privacy
			case "name":
				return r.Reason, name == r.Tool
			}
			return "", false
		})
	}
	return out
}

func gateJSON(r GateResult) map[string]any {
	if r.OK {
		return map[string]any{"ok": true}
	}
	m := map[string]any{"ok": false, "reason": r.Reason}
	if r.Cap {
		m["cap"] = true
	}
	return m
}

// runGateSpec 回 (每一步結果, snapshot, 是否用完)。
func runGateSpec(s gateSpec) ([]any, BudgetState) {
	l := DefaultLimits()
	l.PerTurn, l.PerSession, l.PerTool = s.Limits.PerTurn, s.Limits.PerSession, s.Limits.PerTool
	var st *BudgetState
	if s.Restore != nil {
		st = restoreJSON(string(s.Restore))
	}
	b := NewBudget(l, st)
	p := Policy{Tools: s.Tools, Limits: l, Rules: s.rules()}
	var steps []any
	for _, x := range s.Steps {
		switch x.Op {
		case "call":
			steps = append(steps, gateJSON(CheckToolCall(x.Agent, x.Tool, map[string]any{}, p, b, s.Privacy)))
		case "reset":
			b.ResetTurn(x.Agent)
			steps = append(steps, nil)
		case "spend":
			steps = append(steps, b.Spend())
		case "count":
			steps = append(steps, b.TurnCount(x.Agent, x.Tool))
		}
	}
	return steps, b.Snapshot()
}

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

func randGateSpec(r *rand.Rand) gateSpec {
	var s gateSpec
	// 原型鍵(constructor、__proto__)是 JS 物件查表的已知漏洞,不放進 golden 測試(TestBudgetPrototypeKeyTool 鎖 Go 的正確行為)
	tools := []string{"a", "b", "c", "x", "工具"}
	s.Limits.PerTurn, s.Limits.PerSession = r.IntN(6), r.IntN(12)
	if r.IntN(2) == 0 {
		s.Limits.PerTool = map[string]int{pick(r, tools...): r.IntN(4)}
	}
	for _, tl := range tools[:4] {
		if r.IntN(2) == 0 {
			s.Tools = append(s.Tools, tl)
		}
	}
	if s.Tools == nil {
		s.Tools = []string{}
	}
	s.Privacy = pick(r, Local, Remote)
	reasons := []string{"", "不行", "擋下:全形 ＡＢＣ　１２３", "null"}
	for range r.IntN(4) {
		s.Rules = append(s.Rules, gateRuleSpec{
			Kind: pick(r, "pass", "block", "turnGt", "privacy", "name"), Reason: pick(r, reasons...),
			Tool: pick(r, tools...), N: r.IntN(4), Privacy: string(pick(r, Local, Remote)),
		})
	}
	for range 1 + r.IntN(15) {
		s.Steps = append(s.Steps, gateStep{
			Op: pick(r, "call", "call", "call", "call", "reset", "spend", "count"), Agent: pick(r, "main", "sub"), Tool: pick(r, tools...),
		})
	}
	if r.IntN(3) == 0 {
		cnt := func() string {
			if r.IntN(5) == 0 {
				return pick(r, "-1", "1.5", "null", `"0"`)
			}
			return pick(r, "0", "1", "2", "5")
		}
		turn := "{"
		for i := range r.IntN(3) {
			if i > 0 {
				turn += ","
			}
			turn += fmt.Sprintf(`%q:`, pick(r, "main", "sub"))
			turn += pick(r, fmt.Sprintf(`{"calls":%s,"perTool":{%q:%s}}`, cnt(), pick(r, tools...), cnt()),
				fmt.Sprintf(`{"calls":%s,"perTool":{}}`, cnt()), "null", `{"calls":0,"perTool":[]}`)
		}
		turn += "}"
		s.Restore = json.RawMessage(fmt.Sprintf(`{"session":%s,"turn":%s}`, cnt(), pick(r, turn, turn, turn, "null", "[]")))
	}
	return s
}

func TestGateGolden(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	var specs []gateSpec
	for range 2000 {
		specs = append(specs, randGateSpec(r))
	}
	cases := make([]goldenCase, len(specs))
	for i, s := range specs {
		b, _ := json.Marshal(s)
		cases[i] = goldenCase{Fn: "gate", Spec: b}
	}
	want := loadGolden(t, cases)
	fails, exhausted := 0, 0
	for i, s := range specs {
		steps, snap := runGateSpec(s)
		gb, _ := json.Marshal(map[string]any{"steps": steps, "snapshot": snap})
		got, wantM := normalize(t, gb).(map[string]any), normalize(t, want[i]).(map[string]any)
		if snap.Exhausted {
			// 用完時 golden 的壞計數是 Infinity(JSON 成 null),Go 沒存;只比 session 與非 count 步驟
			exhausted++
			for _, m := range []map[string]any{got, wantM} {
				m["snapshot"].(map[string]any)["turn"] = nil
				for j, x := range s.Steps {
					if x.Op == "count" {
						m["steps"].([]any)[j] = nil
					}
				}
			}
		}
		if !reflectEqual(got, wantM) {
			fails++
			if fails <= 10 {
				sb, _ := json.Marshal(s)
				t.Errorf("spec %s:\n  go: %s\n  golden: %s", sb, gb, want[i])
			}
		}
	}
	t.Logf("差異測試 %d 筆(其中還原後用完 %d 筆),不一致 %d 筆", len(specs), exhausted, fails)
}
