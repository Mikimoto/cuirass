//go:build test

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass"
)

// play 用 stub 劇本跑一輪,回傳收到的事件。
func play(t *testing.T, ws *Workspace, steps ...cuirass.StubStep) []cuirass.Event {
	t.Helper()
	p := cuirass.NewStubProvider(cuirass.StubOptions{Script: &steps})
	a, err := NewAgent(p, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Dispose()
	var evs []cuirass.Event
	a.On(func(e cuirass.Event) { evs = append(evs, e) })
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return evs
}

func find(evs []cuirass.Event, typ string) *cuirass.Event {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

func TestDemoScriptFixesBug(t *testing.T) {
	ws := NewWorkspace()
	steps := *demoScript()
	evs := play(t, ws, steps...)
	if c, _ := ws.Read("add.go"); c != fixedAdd {
		t.Fatalf("add.go 沒被修好:%q", c)
	}
	if find(evs, cuirass.EventToolBlocked) != nil {
		t.Fatal("劇本不該有工具被擋")
	}
}

func TestMissingPathIsErrorResult(t *testing.T) {
	evs := play(t, NewWorkspace(),
		cuirass.StubStep{ToolCall: &cuirass.StubCall{Name: "read_file", Args: map[string]any{"path": "nope.go"}}},
		cuirass.StubStep{Text: "找不到"})
	e := find(evs, cuirass.EventToolEnd)
	if e == nil || !e.IsError {
		t.Fatalf("應該得到錯誤的工具結果:%+v", e)
	}
}

func TestExtraArgRejectedBySchema(t *testing.T) {
	ws := NewWorkspace()
	evs := play(t, ws,
		cuirass.StubStep{ToolCall: &cuirass.StubCall{Name: "write_file", Args: map[string]any{"path": "x.go", "content": "x", "mode": "0777"}}},
		cuirass.StubStep{Text: "done"})
	// schema 不符在最前面就被拒:不發 tool_start,工具根本沒跑(模型只收到一則「參數不符 schema」的錯誤結果)。
	if find(evs, cuirass.EventToolStart) != nil {
		t.Fatal("多餘參數應被封閉 schema 擋下,工具不該開始")
	}
	if _, err := ws.Read("x.go"); err == nil {
		t.Fatal("工具不該被執行")
	}
}

func TestWorkspaceLimits(t *testing.T) {
	ws := NewWorkspace()
	for _, p := range []string{"", "../etc/passwd", "/abs"} {
		if ws.Write(p, "x") == nil {
			t.Errorf("路徑 %q 應被拒絕", p)
		}
	}
	if ws.Write("big", strings.Repeat("x", maxFileSize+1)) == nil {
		t.Error("超大內容應被拒絕")
	}
	if len(ws.Search("BUG")) != 1 {
		t.Error("search 應找到一行")
	}
}

func TestSearchToolRejectsEmpty(t *testing.T) {
	evs := play(t, NewWorkspace(),
		cuirass.StubStep{ToolCall: &cuirass.StubCall{Name: "search", Args: map[string]any{"query": ""}}},
		cuirass.StubStep{Text: "x"})
	if e := find(evs, cuirass.EventToolEnd); e == nil || !e.IsError {
		t.Fatalf("空 query 應回錯誤結果:%+v", e)
	}
}

func ExampleNewAgent() {
	ws := NewWorkspace()
	steps := []cuirass.StubStep{
		{ToolCall: &cuirass.StubCall{Name: "search", Args: map[string]any{"query": "BUG"}}},
		{Text: "bug 在 add.go 第 5 行"},
	}
	p := cuirass.NewStubProvider(cuirass.StubOptions{Script: &steps})
	a, err := NewAgent(p, ws, nil)
	if err != nil {
		panic(err)
	}
	defer a.Dispose()
	a.On(func(e cuirass.Event) {
		if e.Type == cuirass.EventMessage && e.Final {
			fmt.Println(cuirass.TextOf(*e.Message))
		}
	})
	if err := a.Prompt(context.Background(), "bug 在哪?"); err != nil {
		panic(err)
	}
	// Output:
	// bug 在 add.go 第 5 行
}

// 離線示範(沒有 API key)整段跑得通。
func TestRunOffline(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if err := run(); err != nil {
		t.Fatal(err)
	}
}
