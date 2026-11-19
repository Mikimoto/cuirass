package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// 工具執行管線:schema → gate → 稽核 → 執行 → 機密遮蔽 → 截斷 → 不可信包裝 → 遮蔽集合。
// 任何防護步驟回 error 或 panic 都當成擋(spec §7.1);hook 與 listener 的 panic 都接住,不 crash。

const (
	truncated   = "\n…(已截斷)"
	auditFailed = "稽核寫入失敗"
	badSchema   = "參數不符 schema"
	ruleFailed  = "規則檢查失敗"
	// publicReason panic 時給模型的固定原因(不給真原因:可能帶出不該給模型看的細節)。
	genericBlocked = "工具呼叫被擋下"
	// 最外層保險:管線本身出乎意料地 panic 時的固定錯誤,不帶任何未處理的文字。
	execFailed = "工具執行失敗"
)

type ToolExecResult struct {
	Message entity.Message
	Cap     bool
	Details any // 成功時是 redact 過、JSON 正規化的 details;isError 時 nil
}

// ToolExecDeps 是 ExecuteToolCall 的相依;取消由 ctx 帶。
type ToolExecDeps struct {
	AgentID string
	Tools   map[string]entity.ToolDef
	Policy  entity.Policy
	Budget  *entity.Budget
	Privacy entity.Privacy // 這次呼叫當下的 provider 隱私
	Audit   entity.AuditSink
	Emit    *Emitter
	HTTP    entity.HTTPClient
	MaskSet *entity.MaskSet
	Masking Masking
}

// errContent 是錯誤結果的內容;用 struct 保持 { ok: false, error } 的鍵順序(map 會排序)。
type errContent struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// truncateBytes 依 UTF-8 位元組截斷;切到半個字元時丟掉殘字。結尾的 U+FFFD(含原本就有的)一併去掉。
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(strings.ToValidUTF8(s[:max(0, n)], "\uFFFD"), "\uFFFD") + truncated
}

// redactDetails:JSON 來回並 redact;無法序列化(含 panic)或 redact 後解析失敗 → nil。
func redactDetails(details any) (out any) {
	if try(func() error {
		j, err := entity.Stringify(details)
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(entity.Redact(string(j))), &out)
	}) != nil {
		return nil
	}
	return out
}

// runTool 用 args 的深複本執行工具;回 error 或 panic → failed,msg 是錯誤訊息。
// Execute 在另一個 goroutine 跑,與 ctx 賽跑:ctx 先結束就回 abortedText,不等 Execute。
// 那個 goroutine 等 Execute 自己回來才結束,結果直接丟掉:後面的遮蔽、Collect 都不會為它跑,寫不進共用狀態。
func runTool(ctx context.Context, def entity.ToolDef, args map[string]any, http entity.HTTPClient) (out entity.ToolOutput, msg string, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			out, msg, failed = entity.ToolOutput{}, fmt.Sprint(r), true
		}
	}()
	execArgs, err := entity.CloneJSON(args)
	if err != nil {
		return entity.ToolOutput{}, err.Error(), true
	}
	type res struct {
		out    entity.ToolOutput
		msg    string
		failed bool
	}
	// 規則、稽核寫入這些同步步驟期間可能已中止:這時不開始執行,有副作用的工具才不會在中止後動手
	if ctx.Err() != nil {
		return entity.ToolOutput{}, abortedText, true
	}
	ch := make(chan res, 1) // 有緩衝:turn 不收了,goroutine 照樣送得出去、結束
	go func() {
		var r res
		defer func() {
			if p := recover(); p != nil {
				r = res{msg: fmt.Sprint(p), failed: true}
			}
			ch <- r
		}()
		o, err := def.Execute(ctx, execArgs, entity.ToolCtx{HTTP: http})
		if err != nil {
			r = res{msg: err.Error(), failed: true}
			return
		}
		r = res{out: o}
	}()
	select {
	case r := <-ch:
		return r.out, r.msg, r.failed
	case <-ctx.Done():
		return entity.ToolOutput{}, abortedText, true
	}
}

