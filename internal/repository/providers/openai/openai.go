// Package openai 是 OpenAI 相容 provider(chat/completions 串流),涵蓋 Ollama,含 Apple FM(`fm serve`)相容。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

var stopReasons = map[string]string{"stop": "stop", "length": "length", "tool_calls": "toolUse", "function_call": "toolUse"}

// toOpenAI:framework Message → OpenAI messages;thinking part 一律不送。
func toOpenAI(system string, messages []entity.Message) []any {
	out := []any{map[string]any{"role": "system", "content": system}}
	for _, m := range messages {
		switch m.Role {
		case entity.RoleUser:
			parts := []any{}
			for _, p := range m.Content {
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			}
			out = append(out, map[string]any{"role": "user", "content": parts})
		case entity.RoleToolResult:
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": entity.TextOf(m)})
		default:
			var calls []any
			for _, p := range m.Content {
				if p.Type != entity.PartToolCall {
					continue
				}
				args := p.Args
				if args == nil {
					args = map[string]any{}
				}
				b, _ := entity.Stringify(args) // 注意:Go map 鍵會排序,不保留插入順序
				calls = append(calls, map[string]any{"id": p.ID, "type": "function", "function": map[string]any{"name": p.Name, "arguments": string(b)}})
			}
			var content any = entity.TextOf(m)
			if content == "" && len(calls) > 0 {
				content = nil
			}
			msg := map[string]any{"role": "assistant", "content": content}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		}
	}
	return out
}

// Deps:HTTP 照 repository.ProviderClient 處理:nil(或沒 Transport)時框架全新建立傳輸,不讀 proxy 環境變數,
// Privacy 是 local 時撥號只准 loopback(ErrNotLoopback);一律不跟隨轉址。HTTP 只給 internal 測試注入。
type Deps struct {
	HTTP    *http.Client
	Privacy entity.Privacy // "" = remote
}

// Provider 實作 usecase.LLMProvider。
type Provider struct {
	cfg           repository.ProviderConfig
	client        *http.Client
	privacy       entity.Privacy
	contextWindow int
	tokenField    string
}

var _ usecase.LLMProvider = (*Provider)(nil)

// New 對應 createOpenAiProvider。
func New(cfg repository.ProviderConfig, deps Deps) *Provider {
	p := &Provider{cfg: cfg, client: repository.ProviderClient(deps.HTTP, deps.Privacy), privacy: deps.Privacy, contextWindow: cfg.ContextWindow, tokenField: cfg.Compat.MaxTokensField}
	if p.privacy == "" {
		p.privacy = entity.Remote
	}
	if p.tokenField == "" {
		p.tokenField = "max_tokens"
	}
	if p.contextWindow == 0 {
		p.contextWindow = repository.DefaultContextWindow
		if cfg.Compat.AppleFM {
			p.contextWindow = 8192
		}
	}
	return p
}

func (p *Provider) Name() string            { return p.cfg.Name }
func (p *Provider) Model() string           { return p.cfg.Model }
func (p *Provider) Privacy() entity.Privacy { return p.privacy }
func (p *Provider) ContextWindow() int      { return p.contextWindow }
func (p *Provider) MaxTokens() int {
	if p.cfg.MaxTokens == 0 {
		return repository.DefaultMaxTokens
	}
	return p.cfg.MaxTokens
}

