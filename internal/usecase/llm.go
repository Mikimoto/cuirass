package usecase

import (
	"context"
	"iter"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// 以下是 LLM provider 的 port:usecase 定義,repository/providers 實作(相依方向 entity ← usecase ← repository)。

// ToolSpec:Parameters 是 JSON Schema(任何可 JSON 序列化的值)。
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// LLMRequest 是送進 provider 的請求;取消用 Stream 的 context.Context。
type LLMRequest struct {
	System    string
	Messages  []entity.Message
	Tools     []ToolSpec
	MaxTokens int
}

// LLMEvent 的類型。錯誤也用 done:StopReason "error" + ErrorMessage。
const (
	LLMEventTextDelta     = "text_delta"
	LLMEventThinkingDelta = "thinking_delta"
	LLMEventNotice        = "notice"
	LLMEventDone          = "done"
)

// LLMEvent 是串流事件;Type 決定 Text 或 Message 有意義。
type LLMEvent struct {
	Type    string
	Text    string
	Message *entity.Message
}

// LLMProvider:Stream 回 iter.Seq(Go 1.23 range-over-func)。
//
// 選 iterator 不選 channel:是「拉」的語意,消費端 break 時 yield 回 false,
// provider 在同一個 goroutine 裡 return、defer 關掉 HTTP body;不必開 goroutine,也就沒有「消費端提早離開
// 導致生產端卡在送 channel」的洩漏,也不用約定誰 close。取消用 ctx。
type LLMProvider interface {
	Name() string
	Model() string
	Privacy() entity.Privacy
	ContextWindow() int
	MaxTokens() int
	Stream(ctx context.Context, req LLMRequest) iter.Seq[LLMEvent]
}
