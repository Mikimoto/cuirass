---
id: turn-lifecycle
tests: [TestAgent, TestAgentConcurrentSendQueues, TestAgentSetProviderMidTurnQueued, TestAgentQueuedPromptExits, TestRunTurn, TestTurnAbortNoGoroutineLeak, TestTurnLateWritesRaceClean, TestTurnAskAfterReturnNoSpend, TestTurnVerifierHangTimesOut, TestTurnSystemCalledEachTurn]
behaviors: [prompt-queues, abort-current-turn, set-provider-between-turns, system-reset-each-turn, turn-timeout]
---
# 一個 agent 同時只跑一輪，中止、換 provider、逾時都只影響輪與輪之間的邊界

## 義務

- `Agent.Prompt(ctx, text)` 同時只跑一輪，其餘依呼叫順序排隊，阻塞到自己那一輪結束。多個 goroutine 同時呼叫也一樣，輪與輪不重疊。
- 排隊中（還沒輪到）的 `Prompt`：
  - `ctx` 取消時，馬上回傳 `ctx.Err()`，不跑那一輪，不影響佇列裡其他輪與 `SetProvider` 的順序。
  - `Dispose()` 時，回傳「agent 已停用」，不跑那一輪。
- 已經輪到的 `Prompt`，`ctx` 取消時這輪照跑，以 `turn_end` 的 `Reason == "aborted"` 結束，`Prompt` 回傳 `nil`。
- 只有設定或 hook 出錯、排隊中被取消、agent 已停用時 `Prompt` 才回 error。這輪怎麼結束，看 `turn_end` 事件。
- `Agent.Abort()` 只中止目前這輪：
  - 這輪以 `aborted` 結束。
  - 排隊中的輪照跑。
  - 之後可以再 `Prompt`。
  - 沒有輪在跑時什麼都不做。
- `Agent.Dispose()` 中止目前這輪，排隊中與之後的 `Prompt` 都回「agent 已停用」。可重複呼叫。
- `Agent.SetProvider(name)` 在輪與輪之間生效：
  - 閒置時立刻換。
  - 有輪在跑或排隊時，排進佇列。排在它前面的輪仍用舊的 provider，它後面才排隊的輪用新的。
  - 名稱找不到回傳 `false`，不改任何東西。
  - 輪進行中 `Agent.Provider()` 仍回這輪用的那一個。
- 每一輪開始時才呼叫 `AgentOptions.System`，送給 provider 的 system 一律是 `SecurityPreamble + "\n\n" + System()`。安全前言固定在最前面，開發者沒有關掉它的選項。
- 本輪逾時：
  - 逾時長度取 `AgentOptions.TurnTimeout(info)`，回傳 ≤ 0 或函式為 `nil` 時取 `Policy.Limits.TurnTimeoutMs`。
  - 逾時後 `Prompt` 馬上返回，不等不理會 `ctx` 的 provider、工具或驗證器。
  - 在 `turn_end` 之前寫一筆 `Kind == "timeout"`、`Reason` 為 `"<毫秒>ms"` 的稽核，並送一則 `notice` 事件。`turn_end` 的 `Reason` 是 `"timeout"`。
  - 逾時發生在驗證器階段時（卡住的驗證器不拖住這輪），這輪不留下回答。
  - 逾時落在最後幾個不與 `ctx` 賽跑的檢查步驟（驗證器之後的連結拿掉與外洩再檢查）時，這輪照常以 `done` 結束，不寫 `timeout` 稽核。
- 中止（`Abort`、`ctx` 取消、`Dispose`）或逾時後，還沒回來的工具 `Execute`、驗證器與它呼叫的 `Ask` 成為被放棄的 goroutine：
  - 它們繼續跑到自己返回為止，框架不強制結束。
  - 它們的結果一律丟掉，寫不進對話紀錄、遮蔽集合（`MaskSet`）與數字帳本，也不觸發後面的步驟（例如 `Masking.Collect`）。
  - `Prompt` 返回之後，被放棄的 `Ask` 不再扣額度，並回傳錯誤。還沒返回時已扣的那一次算在那一輪。
- `AuditSink.Write`、`Masking.Snapshot`、`ContentRewrite` 與 provider 的 `Stream` 可能在 `Prompt` 返回之後（甚至下一輪進行中）被放棄的 goroutine 呼叫，所以要能併發使用。
- `System`、`LeakSources`、`Masking.Collect`、`Policy.Rules`、`Policy.PublicReason`、listener、`TurnTimeout` 只在這輪自己的 goroutine 上同步呼叫。它們卡住，這輪就卡住，中止也救不回來。
- 在 listener 或這些 hook 裡同步呼叫 `Prompt` 會卡住：新的那輪排在自己這輪後面。只有那個 `Prompt` 的 `ctx` 取消或逾時、或另一個 goroutine 呼叫 `Dispose()` 才放得開。接著問要用 `go agent.Prompt(...)`。

## 原因

Agent 有一份共用的對話紀錄、遮蔽集合、數字帳本與額度。兩輪同時跑會互相寫壞這些狀態，遮蔽集合少加一個數字就等於外洩。所以框架用佇列把輪串起來，`Abort` 與 `SetProvider` 都只在輪的邊界動作：中止不會殃及排隊中的別人的問題，換 provider 也不會讓一輪進行到一半換了遮蔽規則。

Go 沒有辦法強制結束一個不理會 `ctx` 的 goroutine，只能不等它。所以規則是把它的結果整個丟掉，讓晚回來的工具或驗證器沒有機會寫進共用狀態，也沒有機會在輪結束後繼續花額度。安全前言每輪重新加在最前面，開發者的 `System()` 即使寫得再鬆，也不會少掉這一段。逾時用同一套機制：這輪結束的當下就是邊界。
