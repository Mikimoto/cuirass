//go:build test

package openai

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

func cfg(compat repository.Compat) repository.ProviderConfig {
	return repository.ProviderConfig{Name: "oa", API: "openai-completions", Model: "m", BaseURL: "https://api.example.com/v1", APIKey: "k-123", Compat: compat}
}

func u(text string) entity.Message {
	return entity.Message{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: text}}, Privacy: entity.Local, Ts: 1}
}

func req() usecase.LLMRequest {
	return usecase.LLMRequest{System: "sys", Messages: []entity.Message{u("hi")}, MaxTokens: 100}
}

func run(ctx context.Context, c repository.ProviderConfig, r usecase.LLMRequest, client *http.Client) []usecase.LLMEvent {
	var ev []usecase.LLMEvent
	for e := range New(c, Deps{HTTP: client}).Stream(ctx, r) {
		ev = append(ev, e)
	}
	return ev
}

func done(t *testing.T, ev []usecase.LLMEvent) *entity.Message {
	t.Helper()
	for _, e := range ev {
		if e.Type == usecase.LLMEventDone {
			return e.Message
		}
	}
	t.Fatal("沒有 done 事件")
	return nil
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %#v\nwant: %#v", got, want)
	}
}

func TestOpenAIProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("文字串流 + usage + 請求格式", func(t *testing.T) {
		client, calls := capture(t, sse(fx(t, "openai_text.sse")))
		ev := run(ctx, cfg(repository.Compat{}), req(), client)
		var text strings.Builder
		for _, e := range ev {
			if e.Type == usecase.LLMEventTextDelta {
				text.WriteString(e.Text)
			}
		}
		eq(t, text.String(), "你好")
		m := done(t, ev)
		eq(t, m.StopReason, "stop")
		eq(t, m.Usage, &entity.Usage{Input: 12, Output: 3})
		eq(t, m.Content, []entity.Part{{Type: entity.PartText, Text: "你好"}})
		c := (*calls)[0]
		eq(t, c.url, "https://api.example.com/v1/chat/completions")
		eq(t, c.header.Get("Authorization"), "Bearer k-123")
		// redirect: "manual" 的對應:provider 的 client 一律不跟隨轉址
		if New(cfg(repository.Compat{}), Deps{HTTP: client}).client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
			t.Fatal("client 會跟隨轉址")
		}
		eq(t, c.body["model"], "m")
		eq(t, c.body["stream"], true)
		eq(t, c.body["stream_options"], map[string]any{"include_usage": true})
		eq(t, c.body["max_tokens"], 100.0)
		eq(t, c.body["messages"].([]any)[0], map[string]any{"role": "system", "content": "sys"})
	})

	t.Run("工具呼叫 arguments 分段拼接", func(t *testing.T) {
		client, _ := capture(t, sse(fx(t, "openai_tool.sse")))
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		eq(t, m.StopReason, "toolUse")
		eq(t, m.Content, []entity.Part{{Type: entity.PartToolCall, ID: "call_1", Name: "quote", Args: map[string]any{"symbol": "2330"}}})
	})

	t.Run("maxTokensField 兩種值", func(t *testing.T) {
		client, calls := capture(t, sse(fx(t, "openai_text.sse")))
		run(ctx, cfg(repository.Compat{MaxTokensField: "max_completion_tokens"}), req(), client)
		eq(t, (*calls)[0].body["max_completion_tokens"], 100.0)
		if _, ok := (*calls)[0].body["max_tokens"]; ok {
			t.Fatal("不應有 max_tokens")
		}
	})

	t.Run("歷史訊息轉換:thinking 不送、toolCall → tool_calls、toolResult → tool", func(t *testing.T) {
		msgs := []entity.Message{
			u("q"),
			{Role: entity.RoleAssistant, Provider: "x", Privacy: entity.Local, Ts: 2, StopReason: "toolUse", Content: []entity.Part{
				{Type: entity.PartThinking, Text: "SECRET-THINK"}, {Type: entity.PartToolCall, ID: "c1", Name: "quote", Args: map[string]any{"a": 1.0}},
			}},
			{Role: entity.RoleToolResult, ToolCallID: "c1", ToolName: "quote", Content: []entity.Part{{Type: entity.PartText, Text: "ok"}}, Privacy: entity.Local, Ts: 3},
		}
		client, calls := capture(t, sse(fx(t, "openai_text.sse")))
		r := req()
		r.Messages = msgs
		run(ctx, cfg(repository.Compat{}), r, client)
		sent := (*calls)[0].body["messages"].([]any)
		b, _ := json.Marshal(sent)
		if strings.Contains(string(b), "SECRET-THINK") {
			t.Fatal("thinking 被送出")
		}
		asst := sent[2].(map[string]any)
		eq(t, asst["role"], "assistant")
		eq(t, asst["tool_calls"], []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "quote", "arguments": `{"a":1}`}}})
		eq(t, sent[3], map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"})
	})

	t.Run("HTTP 錯誤:done error,訊息已遮蔽", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response { return response(401, "bad key sk-abcdefghijklmnopqrstuvwx", nil) })
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		eq(t, m.StopReason, "error")
		if !strings.Contains(m.ErrorMessage, "401") || strings.Contains(m.ErrorMessage, "sk-abcdefghijklmnopqrstuvwx") {
			t.Fatal(m.ErrorMessage)
		}
	})

	t.Run("HTTP 錯誤本文的 key 跨第 500 字:先遮再截,不留半截", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			return response(401, strings.Repeat("x", 490)+" sk-abcdefghijklmnopqrstuvwxyz0123", nil)
		})
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		eq(t, m.StopReason, "error")
		if strings.Contains(m.ErrorMessage, "sk-a") {
			t.Fatal(m.ErrorMessage)
		}
	})

	// 審查 I2:本文讀到一半斷線,半截 key 短於規則門檻遮不到,所以整段丟掉。
	t.Run("HTTP 錯誤本文讀到一半出錯:整段丟掉,不留半截 key", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			r := response(401, "", nil)
			r.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"error":"Incorrect API key provided: sk-proj-abcdefgh`), iotest.ErrReader(errors.New("connection reset"))))
			return r
		})
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		eq(t, m.StopReason, "error")
		eq(t, m.ErrorMessage, "HTTP 401")
	})

	t.Run("3xx 不跟隨:done error", func(t *testing.T) {
		client, calls := capture(t, func() *http.Response {
			return response(302, "", map[string]string{"location": "https://evil.example"})
		})
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		eq(t, m.StopReason, "error")
		eq(t, len(*calls), 1)
	})

	t.Run("fetch 丟例外:done error;已中止:aborted", func(t *testing.T) {
		boom := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("net down k-123") })}
		eq(t, done(t, run(ctx, cfg(repository.Compat{}), req(), boom)).StopReason, "error")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		eq(t, done(t, run(cctx, cfg(repository.Compat{}), req(), boom)).StopReason, "aborted")
	})

	t.Run("appleFm:tools schema 只剩 allowlist 欄位、user content 為字串", func(t *testing.T) {
		r := req()
		r.Tools = []usecase.ToolSpec{{Name: "q", Description: "d", Parameters: map[string]any{
			"type": "object", "additionalProperties": false, "properties": map[string]any{"s": map[string]any{"type": "string", "pattern": "^x$"}},
		}}}
		client, calls := capture(t, sse(fx(t, "openai_text.sse")))
		run(ctx, cfg(repository.Compat{AppleFM: true}), r, client)
		body := (*calls)[0].body
		fn := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		eq(t, fn["parameters"], map[string]any{"type": "object", "properties": map[string]any{"s": map[string]any{"type": "string"}}})
		eq(t, body["messages"].([]any)[1].(map[string]any)["content"], "hi")
	})

	t.Run("appleFm 超出 budget:丟最舊輪並先 yield notice", func(t *testing.T) {
		big := strings.Repeat("字", 4000)
		a := func(ts int64) entity.Message {
			return entity.Message{Role: entity.RoleAssistant, Provider: "x", Privacy: entity.Local, Ts: ts, StopReason: "stop", Content: []entity.Part{{Type: entity.PartText, Text: big}}}
		}
		r := req()
		r.Messages, r.MaxTokens = []entity.Message{u(big), a(2), u(big), a(3), u("last")}, 1024
		c := cfg(repository.Compat{AppleFM: true})
		c.ContextWindow, c.MaxTokens = 8192, 1024
		client, calls := capture(t, sse(fx(t, "openai_text.sse")))
		ev := run(ctx, c, r, client)
		notices := 0
		for _, e := range ev {
			if e.Type == usecase.LLMEventNotice {
				notices++
			}
		}
		eq(t, notices, 1)
		eq(t, ev[0], usecase.LLMEvent{Type: usecase.LLMEventNotice, Text: "• 對話太長,已略過最早 2 輪"})
		var roles []any
		for _, m := range (*calls)[0].body["messages"].([]any) {
			roles = append(roles, m.(map[string]any)["role"])
		}
		eq(t, roles, []any{"system", "user"})
	})

	// 消費端提早 break,body 要被關閉(iterator 選擇的理由之一)
	t.Run("消費端提早 break 會關閉 body", func(t *testing.T) {
		closed := false
		client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			resp := response(200, fx(t, "openai_text.sse"), nil)
			resp.Body = closeSpy{resp.Body, &closed}
			return resp, nil
		})}
		for e := range New(cfg(repository.Compat{}), Deps{HTTP: client}).Stream(ctx, req()) {
			if e.Type == usecase.LLMEventTextDelta {
				break
			}
		}
		if !closed {
			t.Fatal("body 沒關")
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

func TestOpenAIStreamError(t *testing.T) {
	ctx := context.Background()
	text := `data: {"choices":[{"index":0,"delta":{"content":"半截"},"finish_reason":null}]}` + "\n\n"
	cases := map[string]struct{ chunk, want string }{
		"頂層 error 加 finish_reason error": {
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"error"}],"error":{"message":"insufficient memory sk-abcdefghijklmnopqrstuvwx"}}`,
			"insufficient memory",
		},
		"只有頂層 error,沒有 choices":  {`data: {"error":{"message":"boom"}}`, "boom"},
		"只有 finish_reason error": {`data: {"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`, "error"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := capture(t, sse(text+c.chunk+"\n\ndata: [DONE]\n\n"))
			m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
			eq(t, m.StopReason, "error")
			eq(t, m.Content, []entity.Part{})
			if !strings.Contains(m.ErrorMessage, c.want) || strings.Contains(m.ErrorMessage, "sk-abcdefghijklmnopqrstuvwx") {
				t.Fatal(m.ErrorMessage)
			}
		})
	}
	t.Run("錯誤訊息很長:截到 500 字", func(t *testing.T) {
		client, _ := capture(t, sse(`data: {"error":{"message":"`+strings.Repeat("x", 900)+`"}}`+"\n\n"))
		m := done(t, run(ctx, cfg(repository.Compat{}), req(), client))
		if n := len([]rune(m.ErrorMessage)); n > 500 {
			t.Fatal(n)
		}
	})
}
