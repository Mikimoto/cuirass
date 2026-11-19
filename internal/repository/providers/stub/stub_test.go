//go:build test

package stub

import (
	"context"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

var _ usecase.LLMProvider = (*Provider)(nil)

func last(t *testing.T, p *Provider) *entity.Message {
	t.Helper()
	var m *entity.Message
	for e := range p.Stream(context.Background(), usecase.LLMRequest{MaxTokens: 1}) {
		if e.Type == usecase.LLMEventDone {
			m = e.Message
		}
	}
	if m == nil {
		t.Fatal("沒有 done 事件")
	}
	return m
}

func TestStubProvider(t *testing.T) {
	t.Run("依序回工具呼叫、文字、用完回 finalText;onRequest 每次呼叫", func(t *testing.T) {
		calls := 0
		script := []Step{{ToolCall: &Call{Name: "probe", Args: map[string]any{"a": 1}}}, {Text: "hi"}, {}}
		p := New(Options{Script: &script, OnRequest: func(usecase.LLMRequest) { calls++ }})
		m1 := last(t, p)
		if m1.StopReason != "toolUse" || len(m1.Content) != 1 {
			t.Fatalf("%#v", m1)
		}
		if c := m1.Content[0]; c.Type != entity.PartToolCall || c.Name != "probe" || c.Args["a"] != 1 || c.ID == "" {
			t.Fatalf("%#v", c)
		}
		m2 := last(t, p)
		if m2.StopReason != "stop" || len(m2.Content) != 1 || m2.Content[0].Type != entity.PartText || m2.Content[0].Text != "hi" {
			t.Fatalf("%#v", m2)
		}
		if m3 := last(t, p); m3.Content[0].Text != "stub: stopping after blocked/failed tool call" {
			t.Fatalf("%#v", m3)
		}
		if m4 := last(t, p); m4.StopReason != "stop" {
			t.Fatalf("%#v", m4)
		}
		if calls != 4 {
			t.Fatal(calls)
		}
	})
	t.Run("預設 privacy local、可指定 finalText", func(t *testing.T) {
		p := New(Options{FinalText: "end"})
		if p.Privacy() != entity.Local {
			t.Fatal(p.Privacy())
		}
		if m := last(t, p); m.Content[0].Text != "end" {
			t.Fatalf("%#v", m)
		}
	})
	t.Run("共用同一個 script 陣列的 stub 共用一個游標", func(t *testing.T) {
		script := []Step{{Text: "a"}, {Text: "b"}}
		p1 := New(Options{Script: &script})
		p2 := New(Options{Script: &script})
		if m := last(t, p1); m.Content[0].Text != "a" {
			t.Fatalf("%#v", m)
		}
		if m := last(t, p2); m.Content[0].Text != "b" {
			t.Fatalf("%#v", m)
		}
		if len(script) != 0 {
			t.Fatal(len(script))
		}
	})
}

// 順序:先呼叫 onRequest 再取步驟,onRequest 裡補進 script 的步驟由同一次呼叫取用。
func TestStubOnRequestBeforeShift(t *testing.T) {
	script := []Step{}
	p := New(Options{Script: &script, OnRequest: func(usecase.LLMRequest) { script = append(script, Step{Text: "late"}) }})
	if m := last(t, p); m.Content[0].Text != "late" {
		t.Fatal(m.Content[0].Text)
	}
}

// 預設值、訊息欄位、文字步先送 text_delta、工具呼叫沒 args 給空 map、消費端提早 break 不送 done。
func TestStubProviderDetails(t *testing.T) {
	script := []Step{{ToolCall: &Call{Name: "x"}}, {Text: "t"}}
	p := New(Options{Script: &script})
	if p.Name() != "stub" || p.Model() != "stub" || p.ContextWindow() != 200000 || p.MaxTokens() != 4096 {
		t.Fatal(p.Name(), p.Model(), p.ContextWindow(), p.MaxTokens())
	}
	m := last(t, p)
	if m.Role != entity.RoleAssistant || m.Privacy != entity.Local || m.Provider != "stub" || m.Usage == nil || *m.Usage != (entity.Usage{}) || m.Ts == 0 {
		t.Fatalf("%#v", m)
	}
	if m.Content[0].Args == nil {
		t.Fatal("args 應為空 map")
	}
	var types []string
	for e := range p.Stream(context.Background(), usecase.LLMRequest{}) {
		types = append(types, e.Type)
	}
	if len(types) != 2 || types[0] != usecase.LLMEventTextDelta || types[1] != usecase.LLMEventDone {
		t.Fatal(types)
	}
	n := 0
	for range New(Options{Name: "s", Privacy: entity.Remote}).Stream(context.Background(), usecase.LLMRequest{}) {
		n++
		break
	}
	if n != 1 {
		t.Fatal(n)
	}
	if q := New(Options{Name: "s", Privacy: entity.Remote}); q.Name() != "s" || q.Model() != "s" || q.Privacy() != entity.Remote {
		t.Fatal(q.Name(), q.Privacy())
	}
}
