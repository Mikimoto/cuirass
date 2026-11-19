package entity

import "context"

// Effects 是工具的副作用等級;fs / exec 在專案 1 註冊時直接拒絕。
type Effects string

const (
	EffectsNone    Effects = "none"
	EffectsNetwork Effects = "network"
	EffectsFS      Effects = "fs"
	EffectsExec    Effects = "exec"
)

type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    string
}

type HTTPOptions struct {
	Method  string
	Headers map[string]string
	Body    string
}

// HTTPClient 由 Agent 依 policy.Hosts 建立;取消走 ctx。
type HTTPClient func(ctx context.Context, url string, opts HTTPOptions) (HTTPResponse, error)

// ToolCtx 是工具執行時拿到的能力;取消走 Execute 的 ctx。
type ToolCtx struct{ HTTP HTTPClient }

// ToolOutput 是工具的回傳:{ content, isError, details }。Content 會先 JSON 字串化再 redact、截斷,字串也照 JSON 規則處理。
type ToolOutput struct {
	Content any
	Details any
	IsError bool
}

type ToolDef struct {
	Name, Description string
	// Parameters 註冊時強制 additionalProperties:false,不是就丟錯。
	Parameters Schema
	Effects    Effects
	Execute    func(ctx context.Context, args map[string]any, tc ToolCtx) (ToolOutput, error)
	// AuditArgs 是可寫進稽核的參數名;未列的不寫。
	AuditArgs []string
}

type RuleCtx struct {
	Privacy   Privacy
	TurnCount func(tool string) int
}

// ToolRule 回 block=true 就擋(reason 空字串給預設原因);只有 block=false 才放行。
type ToolRule func(name string, args map[string]any, ctx RuleCtx) (reason string, block bool)

// Limits 是各項額度;PerTool 是 perToolPerTurn。
type Limits struct {
	PerTurn              int
	PerSession           int
	PerTool              map[string]int
	TurnTimeoutMs        int
	MaxOutputTokens      int
	ToolResultBytes      int
	MaxModelCallsPerTurn int
}

// DefaultLimits 回預設額度的複本。回傳值的 PerTool 是 nil(等同 {}):直接寫入會 panic,要先 make。
func DefaultLimits() Limits { return defaultLimits }

var defaultLimits = Limits{
	PerTurn:              20,
	PerSession:           200,
	TurnTimeoutMs:        300_000,
	MaxOutputTokens:      4096,
	ToolResultBytes:      200_000,
	MaxModelCallsPerTurn: 50,
}

type Policy struct {
	Tools  []string
	Hosts  []string
	Limits Limits
	Rules  []ToolRule
	// AuditFailure 為 "" / "halt" / "warn";"" 時 effects none/network → warn,fs/exec → halt。
	AuditFailure string
	// PublicReason 給模型看的原因;nil 時回原因。
	PublicReason func(tool, reason string, cap bool) string
}
