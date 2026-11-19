// Package google 是 Google Generative AI(Gemini)provider:streamGenerateContent SSE。
package google

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

// skipSignature:Gemini 3 要求 functionCall 帶 thoughtSignature;別家產生或沒有簽章的呼叫用官方認可的佔位值,否則 400。
const skipSignature = "skip_thought_signature_validator"

type (
	gpart    = map[string]any
	gcontent struct {
		Role  string  `json:"role"`
		Parts []gpart `json:"parts"`
	}
)

// toContents:連續的 toolResult 併進同一個 user content(Gemini 要求一個 model 回合的 functionResponse 放一起)。thinking 不送。
func toContents(messages []entity.Message, name string) []*gcontent {
	out := []*gcontent{}
	for _, m := range messages {
		switch m.Role {
		case entity.RoleUser:
			parts := []gpart{}
			for _, p := range m.Content {
				parts = append(parts, gpart{"text": p.Text})
			}
			out = append(out, &gcontent{"user", parts})
		case entity.RoleAssistant:
			parts := []gpart{}
			for _, p := range m.Content {
				switch p.Type {
				case entity.PartText:
					parts = append(parts, gpart{"text": p.Text})
				case entity.PartToolCall:
					args := p.Args
					if args == nil {
						args = map[string]any{}
					}
					sig := skipSignature
					if m.Provider == name && p.Signature != "" {
						sig = p.Signature
					}
					parts = append(parts, gpart{"functionCall": map[string]any{"name": p.Name, "args": args}, "thoughtSignature": sig})
				}
			}
			if len(parts) > 0 {
				out = append(out, &gcontent{"model", parts})
			}
		default:
			text := entity.TextOf(m)
			response := map[string]any{"content": text}
			if m.IsError {
				response = map[string]any{"error": text}
			}
			part := gpart{"functionResponse": map[string]any{"name": m.ToolName, "response": response}}
			if n := len(out); n > 0 && out[n-1].Role == "user" && allFunctionResponses(out[n-1].Parts) {
				out[n-1].Parts = append(out[n-1].Parts, part)
			} else {
				out = append(out, &gcontent{"user", []gpart{part}})
			}
		}
	}
	return out
}

func allFunctionResponses(parts []gpart) bool {
	for _, p := range parts {
		if _, ok := p["functionResponse"]; !ok {
			return false
		}
	}
	return true
}

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

// New 對應 createGoogleProvider。
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

