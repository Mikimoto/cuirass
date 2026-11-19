package usecase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// 組裝一個 agent(spec §6.4)。一個 agent 一份遮蔽集合、數字帳本、額度、事件;同時只跑一輪。

// LimitsOptions 是 entity.Limits 的逐欄選填版:nil = 用 entity.DefaultLimits。
// 0 是有效值(例如 PerTurn 0 = 不准呼叫工具),所以用指標。PerTool 的值必須是非負整數。
type LimitsOptions struct {
	PerTurn              *int
	PerSession           *int
	PerTool              map[string]int
	TurnTimeoutMs        *int
	MaxOutputTokens      *int
	ToolResultBytes      *int
	MaxModelCallsPerTurn *int
}

// PolicyOptions 是 entity.Policy 的全選填版。
// Tools 為 nil = 全部已註冊工具(依 AgentOptions.Tools 的順序);非 nil 只能是子集(空切片 = 不開任何工具)。
// AuditFailure 只收 ""、"halt"、"warn";打錯字不會悄悄降成 warn,建立時就回錯。
type PolicyOptions struct {
	Tools        []string
	Hosts        []string
	Limits       LimitsOptions
	Rules        []entity.ToolRule
	AuditFailure string
	PublicReason func(tool, reason string, cap bool) string
}

// AgentOptions 是建立 agent 的選項。
//
// 併發:同一個 Agent 同時只跑一輪,但下面這些 hook 可能被不同 goroutine 同時呼叫,必須能併發使用:
// Audit(AuditSink.Write)、Masking.Snapshot、Rewrite(ContentRewrite)、各 provider 的 Stream。
// Masking.HideResult 也會交給複核(Ask)的送出管線;目前複核請求不含工具結果,實際不會在那裡被呼叫,保守起見也當成可能併發。
// (repository.NewFileAudit 已上鎖。)
//
// 時序:中止(Abort、Prompt 的 ctx 取消、Dispose)或逾時後,Prompt 馬上返回,不等不理會 ctx 的工作。
// 這時還在跑的工具 Execute、驗證器(Verifiers)與它的 Ask 成了被放棄的 goroutine:它們自己回來之前仍會繼續跑,
// 所以在 Prompt 返回之後(甚至下一輪進行中)仍可能呼叫上面那些 hook。它們的結果一律丟掉,寫不進對話紀錄、
// 遮蔽集合、數字帳本與額度。工具與驗證器應該尊重 ctx 盡快返回。
//
// 其他 hook(System、LeakSources、Masking.Collect、Policy.Rules、Policy.PublicReason、listener、TurnTimeout)
// 只在這輪自己的 goroutine 上同步呼叫;它們卡住,這輪就卡住。
// 這些 hook 不可以同步呼叫 Prompt:新的那輪排在自己這輪後面,兩邊互等。
// 要接著問就用 go a.Prompt(ctx, ...);萬一同步呼叫了,只有那個 Prompt 的 ctx 被取消或 Dispose 才放得開。
type AgentOptions struct {
	Providers       []LLMProvider
	InitialProvider string // "" = Providers[0]
	System          func() string
	Tools           []entity.ToolDef
	Policy          PolicyOptions
	Audit           entity.AuditSink // nil = entity.NullAudit()
	Masking         *Masking
	Rewrite         ContentRewrite
	Verifiers       []Verifier
	// LeakSources 是外洩檢查的比對來源;nil = 系統提示詞。
	LeakSources func() []string
	// LeakAllow 每一句都要過 ValidateLeakAllow。
	LeakAllow []string
	// TurnTimeout 依 provider 決定這輪的逾時;nil 或回 ≤ 0 = Policy.Limits.TurnTimeoutMs。
	TurnTimeout func(ProviderInfo) time.Duration
	// Restore 還原額度;壞值(含零值 BudgetState)一律當成整個 session 用完(entity.NewBudget)。
	Restore *entity.BudgetState
}

// ProviderInfo 是目前 provider 的公開資訊;不交出 Stream。
type ProviderInfo struct {
	Name          string
	Model         string
	Privacy       entity.Privacy
	ContextWindow int
	MaxTokens     int
}

func infoOf(p LLMProvider) ProviderInfo {
	return ProviderInfo{Name: p.Name(), Model: p.Model(), Privacy: p.Privacy(), ContextWindow: p.ContextWindow(), MaxTokens: p.MaxTokens()}
}

// 名稱會插進 <untrusted-data tool="..."> 標記,只收安全字元
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

