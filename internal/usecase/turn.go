package usecase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// 一輪的迴圈:送出 → 輸出檢查 → 工具執行 → 驗證器。任何防護步驟回 error 或 panic 都當成擋(spec §7.1)。
//
// 併發規則:MaskSet、NumberBook 沒上鎖,只有 turn 自己的 goroutine 會碰。
//   - 串流在另一個 goroutine 讀,與 ctx 賽跑;它不碰共用狀態,事件由 turn 的 goroutine 轉發。
//   - 工具的 Execute 在 runTool 裡與 ctx 賽跑;被放棄的 Execute 回來後結果直接丟掉,後面的步驟(含 Collect)不會跑。
//   - 驗證器在另一個 goroutine 跑,與 ctx 賽跑;它與它的 Ask 只碰 MaskSet、NumberBook 的複本,
//     驗證器準時回來才由 turn 把複本裡新增的遮蔽數字併回去。
//
// 因此被放棄的 goroutine 在 RunTurn 返回後仍可能呼叫 Audit.Write、Masking.Snapshot、Rewrite、provider 的 Stream:
// 這些要能併發呼叫(檔案稽核已上鎖)。

// TurnDeps 是 RunTurn 的相依;取消由 RunTurn 的 ctx 帶。
type TurnDeps struct {
	AgentID     string
	Provider    func() LLMProvider
	System      func() string
	LeakSources func() []string
	LeakAllow   []string // 每一句都要過 ValidateLeakAllow
	Tools       map[string]entity.ToolDef
	Policy      entity.Policy
	Budget      *entity.Budget
	Audit       entity.AuditSink // nil = entity.NullAudit()
	Emit        *Emitter
	HTTP        entity.HTTPClient
	TurnTimeout time.Duration
	MaskSet     *entity.MaskSet
	Masking     Masking
	Rewrite     ContentRewrite
	Verifiers   []Verifier
	NumberBook  *entity.NumberBook
}

// TurnResult:Messages 是本輪新增的訊息(含使用者訊息);StopReason 是 turn_end 的 reason。
type TurnResult struct {
	Messages   []entity.Message
	StopReason string
}

const (
	blockedText = "(已攔下:回答含系統指令內容)"
	abortedText = "已中止"
	// unverifiedLine 驗證器失敗時加在回答結尾(R15):不放行沒標記的未核對內容(spec §11.3、§7.1)。
	unverifiedLine = "⚠ 這則回答未能完成核對"
	noDoneText     = "provider 沒有回傳完成訊息"
)

var (
	errTurnTimeout = errors.New("timeout")
	errStopped     = errors.New("本輪已中止")
	errAskFailed   = errors.New("複核請求失敗")
	errNoBudget    = errors.New("複核額度已用完")
)

// ValidateLeakAllow:允許句(回答可以照抄的提示詞句子)正規化後至少 entity.LeakMin 個字。
// 外洩比對是在提示詞上挖掉允許句再找長 LeakMin 的片段:空字串會把提示詞拆成單字、常見短詞會把它切碎,外洩偵測等於關掉。
// 短於 LeakMin 的句子本身也不會被判外洩,不需要允許。Agent 建立時也要呼叫。
func ValidateLeakAllow(allow []string) error {
	for i, a := range allow {
		if n := entity.UTF16Len(entity.NormalizeLeak(a)); n < entity.LeakMin {
			return fmt.Errorf("leakAllow[%d] 太短:正規化後只有 %d 字,至少要 %d 字(空字串或短句會讓外洩偵測失效)", i, n, entity.LeakMin)
		}
	}
	return nil
}

type turn struct {
	deps    TurnDeps
	ctx     context.Context
	history []entity.Message
	user    entity.Message
	added   []entity.Message
	last    LLMProvider
	// mu 保護 closed:Ask 在鎖裡「檢查還沒結束 → 扣額度」,RunTurn 返回前在鎖裡關門,
	// 所以 RunTurn 返回之後,被放棄的 Ask 不會再扣額度(沒有檢查與 Spend 之間的空窗)。
	mu     sync.Mutex
	closed bool
}

// spend:這輪還沒結束(含 Ask 自己的 ctx)才扣額度。stopped = 已結束,沒扣。
func (t *turn) spend(ctxs ...context.Context) (ok, stopped bool) {
	t.mu.Lock()
	defer t.mu.Unlock() // Spend panic 也要放鎖,否則 RunTurn 關門時卡死
	if t.closed {
		return false, true
	}
	for _, c := range ctxs {
		if c.Err() != nil {
			return false, true
		}
	}
	return t.deps.Budget.Spend(), false
}

