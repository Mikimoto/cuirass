package example_test

import (
	"context"
	"fmt"

	"github.com/Mikimoto/cuirass"
)

func ExampleNew() {
	// stub provider:第一次呼叫工具,第二次回答(實際使用換成 cuirass.CreateProvider)
	script := []cuirass.StubStep{
		{ToolCall: &cuirass.StubCall{Name: "get_price", Args: map[string]any{"symbol": "2330"}}},
		{Text: "台積電收盤價 1000 元"},
	}
	provider := cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: &script})

	getPrice := cuirass.ToolDef{
		Name:        "get_price",
		Description: "查股價",
		Parameters: cuirass.Schema{
			"type":                 "object",
			"additionalProperties": false, // 必填:參數只收列出的欄位
			"properties":           map[string]any{"symbol": map[string]any{"type": "string"}},
			"required":             []string{"symbol"},
		},
		Effects: cuirass.EffectsNone,
		Execute: func(_ context.Context, args map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
			return cuirass.ToolOutput{Content: map[string]any{"symbol": args["symbol"], "close": 1000}}, nil
		},
	}

	agent, err := cuirass.New(cuirass.AgentOptions{
		Providers: []*cuirass.Provider{provider},
		System:    func() string { return "你是台股助理。" },
		Tools:     []cuirass.ToolDef{getPrice},
	})
	if err != nil {
		panic(err)
	}
	defer agent.Dispose()

	agent.On(func(e cuirass.Event) {
		switch e.Type {
		case cuirass.EventToolEnd:
			fmt.Println("工具:", e.Name, "錯誤:", e.IsError)
		case cuirass.EventMessage:
			if e.Final {
				fmt.Println("回答:", e.Message.Content[0].Text)
			}
		case cuirass.EventTurnEnd:
			fmt.Println("結束:", e.Reason)
		}
	})
	if err := agent.Prompt(context.Background(), "台積電多少?"); err != nil {
		panic(err)
	}
	fmt.Println("額度已用:", agent.Budget().Session)
	// Output:
	// 工具: get_price 錯誤: false
	// 回答: 台積電收盤價 1000 元
	// 結束: done
	// 額度已用: 1
}

func ExampleNumberVerifier() {
	// stub provider:呼叫工具 → 回答(第二行的 3150 算錯了,應是 3215)→ 複核回覆
	script := []cuirass.StubStep{
		{ToolCall: &cuirass.StubCall{Name: "get_rate", Args: map[string]any{}}},
		{Text: "匯率 32.15\n100 美元約 3150 新台幣"},
		{Text: "[]"}, // 複核回覆:這裡寫死不認可(真的模型會依工具資料判斷)
	}
	provider := cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: &script})

	getRate := cuirass.ToolDef{
		Name:        "get_rate",
		Description: "查美元兌新台幣匯率",
		Parameters: cuirass.Schema{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		Effects: cuirass.EffectsNone,
		Execute: func(_ context.Context, _ map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
			rate := map[string]any{"usd_twd": 32.15}
			// 數字核對只認 Details 裡的數字
			return cuirass.ToolOutput{Content: rate, Details: rate}, nil
		},
	}

	agent, err := cuirass.New(cuirass.AgentOptions{
		Providers: []*cuirass.Provider{provider},
		System:    func() string { return "你是匯率助理。" },
		Tools:     []cuirass.ToolDef{getRate},
		Verifiers: []cuirass.Verifier{cuirass.NumberVerifier(0)},
	})
	if err != nil {
		panic(err)
	}
	defer agent.Dispose()

	agent.On(func(e cuirass.Event) {
		switch e.Type {
		case cuirass.EventToolEnd:
			fmt.Println("工具:", e.Name, "錯誤:", e.IsError)
		case cuirass.EventMessage:
			if e.Final {
				fmt.Println("回答:", e.Message.Content[0].Text)
			}
		case cuirass.EventTurnEnd:
			fmt.Println("結束:", e.Reason)
		}
	})
	if err := agent.Prompt(context.Background(), "100 美元換多少新台幣?"); err != nil {
		panic(err)
	}
	// Output:
	// 工具: get_rate 錯誤: false
	// 回答: 匯率 32.15
	// 100 美元約 3150 新台幣 ⚠未核對
	//
	// 有 1 個數字無法對到工具資料
	// 結束: done
}
