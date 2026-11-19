//go:build test

package cuirass_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Mikimoto/cuirass"
)

// 經 cuirass.New 建的 agent:工具連白名單外主機會被擋,而且 egress_block 稽核進使用者的 Audit
// (根套件把 Audit 交給 HTTP client 工廠)。
func TestEgressBlockAuditedViaRoot(t *testing.T) {
	script := []cuirass.StubStep{{ToolCall: &cuirass.StubCall{Name: "fetch"}}, {Text: "完"}}
	var got string
	var events []cuirass.AuditEvent
	audit := memAudit{mu: &sync.Mutex{}, out: &events}
	a, err := cuirass.New(cuirass.AgentOptions{
		Providers: []*cuirass.Provider{cuirass.NewStubProvider(cuirass.StubOptions{Privacy: cuirass.Remote, Script: &script})},
		System:    func() string { return "s" },
		Tools: []cuirass.ToolDef{{
			Name: "fetch", Description: "d", Parameters: cuirass.Schema{"type": "object", "additionalProperties": false}, Effects: cuirass.EffectsNetwork,
			Execute: func(ctx context.Context, _ map[string]any, tc cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				if _, err := tc.HTTP(ctx, "https://evil.example/x", cuirass.HTTPOptions{}); err != nil {
					got = err.Error()
				}
				return cuirass.ToolOutput{Content: "ok"}, nil
			},
		}},
		Policy: cuirass.PolicyOptions{Hosts: []string{"api.example.com"}},
		Audit:  audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "一"); err != nil {
		t.Fatal(err)
	}
	if got != "blocked host" {
		t.Fatalf("白名單外主機應被擋:%q", got)
	}
	n := 0
	audit.mu.Lock()
	defer audit.mu.Unlock()
	for _, e := range events {
		if e.Kind == "egress_block" && e.Host == "evil.example" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("egress_block 稽核應該剛好一筆:%+v", events)
	}
}

type memAudit struct {
	mu  *sync.Mutex
	out *[]cuirass.AuditEvent
}

func (m memAudit) Write(e cuirass.AuditEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	*m.out = append(*m.out, e)
	return true
}
