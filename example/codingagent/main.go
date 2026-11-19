// 教學用 coding agent:用 cuirass 組一個只能操作記憶體工作區的小 agent。
//
//	go run ./example/codingagent                      # 離線,stub provider 照劇本演
//	ANTHROPIC_API_KEY=... go run ./example/codingagent # 真的 Claude
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Mikimoto/cuirass"
)

const fixedAdd = "package main\n\n// Add 回傳兩數之和。\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

// demoScript 是離線劇本:list -> read -> write -> 回答。
func demoScript() *[]cuirass.StubStep {
	return &[]cuirass.StubStep{
		{ToolCall: &cuirass.StubCall{Name: "list_files", Args: map[string]any{}}},
		{ToolCall: &cuirass.StubCall{Name: "read_file", Args: map[string]any{"path": "add.go"}}},
		{ToolCall: &cuirass.StubCall{Name: "write_file", Args: map[string]any{"path": "add.go", "content": fixedAdd}}},
		{Text: "已修好 add.go:Add 原本用減法,改成加法。"},
	}
}

func provider() (*cuirass.Provider, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return cuirass.CreateProvider(cuirass.ProviderConfig{
			Name: "claude", API: "anthropic-messages", Model: "claude-sonnet-4-5",
			BaseURL: "https://api.anthropic.com", APIKey: key,
		}, cuirass.CreateDeps{})
	}
	fmt.Println("(沒有 ANTHROPIC_API_KEY,使用 stub provider 離線示範)")
	return cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: demoScript()}), nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "錯誤:", err)
		os.Exit(1)
	}
}

func run() error {
	p, err := provider()
	if err != nil {
		return err
	}
	ws := NewWorkspace()
	agent, err := NewAgent(p, ws, nil)
	if err != nil {
		return err
	}
	defer agent.Dispose()

	agent.On(func(e cuirass.Event) {
		switch e.Type {
		case cuirass.EventToolStart:
			fmt.Printf("[工具開始] %s path=%v\n", e.Name, e.Args["path"]) // 不印 content
		case cuirass.EventToolBlocked:
			fmt.Printf("[工具被擋] %s:%s\n", e.Name, e.Reason)
		case cuirass.EventToolEnd:
			fmt.Printf("[工具結束] %s 錯誤=%v\n", e.Name, e.IsError)
		case cuirass.EventMessage:
			if e.Final {
				fmt.Println("[回答]", cuirass.TextOf(*e.Message))
			}
		}
	})
	if err := agent.Prompt(context.Background(), "add.go 的 Add 有 bug,請修好。"); err != nil {
		return err
	}
	c, _ := ws.Read("add.go")
	fmt.Printf("--- add.go 現在的內容 ---\n%s", c)
	return nil
}
