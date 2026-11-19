package entity

import "slices"

// GateResult 是 { ok: true } 或 { ok: false; reason; cap? }。
type GateResult struct {
	OK     bool
	Reason string
	Cap    bool
}

// CheckToolCall 順序:允許清單 → 額度(先計數)→ 規則。任何 panic 都當成擋(spec §7.1)。
// privacy 是這次呼叫當下的 provider 隱私,給規則看。
func CheckToolCall(agentID, name string, args map[string]any, p Policy, b *Budget, privacy Privacy) (res GateResult) {
	defer func() {
		if recover() != nil {
			res = GateResult{Reason: "規則檢查失敗"}
		}
	}()
	if !slices.Contains(p.Tools, name) {
		return GateResult{Reason: "工具不在允許清單:" + name}
	}
	if r := b.Reserve(agentID, name); !r.OK {
		return r
	}
	ctx := RuleCtx{Privacy: privacy, TurnCount: func(tool string) int { return b.TurnCount(agentID, tool) }}
	for _, rule := range p.Rules {
		if why, block := rule(name, args, ctx); block {
			if why == "" {
				why = "規則擋下"
			}
			return GateResult{Reason: why}
		}
	}
	return GateResult{OK: true}
}
