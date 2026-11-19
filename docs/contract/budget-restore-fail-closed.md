---
id: budget-restore-fail-closed
tests: [TestAgentRestoreBudget, TestBudget, TestBudgetRestoreInvalidExhausts]
behaviors: [budget-restore-fail-closed]
---
# 還原的額度有任何壞值，整個 session 額度用完

## 義務

- `AgentOptions.Restore`（`*BudgetState`）為 `nil` 時，額度從 0 開始。
- `Restore` 不是 `nil` 時，下列任一情況讓整個 session 額度用完：之後每次工具呼叫都被擋（`tool_blocked`，原因為「本次對話工具呼叫已達上限(<PerSession> 次),請重啟」），非工具的模型呼叫（複核）也拿不到額度，`Agent.Budget()` 回傳的 `BudgetState.Exhausted` 為 `true`。
  - `Session` 是負數，或大於 2^53-1。
  - `Turn` 是 `nil`（零值 `BudgetState{}` 屬於這種）。
  - 任一個 agent 的 `TurnState.PerTool` 是 `nil`。
  - 任一個 `TurnState.Calls`、`PerTool` 的值是負數或大於 2^53-1。
  - `Exhausted` 已是 `true`。
- 用 `json.Unmarshal` 解 `BudgetState` 時，`UnmarshalJSON` 不回錯，下列情況一律解成用完的狀態（`Exhausted: true`）：
  - `session` 是負數、小數、`null`、字串、物件、陣列、布林，或大於 2^53-1。
  - `session` 或 `turn` 缺少；頂層不是物件。
  - 頂層或 `turn.<agent>` 內有未知欄位。
  - `calls` 缺少、`null`、負數、非整數；`perTool` 缺少或不是物件；`perTool` 的值不是合法計數。
  - 任何一個 agent 的 turn 壞掉，影響整個 session，不只那個 agent。
- JSON 語法錯誤時，`json.Unmarshal` 在呼叫 `UnmarshalJSON` 之前就回錯，目標保持零值，零值還原後同樣是用完。
- 單輪計數壞掉也讓整個 session 用完，`ResetTurn` 清不掉，因為單輪計數每輪都會被清掉。
- 合法值照用：`session` 為 `7` 還原後第一次工具呼叫使 `Session` 變 8；`-0`、`1e1`、`2.0` 這類值等於合法整數，照用。
- `Restore` 會被深複製，之後改呼叫端的 `BudgetState` 不影響 agent。
- `Exhausted` 為 `true` 時，序列化後 `"session"` 為 `null`，`Session` 欄位沒有意義，不要拿來算剩餘額度。

## 原因

額度存檔來自檔案或應用層，不可信。最危險的失敗是「還原失敗就當成從 0 開始」：攻擊者或一個壞掉的存檔就能把已用掉的 session 額度洗回 0，繞過 `PerSession` 上限。所以還原採 fail closed，任何壞值都讓額度用完，寧可擋掉合法使用者要求重啟，也不放行。

壞值要影響整個 session，不能只標那一輪。單輪計數每輪開始都會被 `ResetTurn` 清掉，若壞值只作用在單輪，下一輪就恢復正常，等於壞值被洗掉。`UnmarshalJSON` 不回錯也是同一個理由：呼叫端常寫 `_ = json.Unmarshal(...)`，若解碼失敗保留 0 值，就會變成放行。