var errDisposed = errors.New("agent 已停用")

func checkTools(defs []entity.ToolDef) (map[string]entity.ToolDef, error) {
	tools := map[string]entity.ToolDef{}
	for _, t := range defs {
		if !toolName.MatchString(t.Name) {
			j, _ := entity.Stringify(t.Name)
			return nil, fmt.Errorf("工具名不合法:%s", j)
		}
		if _, dup := tools[t.Name]; dup {
			return nil, fmt.Errorf("工具名重複:%s", t.Name)
		}
		if t.Effects == entity.EffectsFS || t.Effects == entity.EffectsExec {
			return nil, errors.New("專案 1 不支援 fs/exec 工具")
		}
		if t.Effects != entity.EffectsNone && t.Effects != entity.EffectsNetwork {
			return nil, fmt.Errorf("工具 %s 的 effects 無法辨識", t.Name)
		}
		if _, err := entity.CompileSchema(t.Parameters); err != nil {
			return nil, fmt.Errorf("工具 %s 的 %w", t.Name, err)
		}
		// 深複製:之後改呼叫端的 map / 切片,改不到驗證與稽核(執行時每次照這份重新編譯 schema)
		params, err := entity.CloneJSON(t.Parameters)
		if err != nil {
			return nil, fmt.Errorf("工具 %s 的 schema 無法複製:%w", t.Name, err)
		}
		t.Parameters, t.AuditArgs = params, slices.Clone(t.AuditArgs)
		tools[t.Name] = t
	}
	return tools, nil
}

// maxLimit 是 2^53-1(Number.isSafeInteger 的上限)。
const maxLimit = 1<<53 - 1

// limit:nil 用預設;負數或超過 2^53-1 拒絕建立(比較才不會永遠是 false 而 fail open)。
func limit(name string, v *int, d int) (int, error) {
	if v == nil {
		return d, nil
	}
	if *v < 0 || *v > maxLimit {
		return 0, fmt.Errorf("policy.limits.%s 必須是非負整數:%d", name, *v)
	}
	return *v, nil
}

func checkLimits(l LimitsOptions) (entity.Limits, error) {
	d := entity.DefaultLimits()
	out := entity.Limits{PerTool: map[string]int{}}
	for _, k := range slices.Sorted(maps.Keys(l.PerTool)) {
		v := l.PerTool[k]
		n, err := limit("perToolPerTurn."+k, &v, 0)
		if err != nil {
			return out, err
		}
		out.PerTool[k] = n
	}
	var err error
	for _, f := range []struct {
		name string
		v    *int
		d    int
		dst  *int
	}{
		{"perTurn", l.PerTurn, d.PerTurn, &out.PerTurn},
		{"perSession", l.PerSession, d.PerSession, &out.PerSession},
		{"turnTimeoutMs", l.TurnTimeoutMs, d.TurnTimeoutMs, &out.TurnTimeoutMs},
		{"maxOutputTokens", l.MaxOutputTokens, d.MaxOutputTokens, &out.MaxOutputTokens},
		{"toolResultBytes", l.ToolResultBytes, d.ToolResultBytes, &out.ToolResultBytes},
		{"maxModelCallsPerTurn", l.MaxModelCallsPerTurn, d.MaxModelCallsPerTurn, &out.MaxModelCallsPerTurn},
	} {
		if *f.dst, err = limit(f.name, f.v, f.d); err != nil {
			return out, err
		}
	}
	return out, nil
}

// msDuration:毫秒轉 Duration,超過 int64 奈秒的上限就飽和(2^53-1 ms 會溢位)。
func msDuration(ms int) time.Duration {
	if int64(ms) > math.MaxInt64/int64(time.Millisecond) {
		return math.MaxInt64
	}
	return time.Duration(ms) * time.Millisecond
}

// qitem 是佇列的一格:wake 非 nil = 一輪(輪到時關閉);否則是切換 provider。
type qitem struct {
	wake chan struct{}
	to   LLMProvider
}

// Agent 是一個 agent。所有方法都可以併發呼叫。只能用 NewAgent(cuirass.New)建立:零值不能用(例如 On 會 panic)。
type Agent struct {
	providers   []LLMProvider
	deps        TurnDeps // 每輪複製一份,填上 Provider 與 TurnTimeout
	turnTimeout func(ProviderInfo) time.Duration

	mu       sync.Mutex
	current  LLMProvider
	history  []entity.Message
	queue    []*qitem // 排隊中 + 執行中;頭一格永遠是正在跑(或剛被叫醒)的輪,切換一到頭就套用
	cancel   context.CancelFunc
	disposed bool
	done     chan struct{} // Dispose 時關閉:放掉排隊中的 Prompt
}

