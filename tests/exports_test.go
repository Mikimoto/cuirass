//go:build test

package cuirass_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Mikimoto/cuirass"
)

// 少了任何一個別名或函式,這個檔就編不過。
var _ = []any{
	(*cuirass.Agent)(nil), cuirass.AgentOptions{}, cuirass.PolicyOptions{}, cuirass.LimitsOptions{}, cuirass.ProviderInfo{},
	cuirass.Event{}, cuirass.Listener(nil), cuirass.Masking{}, cuirass.ContentRewrite(nil), cuirass.Verifier(nil), cuirass.VerifyCtx{},
	cuirass.ToolDef{}, cuirass.ToolCtx{}, cuirass.ToolOutput{}, cuirass.ToolRule(nil), cuirass.RuleCtx{}, cuirass.Effects(""),
	cuirass.Schema{}, cuirass.Policy{}, cuirass.Limits{}, cuirass.HTTPClient(nil), cuirass.HTTPOptions{}, cuirass.HTTPResponse{},
	cuirass.Message{}, cuirass.Part{}, cuirass.Usage{}, cuirass.Privacy(""), cuirass.AuditSink(nil), cuirass.AuditEvent{},
	cuirass.BudgetState{}, cuirass.TurnState{}, cuirass.RedactPattern{}, cuirass.NumToken{}, (*cuirass.NumberBook)(nil),
	(*cuirass.Provider)(nil), cuirass.ProviderConfig{}, cuirass.Compat{}, cuirass.CreateDeps{},
	cuirass.StubOptions{}, cuirass.StubStep{}, cuirass.StubCall{},
	cuirass.EffectsNone, cuirass.EffectsNetwork, cuirass.Local, cuirass.Remote,
	cuirass.EventTurnStart, cuirass.EventTurnEnd, cuirass.EventTextDelta, cuirass.EventThinkingDelta, cuirass.EventToolStart,
	cuirass.EventToolBlocked, cuirass.EventToolEnd, cuirass.EventMessage, cuirass.EventNotice,
	cuirass.New, cuirass.CreateProvider, cuirass.NewStubProvider, cuirass.NewFileAudit, cuirass.NumberVerifier,
	cuirass.DefaultRedactPatterns, cuirass.DefaultLimits, cuirass.NullAudit,
	cuirass.Redact, cuirass.ResolvePrivacy, cuirass.FilterAuditArgs, cuirass.TextOf, cuirass.CollectNumbers, cuirass.NewNumberBook,
	cuirass.ExtractNumbers, cuirass.FindUnverified, cuirass.MarkUnverified, cuirass.LeaksPrompt, cuirass.StripUnsafeLinks, cuirass.NormalizeLeak,
}

func TestExports(t *testing.T) {
	if _, err := cuirass.New(cuirass.AgentOptions{System: func() string { return "s" }}); err == nil || err.Error() != "至少要有一個 provider" {
		t.Fatal(err)
	}
	if _, err := cuirass.CreateProvider(cuirass.ProviderConfig{Name: "x", API: "nope"}, cuirass.CreateDeps{}); err == nil {
		t.Fatal("不支援的 api 應該回錯")
	}
	if p := cuirass.NewStubProvider(cuirass.StubOptions{}); p.Name() != "stub" || p.Model() != "stub" || p.Privacy() != cuirass.Local ||
		p.ContextWindow() != 200000 || p.MaxTokens() != 4096 {
		t.Fatal("stub")
	}
	if cuirass.Redact("a@b.com") != "[REDACTED:email]" || cuirass.TextOf(cuirass.Message{Content: []cuirass.Part{{Type: "text", Text: "x"}}}) != "x" {
		t.Fatal("純函式")
	}
}

// 回歸:兩個 stub 共用同一個 *[]StubStep 就共用一個游標;切換 provider 後接著跑,回答照共用腳本的順序(切換模型的情境靠它)。
func TestStubSharedScriptCursor(t *testing.T) {
	script := []cuirass.StubStep{{Text: "一"}, {Text: "二"}, {Text: "三"}}
	a := cuirass.NewStubProvider(cuirass.StubOptions{Name: "a", Script: &script})
	b := cuirass.NewStubProvider(cuirass.StubOptions{Name: "b", Script: &script})
	agent, err := cuirass.New(cuirass.AgentOptions{Providers: []*cuirass.Provider{a, b}, System: func() string { return "s" }})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Dispose()
	var got []string
	for _, name := range []string{"a", "b", "a"} {
		if !agent.SetProvider(name) {
			t.Fatal("SetProvider", name)
		}
		if err := agent.Prompt(context.Background(), "q"); err != nil {
			t.Fatal(err)
		}
		ms := agent.Messages()
		got = append(got, cuirass.TextOf(ms[len(ms)-1]))
	}
	if want := []string{"一", "二", "三"}; !slices.Equal(got, want) || len(script) != 0 {
		t.Fatalf("回答 %q,應為 %q;腳本剩 %d 步", got, want, len(script))
	}
}