// RunTurn 跑一輪。只有設定檢查(ValidateLeakAllow)失敗會回 error,此時不送任何事件;其餘結果都在 TurnResult。
// ctx 取消或逾時後馬上返回,不等不理會 ctx 的工具、驗證器或 provider。
func RunTurn(ctx context.Context, history []entity.Message, user entity.Message, deps TurnDeps) (TurnResult, error) {
	if err := ValidateLeakAllow(deps.LeakAllow); err != nil {
		return TurnResult{}, err
	}
	if deps.Audit == nil {
		deps.Audit = entity.NullAudit()
	}
	t := &turn{deps: deps, history: history, user: user, added: []entity.Message{user}}
	deps.Emit.Emit(Event{Type: "turn_start", Agent: deps.AgentID})
	tctx, cancel := context.WithTimeoutCause(ctx, deps.TurnTimeout, errTurnTimeout)
	t.ctx = tctx

	reason := t.run()
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	// 中止還在跑的請求(如逾時被放棄的複核)
	cancel()
	if reason == "timeout" {
		// 不在計時器觸發當下寫,而在 turn 的 goroutine 寫,不與其他稽核併發,順序同樣在 turn_end 之前
		ms := deps.TurnTimeout.Milliseconds()
		t.write(entity.AuditEvent{Kind: "timeout", Reason: fmt.Sprintf("%dms", ms)})
		deps.Emit.Emit(Event{Type: "notice", Agent: deps.AgentID, Text: fmt.Sprintf("✗ 本輪逾時已中止(%d 秒)", int64(math.Round(float64(ms)/1000)))})
	}
	// local 輪結束時快照(之後帳本被改也遮得到);失敗忽略,送 remote 時會再快照
	_ = try(func() error {
		if t.last != nil && t.last.Privacy() == entity.Local && deps.Masking.Snapshot != nil {
			snap, err := deps.Masking.Snapshot()
			if err != nil {
				return err
			}
			deps.MaskSet.Add(snap)
		}
		return nil
	})
	deps.Emit.Emit(Event{Type: "turn_end", Agent: deps.AgentID, Reason: reason})
	return TurnResult{Messages: t.added, StopReason: reason}, nil
}

// run:loop 回 error 或 panic → 已中止就是 aborted/timeout,否則 error。
func (t *turn) run() (reason string) {
	defer func() {
		if recover() != nil {
			reason = t.orError()
		}
	}()
	reason, err := t.loop()
	if err != nil {
		return t.orError()
	}
	return reason
}

func (t *turn) orError() string {
	if s := t.stopped(); s != "" {
		return s
	}
	return "error"
}

func (t *turn) stopped() string {
	if t.ctx.Err() == nil {
		return ""
	}
	if errors.Is(context.Cause(t.ctx), errTurnTimeout) {
		return "timeout"
	}
	return "aborted"
}

// write:稽核失敗(含 panic)不影響輪的結束。
func (t *turn) write(e entity.AuditEvent) {
	e.Agent = t.deps.AgentID
	_ = try(func() error { t.deps.Audit.Write(e); return nil })
}

// stripMessageLinks 對每個 text part 跑 StripUnsafeLinks;Content 複製一份,不改呼叫端的切片。
func stripMessageLinks(m entity.Message) entity.Message {
	content := make([]entity.Part, len(m.Content))
	for i, p := range m.Content {
		if p.Type == entity.PartText {
			p.Text = entity.StripUnsafeLinks(p.Text)
		}
		content[i] = p
	}
	m.Content = content
	return m
}

func (t *turn) emitMessage(m entity.Message, final bool) {
	t.deps.Emit.Emit(Event{Type: "message", Agent: t.deps.AgentID, Message: &m, Final: final})
}

// stringsOf 遞迴收字串值(toolCall args 的外洩檢查用);map 依鍵排序(JS 照插入順序,Go 沒有)。
func stringsOf(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = stringsOf(e, out)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out = stringsOf(x[k], out)
		}
	}
	return out
}

