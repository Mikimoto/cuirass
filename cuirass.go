// Package cuirass 是帶 OWASP-LLM 防護層的 agent framework。
//
// 一般使用只要這個套件:New 建 agent、CreateProvider 建 LLM provider,其餘是常用型別的別名與純函式。
// entity、usecase、repository 在 internal/ 底下,外部無法 import;RunTurn、ExecuteToolCall 這類內部步驟不匯出。
package cuirass

import (
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/repository/providers"
	"github.com/Mikimoto/cuirass/internal/repository/providers/stub"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// Agent 與設定
type (
	Agent          = usecase.Agent
	PolicyOptions  = usecase.PolicyOptions
	LimitsOptions  = usecase.LimitsOptions
	ProviderInfo   = usecase.ProviderInfo
	Event          = usecase.Event
	Listener       = usecase.Listener
	Masking        = usecase.Masking
	ContentRewrite = usecase.ContentRewrite
	Verifier       = usecase.Verifier
	VerifyCtx      = usecase.VerifyCtx
)

// 工具、政策、訊息
type (
	ToolDef       = entity.ToolDef
	ToolCtx       = entity.ToolCtx
	ToolOutput    = entity.ToolOutput
	ToolRule      = entity.ToolRule
	RuleCtx       = entity.RuleCtx
	Effects       = entity.Effects
	Schema        = entity.Schema
	Policy        = entity.Policy
	Limits        = entity.Limits
	HTTPClient    = entity.HTTPClient
	HTTPOptions   = entity.HTTPOptions
	HTTPResponse  = entity.HTTPResponse
	Message       = entity.Message
	Part          = entity.Part
	Usage         = entity.Usage
	Privacy       = entity.Privacy
	AuditSink     = entity.AuditSink
	AuditEvent    = entity.AuditEvent
	BudgetState   = entity.BudgetState
	TurnState     = entity.TurnState
	RedactPattern = entity.RedactPattern
	NumToken      = entity.NumToken
	NumberBook    = entity.NumberBook
)

// Provider 設定
type (
	ProviderConfig = repository.ProviderConfig
	Compat         = repository.Compat
	StubStep       = stub.Step
	StubCall       = stub.Call
)

const (
	EffectsNone    = entity.EffectsNone
	EffectsNetwork = entity.EffectsNetwork
	Local          = entity.Local
	Remote         = entity.Remote
)

// Event.Type 的值。
const (
	EventTurnStart     = usecase.EventTurnStart
	EventTurnEnd       = usecase.EventTurnEnd
	EventTextDelta     = usecase.EventTextDelta
	EventThinkingDelta = usecase.EventThinkingDelta
	EventToolStart     = usecase.EventToolStart
	EventToolBlocked   = usecase.EventToolBlocked
	EventToolEnd       = usecase.EventToolEnd
	EventMessage       = usecase.EventMessage
	EventNotice        = usecase.EventNotice
)

// Provider 是 LLM provider。只能由 CreateProvider / NewStubProvider 取得;零值不可用。外部無法實作或包裝(只准用內建 provider)。
type Provider struct{ p usecase.LLMProvider }

func (p *Provider) Name() string       { return p.p.Name() }
func (p *Provider) Model() string      { return p.p.Model() }
func (p *Provider) Privacy() Privacy   { return p.p.Privacy() }
func (p *Provider) ContextWindow() int { return p.p.ContextWindow() }
func (p *Provider) MaxTokens() int     { return p.p.MaxTokens() }

// AgentOptions 欄位意義與併發、時序約定同 internal 的 usecase.AgentOptions。
type AgentOptions struct {
	Providers       []*Provider
	InitialProvider string // "" = Providers[0]
	System          func() string
	Tools           []ToolDef
	Policy          PolicyOptions
	Audit           AuditSink // nil = NullAudit()
	Masking         *Masking
	Rewrite         ContentRewrite
	Verifiers       []Verifier
	// LeakSources 是外洩檢查的比對來源;nil = 系統提示詞。
	LeakSources func() []string
	// LeakAllow 是外洩檢查的允許句。
	LeakAllow []string
	// TurnTimeout 依 provider 決定這輪的逾時;nil 或回 ≤ 0 = Policy.Limits.TurnTimeoutMs。
	TurnTimeout func(ProviderInfo) time.Duration
	// Restore 還原額度;壞值(含零值 BudgetState)一律當成整個 session 用完。
	Restore *BudgetState
}

// toInternal 逐欄轉成 internal 選項。nil 與零值 *Provider 轉成 nil 介面,由 NewAgent 照原本的訊息回錯。
func (o AgentOptions) toInternal() usecase.AgentOptions {
	var ps []usecase.LLMProvider
	if o.Providers != nil {
		ps = make([]usecase.LLMProvider, len(o.Providers))
		for i, p := range o.Providers {
			if p != nil && p.p != nil {
				ps[i] = p.p
			}
		}
	}
	return usecase.AgentOptions{
		Providers: ps, InitialProvider: o.InitialProvider, System: o.System, Tools: o.Tools, Policy: o.Policy,
		Audit: o.Audit, Masking: o.Masking, Rewrite: o.Rewrite, Verifiers: o.Verifiers, LeakSources: o.LeakSources,
		LeakAllow: o.LeakAllow, TurnTimeout: o.TurnTimeout, Restore: o.Restore,
	}
}

// New 建立 agent;設定不合法回 error。
func New(o AgentOptions) (*Agent, error) { return usecase.NewAgent(o.toInternal(), newHTTPClient) }

// newHTTPClient 是交給 usecase 的工具用 HTTP client 工廠:白名單 hosts,擋下的連線寫進使用者的 audit。
func newHTTPClient(hosts []string, audit AuditSink) HTTPClient {
	return repository.NewHTTPClient(repository.HTTPClientOptions{Hosts: hosts, Audit: audit})
}

// CreateDeps 是 CreateProvider 的依賴。沒有 HTTP 欄位:provider 的連線一律由框架自己建(不讀 proxy 環境變數;local 只准撥 loopback)。
type CreateDeps struct {
	Audit AuditSink // nil = NullAudit();privacy_override 寫在這裡
}

// CreateProvider 依 cfg.API 建 provider(anthropic-messages、openai-completions、google-generative-ai)。
// 明填 Privacy: local 但 BaseURL 不是 loopback 時回錯(先寫一筆 privacy_override 稽核)。cfg.Headers 會複製,建立後再改不影響送出。
func CreateProvider(cfg ProviderConfig, deps CreateDeps) (*Provider, error) {
	p, err := providers.CreateProvider(cfg, providers.CreateDeps{Audit: deps.Audit})
	if err != nil {
		return nil, err
	}
	return &Provider{p}, nil
}

// StubOptions 是 stub provider 的設定;零值欄位用預設。
type StubOptions struct {
	Name      string      // "" = "stub"
	Privacy   Privacy     // "" = local
	Script    *[]StubStep // 刻意共用呼叫端的切片:共用同一個 *[]StubStep 的多個 stub 共用一個游標
	FinalText string      // "" = "stub: stopping after blocked/failed tool call"
}

// NewStubProvider 建確定性的 stub provider(測試、範例用;不碰網路)。
func NewStubProvider(o StubOptions) *Provider {
	return &Provider{stub.New(stub.Options{Name: o.Name, Privacy: o.Privacy, Script: o.Script, FinalText: o.FinalText})}
}

// NewFileAudit 建檔案稽核(JSON Lines,目錄 0700、檔案 0600);warn 收寫入失敗的訊息;nil = 寫到標準 log(stderr)。
func NewFileAudit(dir string, warn func(string)) AuditSink { return repository.NewFileAudit(dir, warn) }

// NumberVerifier 核對回答裡的數字;timeout ≤ 0 用預設。
func NumberVerifier(timeout time.Duration) Verifier { return usecase.NumberVerifier(timeout) }

// 預設值:每次回新的複本,改了不影響框架。

// DefaultRedactPatterns 回內建遮蔽規則的複本。
func DefaultRedactPatterns() []RedactPattern { return entity.DefaultRedactPatterns() }

// DefaultLimits 回預設額度的複本。PerTool 是 nil,直接寫入會 panic,要先 make。
func DefaultLimits() Limits { return entity.DefaultLimits() }

// NullAudit 什麼都不記(AgentOptions.Audit 為 nil 時的預設)。
func NullAudit() AuditSink { return entity.NullAudit() }

// 純函式(包一層,不是可改寫的變數)

// Redact 用內建規則遮掉文字裡的機密(API key、token 等)。
func Redact(text string) string { return entity.Redact(text) }

// ResolvePrivacy 判斷 provider 的隱私等級:明填優先(無法辨識的值當 remote);沒明填時有自訂 header 或非 loopback 一律 remote。
// 第二個回傳值 override = 明填 local 但 baseURL 不是 loopback。
func ResolvePrivacy(baseURL string, privacy *Privacy, headers map[string]string) (Privacy, bool) {
	return entity.ResolvePrivacy(baseURL, privacy, headers)
}

// FilterAuditArgs 只留 keys 內的原始型別值給稽核用;字串先遮蔽再壓成單行並截短,物件與陣列丟掉。
func FilterAuditArgs(args map[string]any, keys []string) map[string]any {
	return entity.FilterAuditArgs(args, keys)
}

// TextOf 只串訊息裡的 text part(不含 thinking、toolCall)。
func TextOf(m Message) string { return entity.TextOf(m) }

// CollectNumbers 遞迴收工具資料裡的數字(含字串裡的數字)。
func CollectNumbers(v any) []float64 { return entity.CollectNumbers(v) }

// NewNumberBook 建數字帳本;max 是已知數字的上限,≤ 0 什麼都不留。
func NewNumberBook(max int) *NumberBook { return entity.NewNumberBook(max) }

// ExtractNumbers 抓回答裡的數字主張;個位整數(無小數、無單位)不算。
func ExtractNumbers(text string) []NumToken { return entity.ExtractNumbers(text) }

// FindUnverified 回 text 裡對不到 known 的數字。
func FindUnverified(text string, known []float64) []NumToken {
	return entity.FindUnverified(text, known)
}

// MarkUnverified 在對應行尾加 ⚠未核對,結尾補一行總數。只給畫面用。
func MarkUnverified(text string, unverified []NumToken) string {
	return entity.MarkUnverified(text, unverified)
}

// LeaksPrompt 在 text 含 prompt(去掉 allow 句)夠長的連續片段時回 true。
func LeaksPrompt(text, prompt string, allow []string) bool {
	return entity.LeaksPrompt(text, prompt, allow)
}

// StripUnsafeLinks 只放行 https:// 連結,其他協定換成 [連結已移除]。
func StripUnsafeLinks(text string) string { return entity.StripUnsafeLinks(text) }

// NormalizeLeak 是外洩比對前的正規化(NFKC、小寫、去標點與空白等)。
func NormalizeLeak(s string) string { return entity.NormalizeLeak(s) }
