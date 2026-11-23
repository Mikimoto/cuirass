//go:build test

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass"
)

// play 用 stub 劇本跑一輪,回傳 agent(讀 Messages 用)與收到的事件。
func play(t *testing.T, s *Store, audit cuirass.AuditSink, steps ...cuirass.StubStep) (*cuirass.Agent, []cuirass.Event) {
	t.Helper()
	p := cuirass.NewStubProvider(cuirass.StubOptions{Script: &steps})
	a, err := NewAgent(p, s, audit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Dispose)
	var evs []cuirass.Event
	a.On(func(e cuirass.Event) { evs = append(evs, e) })
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return a, evs
}

func find(evs []cuirass.Event, typ string) *cuirass.Event {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

func call(name string, args map[string]any) cuirass.StubStep {
	return cuirass.StubStep{ToolCall: &cuirass.StubCall{Name: name, Args: args}}
}

// toolResultText 回某工具在對話紀錄裡的工具結果文字(模型看到的內容)。
func toolResultText(a *cuirass.Agent, tool string) string {
	var b strings.Builder
	for _, m := range a.Messages() {
		if m.Role == "toolResult" && m.ToolName == tool {
			b.WriteString(cuirass.TextOf(m))
		}
	}
	return b.String()
}

func refunded(s *Store, id string) bool {
	o, _ := s.Get(id)
	return o.Refunded
}

func TestRuleAllowsSmallRefund(t *testing.T) {
	s := NewStore()
	_, evs := play(t, s, &MemAudit{}, call("refund", map[string]any{"order_id": "A"}), cuirass.StubStep{Text: "已退款"})
	if !refunded(s, "A") {
		t.Fatal("A 應該退款成功")
	}
	if find(evs, cuirass.EventToolBlocked) != nil {
		t.Fatal("1200 元不該被擋")
	}
}

func TestRuleBlocksLargeRefund(t *testing.T) {
	s := NewStore()
	a, evs := play(t, s, &MemAudit{},
		call("refund", map[string]any{"order_id": "B"}),
		call("create_ticket", map[string]any{"order_id": "B", "reason": "金額較大"}),
		cuirass.StubStep{Text: "已轉專人審核,單號 T-001"})
	if refunded(s, "B") {
		t.Fatal("5000 元不該退款")
	}
	if got := s.Tickets(); len(got) != 1 || got[0] != "T-001" {
		t.Fatalf("工單 = %v", got)
	}
	if e := find(evs, cuirass.EventToolBlocked); e == nil || e.Name != "refund" {
		t.Fatalf("refund 應被擋:%+v", e)
	}
	if txt := toolResultText(a, "refund"); !strings.Contains(txt, "create_ticket") {
		t.Fatalf("模型應收到改用 create_ticket 的原因,實際:%q", txt)
	}
}

// 繞過測試:用 A(規則本來會放行),證明多塞的 amount 是被封閉 schema 擋下,不是被規則擋下。
// refund 只收 order_id,模型沒有任何參數能改變規則看到的金額。
func TestExtraArgRejected(t *testing.T) {
	s := NewStore()
	a, evs := play(t, s, &MemAudit{}, call("refund", map[string]any{"order_id": "A", "amount": 3000}), cuirass.StubStep{Text: "x"})
	if find(evs, cuirass.EventToolStart) != nil || find(evs, cuirass.EventToolBlocked) != nil {
		t.Fatal("多餘參數應在 schema 驗證就被拒,不會開始也不經過規則")
	}
	if refunded(s, "A") {
		t.Fatal("工具不該被執行")
	}
	if txt := toolResultText(a, "refund"); !strings.Contains(txt, "參數不符 schema") {
		t.Fatalf("工具結果 = %q", txt)
	}
}

func TestUnknownOrderIsErrorResult(t *testing.T) {
	_, evs := play(t, NewStore(), &MemAudit{}, call("refund", map[string]any{"order_id": "Z"}), cuirass.StubStep{Text: "找不到"})
	if e := find(evs, cuirass.EventToolEnd); e == nil || !e.IsError {
		t.Fatalf("應得到錯誤的工具結果:%+v", e)
	}
}

func TestPerTurnCap(t *testing.T) {
	s := NewStore()
	audit := &MemAudit{}
	_, evs := play(t, s, audit, call("refund", map[string]any{"order_id": "C"}), call("refund", map[string]any{"order_id": "D"}))
	if !refunded(s, "C") || refunded(s, "D") {
		t.Fatal("每輪只能退 1 次:C 應已退、D 不該退")
	}
	if e := find(evs, cuirass.EventTurnEnd); e == nil || e.Reason != "cap" {
		t.Fatalf("這輪應以 cap 結束:%+v", e)
	}
	kinds := map[string]bool{}
	for _, e := range audit.Events() {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"tool", "block", "cap"} {
		if !kinds[k] {
			t.Errorf("稽核缺 %s 紀錄:%v", k, kinds)
		}
	}
}

type failAudit struct{}

func (failAudit) Write(cuirass.AuditEvent) bool { return false }

func TestAuditFailureHaltsRefund(t *testing.T) {
	s := NewStore()
	play(t, s, failAudit{}, call("refund", map[string]any{"order_id": "A"}), cuirass.StubStep{Text: "x"})
	if refunded(s, "A") {
		t.Fatal("稽核寫不進去時不該退款")
	}
}

func TestDemoScript(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var buf strings.Builder
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"訂單 A 已退款 1200 元,款項", // 第 1 段的最終回答
		"單號 T-001,審核完",       // 第 2 段的最終回答
		"這輪碰到上限,剩下的交給真人",
		"block refund", "cap refund",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("輸出缺 %q:\n%s", want, out)
		}
	}
}

// 沒有稽核就不給建:否則 AuditFailure: "halt" 形同虛設(nil 會變成什麼都不記的 NullAudit)。
func TestNilAuditRejected(t *testing.T) {
	p := cuirass.NewStubProvider(cuirass.StubOptions{Script: &[]cuirass.StubStep{}})
	if _, err := NewAgent(p, NewStore(), nil); err == nil {
		t.Fatal("audit 為 nil 應回 error")
	}
}

// 退錯訂單也會用掉這輪唯一的退款次數(README 有寫)。
func TestWrongOrderUsesTurnRefund(t *testing.T) {
	s := NewStore()
	_, evs := play(t, s, &MemAudit{}, call("refund", map[string]any{"order_id": "Z"}), call("refund", map[string]any{"order_id": "A"}))
	if refunded(s, "A") {
		t.Fatal("退錯訂單後,這輪不該再退款")
	}
	if e := find(evs, cuirass.EventTurnEnd); e == nil || e.Reason != "cap" {
		t.Fatalf("應以 cap 結束:%+v", e)
	}
}

// 回答照抄 system prompt 連續 20 字以上,會被輸出攔截整則換掉(README 有寫)。
func TestEchoingSystemPromptIsBlocked(t *testing.T) {
	_, evs := play(t, NewStore(), &MemAudit{}, cuirass.StubStep{Text: systemPrompt})
	for _, e := range evs {
		if e.Type == cuirass.EventMessage && e.Final && strings.Contains(cuirass.TextOf(*e.Message), "退款客服") {
			t.Fatal("照抄 system prompt 的回答應被攔下")
		}
	}
}