// NewAgent 檢查設定並組裝 agent;錯誤訊息是固定字串。
//
// newClient 依白名單建工具用的 HTTP client(由根套件傳 repository.NewHTTPClient;usecase 不呼叫實作)。
// 擋下的連線要寫進 audit(egress_block)。nil 回錯,絕不退化成沒有白名單。
func NewAgent(o AgentOptions, newClient func(hosts []string, audit entity.AuditSink) entity.HTTPClient) (*Agent, error) {
	if newClient == nil {
		return nil, errors.New("缺少 HTTP client 工廠")
	}
	tools, err := checkTools(o.Tools)
	if err != nil {
		return nil, err
	}
	p := o.Policy
	limits, err := checkLimits(p.Limits)
	if err != nil {
		return nil, err
	}
	active := slices.Clone(p.Tools)
	if p.Tools == nil {
		for _, t := range o.Tools {
			active = append(active, t.Name)
		}
	}
	for _, n := range active {
		if _, ok := tools[n]; !ok {
			return nil, fmt.Errorf("policy.tools 含未註冊的工具:%s", n)
		}
	}
	policy := entity.Policy{
		Tools: active, Hosts: slices.Clone(p.Hosts), Rules: slices.Clone(p.Rules), Limits: limits,
		AuditFailure: p.AuditFailure, PublicReason: p.PublicReason,
	}

	if len(o.Providers) == 0 {
		return nil, errors.New("至少要有一個 provider")
	}
	names := map[string]bool{}
	for i, pr := range o.Providers {
		if pr == nil {
			return nil, fmt.Errorf("providers[%d] 是 nil", i)
		}
		if names[pr.Name()] {
			return nil, fmt.Errorf("provider 名稱重複:%s", pr.Name())
		}
		names[pr.Name()] = true
	}
	a := &Agent{providers: slices.Clone(o.Providers), turnTimeout: o.TurnTimeout, done: make(chan struct{})}
	a.current = o.Providers[0]
	if o.InitialProvider != "" {
		if a.current = a.find(o.InitialProvider); a.current == nil {
			return nil, fmt.Errorf("找不到 provider:%s", o.InitialProvider)
		}
	}
	// 其他檢查
	if o.System == nil {
		return nil, errors.New("system 必填")
	}
	if err := ValidateLeakAllow(o.LeakAllow); err != nil {
		return nil, err
	}
	switch p.AuditFailure {
	case "", "halt", "warn":
	default:
		return nil, fmt.Errorf(`policy.auditFailure 只能是 "halt" 或 "warn"(或不填):%q`, p.AuditFailure)
	}

	audit := o.Audit
	if audit == nil {
		audit = entity.NullAudit()
	}
	leakSources := o.LeakSources
	if leakSources == nil {
		system := o.System
		leakSources = func() []string { return []string{system()} }
	}
	var masking Masking
	if o.Masking != nil {
		masking = *o.Masking
	}
	a.deps = TurnDeps{
		AgentID: "main", System: o.System, LeakSources: leakSources, LeakAllow: slices.Clone(o.LeakAllow),
		Tools: tools, Policy: policy, Budget: entity.NewBudget(limits, o.Restore), Audit: audit, Emit: &Emitter{},
		HTTP:    newClient(policy.Hosts, audit),
		MaskSet: entity.NewMaskSet(), Masking: masking, Rewrite: o.Rewrite, Verifiers: slices.Clone(o.Verifiers),
		NumberBook: entity.NewNumberBook(entity.DefaultNumberBookMax),
	}
	return a, nil
}

