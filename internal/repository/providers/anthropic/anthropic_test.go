//go:build test

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// 測試注入 http.Client 的 Transport。

func fx(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type captured struct {
	url    string
	header http.Header
	body   map[string]any
}

func response(status int, body string, h map[string]string) *http.Response {
	r := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func sse(body string) func() *http.Response {
	return func() *http.Response {
		return response(200, body, map[string]string{"content-type": "text/event-stream"})
	}
}

func capture(t *testing.T, res func() *http.Response) (*http.Client, *[]captured) {
	calls := &[]captured{}
	c := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		*calls = append(*calls, captured{r.URL.String(), r.Header, body})
		resp := res()
		resp.Request = r
		return resp, nil
	})}
	return c, calls
}

var cfg = repository.ProviderConfig{
	Name: "anth", API: "anthropic-messages", Model: "claude-x", BaseURL: "https://api.anthropic.com", APIKey: "sk-test",
	Headers: map[string]string{"x-extra": "1"},
}

func user(text string) entity.Message {
	return entity.Message{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: text}}, Privacy: entity.Local, Ts: 1}
}

func req(messages []entity.Message, tools []usecase.ToolSpec) usecase.LLMRequest {
	return usecase.LLMRequest{System: "sys", Messages: messages, Tools: tools, MaxTokens: 100}
}

func run(ctx context.Context, r usecase.LLMRequest, client *http.Client) ([]usecase.LLMEvent, *entity.Message) {
	return runP(ctx, New(cfg, Deps{HTTP: client, Privacy: entity.Remote}), r)
}

func runP(ctx context.Context, p *Provider, r usecase.LLMRequest) ([]usecase.LLMEvent, *entity.Message) {
	var ev []usecase.LLMEvent
	for e := range p.Stream(ctx, r) {
		ev = append(ev, e)
	}
	if len(ev) == 0 || ev[len(ev)-1].Type != usecase.LLMEventDone {
		return ev, &entity.Message{StopReason: "最後一個事件不是 done"}
	}
	return ev, ev[len(ev)-1].Message
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %#v\nwant: %#v", got, want)
	}
}

func deltas(ev []usecase.LLMEvent, typ string) []string {
	out := []string{}
	for _, e := range ev {
		if e.Type == typ {
			out = append(out, e.Text)
		}
	}
	return out
}

func TestAnthropicProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("純文字", func(t *testing.T) {
		client, _ := capture(t, sse(fx(t, "anthropic_text.sse")))
		ev, msg := run(ctx, req([]entity.Message{user("hi")}, nil), client)
		eq(t, deltas(ev, usecase.LLMEventTextDelta), []string{"你好,", "世界"})
		eq(t, msg.Content, []entity.Part{{Type: entity.PartText, Text: "你好,世界"}})
		eq(t, msg.StopReason, "stop")
		eq(t, msg.Provider, "anth")
		eq(t, msg.Privacy, entity.Remote)
		eq(t, msg.Usage, &entity.Usage{Input: 12, Output: 7})
	})

	t.Run("工具:input_json_delta 拼回 args", func(t *testing.T) {
		client, _ := capture(t, sse(fx(t, "anthropic_tool.sse")))
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.Content, []entity.Part{{Type: entity.PartToolCall, ID: "toolu_1", Name: "read_file", Args: map[string]any{"path": "a.txt"}}})
		eq(t, msg.StopReason, "toolUse")
	})

	t.Run("thinking 帶 signature", func(t *testing.T) {
		client, _ := capture(t, sse(fx(t, "anthropic_thinking.sse")))
		ev, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		if len(deltas(ev, usecase.LLMEventThinkingDelta)) == 0 {
			t.Fatal("沒有 thinking_delta")
		}
		eq(t, msg.Content, []entity.Part{
			{Type: entity.PartThinking, Text: "先想一下", Signature: "sig-abc"},
			{Type: entity.PartText, Text: "答案"},
		})
	})

	t.Run("請求格式", func(t *testing.T) {
		client, calls := capture(t, sse(fx(t, "anthropic_text.sse")))
		asst := func(provider, sig string) entity.Message {
			return entity.Message{Role: entity.RoleAssistant, Privacy: entity.Remote, Ts: 2, Provider: provider, StopReason: "toolUse", Content: []entity.Part{
				{Type: entity.PartThinking, Text: "t", Signature: sig},
				{Type: entity.PartText, Text: "a"},
				{Type: entity.PartToolCall, ID: "c1", Name: "n", Args: map[string]any{"k": 1.0}},
				{Type: entity.PartToolCall, ID: "c2", Name: "n"}, // Args nil 也要送 {}
			}}
		}
		tr := func(id string) entity.Message {
			return entity.Message{Role: entity.RoleToolResult, ToolCallID: id, ToolName: "n", Content: []entity.Part{{Type: entity.PartText, Text: "r-" + id}}, IsError: id == "c2", Privacy: entity.Local, Ts: 3}
		}
		run(ctx, req(
			[]entity.Message{user("q"), asst("other", "s"), tr("c1"), tr("c2"), user("next"), asst("anth", "s1"), asst("anth", "")},
			[]usecase.ToolSpec{{Name: "n", Description: "d", Parameters: map[string]any{"type": "object"}}},
		), client)
		c := (*calls)[0]
		eq(t, c.url, "https://api.anthropic.com/v1/messages")
		// redirect: "manual" 的對應:provider 的 client 一律不跟隨轉址
		if New(cfg, Deps{HTTP: client}).client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
			t.Fatal("client 會跟隨轉址")
		}
		eq(t, c.header.Get("x-api-key"), "sk-test")
		eq(t, c.header.Get("anthropic-version"), "2023-06-01")
		eq(t, c.header.Get("x-extra"), "1")
		b := c.body
		eq(t, b["model"], "claude-x")
		eq(t, b["max_tokens"], 100.0)
		eq(t, b["system"], "sys")
		eq(t, b["stream"], true)
		eq(t, b["tools"], []any{map[string]any{"name": "n", "description": "d", "input_schema": map[string]any{"type": "object"}}})
		msgs := b["messages"].([]any)
		eq(t, len(msgs), 6)
		types := func(m any) []any {
			out := []any{}
			for _, x := range m.(map[string]any)["content"].([]any) {
				out = append(out, x.(map[string]any)["type"])
			}
			return out
		}
		a1, trs, nextUser, a2, a3 := msgs[1].(map[string]any), msgs[2].(map[string]any), msgs[3].(map[string]any), msgs[4].(map[string]any), msgs[5]
		eq(t, types(a1), []any{"text", "tool_use", "tool_use"}) // 別家 thinking 丟掉
		eq(t, a1["content"].([]any)[2], map[string]any{"type": "tool_use", "id": "c2", "name": "n", "input": map[string]any{}})
		eq(t, trs["role"], "user")
		eq(t, trs["content"], []any{
			map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "r-c1"},
			map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": "r-c2", "is_error": true},
		})
		eq(t, nextUser["role"], "user")
		eq(t, a2["content"].([]any)[0], map[string]any{"type": "thinking", "thinking": "t", "signature": "s1"})
		eq(t, types(a3), []any{"text", "tool_use", "tool_use"}) // 同家但無 signature
	})

	t.Run("assistant 空文字 part 不送;整則沒內容就整則不送", func(t *testing.T) {
		client, calls := capture(t, sse(fx(t, "anthropic_text.sse")))
		empty := entity.Message{Role: entity.RoleAssistant, Provider: "other", Privacy: entity.Remote, StopReason: "stop", Content: []entity.Part{{Type: entity.PartText}, {Type: entity.PartThinking, Text: "t"}}}
		run(ctx, req([]entity.Message{user("q"), empty, user("r")}, nil), client)
		eq(t, len((*calls)[0].body["messages"].([]any)), 2)
		if _, ok := (*calls)[0].body["tools"]; ok {
			t.Fatal("沒有工具時不應送 tools")
		}
	})

	t.Run("HTTP 400 → error,錯誤文字經 redact", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			return response(400, `{"error":"bad key sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"}`, nil)
		})
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		if !strings.Contains(msg.ErrorMessage, "400") || strings.Contains(msg.ErrorMessage, "abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Fatal(msg.ErrorMessage)
		}
	})

	t.Run("HTTP 錯誤本文的 key 跨第 500 字:先遮再截,不留半截(T8b)", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			return response(400, strings.Repeat("x", 490)+" sk-abcdefghijklmnopqrstuvwxyz0123", nil)
		})
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		if strings.Contains(msg.ErrorMessage, "sk-a") {
			t.Fatal(msg.ErrorMessage)
		}
	})

	t.Run("HTTP 錯誤本文讀到一半出錯:整段丟掉,不留半截 key", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			r := response(401, "", nil)
			r.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"error":"invalid x-api-key: sk-ant-abcdefgh`), iotest.ErrReader(errors.New("connection reset"))))
			return r
		})
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		eq(t, msg.ErrorMessage, "HTTP 401")
	})

	t.Run("3xx 不跟轉址 → error", func(t *testing.T) {
		client, calls := capture(t, func() *http.Response {
			return response(302, "", map[string]string{"location": "https://evil.example"})
		})
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		eq(t, msg.ErrorMessage, "HTTP 302")
		eq(t, len(*calls), 1)
	})

	t.Run("SSE error 事件 → error,保留已完成的內容", func(t *testing.T) {
		body := strings.Split(fx(t, "anthropic_text.sse"), "event: message_delta")[0] +
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
		client, _ := capture(t, sse(body))
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		eq(t, msg.ErrorMessage, "overloaded_error: Overloaded")
		eq(t, msg.Content, []entity.Part{{Type: entity.PartText, Text: "你好,世界"}})
	})

	t.Run("壞 JSON 事件略過、壞 tool args 留空物件、怪 index 不 panic", func(t *testing.T) {
		body := "data: {not json\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":{},\"content_block\":{\"type\":\"text\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":[1],\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":\"a\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"n\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":\"a\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"[1,\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":\"a\"}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":{}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"
		client, _ := capture(t, sse(body))
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "toolUse")
		eq(t, msg.Content, []entity.Part{{Type: entity.PartToolCall, ID: "t1", Name: "n", Args: map[string]any{}}})
		eq(t, msg.Usage, &entity.Usage{Input: 0, Output: 0})
	})

	t.Run("data 是 null → error", func(t *testing.T) {
		client, _ := capture(t, sse("data: null\n\n"+fx(t, "anthropic_text.sse")))
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		eq(t, msg.ErrorMessage, "串流事件是 null")
	})

	// 審查 Important #1:合法但型別不符的 JSON 不能整筆略過(error 事件被吞掉後接 stop_reason 會變成正常結束)
	t.Run("合法 JSON 型別不符:照 JS 語意取值或 fail-closed,不略過", func(t *testing.T) {
		stop := "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
		start := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"
		cases := []struct{ name, body, stop, errMsg string }{
			// "x".type 是 undefined → `error: `
			{"(a) error 是字串", "data: {\"type\":\"error\",\"error\":\"overloaded sk-abcdefghijklmnopqrstuvwxyz0123\"}\n\n" + stop, "error", "error: "},
			// 樣板字串把 5 轉成 "5"
			{"(b) error.message 是數字", "data: {\"type\":\"error\",\"error\":{\"type\":\"x\",\"message\":5}}\n\n" + stop, "error", "x: 5"},
			{"error.type 是 null → error", "data: {\"type\":\"error\",\"error\":{\"type\":null,\"message\":[1,null,2]}}\n\n" + stop, "error", "error: 1,,2"},
			{"text 不是字串", start + "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":5}}\n\n" + stop, "error", "串流事件欄位型別不符:delta.text"},
			{"partial_json 不是字串", "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"n\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":{}}}\n\n" + stop, "error", "串流事件欄位型別不符:delta.partial_json"},
			{"stop_reason 不是字串", "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":1}}\n\n", "error", "串流事件欄位型別不符:delta.stop_reason"},
			{"input_tokens 不是數字", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":\"9\"}}}\n\n" + stop, "error", "串流事件欄位型別不符:message.usage.input_tokens"},
			{"數字超出 float64", "data: {\"type\":\"ping\",\"index\":1e400}\n\n" + stop, "error", ""},
			// d.type 是 undefined,哪一支都不進
			{"頂層是陣列或字串:略過", "data: [1]\n\ndata: \"x\"\n\n" + stop, "stop", ""},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				client, _ := capture(t, sse(c.body))
				_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
				eq(t, msg.StopReason, c.stop)
				if c.errMsg != "" {
					eq(t, msg.ErrorMessage, c.errMsg)
				}
				if c.stop == "error" && msg.ErrorMessage == "" {
					t.Fatal("沒有 errorMessage")
				}
			})
		}
	})

	t.Run("(c) tool_use 的 id 是數字:照 String() 轉成字串", func(t *testing.T) {
		body := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":7,\"name\":\"n\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"
		client, _ := capture(t, sse(body))
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "toolUse")
		eq(t, msg.Content, []entity.Part{{Type: entity.PartToolCall, ID: "7", Name: "n", Args: map[string]any{}}})
	})

	t.Run("fetch 丟例外:done error;已中止:aborted,不帶 errorMessage", func(t *testing.T) {
		boom := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("net down sk-abcdefghijklmnopqrstuvwxyz0123")
		})}
		_, msg := run(ctx, req([]entity.Message{user("x")}, nil), boom)
		eq(t, msg.StopReason, "error")
		if !strings.HasPrefix(msg.ErrorMessage, "net down ") || strings.Contains(msg.ErrorMessage, "sk-a") {
			t.Fatal(msg.ErrorMessage)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, msg = run(cctx, req([]entity.Message{user("x")}, nil), boom)
		eq(t, msg.StopReason, "aborted")
		eq(t, msg.ErrorMessage, "")
	})

	t.Run("Deps.Privacy 沒填 → remote(送出前遮蔽靠這個值)", func(t *testing.T) {
		p := New(cfg, Deps{})
		eq(t, p.Privacy(), entity.Remote)
		eq(t, New(cfg, Deps{Privacy: entity.Local}).Privacy(), entity.Local)
		eq(t, p.ContextWindow(), repository.DefaultContextWindow)
		eq(t, p.MaxTokens(), repository.DefaultMaxTokens)
	})

	t.Run("消費端提早 break 會關閉 body", func(t *testing.T) {
		closed := false
		client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			resp := response(200, fx(t, "anthropic_text.sse"), nil)
			resp.Body = closeSpy{resp.Body, &closed}
			return resp, nil
		})}
		for e := range New(cfg, Deps{HTTP: client}).Stream(ctx, req([]entity.Message{user("x")}, nil)) {
			if e.Type == usecase.LLMEventTextDelta {
				break
			}
		}
		if !closed {
			t.Fatal("body 沒關")
		}
	})
}

