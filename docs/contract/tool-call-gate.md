---
id: tool-call-gate
tests: [TestGate, TestGateRulePanicIsBlock, TestBudget, TestExecuteToolCall, TestToolExecPanicIsBlock, TestToolExecArgsCloned, TestToolExecPublicReasonPanicGeneric, TestAgentRestoreBudget, TestRunTurn, TestExtraArgRejectedBySchema, TestToolExecNotInAllowlist]
behaviors: [gate-tool-allowlist, gate-schema-check, gate-rules, gate-call-limits]
---
# 每個工具呼叫都先過 schema、允許清單、額度、規則，任何一關擋下就不執行工具

## 義務

- 每個工具呼叫依固定順序檢查：schema，允許清單，額度，`Policy.Rules`，稽核。任何一關擋下，工具的 `Execute` 不會被呼叫，模型收到一則 `IsError` 的 toolResult。
- schema 關：
  - 工具名沒有註冊，或參數不符該工具的 `Parameters`，模型看到固定文字 `參數不符 schema`。
  - 工具的 `Parameters` 本身編譯失敗，也當成不符。
  - 這一關擋下的呼叫不計數、不寫稽核、不發 `tool_blocked` 事件。
  - `Parameters` 在註冊時必須是 `additionalProperties:false`，所以多餘的參數會在這一關被擋下，工具不會開始（不發 `tool_start`）。
- 允許清單關：工具已註冊，但名稱不在 `Policy.Tools`，擋下，原因是 `工具不在允許清單:<name>`。這種擋下不計入額度，`Cap` 為 false。
- 額度關（`Policy.Limits`，預設 `PerTurn` 20、`PerSession` 200）：
  - 計數發生在規則之前，被規則擋下的呼叫也計一次。
  - 依序檢查：session 用完，`PerSession`，`PerTurn`，`PerTool[tool]`。超過就擋下，`Cap` 為 true。
  - `PerSession` 超過的原因文字是 `本次對話工具呼叫已達上限(<PerSession> 次),請重啟`。
  - `PerTurn` 超過的原因文字是 `工具呼叫已達上限(<PerTurn> 次)`。
  - `PerTool` 超過的原因文字是 `<tool> 已達上限(<上限> 次)`，只影響該工具。
  - 單輪計數每輪開始清零；session 計數與「用完」狀態不清。
  - 還原的額度（`AgentOptions.Restore`）有任何壞值（零值、負數、非整數、null、缺欄位、未知欄位），整個 session 當成用完，之後每個呼叫都擋。
- 規則關：
  - `Policy.Rules` 依序執行，第一個回 `block=true` 的規則擋下。後面的規則不執行。
  - 規則回 `block=false` 就放行，不管 reason 寫什麼。
  - `block=true` 且 reason 為空字串，原因是 `規則擋下`。
  - 規則 panic、規則為 nil、或 gate 本身出錯，一律擋下，原因是 `規則檢查失敗`，不放行也不往外丟。
  - 規則拿到參數的深複本，改了也影響不到工具收到的參數與原本的 call。
  - 規則的 `RuleCtx.Privacy` 是這次呼叫當下的 provider 隱私；`RuleCtx.TurnCount(tool)` 回本輪該工具已計的次數，含這一次。
- 擋下的呼叫，gate 這一關寫兩種稽核：`block`（含 `Tool`、依 `AuditArgs` 過濾的 `Args`、`Reason`、`Agent`）；額度擋下另外寫一筆 `cap`。同時發 `tool_blocked` 事件，事件的 `Reason` 是真原因。
- 模型看到的擋下文字：
  - 預設就是上面的原因文字。
  - `Policy.PublicReason(tool, reason, cap)` 有設，就用它的回傳值。
  - `PublicReason` panic，模型看到固定文字 `工具呼叫被擋下`。
- `Cap` 為 true 的擋下，讓這一輪以 `cap` 結束（`turn_end` 的原因是 `cap`）。同一批的其他工具呼叫仍各自得到一則 toolResult，不會少結果。
- 管線本身出乎意料地 panic，模型看到固定文字 `工具執行失敗`，不帶任何未處理的文字。

## 原因

模型輸出不可信，工具呼叫是它能影響外界的唯一出口。所以工具執行前先驗證：參數要符合開發者宣告的 schema，工具要在開發者開放的清單內，次數要在額度內，開發者自訂的規則也要放行。任何一關出錯都當成擋，不當成放行，因為 fail-open 的防護等於沒有防護。

先計數再跑規則，是為了讓被規則擋下的呼叫也吃額度。否則模型可以用大量會被擋的呼叫反覆試探規則。schema 失敗與找不到工具不計數、不寫稽核，因為這類呼叫通常是模型格式錯誤，沒有碰到任何受保護的資源；真正有意義的稽核事件留給通過 schema 之後的擋下。`PublicReason` 讓開發者決定模型看到多少細節，事件與稽核仍保留真原因。

還原的額度不可信（來自檔案或應用層）。壞值一律視為用完，避免「解碼失敗就從 0 開始」讓額度被繞過。
