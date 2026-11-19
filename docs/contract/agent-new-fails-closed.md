---
id: agent-new-fails-closed
tests: [TestAgent, TestAgentRegistrationChecks, TestAgentLimitsValidation, TestNewRejectsNilAndZeroProvider, TestSchemaRejectsAtCompile, TestSchemaRoot]
behaviors: [new-rejects-bad-tool, new-rejects-open-schema, new-rejects-bad-policy, new-rejects-bad-provider]
---
# 設定不合法時 `New` 回 error，不帶著壞設定跑

## 義務

- `cuirass.New(o)` 對下列任一情況回傳 `error`，不回傳 `*Agent`。錯誤訊息指出是哪個工具、哪個欄位或哪個 provider。
- 工具（`AgentOptions.Tools`）：
  - `Name` 不符 `^[a-zA-Z0-9_-]{1,64}$`（含空字串、65 字、含引號或空白）。
  - `Name` 與前面的工具重複。
  - `Effects` 是 `EffectsFS` 或 `EffectsExec`，訊息為「專案 1 不支援 fs/exec 工具」。
  - `Effects` 不是 `EffectsNone`、`EffectsNetwork`，也不是 fs/exec（例如空字串）。
- 工具 schema（`ToolDef.Parameters`）：
  - 根層沒有 `additionalProperties: false`，回傳的錯誤可用 `errors.Is` 對到 `entity.ErrSchemaNotClosed`。
  - 根層 `type` 不是 `"object"`。
  - 任何層級出現不支援的關鍵字（例如 `$ref`、`oneOf`、`format`、`patternProperties`、`multipleOf`）。支援的關鍵字只有：`type`、`properties`、`required`、`additionalProperties`、`items`、`enum`、`const`、`anyOf`、`minimum`、`maximum`、`exclusiveMinimum`、`minLength`、`maxLength`、`minItems`、`maxItems`、`pattern`、`description`、`title`、`default`。
  - 關鍵字的值型別不對（例如 `type` 是未知字串或陣列、`minLength` 是小數或負數、`required` 不是字串陣列、`items` 是陣列、`anyOf` 是空陣列、`additionalProperties` 不是布林）。
  - schema 無法轉成 JSON（含 func、NaN）。
  - 這些檢查在建立時做完，不會留到執行時才發現。
- 政策（`AgentOptions.Policy`）：
  - `Policy.Tools` 含未註冊的工具名，訊息為「policy.tools 含未註冊的工具:<名稱>」。`Policy.Tools` 為 `nil` 表示全部已註冊工具，空切片表示一個都不開，兩者都不回錯。
  - `Policy.Limits` 任一欄位（`PerTurn`、`PerSession`、`TurnTimeoutMs`、`MaxOutputTokens`、`ToolResultBytes`、`MaxModelCallsPerTurn`、`PerTool` 的每個值）是負數或大於 2^53-1，訊息為「policy.limits.<欄位> 必須是非負整數:<值>」。欄位為 `nil` 用預設值，`0` 是有效值。
  - `Policy.AuditFailure` 不是 `""`、`"halt"`、`"warn"`（例如 `"Halt"`），不會悄悄當成 `"warn"`。
- provider 與 system：
  - `Providers` 為空或 `nil`，訊息為「至少要有一個 provider」。
  - `Providers` 含 `nil`，或含零值 `*cuirass.Provider`，訊息為「providers[<索引>] 是 nil」。
  - `Providers` 有兩個同名，訊息為「provider 名稱重複:<名稱>」。
  - `InitialProvider` 不是空字串，且找不到同名 provider，訊息為「找不到 provider:<名稱>」。
  - `System` 是 `nil`，訊息為「system 必填」。
- 合法的設定照常建立：`AuditFailure` 為 `""`、`"halt"`、`"warn"`，`Limits` 為 `0` 或 2^53-1。
- 這份 contract 不涵蓋 `LeakAllow` 太短的檢查，見 `output-prompt-leak`。

## 原因

框架的防護靠設定撐起來：工具白名單、封閉 schema、次數上限、稽核失敗策略。設定壞掉時，最危險的結果是靜默退化。負數上限會讓「次數 > 上限」的比較永遠不成立，等於沒有上限。`AuditFailure` 打錯字若變成 `warn`，稽核寫失敗就不會停下。沒封閉的 schema 讓模型可以夾帶任意參數。所以每一項都在 `New` 就失敗（fail closed），不留到執行時。

schema 不支援的關鍵字也一樣拒絕，不略過。若略過，開發者以為 `format`、`oneOf` 在擋輸入，實際上沒有。`fs`、`exec` 工具要等沙箱層才能安全執行，在那之前直接拒絕註冊，比註冊了卻沒有隔離好。