// chunk 是串流 data 的 JSON。數字用 float64 接;part 的 text/thought/thoughtSignature 用 any,照 typeof 判斷。
// 其他欄位型別不符(例如 functionCall.args 不是物件)→ 解碼失敗 → error(不帶著怪值往下走)。
type chunk struct {
	Candidates []struct {
		Content *struct {
			Parts []struct {
				FunctionCall *struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"functionCall"`
				Text             any `json:"text"`
				Thought          any `json:"thought"`
				ThoughtSignature any `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     float64 `json:"promptTokenCount"`
		CandidatesTokenCount float64 `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
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
		stopReason, errorMessage := "stop", ""
		var usage *entity.Usage
		var text, thinking strings.Builder
		calls := 0
		flushText := func() {
			if thinking.Len() > 0 {
				content = append(content, entity.Part{Type: entity.PartThinking, Text: thinking.String()})
			}
			if text.Len() > 0 {
				content = append(content, entity.Part{Type: entity.PartText, Text: text.String()})
			}
			thinking.Reset()
			text.Reset()
		}

		// 回 error 等同 throw,由下方統一處理。消費端 break 時回 (true, nil)。
		stopped, err := func() (bool, error) {
			body := map[string]any{
				"contents":         toContents(req.Messages, p.cfg.Name),
				"generationConfig": map[string]any{"maxOutputTokens": req.MaxTokens},
			}
			if req.System != "" {
				body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": req.System}}}
			}
			if len(req.Tools) > 0 {
				decls := []any{}
				for _, t := range req.Tools {
					// parametersJsonSchema 收完整 JSON Schema(含 const、additionalProperties);parameters 不收
					decls = append(decls, map[string]any{"name": t.Name, "description": t.Description, "parametersJsonSchema": t.Parameters})
				}
				body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
			}
			payload, err := entity.Stringify(body)
			if err != nil {
				return false, err
			}
			// 注意:PathEscape 不像 encodeURIComponent,不跳脫 : @ & = + $(仍跳脫 / ? #,model 名稱跑不出這一段路徑)
			endpoint := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1beta/models/" + url.PathEscape(p.cfg.Model) + ":streamGenerateContent?alt=sse"
			newReq := func() (*http.Request, error) {
				r, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
				if err != nil {
					return nil, err
				}
				r.Header.Set("content-type", "application/json")
				for k, v := range p.cfg.Headers {
					r.Header.Set(k, v)
				}
				// key 放 header 不放 URL(URL 會進 log);最後設,cfg.Headers 蓋不掉
				r.Header.Set("x-goog-api-key", p.cfg.APIKey)
				return r, nil
			}
			res, err := repository.FetchWithRetry(ctx, p.client.Do, newReq, 2)
			if err != nil {
				return false, errors.New(errText(err))
			}
			if res.Body == nil {
				res.Body = http.NoBody
			}
			defer res.Body.Close()
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return false, fmt.Errorf("HTTP %d 轉址,不跟隨", res.StatusCode)
			}
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				// 讀到一半出錯就整段丟掉
				detail := ""
				if b, err := io.ReadAll(res.Body); err == nil {
					detail = string(b)
				}
				// 先遮再截:先截可能把秘密切成短於規則門檻的半截
				return false, errors.New("HTTP " + strconv.Itoa(res.StatusCode) + ": " + entity.SliceUTF16(repository.RedactKey(detail, p.cfg.APIKey), 500))
			}
			if res.Body == http.NoBody {
				return false, errors.New("回應沒有 body")
			}
			for ev, err := range repository.ParseSSE(res.Body) {
				if err != nil {
					return false, errors.New(errText(err))
				}
				var j *chunk
				if err := json.Unmarshal([]byte(ev.Data), &j); err != nil {
					return false, err
				}
				if j == nil { // 讀 null 的屬性當錯誤
					return false, errors.New("串流事件是 null")
				}
				if u := j.UsageMetadata; u != nil {
					usage = &entity.Usage{Input: int(u.PromptTokenCount), Output: int(u.CandidatesTokenCount)}
				}
				if len(j.Candidates) == 0 {
					continue
				}
				c := j.Candidates[0]
				if c.Content != nil {
					for _, part := range c.Content.Parts {
						if fc := part.FunctionCall; fc != nil {
							flushText()
							args := fc.Args
							if args == nil {
								args = map[string]any{}
							}
							sig, _ := part.ThoughtSignature.(string)
							content = append(content, entity.Part{Type: entity.PartToolCall, ID: fmt.Sprintf("%s-%d", fc.Name, calls), Name: fc.Name, Args: args, Signature: sig})
							calls++
						} else if s, ok := part.Text.(string); ok && s != "" {
							if part.Thought == true {
								thinking.WriteString(s)
								if !yield(usecase.LLMEvent{Type: usecase.LLMEventThinkingDelta, Text: s}) {
									return true, nil
								}
							} else {
								text.WriteString(s)
								if !yield(usecase.LLMEvent{Type: usecase.LLMEventTextDelta, Text: s}) {
									return true, nil
								}
							}
						}
					}
				}
				if c.FinishReason == "MAX_TOKENS" {
					stopReason = "length"
				}
			}
			flushText()
			if calls > 0 && stopReason == "stop" {
				stopReason = "toolUse"
			}
			return false, nil
		}()
		if stopped {
			return
		}
		if err != nil {
			flushText()
			if ctx.Err() != nil {
				stopReason = "aborted"
			} else {
				stopReason, errorMessage = "error", repository.RedactKey(err.Error(), p.cfg.APIKey)
			}
		}
		yield(usecase.LLMEvent{Type: usecase.LLMEventDone, Message: &entity.Message{
			Role: entity.RoleAssistant, Content: content, Privacy: p.privacy, Ts: time.Now().UnixMilli(), Provider: p.cfg.Name,
			StopReason: stopReason, ErrorMessage: errorMessage, Usage: usage,
		}})
	}
}