// ExecuteToolCall 執行一個工具呼叫,永遠回一則 toolResult(不 panic)。
func ExecuteToolCall(ctx context.Context, call entity.Part, deps ToolExecDeps) (res ToolExecResult) {
	name, id, args, agent := call.Name, call.ID, call.Args, deps.AgentID
	result := func(text string, isError bool) entity.Message {
		return entity.Message{
			Role: entity.RoleToolResult, ToolCallID: id, ToolName: name,
			Content: []entity.Part{{Type: entity.PartText, Text: text}},
			IsError: isError, Privacy: deps.Privacy, Ts: time.Now().UnixMilli(),
		}
	}
	defer func() {
		if recover() != nil {
			res = ToolExecResult{Message: result(execFailed, true)}
		}
	}()
	// 稽核 panic 當成寫入失敗
	write := func(e entity.AuditEvent) (ok bool) {
		_ = try(func() error { ok = deps.Audit.Write(e); return nil })
		return ok
	}
	blocked := func(reason string, cap bool) ToolExecResult {
		deps.Emit.Emit(Event{Type: "tool_blocked", Agent: agent, ID: id, Name: name, Args: args, Reason: reason})
		pub := reason
		if p := deps.Policy.PublicReason; p != nil {
			if try(func() error { pub = p(name, reason, cap); return nil }) != nil {
				pub = genericBlocked
			}
		}
		return ToolExecResult{Message: result(pub, true), Cap: cap}
	}

	// ① 找不到工具或 schema 不符:不計數、不寫稽核
	def, found := deps.Tools[name]
	valid := false
	if found {
		_ = try(func() error {
			cs, err := entity.CompileSchema(def.Parameters)
			if err != nil {
				return err
			}
			valid = cs.Check(args)
			return nil
		})
	}
	if !valid {
		return ToolExecResult{Message: result(badSchema, true)}
	}

	auditArgs := entity.FilterAuditArgs(args, def.AuditArgs)

	// ② gate;任何錯誤或 panic 都當成擋。規則拿深複本,改了也影響不到驗過 schema 的參數
	var gate entity.GateResult
	if try(func() error {
		ruleArgs, err := entity.CloneJSON(args)
		if err != nil {
			return err
		}
		gate = entity.CheckToolCall(agent, name, ruleArgs, deps.Policy, deps.Budget, deps.Privacy)
		return nil
	}) != nil {
		gate = entity.GateResult{Reason: ruleFailed}
	}
	if !gate.OK {
		write(entity.AuditEvent{Kind: "block", Tool: name, Args: auditArgs, Reason: gate.Reason, Agent: agent})
		if gate.Cap {
			write(entity.AuditEvent{Kind: "cap", Tool: name, Reason: gate.Reason, Agent: agent})
		}
		return blocked(gate.Reason, gate.Cap)
	}

	// ③ 稽核;halt 時寫不進去就不執行。只有 "halt" 才 halt,空字串依 effects 決定
	mode := deps.Policy.AuditFailure
	if mode == "" {
		mode = "warn"
		if def.Effects == entity.EffectsFS || def.Effects == entity.EffectsExec {
			mode = "halt"
		}
	}
	if !write(entity.AuditEvent{Kind: "tool", Tool: name, Args: auditArgs, Agent: agent}) && mode == "halt" {
		write(entity.AuditEvent{Kind: "audit_halt", Tool: name, Reason: auditFailed, Agent: agent})
		return blocked(auditFailed, false)
	}

	// ④ 執行;工具拿另一份深複本,改了也不影響歷史訊息、稽核與事件
	deps.Emit.Emit(Event{Type: "tool_start", Agent: agent, ID: id, Name: name, Args: args})
	out, msg, failed := runTool(ctx, def, args, deps.HTTP)
	content, isError, rawDetails := out.Content, out.IsError, out.Details
	if failed {
		content, isError, rawDetails = errContent{Error: msg}, true, nil
	}

	// ⑤ 序列化 → 機密遮蔽 → 截斷 → 不可信包裝
	var j []byte
	if try(func() (err error) { j, err = entity.Stringify(content); return err }) != nil {
		j, _ = entity.Stringify(errContent{Error: "工具結果無法序列化"})
		isError = true
	}
	red, n := entity.RedactCount(string(j), nil)
	if n > 0 {
		write(entity.AuditEvent{Kind: "redacted", Tool: name, Count: &n, Agent: agent})
	}
	text := entity.WrapUntrusted(name, truncateBytes(red, deps.Policy.Limits.ToolResultBytes))
	var details any
	if !isError {
		details = redactDetails(rawDetails)
	}

	// ⑥ local 輪:收集要遮的數字;失敗忽略(送出時 Snapshot 再補)。
	// 錯誤結果不收:錯誤文字可能回顯模型給的參數,收了等於讓模型決定遮哪些數字
	if deps.Privacy == entity.Local && !isError && deps.Masking.Collect != nil {
		_ = try(func() error {
			nums, err := deps.Masking.Collect(name, rawDetails)
			if err != nil {
				return err
			}
			deps.MaskSet.Add(nums)
			return nil
		})
	}

	// ⑦
	deps.Emit.Emit(Event{Type: "tool_end", Agent: agent, ID: id, Name: name, Args: args, IsError: isError, Details: details})
	return ToolExecResult{Message: result(text, isError), Details: details}
}
