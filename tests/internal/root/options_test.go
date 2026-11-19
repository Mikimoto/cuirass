//go:build test

package root_test

import (
	. "github.com/Mikimoto/cuirass"
	"reflect"
	"testing"
	"time"
)

type testSink struct{}

func (testSink) Write(AuditEvent) bool { return true }

// 漏轉一欄防護就會靜靜關掉:根 AgentOptions 每欄手填非零值,轉換後 internal 選項不得有零值欄位。
// 根 struct 自己也不得有零值欄位:新增欄位沒在這裡填就失敗,逼著同時補轉換。
func TestOptionsConvertAllFields(t *testing.T) {
	o := AgentOptions{
		Providers:       []*Provider{NewStubProvider(StubOptions{})},
		InitialProvider: "stub",
		System:          func() string { return "s" },
		Tools:           []ToolDef{{Name: "t"}},
		Policy:          PolicyOptions{Tools: []string{"t"}},
		Audit:           testSink{},
		Masking:         &Masking{},
		Rewrite:         func(p []Part, _ string) ([]Part, error) { return p, nil },
		Verifiers:       []Verifier{NumberVerifier(0)},
		LeakSources:     func() []string { return nil },
		LeakAllow:       []string{"x"},
		TurnTimeout:     func(ProviderInfo) time.Duration { return time.Second },
		Restore:         &BudgetState{},
	}
	zero := func(name string, v reflect.Value) {
		for i := range v.NumField() {
			if v.Field(i).IsZero() {
				t.Errorf("%s.%s 是零值", name, v.Type().Field(i).Name)
			}
		}
	}
	zero("根 AgentOptions", reflect.ValueOf(o))
	in := o.ToInternal()
	zero("internal AgentOptions", reflect.ValueOf(in))
	if in.Providers[0] == nil || in.Providers[0].Name() != "stub" {
		t.Fatal("provider 沒轉過去")
	}
}

// nil 與零值 *Provider 轉成 nil 介面交給 internal,照原本的訊息回錯,不 panic。
func TestNewRejectsNilAndZeroProvider(t *testing.T) {
	sys := func() string { return "s" }
	p := NewStubProvider(StubOptions{})
	if _, err := New(AgentOptions{Providers: []*Provider{p, nil}, System: sys}); err == nil || err.Error() != "providers[1] 是 nil" {
		t.Fatal(err)
	}
	if _, err := New(AgentOptions{Providers: []*Provider{{}}, System: sys}); err == nil || err.Error() != "providers[0] 是 nil" {
		t.Fatal(err)
	}
}

// 預設值回複本:改了回傳值,下次拿到的不變。
func TestDefaultsAreCopies(t *testing.T) {
	l := DefaultLimits()
	want := l.PerTurn
	l.PerTurn = -1
	if DefaultLimits().PerTurn != want {
		t.Fatal("DefaultLimits 被改到")
	}
	ps := DefaultRedactPatterns()
	n, name := len(ps), ps[0].Name
	ps[0].Name = "x"
	if ps := DefaultRedactPatterns(); len(ps) != n || ps[0].Name != name {
		t.Fatal("DefaultRedactPatterns 被改到")
	}
	if NullAudit() == nil || !NullAudit().Write(AuditEvent{}) {
		t.Fatal("NullAudit")
	}
}
