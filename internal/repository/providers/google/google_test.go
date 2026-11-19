//go:build test

package google

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
	r.Header.Set("content-type", "text/event-stream")
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
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

func sse(t *testing.T, name string) func() *http.Response {
	body := fx(t, name)
	return func() *http.Response { return response(200, body, nil) }
}

var cfg = repository.ProviderConfig{Name: "g", API: "google-generative-ai", Model: "gemini-x", BaseURL: "https://g.example.com", APIKey: "SECRET-KEY"}

func user(text string) entity.Message {
	return entity.Message{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: text}}, Privacy: entity.Local}
}

func req(messages ...entity.Message) usecase.LLMRequest {
	return usecase.LLMRequest{System: "sys", Messages: messages, MaxTokens: 100, Tools: []usecase.ToolSpec{{
		Name: "get_price", Description: "d", Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
	}}}
}

func run(ctx context.Context, r usecase.LLMRequest, client *http.Client) ([]usecase.LLMEvent, *entity.Message) {
	var ev []usecase.LLMEvent
	for e := range New(cfg, Deps{HTTP: client}).Stream(ctx, r) {
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

func modelSigs(body map[string]any) []any {
	out := []any{}
	for _, c := range body["contents"].([]any) {
		c := c.(map[string]any)
		if c["role"] == "model" {
			out = append(out, c["parts"].([]any)[0].(map[string]any)["thoughtSignature"])
		}
	}
	return out
}

func TestGoogleProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("文字:text_delta、usage、stop;key 在 header 不在 URL;redirect manual", func(t *testing.T) {
		client, calls := capture(t, sse(t, "google_text.sse"))
		ev, msg := run(ctx, req(user("hi")), client)
		texts := []string{}
		for _, e := range ev {
			if e.Type == usecase.LLMEventTextDelta {
				texts = append(texts, e.Text)
			}
		}
		eq(t, texts, []string{"你好", "世界"})
		eq(t, msg.StopReason, "stop")
		eq(t, msg.Usage, &entity.Usage{Input: 7, Output: 3})
		c := (*calls)[0]
		eq(t, c.url, "https://g.example.com/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
		if strings.Contains(c.url, "SECRET-KEY") {
			t.Fatal("key 出現在 URL")
		}
		eq(t, c.header.Get("x-goog-api-key"), "SECRET-KEY")
		if New(cfg, Deps{HTTP: client}).client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
			t.Fatal("client 會跟隨轉址")
		}
		eq(t, c.body["systemInstruction"], map[string]any{"parts": []any{map[string]any{"text": "sys"}}})
		eq(t, c.body["generationConfig"], map[string]any{"maxOutputTokens": 100.0})
		// Gemini 3 的 parameters 不收 const / additionalProperties;parametersJsonSchema 收完整 JSON Schema
		eq(t, c.body["tools"], []any{map[string]any{"functionDeclarations": []any{map[string]any{
			"name": "get_price", "description": "d",
			"parametersJsonSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		}}}})
	})

	t.Run("工具 schema 原樣用 parametersJsonSchema 送(含 const / anyOf),不送 parameters", func(t *testing.T) {
		client, calls := capture(t, sse(t, "google_text.sse"))
		schema := map[string]any{"type": "object", "properties": map[string]any{"market": map[string]any{"anyOf": []any{
			map[string]any{"const": "tw", "type": "string"}, map[string]any{"const": "us", "type": "string"},
		}}}}
		r := req()
		r.Tools = []usecase.ToolSpec{{Name: "quote", Description: "q", Parameters: schema}}
		run(ctx, r, client)
		d := (*calls)[0].body["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
		eq(t, d["parametersJsonSchema"], schema)
		if _, ok := d["parameters"]; ok {
			t.Fatal("不應送 parameters")
		}
	})

	t.Run("functionCall 的 thoughtSignature 收進 toolCall.signature", func(t *testing.T) {
		client, _ := capture(t, sse(t, "google_tool_sig.sse"))
		_, msg := run(ctx, req(), client)
		eq(t, msg.Content, []entity.Part{{Type: entity.PartToolCall, ID: "get_price-0", Name: "get_price", Args: map[string]any{"symbol": "2330"}, Signature: "SIG-abc"}})
	})

	t.Run("重送歷史:自己的 toolCall 帶回 signature;別家或沒有 signature 的用 Gemini 認可的佔位值(否則 400)", func(t *testing.T) {
		client, calls := capture(t, sse(t, "google_text.sse"))
		call := func(provider, sig string) entity.Message {
			return entity.Message{
				Role: entity.RoleAssistant, Privacy: entity.Local, Provider: provider, StopReason: "toolUse",
				Content: []entity.Part{{Type: entity.PartToolCall, ID: "a-0", Name: "a", Args: map[string]any{}, Signature: sig}},
			}
		}
		tr := entity.Message{Role: entity.RoleToolResult, ToolCallID: "a-0", ToolName: "a", Content: []entity.Part{{Type: entity.PartText, Text: "r"}}, Privacy: entity.Local}
		run(ctx, req(user("q"), call("g", "SIG-own"), tr, call("other", "SIG-other"), tr, call("g", ""), tr), client)
		// 第三則:自己的但沒有 signature 也用佔位值
		eq(t, modelSigs((*calls)[0].body), []any{"SIG-own", "skip_thought_signature_validator", "skip_thought_signature_validator"})
	})

	t.Run("functionCall:自產 id、stopReason toolUse", func(t *testing.T) {
		client, _ := capture(t, sse(t, "google_tool.sse"))
		_, msg := run(ctx, req(), client)
		eq(t, msg.StopReason, "toolUse")
		eq(t, msg.Content, []entity.Part{
			{Type: entity.PartText, Text: "查一下"},
			{Type: entity.PartToolCall, ID: "get_price-0", Name: "get_price", Args: map[string]any{"symbol": "2330"}},
		})
	})

	t.Run("toolResult 轉 functionResponse,連續的併進同一個 user content;assistant 轉 model、thinking 不送", func(t *testing.T) {
		client, calls := capture(t, sse(t, "google_text.sse"))
		tr := func(name, text string, isError bool) entity.Message {
			return entity.Message{Role: entity.RoleToolResult, ToolCallID: name + "-0", ToolName: name, Content: []entity.Part{{Type: entity.PartText, Text: text}}, IsError: isError, Privacy: entity.Local}
		}
		asst := entity.Message{Role: entity.RoleAssistant, Privacy: entity.Local, Provider: "g", StopReason: "toolUse", Content: []entity.Part{
			{Type: entity.PartThinking, Text: "hmm"},
			{Type: entity.PartToolCall, ID: "a-0", Name: "a", Args: map[string]any{"x": 1.0}},
			{Type: entity.PartToolCall, ID: "b-1", Name: "b"}, // Args nil 也要送 {}
		}}
		run(ctx, req(user("q"), asst, tr("a", "ra", false), tr("b", "boom", true)), client)
		eq(t, (*calls)[0].body["contents"], []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "q"}}},
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "a", "args": map[string]any{"x": 1.0}}, "thoughtSignature": "skip_thought_signature_validator"},
				map[string]any{"functionCall": map[string]any{"name": "b", "args": map[string]any{}}, "thoughtSignature": "skip_thought_signature_validator"},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{"name": "a", "response": map[string]any{"content": "ra"}}},
				map[string]any{"functionResponse": map[string]any{"name": "b", "response": map[string]any{"error": "boom"}}},
			}},
		})
	})

	t.Run("3xx 不跟隨 → error;HTTP 錯誤訊息過 redact", func(t *testing.T) {
		client, calls := capture(t, func() *http.Response { return response(302, "", map[string]string{"location": "http://evil"}) })
		_, msg := run(ctx, req(), client)
		eq(t, msg.StopReason, "error")
		eq(t, msg.ErrorMessage, "HTTP 302 轉址,不跟隨")
		eq(t, len(*calls), 1)
		client, _ = capture(t, func() *http.Response {
			return response(400, "bad key sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123", nil)
		})
		_, msg = run(ctx, req(), client)
		eq(t, msg.StopReason, "error")
		if strings.Contains(msg.ErrorMessage, "abcdefghijklmnopqrstuvwxyz0123") || !strings.HasPrefix(msg.ErrorMessage, "HTTP 400: ") {
			t.Fatal(msg.ErrorMessage)
		}
	})

	t.Run("HTTP 錯誤本文的 key 跨第 500 字:先遮再截,不留半截", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response {
			return response(400, strings.Repeat("x", 490)+" sk-abcdefghijklmnopqrstuvwxyz0123", nil)
		})
		_, msg := run(ctx, req(), client)
		eq(t, msg.StopReason, "error")
		if strings.Contains(msg.ErrorMessage, "sk-a") {
			t.Fatal(msg.ErrorMessage)
		}
	})

	t.Run("中止 → aborted", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		client, _ := capture(t, func() *http.Response { return response(200, "", nil) })
		_, msg := run(cctx, req(), client)
		eq(t, msg.StopReason, "aborted")
		eq(t, msg.ErrorMessage, "")
	})

	t.Run("thought 文字 → thinking_delta;壞 JSON 前收到的內容保留", func(t *testing.T) {
		body := `data: {"candidates":[{"content":{"parts":[{"text":"想","thought":true},{"text":"答"}]}}]}` + "\n\n" + "data: {\"candidates\":[\n\n"
		client, _ := capture(t, func() *http.Response { return response(200, body, nil) })
		ev, msg := run(ctx, req(), client)
		eq(t, ev[0], usecase.LLMEvent{Type: usecase.LLMEventThinkingDelta, Text: "想"})
		eq(t, msg.StopReason, "error")
		eq(t, msg.Content, []entity.Part{{Type: entity.PartThinking, Text: "想"}, {Type: entity.PartText, Text: "答"}})
		if msg.Usage != nil {
			t.Fatal("沒收到 usageMetadata 不應有 usage")
		}
	})

	t.Run("data 是 null → error", func(t *testing.T) {
		client, _ := capture(t, func() *http.Response { return response(200, "data: null\n\n", nil) })
		_, msg := run(ctx, req(), client)
		eq(t, msg.StopReason, "error")
	})

	// 審查 Important #1 的對照:google 沒有「型別不符就略過」的路徑,解不進 struct 一律 error
	t.Run("合法 JSON 型別不符 → error,不略過", func(t *testing.T) {
		ok := `data: {"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}` + "\n\n"
		for _, bad := range []string{
			`{"candidates":[{"finishReason":5}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":[1]}}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":7}}]}}]}`,
			`{"usageMetadata":{"promptTokenCount":"9"}}`,
			`{"candidates":"x"}`,
		} {
			body := "data: " + bad + "\n\n" + ok
			client, _ := capture(t, func() *http.Response { return response(200, body, nil) })
			_, msg := run(ctx, req(), client)
			if msg.StopReason != "error" || msg.ErrorMessage == "" {
				t.Fatalf("%s → stopReason=%q errorMessage=%q", bad, msg.StopReason, msg.ErrorMessage)
			}
		}
	})

	t.Run("MAX_TOKENS → length;model 名稱編碼進路徑", func(t *testing.T) {
		body := `data: {"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"MAX_TOKENS"}]}` + "\n\n"
		client, calls := capture(t, func() *http.Response { return response(200, body, nil) })
		c := cfg
		c.Model = "a/b?c#d"
		var msg *entity.Message
		for e := range New(c, Deps{HTTP: client}).Stream(ctx, req()) {
			msg = e.Message
		}
		eq(t, msg.StopReason, "length")
		eq(t, (*calls)[0].url, "https://g.example.com/v1beta/models/a%2Fb%3Fc%23d:streamGenerateContent?alt=sse")
	})

	t.Run("cfg.Headers 蓋不掉 x-goog-api-key;fetch 丟例外 → error", func(t *testing.T) {
		client, calls := capture(t, sse(t, "google_text.sse"))
		c := cfg
		c.Headers = map[string]string{"X-Goog-Api-Key": "other", "x-extra": "1"}
		for range New(c, Deps{HTTP: client}).Stream(ctx, req()) {
		}
		eq(t, (*calls)[0].header.Values("x-goog-api-key"), []string{"SECRET-KEY"})
		eq(t, (*calls)[0].header.Get("x-extra"), "1")
		boom := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("net down sk-abcdefghijklmnopqrstuvwxyz0123")
		})}
		_, msg := run(ctx, req(), boom)
		eq(t, msg.StopReason, "error")
		if !strings.HasPrefix(msg.ErrorMessage, "net down ") || strings.Contains(msg.ErrorMessage, "sk-a") {
			t.Fatal(msg.ErrorMessage)
		}
	})

	t.Run("Deps.Privacy 沒填 → remote(送出前遮蔽靠這個值)", func(t *testing.T) {
		client, _ := capture(t, sse(t, "google_text.sse"))
		p := New(cfg, Deps{HTTP: client})
		eq(t, p.Privacy(), entity.Remote)
		_, msg := run(ctx, req(), client)
		eq(t, msg.Privacy, entity.Remote)
		eq(t, msg.Provider, "g")
		eq(t, New(cfg, Deps{Privacy: entity.Local}).Privacy(), entity.Local)
	})
}

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
