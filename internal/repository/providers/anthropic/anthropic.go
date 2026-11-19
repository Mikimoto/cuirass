// Package anthropic 是 Anthropic Messages API provider(SSE 串流)。
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

type (
	block      = map[string]any
	apiMessage struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
)

// toAPIMessages:內部訊息 → Anthropic 格式。連續 toolResult 併成同一則 user;別家或無 signature 的 thinking 丟掉。
func toAPIMessages(messages []entity.Message, name string) []*apiMessage {
	out := []*apiMessage{}
	for _, m := range messages {
		switch m.Role {
		case entity.RoleToolResult:
			b := block{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": entity.TextOf(m)}
			if m.IsError {
				b["is_error"] = true
			}
			if n := len(out); n > 0 && out[n-1].Role == "user" && allToolResults(out[n-1].Content) {
				out[n-1].Content = append(out[n-1].Content, b)
			} else {
				out = append(out, &apiMessage{"user", []block{b}})
			}
		case entity.RoleUser:
			content := []block{}
			for _, p := range m.Content {
				content = append(content, block{"type": "text", "text": p.Text})
			}
			out = append(out, &apiMessage{"user", content})
		default:
			content := []block{}
			for _, p := range m.Content {
				switch p.Type {
				case entity.PartText:
					if p.Text != "" {
						content = append(content, block{"type": "text", "text": p.Text})
					}
				case entity.PartThinking:
					if m.Provider == name && p.Signature != "" {
						content = append(content, block{"type": "thinking", "thinking": p.Text, "signature": p.Signature})
					}
				default:
					args := p.Args
					if args == nil {
						args = map[string]any{}
					}
					content = append(content, block{"type": "tool_use", "id": p.ID, "name": p.Name, "input": args})
				}
			}
			if len(content) > 0 {
				out = append(out, &apiMessage{"assistant", content})
			}
		}
	}
	return out
}

func allToolResults(bs []block) bool {
	for _, b := range bs {
		if b["type"] != "tool_result" {
			return false
		}
	}
	return true
}

var stopReasons = map[string]string{"end_turn": "stop", "stop_sequence": "stop", "tool_use": "toolUse", "max_tokens": "length"}

// Deps 與 openai.Deps 相同:HTTP 照 repository.ProviderClient 處理(不讀 proxy 環境變數、不跟隨轉址、local 只准撥 loopback)。
type Deps struct {
	HTTP    *http.Client
	Privacy entity.Privacy // "" = remote
}

// Provider 實作 usecase.LLMProvider。
type Provider struct {
	cfg     repository.ProviderConfig
	client  *http.Client
	privacy entity.Privacy
}

var _ usecase.LLMProvider = (*Provider)(nil)

// New 對應 createAnthropicProvider。
func New(cfg repository.ProviderConfig, deps Deps) *Provider {
	p := &Provider{cfg: cfg, client: repository.ProviderClient(deps.HTTP, deps.Privacy), privacy: deps.Privacy}
	if p.privacy == "" {
		p.privacy = entity.Remote
	}
	return p
}

func (p *Provider) Name() string            { return p.cfg.Name }
func (p *Provider) Model() string           { return p.cfg.Model }
func (p *Provider) Privacy() entity.Privacy { return p.privacy }
func (p *Provider) ContextWindow() int {
	if p.cfg.ContextWindow == 0 {
		return repository.DefaultContextWindow
	}
	return p.cfg.ContextWindow
}

func (p *Provider) MaxTokens() int {
	if p.cfg.MaxTokens == 0 {
		return repository.DefaultMaxTokens
	}
	return p.cfg.MaxTokens
}

// undefinedT 代表 JS 的 undefined(欄位不存在),與 JSON 的 null(Go 的 nil)分開。
type undefinedT struct{}

var undefined any = undefinedT{}

// prop 對應 JS 的 v?.k:v 不是物件或沒有 k → undefined。
func prop(v any, k string) any {
	if m, ok := v.(map[string]any); ok {
		if x, ok := m[k]; ok {
			return x
		}
	}
	return undefined
}

// jsString 對應 JS 的 String(x)(樣板字串相同),只處理 JSON 解出來的值。
func jsString(v any) string {
	switch x := v.(type) {
	case undefinedT:
		return "undefined"
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return entity.JSNumberString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// count 取 token 數:缺值或 null → 0、數字取整數;其他型別回 false(不把怪值存進 usage)。
func count(v any) (int, bool) {
	switch x := v.(type) {
	case undefinedT, nil:
		return 0, true
	case float64:
		return int(x), true
	}
	return 0, false
}

// truthy 對應 JS 的真值判斷(JSON 解出來的值不會有 NaN)。
func truthy(v any) bool {
	switch x := v.(type) {
	case undefinedT, nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// mapKey:undefined、null、數字、字串照 JS 的 Map 當鍵;JSON 物件與陣列在 JS 的 Map 是以參考比對,每次 parse 都是新的,永遠對不到;Go 的 map 鍵放它們會 panic。
func mapKey(v any) (any, bool) {
	switch v.(type) {
	case map[string]any, []any:
		return nil, false
	}
	return v, true
}

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
		content := []entity.Part{}
		usage := &entity.Usage{}
		stopReason, errorMessage := "stop", ""
		finish := func() {
			yield(usecase.LLMEvent{Type: usecase.LLMEventDone, Message: &entity.Message{
				Role: entity.RoleAssistant, Content: content, Privacy: p.privacy, Ts: time.Now().UnixMilli(), Provider: p.cfg.Name,
				StopReason: stopReason, Usage: usage, ErrorMessage: errorMessage,
			}})
		}
		fail := func(text string) {
			stopReason, errorMessage = "error", repository.RedactKey(text, p.cfg.APIKey)
			finish()
		}
		// catch 區塊:已中止 → aborted(不帶 errorMessage)
		caught := func(text string) {
			if ctx.Err() != nil {
				stopReason = "aborted"
				finish()
				return
			}
			fail(text)
		}

		body := map[string]any{
			"model":      p.cfg.Model,
			"max_tokens": req.MaxTokens,
			"system":     req.System,
			"messages":   toAPIMessages(req.Messages, p.cfg.Name),
			"stream":     true,
		}
		if len(req.Tools) > 0 {
			tools := []any{}
			for _, t := range req.Tools {
				tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters})
			}
			body["tools"] = tools
		}
		payload, err := entity.Stringify(body)
		if err != nil {
			caught(err.Error())
			return
		}
		endpoint := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1/messages"
		newReq := func() (*http.Request, error) {
			r, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			r.Header.Set("x-api-key", p.cfg.APIKey)
			r.Header.Set("anthropic-version", "2023-06-01")
			r.Header.Set("content-type", "application/json")
			// cfg.Headers 最後套用,蓋過預設
			for k, v := range p.cfg.Headers {
				r.Header.Set(k, v)
			}
			return r, nil
		}
		res, err := repository.FetchWithRetry(ctx, p.client.Do, newReq, 2)
		if err != nil {
			caught(errText(err))
			return
		}
		if res.Body == nil {
			res.Body = http.NoBody
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			text := ""
			if res.StatusCode >= 400 {
				// 讀到一半出錯就整段丟掉:斷線切出的半截 key 會短於規則門檻遮不到
				if b, err := io.ReadAll(res.Body); err == nil {
					text = string(b)
				}
			}
			msg := fmt.Sprintf("HTTP %d", res.StatusCode)
			if text != "" {
				// 先遮再截:先截可能把秘密切成短於規則門檻的半截
				msg += ": " + entity.SliceUTF16(repository.RedactKey(text, p.cfg.APIKey), 500)
			}
			fail(msg)
			return
		}
		if res.Body == http.NoBody {
			fail("回應沒有 body")
			return
		}

		type partial struct {
			part entity.Part
			json string
		}
		inProgress := map[any]*partial{}
		ended := false
		for ev, err := range repository.ParseSSE(res.Body) {
			if err != nil {
				caught(errText(err))
				return
			}
			// 只有語法錯誤才略過(JSON.parse 失敗 → continue)。合法 JSON 照 JS 的寬鬆取值往下走;
			// 會拼出怪字串或存進怪值的欄位(例如 text 不是字串)→ fail-closed,不略過整筆。
			if !json.Valid([]byte(ev.Data)) {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(ev.Data), &v); err != nil { // 例如 1e400 超出 float64
				caught(err.Error())
				return
			}
			if v == nil { // 讀 null 的屬性當錯誤
				caught("串流事件是 null")
				return
			}
			d, ok := v.(map[string]any)
			if !ok { // 陣列、字串、數字:沒有 type,哪一支都不進
				continue
			}
			bad := func(field string) { fail("串流事件欄位型別不符:" + field) }
			key, keyOK := mapKey(prop(d, "index"))
			switch prop(d, "type") {
			case "message_start":
				n, ok := count(prop(prop(prop(d, "message"), "usage"), "input_tokens"))
				if !ok {
					bad("message.usage.input_tokens")
					return
				}
				usage.Input = n
			case "content_block_start":
				if !keyOK {
					continue
				}
				b := prop(d, "content_block")
				part := entity.Part{Type: entity.PartText}
				switch prop(b, "type") {
				case "tool_use":
					part = entity.Part{Type: entity.PartToolCall, ID: jsString(prop(b, "id")), Name: jsString(prop(b, "name")), Args: map[string]any{}}
				case "thinking":
					part.Type = entity.PartThinking
				}
				inProgress[key] = &partial{part: part}
			case "content_block_delta":
				pp := inProgress[key]
				if !keyOK || pp == nil {
					continue
				}
				x := prop(d, "delta")
				xt := prop(x, "type")
				switch {
				case xt == "text_delta" && pp.part.Type == entity.PartText:
					t, ok := prop(x, "text").(string)
					if !ok {
						bad("delta.text")
						return
					}
					pp.part.Text += t
					if !yield(usecase.LLMEvent{Type: usecase.LLMEventTextDelta, Text: t}) {
						return
					}
				case xt == "thinking_delta" && pp.part.Type == entity.PartThinking:
					t, ok := prop(x, "thinking").(string)
					if !ok {
						bad("delta.thinking")
						return
					}
					pp.part.Text += t
					if !yield(usecase.LLMEvent{Type: usecase.LLMEventThinkingDelta, Text: t}) {
						return
					}
				case xt == "signature_delta" && pp.part.Type == entity.PartThinking:
					t, ok := prop(x, "signature").(string)
					if !ok {
						bad("delta.signature")
						return
					}
					pp.part.Signature += t
				case xt == "input_json_delta":
					switch t := prop(x, "partial_json").(type) {
					case string:
						pp.json += t
					case undefinedT, nil: // 缺值當 ""
					default:
						bad("delta.partial_json")
						return
					}
				}
			case "content_block_stop":
				pp := inProgress[key]
				if !keyOK || pp == nil {
					continue
				}
				delete(inProgress, key)
				if pp.part.Type == entity.PartToolCall && pp.json != "" {
					// 壞 JSON 或不是物件:args 留空物件,讓工具參數驗證擋掉
					var a any
					if json.Unmarshal([]byte(pp.json), &a) == nil {
						if obj, ok := a.(map[string]any); ok {
							pp.part.Args = obj
						}
					}
				}
				content = append(content, pp.part)
			case "message_delta":
				if sr := prop(prop(d, "delta"), "stop_reason"); truthy(sr) {
					s, ok := sr.(string)
					if !ok { // 不認得的值不當成正常結束
						bad("delta.stop_reason")
						return
					}
					ended = true
					stopReason = "stop"
					if r, ok := stopReasons[s]; ok {
						stopReason = r
					}
				}
				if ot := prop(prop(d, "usage"), "output_tokens"); ot != undefined {
					n, ok := count(ot)
					if !ok {
						bad("usage.output_tokens")
						return
					}
					usage.Output = n
				}
			case "error":
				// `${d.error?.type ?? "error"}: ${d.error?.message ?? ""}`:error 不是物件也照樣失敗
				e := prop(d, "error")
				typ, msg := "error", ""
				if t := prop(e, "type"); t != undefined && t != nil {
					typ = jsString(t)
				}
				if m := prop(e, "message"); m != undefined && m != nil {
					msg = jsString(m)
				}
				fail(typ + ": " + msg)
				return
			}
		}
		if !ended {
			fail("串流提前結束(沒有收到 stop_reason)")
			return
		}
		finish()
	}
}
