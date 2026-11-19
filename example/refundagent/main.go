// 教學用退款客服 agent:agent 可以真的退款,但有規則、次數上限與稽核三道煞車。
//
//	go run ./example/refundagent                      # 離線,stub provider 照劇本演
//	ANTHROPIC_API_KEY=... go run ./example/refundagent # 真的 Claude
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Mikimoto/cuirass"
)

// prompts 是三段客人的訊息,各秀一道煞車。
var prompts = []string{
	"我要退訂單 A",     // 1200 元:規則放行
	"我要退訂單 B",     // 5000 元:規則擋下,改開單
	"訂單 C 和 D 都退", // 每輪只能退 1 次:D 碰到上限
}

// demoScript 是離線劇本。stub 不會判斷,只照順序回:「被擋後改開單」是劇本寫好的,
// 真的模型才是自己決定。三段共用一份劇本,第 3 段碰到上限就結束,不需要回答。
func demoScript() *[]cuirass.StubStep {
	call := func(name string, args map[string]any) cuirass.StubStep {
		return cuirass.StubStep{ToolCall: &cuirass.StubCall{Name: name, Args: args}}
	}
	return &[]cuirass.StubStep{
		call("refund", map[string]any{"order_id": "A"}),
		{Text: "訂單 A 已退款 1200 元,款項會在 3 到 5 個工作天內退回。"},
		call("refund", map[string]any{"order_id": "B"}),
		call("create_ticket", map[string]any{"order_id": "B", "reason": "金額超過線上退款上限"}),
		{Text: "訂單 B 金額較大,已轉給專人審核,單號 T-001,審核完會再通知您。"},
		call("refund", map[string]any{"order_id": "C"}),
		call("refund", map[string]any{"order_id": "D"}),
	}
}

func provider(out io.Writer) (*cuirass.Provider, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return cuirass.CreateProvider(cuirass.ProviderConfig{
			Name: "claude", API: "anthropic-messages", Model: "claude-sonnet-4-5",
			BaseURL: "https://api.anthropic.com", APIKey: key,
		}, cuirass.CreateDeps{})
	}
	fmt.Fprintln(out, "(沒有 ANTHROPIC_API_KEY,使用 stub provider 離線示範)")
	return cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: demoScript()}), nil
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "錯誤:", err)
		os.Exit(1)
	}
}

func run(out io.Writer) error {
	p, err := provider(out)
	if err != nil {
		return err
	}
	store := NewStore()
	audit := &MemAudit{}
	agent, err := NewAgent(p, store, audit)
	if err != nil {
		return err
	}
	defer agent.Dispose()

	agent.On(func(e cuirass.Event) {
		switch e.Type {
		case cuirass.EventToolStart:
			fmt.Fprintf(out, "  [工具開始] %s order_id=%v\n", e.Name, e.Args["order_id"])
		case cuirass.EventToolBlocked:
			fmt.Fprintf(out, "  [工具被擋] %s:%s\n", e.Name, e.Reason)
		case cuirass.EventToolEnd:
			fmt.Fprintf(out, "  [工具結束] %s 錯誤=%v\n", e.Name, e.IsError)
		case cuirass.EventMessage:
			if e.Final {
				fmt.Fprintln(out, "  [回答]", cuirass.TextOf(*e.Message))
			}
		case cuirass.EventTurnEnd:
			fmt.Fprintln(out, "  [這輪結束]", e.Reason)
			if e.Reason == "cap" {
				// 上限是硬停:模型不會再說話,由程式自己告訴客人。
				fmt.Fprintln(out, "  這輪碰到上限,剩下的交給真人")
			}
		}
	})

	for _, q := range prompts {
		fmt.Fprintln(out, "客人:", q)
		if err := agent.Prompt(context.Background(), q); err != nil {
			return err
		}
	}

	fmt.Fprintln(out, "--- 稽核紀錄 ---")
	for _, e := range audit.Events() {
		fmt.Fprintln(out, " ", e.Kind, e.Tool, orDash(e.Args["order_id"]), orDash(e.Reason))
	}
	return nil
}

// orDash 把沒有值的欄位印成 -(例如 cap 紀錄沒有參數)。
func orDash(v any) any {
	if v == nil || v == "" {
		return "-"
	}
	return v
}
