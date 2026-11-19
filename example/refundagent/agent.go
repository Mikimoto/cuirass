package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mikimoto/cuirass"
)

// refundLimit 以上(不含)的退款要轉專人審核。
const refundLimit = 3000

// 規則不寫進 system prompt:3000 元的煞車由框架的 Policy.Rules 擋,不靠模型自律。
const systemPrompt = "你是線上商店的退款客服。客人要退款時呼叫 refund。如果退款被拒,改呼叫 create_ticket 轉給專人審核,並把單號告訴客人。"

// closed 建「封閉」schema:additionalProperties:false 是 cuirass 註冊工具的硬性要求,
// 模型多塞的參數(例如自己填 amount)在工具執行前就被擋下。
func closed(props map[string]any, required ...string) cuirass.Schema {
	return cuirass.Schema{"type": "object", "additionalProperties": false, "properties": props, "required": required}
}

func str(desc string, maxLen int) map[string]any {
	return map[string]any{"type": "string", "description": desc, "maxLength": maxLen}
}

// Tools 回傳兩個工具。全部宣告 EffectsNone(只碰記憶體);AuditArgs 只列 order_id,
// create_ticket 的 reason 是自由文字,不進稽核。
func Tools(s *Store) []cuirass.ToolDef {
	return []cuirass.ToolDef{
		{
			Name: "refund", Description: "全額退款一筆訂單", Effects: cuirass.EffectsNone,
			// 只收訂單編號:金額一律從訂單表拿,模型沒辦法自己報金額。
			Parameters: closed(map[string]any{"order_id": str("訂單編號", 20)}, "order_id"),
			AuditArgs:  []string{"order_id"},
			Execute: func(_ context.Context, args map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				o, err := s.Refund(args["order_id"].(string))
				if err != nil {
					// 業務錯誤用 IsError 回給模型;Go error 代表工具本身壞了。
					// 注意:這次呼叫已用掉這輪唯一的退款次數,模型這輪不能再退。
					return cuirass.ToolOutput{Content: err.Error(), IsError: true}, nil
				}
				return cuirass.ToolOutput{Content: fmt.Sprintf("訂單 %s 已退款 %d 元", o.ID, o.Amount)}, nil
			},
		},
		{
			Name: "create_ticket", Description: "開單轉給專人審核退款", Effects: cuirass.EffectsNone,
			Parameters: closed(map[string]any{"order_id": str("訂單編號", 20), "reason": str("轉專人的原因", 200)}, "order_id", "reason"),
			AuditArgs:  []string{"order_id"},
			Execute: func(_ context.Context, args map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				id := s.CreateTicket(args["order_id"].(string), args["reason"].(string))
				return cuirass.ToolOutput{Content: "已開單,單號 " + id}, nil
			},
		},
	}
}

// Rules 是 3000 元的煞車。看的是訂單表裡的真金額,不是模型說的數字。
// 查不到的訂單放行,由 refund 工具回 IsError。
// 照抄到正式環境時注意:這裡的 Get 不會失敗;換成資料庫查詢後,查詢出錯要「擋」,不能放行。
func Rules(s *Store) []cuirass.ToolRule {
	return []cuirass.ToolRule{func(name string, args map[string]any, _ cuirass.RuleCtx) (string, bool) {
		if name != "refund" {
			return "", false
		}
		id, _ := args["order_id"].(string) // schema 已保證是字串
		if o, ok := s.Get(id); ok && o.Amount > refundLimit {
			return "金額超過 3000 元,需轉專人審核,請改用 create_ticket", true
		}
		return "", false
	}}
}

// NewAgent 建退款客服 agent。audit 必填:nil 會變成什麼都不記的 NullAudit,AuditFailure: "halt" 就形同虛設。
func NewAgent(p *cuirass.Provider, s *Store, audit cuirass.AuditSink) (*cuirass.Agent, error) {
	if audit == nil {
		return nil, errors.New("refundagent: audit 不可為 nil")
	}
	return cuirass.New(cuirass.AgentOptions{
		Providers: []*cuirass.Provider{p},
		System:    func() string { return systemPrompt },
		Tools:     Tools(s),
		Policy: cuirass.PolicyOptions{
			Tools:  []string{"refund", "create_ticket"},
			Rules:  Rules(s),
			Limits: cuirass.LimitsOptions{PerTool: map[string]int{"refund": 1}}, // 每輪最多退 1 次
			// 稽核寫不進去就不執行工具。EffectsNone 預設只是警告,動到錢的工具要 halt。
			AuditFailure: "halt",
		},
		Audit: audit,
	})
}