// leaks 外洩檢查;panic 當外洩。
func (t *turn) leaks(m entity.Message) bool {
	leak := false
	err := try(func() error {
		var parts []string
		for _, p := range m.Content {
			if p.Type == entity.PartToolCall {
				parts = stringsOf(p.Args, parts)
			} else {
				parts = append(parts, p.Text)
			}
		}
		if m.ErrorMessage != "" {
			parts = append(parts, m.ErrorMessage)
		}
		text := strings.Join(parts, "\n")
		allow := append(slices.Clone(t.deps.LeakAllow), entity.SecurityPreamble)
		for _, src := range t.deps.LeakSources() {
			if entity.LeaksPrompt(text, src, allow) {
				leak = true
				return nil
			}
		}
		return nil
	})
	return err != nil || leak
}

// notRun 補上未執行的工具結果,對話紀錄才不會有沒結果的工具呼叫。
func (t *turn) notRun(c entity.Part, privacy entity.Privacy) {
	t.added = append(t.added, entity.Message{
		Role: entity.RoleToolResult, ToolCallID: c.ID, ToolName: c.Name,
		Content: []entity.Part{{Type: entity.PartText, Text: abortedText}}, IsError: true, Privacy: privacy, Ts: time.Now().UnixMilli(),
	})
}

// block 整則換掉(不留工具呼叫、thinking、錯誤訊息)。
func (t *turn) block(m entity.Message) string {
	t.write(entity.AuditEvent{Kind: "output_block"})
	m.ErrorMessage, m.StopReason = "", "stop"
	m.Content = []entity.Part{{Type: entity.PartText, Text: blockedText}}
	t.added = append(t.added, m)
	t.emitMessage(m, true)
	return "blocked"
}