// chunk 是串流 data 的 JSON。數字用 float64 接:用 int 接到 12.0 會解碼失敗。
type chunk struct {
	Choices []struct {
		Delta *struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    *float64 `json:"index"`
				ID       string   `json:"id"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     float64 `json:"prompt_tokens"`
		CompletionTokens float64 `json:"completion_tokens"`
	} `json:"usage"`
}

// errText 取出錯誤訊息;*url.Error 只取內層(外層會帶完整 URL,可能含 key)。
func errText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// Stream 逐筆產生串流事件。消費端 break 時 HTTP body 由 defer 關閉。
func (p *Provider) Stream(ctx context.Context, req usecase.LLMRequest) iter.Seq[usecase.LLMEvent] {
	return func(yield func(usecase.LLMEvent) bool) {
		mk := func(m entity.Message) *entity.Message {
			m.Role, m.Privacy, m.Ts, m.Provider = entity.RoleAssistant, p.privacy, time.Now().UnixMilli(), p.cfg.Name
			if m.StopReason == "" {
				m.StopReason = "stop"
			}
			if m.Content == nil {
				m.Content = []entity.Part{}
			}
			return &m
		}
		fail := func(msg string) {
			stop := "error"
			if ctx.Err() != nil {
				stop = "aborted"
			}
			yield(usecase.LLMEvent{Type: usecase.LLMEventDone, Message: mk(entity.Message{StopReason: stop, ErrorMessage: repository.RedactKey(msg, p.cfg.APIKey)})})
		}

		messages := req.Messages
		tools := []any{}
		for _, t := range req.Tools {
			// parameters 先 JSON 正規化成 map/[]any,fmSchema 才走得進去(呼叫端可能傳 struct)
			params, err := entity.CloneJSON[any](t.Parameters)
			if err != nil {
				fail("工具 " + t.Name + " 的 parameters 無法序列化:" + err.Error())
				return
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
		}
		appleFM := p.cfg.Compat.AppleFM
		if appleFM {
			budget := fmBudget(p.contextWindow, req.MaxTokens)
			tb, _ := entity.Stringify(req.Tools)
			fixed := estimateTokens(req.System + string(tb))
			trimmed, dropped := trimTurns(messages, budget, fixed)
			messages = shrinkToolResults(trimmed, budget, fixed)
			if dropped > 0 && !yield(usecase.LLMEvent{Type: usecase.LLMEventNotice, Text: fmt.Sprintf("• 對話太長,已略過最早 %d 輪", dropped)}) {
				return
			}
		}
		body := map[string]any{
			"model":          p.cfg.Model,
			"messages":       toOpenAI(req.System, messages),
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			p.tokenField:     req.MaxTokens,
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
		if appleFM {
			body = fmPayload(body)
		}
		payload, err := entity.Stringify(body)
		if err != nil {
			fail(err.Error())
			return
		}

		endpoint := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
		newReq := func() (*http.Request, error) {
			r, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			r.Header.Set("content-type", "application/json")
			for k, v := range p.cfg.Headers {
				r.Header.Set(k, v)
			}
			// 用 Set 覆蓋:大小寫不同的 authorization 直接取代,不合併兩個值
			r.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
			return r, nil
		}
		res, err := repository.FetchWithRetry(ctx, p.client.Do, newReq, 2)
		if err != nil {
			fail(errText(err))
			return
		}
		if res.Body == nil {
			res.Body = http.NoBody
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			detail, note := "", "(不跟隨轉址)"
			if res.StatusCode >= 400 {
				// 讀到一半出錯就整段丟掉:斷線切出的半截 key 會短於規則門檻遮不到
				if b, err := io.ReadAll(res.Body); err == nil {
					detail = string(b)
				}
				note = ""
			}
			// 先遮再截:先截可能把秘密切成短於規則門檻的半截
			fail(entity.TrimJS(fmt.Sprintf("HTTP %d%s %s", res.StatusCode, note, entity.SliceUTF16(repository.RedactKey(detail, p.cfg.APIKey), 500))))
			return
		}
		if res.Body == http.NoBody {
			fail("回應沒有 body")
			return
		}

		var text strings.Builder
		finish := ""
		var usage *entity.Usage
		type call struct{ id, name, args string }
		calls := map[int]*call{}
		for ev, err := range repository.ParseSSE(res.Body) {
			if err != nil {
				fail(errText(err))
				return
			}
			if ev.Data == "[DONE]" {
				break
			}
			var j chunk
			if err := json.Unmarshal([]byte(ev.Data), &j); err != nil {
				fail(err.Error())
				return
			}
			if j.Usage != nil {
				usage = &entity.Usage{Input: int(j.Usage.PromptTokens), Output: int(j.Usage.CompletionTokens)}
			}
			if len(j.Choices) == 0 {
				continue
			}
			ch := j.Choices[0]
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
			d := ch.Delta
			if d == nil {
				continue
			}
			if d.Content != nil && *d.Content != "" {
				text.WriteString(*d.Content)
				if !yield(usecase.LLMEvent{Type: usecase.LLMEventTextDelta, Text: *d.Content}) {
					return
				}
			}
			for _, tc := range d.ToolCalls {
				idx := 0.0
				if tc.Index != nil {
					idx = *tc.Index
				}
				// JS 的 calls[idx] 只有非負整數才是陣列元素(filter 看得到);其他當屬性,最後被丟掉
				if idx < 0 || idx != math.Trunc(idx) || idx >= math.MaxUint32 {
					continue
				}
				c := calls[int(idx)]
				if c == nil {
					c = &call{}
					calls[int(idx)] = c
				}
				if tc.ID != "" {
					c.id = tc.ID
				}
				if tc.Function != nil {
					c.name += tc.Function.Name
					c.args += tc.Function.Arguments
				}
			}
		}
		keys := make([]int, 0, len(calls))
		for k := range calls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		content := []entity.Part{}
		if text.Len() > 0 {
			content = append(content, entity.Part{Type: entity.PartText, Text: text.String()})
		}
		for i, k := range keys {
			c := calls[k]
			id := c.id
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			args := map[string]any{}
			if c.args != "" {
				// "[1]"、"5" 這種非物件:型別是 map,非物件就當錯誤
				if err := json.Unmarshal([]byte(c.args), &args); err != nil {
					fail(err.Error())
					return
				}
			}
			content = append(content, entity.Part{Type: entity.PartToolCall, ID: id, Name: c.name, Args: args})
		}
		stop := "stop"
		if len(keys) > 0 {
			stop = "toolUse"
		} else if s, ok := stopReasons[finish]; ok {
			stop = s
		}
		yield(usecase.LLMEvent{Type: usecase.LLMEventDone, Message: mk(entity.Message{Content: content, StopReason: stop, Usage: usage})})
	}
}