func (a *Agent) find(name string) LLMProvider {
	for _, p := range a.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Prompt 跑一輪;同時只跑一輪,其餘依呼叫順序排隊。阻塞到這輪結束。
// 排隊中(還沒輪到)ctx 被取消 → 不跑,回 ctx.Err();排隊中 Dispose → 回「agent 已停用」(裁決 G21,不會跑那一輪)。
// 已經輪到才取消,這輪照跑、以 aborted 結束。
// 只有設定或 hook 出錯(含 TurnTimeout panic)、排隊中被取消才回 error;這輪怎麼結束看 turn_end 事件。
//
// listener 等 hook 在這輪自己的 goroutine 上同步呼叫:在裡面同步呼叫 Prompt 會排在自己這輪後面而卡住
// (直到那個 Prompt 的 ctx 被取消或 Dispose)。要接著問就用 go a.Prompt(...)。
func (a *Agent) Prompt(ctx context.Context, text string) error {
	a.mu.Lock()
	if a.disposed {
		a.mu.Unlock()
		return errDisposed
	}
	it := &qitem{wake: make(chan struct{})}
	a.queue = append(a.queue, it)
	head := a.queue[0] == it
	a.mu.Unlock()
	if !head {
		var err error
		select {
		case <-it.wake:
			err = ctx.Err() // 輪到時已取消:同樣不跑
		case <-ctx.Done():
			err = ctx.Err()
		case <-a.done:
			err = errDisposed
		}
		if err != nil {
			a.leave(it)
			return err
		}
	}
	defer a.next()
	return a.runOne(ctx, text)
}

// leave:排隊中放棄。已經被叫醒(在頭一格)就照 next 出列並叫醒下一輪;否則從中間拿掉,前後的切換順序不變。
func (a *Agent) leave(it *qitem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.Index(a.queue, it)
	if i == 0 {
		a.nextLocked()
		return
	}
	a.queue = slices.Delete(a.queue, i, i+1)
}

// next:自己(頭一格)出列,套用緊接著的切換,叫醒下一輪。
func (a *Agent) next() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextLocked()
}

func (a *Agent) nextLocked() {
	a.queue[0] = nil
	a.queue = a.queue[1:]
	for len(a.queue) > 0 && a.queue[0].wake == nil {
		a.current = a.queue[0].to
		a.queue[0] = nil
		a.queue = a.queue[1:]
	}
	if len(a.queue) > 0 {
		close(a.queue[0].wake)
	}
}

func (a *Agent) runOne(ctx context.Context, text string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	a.mu.Lock()
	if a.disposed {
		a.mu.Unlock()
		return errDisposed
	}
	provider := a.current
	history := slices.Clip(a.history)
	tctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.cancel = nil
		a.mu.Unlock()
	}()

	deps := a.deps
	deps.Provider = func() LLMProvider { return provider }
	deps.TurnTimeout = msDuration(deps.Policy.Limits.TurnTimeoutMs)
	if a.turnTimeout != nil {
		if d := a.turnTimeout(infoOf(provider)); d > 0 {
			deps.TurnTimeout = d
		}
	}
	user := entity.Message{
		Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: text}},
		Privacy: provider.Privacy(), Ts: time.Now().UnixMilli(),
	}
	res, err := RunTurn(tctx, history, user, deps)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.history = append(a.history, res.Messages...)
	a.mu.Unlock()
	return nil
}

// Abort 只中止目前這輪;排隊中的照跑,之後可續用。沒有輪在跑時什麼都不做。
func (a *Agent) Abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// SetProvider 在輪與輪之間生效:閒置時立刻換,否則排進佇列(排在它之前的輪仍用舊的)。找不到回 false。
func (a *Agent) SetProvider(name string) bool {
	next := a.find(name)
	if next == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		a.current = next
	} else {
		a.queue = append(a.queue, &qitem{to: next})
	}
	return true
}

// Provider 回目前 provider 的資訊(輪中切換時仍是這輪用的那個)。
func (a *Agent) Provider() ProviderInfo {
	a.mu.Lock()
	p := a.current
	a.mu.Unlock()
	return infoOf(p)
}

// On 訂閱事件;回傳取消訂閱的函式。listener 收到深複本,panic 被吞掉。
func (a *Agent) On(l Listener) (off func()) { return a.deps.Emit.On(l) }

// Messages 回對話紀錄的深複本。
func (a *Agent) Messages() []entity.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out, err := entity.CloneJSON(a.history)
	if err != nil { // 紀錄裡的值都已經 JSON 來回過,實際不會發生
		return nil
	}
	return out
}

// ActiveTools 回開放給模型的工具名稱(複本)。
func (a *Agent) ActiveTools() []string { return slices.Clone(a.deps.Policy.Tools) }

// Budget 回額度的快照(可存檔,之後用 AgentOptions.Restore 還原)。
func (a *Agent) Budget() entity.BudgetState { return a.deps.Budget.Snapshot() }

// Dispose 停用 agent:中止目前這輪,排隊中與之後的 Prompt 都回「agent 已停用」。可重複呼叫。
func (a *Agent) Dispose() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.disposed {
		close(a.done)
	}
	a.disposed = true
	if a.cancel != nil {
		a.cancel()
	}
}
