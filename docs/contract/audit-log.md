---
id: audit-log
tests: [TestExecuteToolCall, TestEgressBlockAuditedViaRoot, TestRunTurn, TestResolveProviderPrivacy, TestCreateProviderLocalOverrideErrors, TestFileAudit, TestAuditFilePerms, TestAuditFileSymlinkRefused, TestAuditFileFiltersArgs, TestAuditFileNonFiniteNumbers, TestAuditFileOptionalFields, TestAuditFileConcurrent, TestAuditFileFIFORefused, TestAuditFileFIFOWithReaderRefused, TestAuditFileWarnReentrant, TestAuditFileNoNofollowRefuses, TestFilterAuditArgs, TestAgent, TestAgentRegistrationChecks, TestAgentToolDefCopied, TestToolExecAbortBeforeExecute, TestToolExecAuditHaltEvent]
behaviors: [audit-events, audit-declared-args-only, audit-file-safe]
---
# 稽核記錄關鍵事件，只記工具宣告的參數，檔案稽核安全落地

## 義務

事件（`AgentOptions.Audit` 的 `AuditSink.Write`）

- 下列情況各寫一筆事件，`Kind` 如下：
  - 工具通過檢查、即將執行：`tool`（含 `Tool`、`Args`、`Agent`）。
  - 工具呼叫被規則、白名單或次數上限擋下：`block`；因次數上限擋下時另寫一筆 `cap`。
  - `ToolCtx.HTTP` 擋下連線：`egress_block`（`Host`）。
  - 一輪逾時：`timeout`（`Reason` 是毫秒數，如 `20ms`），寫在 `turn_end` 之前。
  - 最終輸出被攔下：`output_block`。
  - 工具結果裡的機密被遮蔽：`redacted`（`Count` 是遮蔽個數，不記內容）。
  - `halt` 模式下稽核寫不進去、工具被擋：`audit_halt`。
- 找不到工具或參數不符 schema 的呼叫不寫稽核，也不計次數。
- `Audit` 為 nil 時用什麼都不記的 sink，永遠成功。
- sink 的 `Write` 回 `false` 或 panic 都算寫入失敗，不會讓呼叫端 panic。
- `Policy.AuditFailure` 只收 `""`、`"halt"`、`"warn"`，其他值讓 `New` 回錯（`policy.auditFailure 只能是 "halt" 或 "warn"(或不填)`）。
- 工具呼叫寫 `tool` 稽核失敗時：`halt` 不執行工具，回給模型 `稽核寫入失敗`（工具不執行）；`warn` 照樣執行。`""` 時依工具的 `Effects`：`fs`、`exec` 當 `halt`，`none`、`network` 當 `warn`。
- 稽核寫入期間 context 被中止時，工具的 `Execute` 不會開始。

參數

- 事件的 `Args` 只含工具在 `ToolDef.AuditArgs` 列出的鍵。沒列的參數（例如 `secret`）不進稽核。`AuditArgs` 沒填就是空 map。
- 只收原始型別的值：字串、布林、數字。物件與陣列丟棄。
- 字串先機密遮蔽，再壓成單行（去控制字元），再截到 200 個 UTF-16 code unit。先遮蔽再截斷，所以跨在第 200 字上的 key 也不留片段。
- `FilterAuditArgs` 是公開函式，規則相同。
- 註冊工具時 `AuditArgs` 會被複製，之後改呼叫端的切片不影響稽核。

檔案稽核（`NewFileAudit(dir, warn)`）

- 建立時不碰檔案系統。每次 `Write` 在 `dir/audit.log` 尾端附加一行 JSON（JSON Lines），含 UTC 毫秒時間 `ts`（`2006-01-02T15:04:05.000Z`）與事件欄位，空值欄位不寫。
- 寫入端再做一次清理：所有字串欄位（`kind`、`tool`、`host`、`reason`、`from`、`to`、`agent`）先機密遮蔽再壓單行；`args` 再套一次上述的過濾規則。JSON 寫不出來的數字（NaN、±Inf）寫成 `null`，事件不丟。不轉義 `<`、`>`、`&`。
- 目錄權限 `0700`、檔案 `0600`。目錄或檔案已存在而權限太寬時，每次寫入都收回。
- 以 `O_NOFOLLOW` 開檔：`audit.log` 是 symlink 時拒寫，`Write` 回 `false`，不寫到連結目標，也不改動目標權限。`audit.log` 不是一般檔案（如 FIFO）時立刻拒寫，不阻塞。
- 沒有 `O_NOFOLLOW` 的平台（非 unix，如 Windows）一律拒寫，`Write` 回 `false`。
- 寫入失敗時回 `false`，並呼叫 `warn` 一次，訊息以 `audit 寫入失敗:` 開頭；之後的失敗不再重複 `warn`。`warn` 傳 `nil` 時寫到標準 log，前綴 `cuirass audit:`，不會沒聲音。`warn` panic 不外洩。`warn` 在鎖外呼叫，回頭寫同一個 sink 不會死鎖。
- 多個 goroutine 同時寫，每行完整不交錯。
- 稽核目錄本身是不是 symlink、上層目錄有沒有被換掉，不檢查。

## 原因

稽核是事後追責的唯一憑據，所以要記到足以回答「誰在哪一輪呼叫了什麼、什麼被擋了」，又不能自己變成洩漏管道。工具參數常帶使用者資料與機密，預設全不記，只記工具作者明確宣告安全的欄位，且只收簡單型別，避免整個物件被倒進檔案。字串再遮蔽、壓單行、截斷：遮蔽防 key 外流，壓單行防有人塞換行偽造一整條稽核記錄（log injection），截斷防單筆事件過大。寫入端重做一次，是因為 `AuditSink` 的呼叫端不一定先過濾。

稽核寫不進去時要有人知道。具副作用的工具（`fs`、`exec`）預設 `halt`：寧可不執行，也不要做了無法追責的事；唯讀的工具預設 `warn`，不因為磁碟問題讓整個 agent 停擺。`AuditFailure` 打錯字會在建立時回錯，不會悄悄降成 `warn`。

檔案層面，稽核檔是攻擊者想竄改或轉向的目標。`0600` 避免其他使用者讀到，`O_NOFOLLOW` 避免有人把 `audit.log` 換成指向別處的 symlink 而讓我們覆寫或附加到不該碰的檔案。不支援 `O_NOFOLLOW` 的平台寧可拒寫。稽核目錄與上層路徑不檢查，所以目錄要放在只有執行者可寫的位置，不要放 `/tmp` 這類共用可寫處。