func TestAnthropicEarlyEnd(t *testing.T) {
	t.Run("沒有 stop_reason → error", func(t *testing.T) {
		cut := strings.Split(fx(t, "anthropic_text.sse"), "event: message_delta")[0]
		client, _ := capture(t, func() *http.Response { return response(200, cut, nil) })
		_, msg := run(context.Background(), req([]entity.Message{user("x")}, nil), client)
		eq(t, msg.StopReason, "error")
		if msg.ErrorMessage == "" {
			t.Fatal("沒有 errorMessage")
		}
	})
}

type closeSpy struct {
	io.Reader
	closed *bool
}

func (c closeSpy) Close() error { *c.closed = true; return nil }

// local provider 沒注入 client 時撥號只准 loopback:baseUrl 指到非 loopback IP,done 事件帶 ErrNotLoopback 的訊息
// (Stream 不回 error,錯誤在 done 事件裡)。Control 在 connect 前就拒絕,不會等逾時。
func TestLocalDialsLoopbackOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := repository.ProviderConfig{Name: "x", Model: "m", BaseURL: "http://192.0.2.1:9", APIKey: "k"}
	var done *entity.Message
	for e := range New(c, Deps{Privacy: entity.Local}).Stream(ctx, usecase.LLMRequest{}) {
		if e.Type == usecase.LLMEventDone {
			done = e.Message
		}
	}
	if done == nil || done.StopReason != "error" || !strings.Contains(done.ErrorMessage, repository.ErrNotLoopback.Error()) {
		t.Fatalf("%#v", done)
	}
}
