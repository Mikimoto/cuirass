// Package stub 是確定性 stub provider:依 script 逐次回工具呼叫或文字,不碰網路。
package stub

import (
	"context"
	"iter"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// callSeq 讓所有 stub 實例的工具呼叫 id 都不重複(只用 n 加毫秒,兩個 stub 在同一毫秒會撞號)。
var callSeq atomic.Int64

// Call 是一步工具呼叫;Args 為 nil 時送空 map。
type Call struct {
	Name string
	Args map[string]any
}

// Step 是 script 的一步:ToolCall 非 nil = 工具呼叫,否則 Text 非空 = 文字;兩者皆空 = 空步(回 FinalText)。
type Step struct {
	Text     string
	ToolCall *Call
}

// Options 是 New 的參數;零值欄位用預設。
type Options struct {
	Name      string                   // "" = "stub"
	Privacy   entity.Privacy           // "" = local
	Script    *[]Step                  // 刻意共用呼叫端的切片:共用同一個 *[]Step 的多個 stub 共用一個游標
	FinalText string                   // "" = "stub: stopping after blocked/failed tool call"
	OnRequest func(usecase.LLMRequest) // 只給 internal 測試看請求;根套件的 StubOptions 刻意沒有(封住三條路之一)
}

// Provider 實作 usecase.LLMProvider。
type Provider struct {
	o Options
	n int
}

// mu 保護所有 stub 的 script 游標與呼叫計數(script 可跨 stub 共用,所以用套件層級的鎖)。
var mu sync.Mutex

// New 建立 stub provider。
func New(o Options) *Provider {
	if o.Name == "" {
		o.Name = "stub"
	}
	if o.Privacy == "" {
		o.Privacy = entity.Local
	}
	if o.FinalText == "" {
		o.FinalText = "stub: stopping after blocked/failed tool call"
	}
	return &Provider{o: o}
}

func (p *Provider) Name() string            { return p.o.Name }
func (p *Provider) Model() string           { return p.o.Name }
func (p *Provider) Privacy() entity.Privacy { return p.o.Privacy }
func (p *Provider) ContextWindow() int      { return repository.DefaultContextWindow }
func (p *Provider) MaxTokens() int          { return repository.DefaultMaxTokens }

// next 取出下一步(shift 呼叫端的切片);用完回零值(= null 步)。
func (p *Provider) next() (int, Step) {
	mu.Lock()
	defer mu.Unlock()
	p.n++
	var s Step
	if p.o.Script != nil && len(*p.o.Script) > 0 {
		s = (*p.o.Script)[0]
		*p.o.Script = (*p.o.Script)[1:]
	}
	return p.n, s
}

// Stream:每次迭代才呼叫 OnRequest、計數、取下一步。
// OnRequest 在取步驟之前,它往 script 補的步驟由這次呼叫取用;計數對 OnRequest 不可見,所以放在 next 裡。
func (p *Provider) Stream(_ context.Context, req usecase.LLMRequest) iter.Seq[usecase.LLMEvent] {
	return func(yield func(usecase.LLMEvent) bool) {
		if p.o.OnRequest != nil {
			p.o.OnRequest(req)
		}
		n, step := p.next()
		now := time.Now().UnixMilli()
		m := &entity.Message{Role: entity.RoleAssistant, Privacy: p.o.Privacy, Ts: now, Provider: p.o.Name, Usage: &entity.Usage{}}
		if step.ToolCall != nil {
			args := step.ToolCall.Args
			if args == nil {
				args = map[string]any{}
			}
			id := "stub-call-" + strconv.Itoa(n) + "-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(callSeq.Add(1), 10)
			m.Content = []entity.Part{{Type: entity.PartToolCall, ID: id, Name: step.ToolCall.Name, Args: args}}
			m.StopReason = "toolUse"
		} else {
			text := step.Text
			if text == "" {
				text = p.o.FinalText
			}
			if !yield(usecase.LLMEvent{Type: usecase.LLMEventTextDelta, Text: text}) {
				return
			}
			m.Content = []entity.Part{{Type: entity.PartText, Text: text}}
			m.StopReason = "stop"
		}
		yield(usecase.LLMEvent{Type: usecase.LLMEventDone, Message: m})
	}
}