func toolCalls(m entity.Message) []entity.Part {
	var out []entity.Part
	for _, p := range m.Content {
		if p.Type == entity.PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

type streamItem struct {
	ev       LLMEvent
	panicked string
	isPanic  bool
}

// call 送一次。provider panic 或沒有 done → stopReason error 的訊息。ctx 結束 → errStopped(不等 provider 自己停)。
// PrepareRequest 在呼叫端的 goroutine 跑(用呼叫端給的 MaskSet);只有讀串流在另一個 goroutine。
func (t *turn) call(ctx context.Context, p LLMProvider, system string, msgs []entity.Message, tools []ToolSpec, forward bool, set *entity.MaskSet) (entity.Message, error) {
	req, err := PrepareRequest(PrepareInput{
		Provider: p, System: system, Messages: msgs, Tools: tools, MaxTokens: min(t.deps.Policy.Limits.MaxOutputTokens, p.MaxTokens()),
		Rewrite: t.deps.Rewrite, MaskSet: set, Masking: t.deps.Masking, Audit: t.deps.Audit,
	})
	if err != nil {
		return entity.Message{}, err
	}
	ch, quit := make(chan streamItem), make(chan struct{})
	defer close(quit)
	go func() {
		defer close(ch)
		defer func() {
			if r := recover(); r != nil {
				select {
				case ch <- streamItem{panicked: fmt.Sprint(r), isPanic: true}:
				case <-quit:
				}
			}
		}()
		for ev := range p.Stream(ctx, req) {
			select {
			case ch <- streamItem{ev: ev}:
			case <-quit:
				return // yield 回 false,provider 自己 return、關 body
			}
		}
	}()
	var msg *entity.Message
	errText := noDoneText
read:
	for {
		select {
		case <-ctx.Done():
			return entity.Message{}, errStopped
		case it, ok := <-ch:
			if !ok {
				break read
			}
			if it.isPanic {
				errText = it.panicked
				break read
			}
			if ctx.Err() != nil {
				return entity.Message{}, errStopped
			}
			if it.ev.Type == LLMEventDone {
				msg = it.ev.Message
				break read
			}
			if forward && (it.ev.Type == LLMEventTextDelta || it.ev.Type == LLMEventNotice) {
				t.deps.Emit.Emit(Event{Type: it.ev.Type, Agent: t.deps.AgentID, Text: it.ev.Text})
			}
		}
	}
	if ctx.Err() != nil {
		return entity.Message{}, errStopped
	}
	var m entity.Message
	if msg != nil {
		m = *msg
	} else {
		m = entity.Message{
			Role: entity.RoleAssistant, Content: []entity.Part{}, Ts: time.Now().UnixMilli(), Provider: p.Name(),
			StopReason: "error", ErrorMessage: entity.Redact(errText),
		}
	}
	m.Privacy = p.Privacy()
	return m, nil
}

func (t *turn) loop() (string, error) {
	d := t.deps
	agent := d.AgentID
	// 只影響數字核對(多標記),不影響防護
	_ = try(func() error { d.NumberBook.Add(entity.CollectNumbers(t.user.Content)); return nil })
	d.Budget.ResetTurn(agent)
	system := entity.SecurityPreamble + "\n\n" + d.System()
	// 順序照 Policy.Tools(Agent 預設的 Policy.Tools 就是註冊順序)
	var specs []ToolSpec
	for _, name := range d.Policy.Tools {
		if def, ok := d.Tools[name]; ok && !slices.ContainsFunc(specs, func(s ToolSpec) bool { return s.Name == name }) {
			specs = append(specs, ToolSpec{Name: def.Name, Description: def.Description, Parameters: def.Parameters})
		}
	}

	for n := 1; ; n++ {
		if s := t.stopped(); s != "" {
			return s, nil
		}
		if n > d.Policy.Limits.MaxModelCallsPerTurn {
			t.write(entity.AuditEvent{Kind: "cap", Reason: fmt.Sprintf("模型呼叫已達上限(%d 次)", d.Policy.Limits.MaxModelCallsPerTurn)})
			return "cap", nil
		}
		p := d.Provider()
		t.last = p
		msg, err := t.call(t.ctx, p, system, slices.Concat(t.history, t.added), specs, true, d.MaskSet)
		if err != nil {
			return "", err
		}

		// 輸出檢查:text、thinking、toolCall 參數、錯誤訊息都比
		if t.leaks(msg) {
			return t.block(msg), nil
		}

		// 進對話紀錄與畫面前先拿掉不安全連結:出錯 / 中止的半截回答、帶工具呼叫的中間訊息也一樣(畫面會照 final 顯示)
		msg = stripMessageLinks(msg)

		if msg.StopReason == "error" || msg.StopReason == "aborted" {
			t.added = append(t.added, msg)
			// 串流中途斷掉時 content 可能已有工具呼叫:不執行,但補上結果,
			// 否則對話紀錄留下沒結果的工具呼叫,之後每一輪都被 provider 拒絕(400)
			for _, c := range toolCalls(msg) {
				t.notRun(c, msg.Privacy)
			}
			t.emitMessage(msg, true)
			return msg.StopReason, nil
		}

		if calls := toolCalls(msg); len(calls) > 0 {
			t.added = append(t.added, msg)
			t.emitMessage(msg, false)
			cap := false
			for _, c := range calls {
				if t.stopped() != "" {
					t.notRun(c, msg.Privacy)
					continue
				}
				r := ExecuteToolCall(t.ctx, c, ToolExecDeps{
					AgentID: agent, Tools: d.Tools, Policy: d.Policy, Budget: d.Budget, Privacy: msg.Privacy,
					Audit: d.Audit, Emit: d.Emit, HTTP: d.HTTP, MaskSet: d.MaskSet, Masking: d.Masking,
				})
				// 已中止:結果不用;被放棄的 Execute 回來後也不會寫任何東西
				if t.ctx.Err() != nil {
					t.notRun(c, msg.Privacy)
					continue
				}
				t.added = append(t.added, r.Message)
				if r.Cap {
					cap = true
				}
				// 失敗結果不進:錯誤字串可能回顯參數。只影響數字核對(多標記);不能讓工具呼叫少了結果
				_ = try(func() error {
					if r.Message.IsError || r.Details == nil {
						return nil
					}
					d.NumberBook.Add(entity.CollectNumbers(r.Details))
					// hideResult 的工具整則不送 remote:原始資料也不進複核資料(複核可能送 remote);HideResult panic → 跳過(擋)
					if d.Masking.HideResult != nil {
						if _, hidden := d.Masking.HideResult(c.Name); hidden {
							return nil
						}
					}
					j, err := entity.Stringify(r.Details)
					if err != nil {
						return err
					}
					d.NumberBook.AddData(string(j))
					return nil
				})
			}
			if s := t.stopped(); s != "" {
				return s, nil
			}
			if cap {
				return "cap", nil
			}
			continue
		}

		return t.final(p, msg)
	}
}

// verifyState 是驗證器階段的複本:驗證器與它的 Ask 在別的 goroutine 跑,可能比 turn 活得久,只碰這裡。
type verifyState struct {
	mu      sync.Mutex // 保護 set、snapped;不在 hook 或串流期間持有
	set     *entity.MaskSet
	snapped bool
	book    *entity.NumberBook // 唯讀
}

func (v *verifyState) setCopy() *entity.MaskSet {
	v.mu.Lock()
	defer v.mu.Unlock()
	return entity.NewMaskSet(v.set.Values()...)
}

// final:驗證器 → 連結檢查(放最後,驗證器加的連結也拿掉)→ 對話紀錄也存改過的。
func (t *turn) final(p LLMProvider, msg entity.Message) (string, error) {
	d := t.deps
	text := entity.TextOf(msg)
	remote := p.Privacy() != entity.Local
	vs := &verifyState{set: entity.NewMaskSet(d.MaskSet.Values()...), book: d.NumberBook.Clone()}
	// mask:remote 時遮;快照只拿一次,失敗就 panic(VerifyCtx.Mask 沒有 error 回傳值;驗證器當成失敗、保留標記)
	mask := func(s string) string {
		if !remote {
			return s
		}
		vs.mu.Lock()
		need := !vs.snapped
		vs.mu.Unlock()
		if need {
			var snap []float64
			if d.Masking.Snapshot != nil {
				var err error
				if snap, err = d.Masking.Snapshot(); err != nil {
					panic(fmt.Errorf("遮蔽快照失敗:%w", err))
				}
			}
			vs.mu.Lock()
			vs.set.Add(snap)
			vs.snapped = true
			vs.mu.Unlock()
		}
		vs.mu.Lock()
		defer vs.mu.Unlock()
		return entity.MaskText(s, vs.set)
	}
	vc := VerifyCtx{
		Final: true,
		// remote 時每段先遮再截,截斷處才不會留半個沒遮的數字
		ToolData: func() string {
			if remote {
				return vs.book.Context(mask)
			}
			return vs.book.Context(nil)
		},
		KnownNumbers: vs.book.Values,
		Remote:       func() bool { return remote },
		Mask:         mask,
		// 與 Remote()/Mask 同一個 provider。任何失敗(含 panic)回 error。
		Ask: func(actx context.Context, system, user string) (reply string, err error) {
			defer func() {
				if r := recover(); r != nil {
					reply, err = "", fmt.Errorf("panic: %v", r)
				}
			}()
			// 請求在 Ask 的 ctx 或本輪結束時取消(驗證器可能傳進不相干的 ctx)
			sctx, cancel := context.WithCancel(t.ctx)
			defer cancel()
			defer context.AfterFunc(actx, cancel)()
			// actx 另外檢查:AfterFunc 在另一個 goroutine 取消 sctx,actx 剛結束時 sctx 可能還沒跟上
			if ok, stopped := t.spend(t.ctx, actx, sctx); stopped {
				return "", errStopped
			} else if !ok {
				return "", errNoBudget
			}
			q := entity.Message{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: user}}, Privacy: entity.Local, Ts: time.Now().UnixMilli()}
			m, err := t.call(sctx, p, system, []entity.Message{q}, []ToolSpec{}, false, vs.setCopy()) // 空切片:Apple FM 估 token 時序列化成 "[]" 而不是 "null"
			if err != nil {
				return "", err
			}
			if m.StopReason == "error" || m.StopReason == "aborted" {
				return "", errAskFailed
			}
			return entity.TextOf(m), nil
		},
	}
	failed := false
	for _, v := range d.Verifiers {
		out, err := t.verify(v, text, vc)
		if err != nil {
			failed = true
			t.write(entity.AuditEvent{Kind: "output_flag", Reason: "驗證器失敗"})
		} else {
			text = out
		}
		if s := t.stopped(); s != "" {
			return s, nil
		}
	}
	// 驗證器都準時回來了:快照拿到的遮蔽數字併回共用集合
	d.MaskSet.Add(vs.setCopy().Values())
	// 迴圈後才加:之後的驗證器拿不掉
	if failed {
		text += "\n\n" + unverifiedLine
	}
	text = entity.StripUnsafeLinks(text)
	content := make([]entity.Part, 0, len(msg.Content)+1)
	for _, part := range msg.Content {
		if part.Type != entity.PartText {
			content = append(content, part)
		}
	}
	msg.Content = append(content, entity.Part{Type: entity.PartText, Text: text})
	// 驗證器可能貼進複核回覆:改過的文字再查一次外洩
	if t.leaks(msg) {
		return t.block(msg), nil
	}
	t.added = append(t.added, msg)
	t.emitMessage(msg, true)
	return "done", nil
}

// verify 跑一個驗證器,與 ctx(含逾時)賽跑:卡住的驗證器不會拖住這輪。error 或 panic → 失敗。
func (t *turn) verify(v Verifier, text string, vc VerifyCtx) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		s, err := v(t.ctx, text, vc)
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-t.ctx.Done():
		return "", t.ctx.Err()
	}
}
