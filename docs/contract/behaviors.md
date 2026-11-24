# cuirass 對外行為清單

每個分組對應一份 Contract（`docs/contract/<id>.md`），細節與理由寫在 Contract 裡。

## 建立 agent（agent-new-fails-closed）

- `new-rejects-bad-tool`：工具名不符 `^[a-zA-Z0-9_-]{1,64}$`、重複、`Effects` 是 fs/exec 或無法辨識時，`New` 回 error
- `new-rejects-open-schema`：工具 schema 根層不是封閉的 object、用了不支援的關鍵字或值型別不對時，`New` 回 error
- `new-rejects-bad-policy`：`Policy.Tools` 含未註冊工具、`Limits` 是負數或超過 2^53-1、`AuditFailure` 不是 `""`/`halt`/`warn` 時，`New` 回 error
- `new-rejects-bad-provider`：沒有 provider、provider 是 nil 或重名、`InitialProvider` 找不到、`System` 是 nil 時，`New` 回 error

## 額度還原（budget-restore-fail-closed）

- `budget-restore-fail-closed`：還原的 `BudgetState` 有任何壞值，整個 session 額度直接用完，不從 0 重來

## 一輪對話（turn-lifecycle）

- `prompt-queues`：`Prompt` 排隊，同一個 agent 同時只跑一輪；排隊中被取消或 agent 停用時不跑
- `abort-current-turn`：`Abort` 只中止目前這輪，排隊中的輪照跑，之後還能繼續問
- `set-provider-between-turns`：`SetProvider` 在輪與輪之間才生效，排在它前面的輪用舊 provider
- `system-reset-each-turn`：每輪開始才呼叫 `System()`，前面固定加 `SecurityPreamble`
- `turn-timeout`：本輪超過逾時就結束並寫 `timeout` 稽核；被放棄的工具與驗證器結果一律丟掉

## 送出管線與遮蔽（send-pipeline-masking）

- `single-send-pipeline`：所有送進 provider 的請求（含 `VerifyCtx.Ask`）都經過同一條送出管線，公開 API 沒有看到或改走請求的管道
- `remote-masking`：有設 `Masking` 時，送到 remote provider 前遮掉 local 訊息的敏感數字；遮蔽失敗就丟掉 local 訊息並寫 `mask_fallback`

## 工具授權（tool-call-gate）

- `gate-tool-allowlist`：模型只能呼叫 `Policy.Tools` 開放的已註冊工具
- `gate-schema-check`：沒註冊的工具或參數不符 schema 時，工具不執行，模型看到 `參數不符 schema`
- `gate-rules`：`Policy.Rules` 第一個回 block 的規則擋下；規則 panic 也算擋下
- `gate-call-limits`：每 session、每輪、每個工具的呼叫次數有上限，超過擋下並讓這輪以 `cap` 結束

## 工具結果（tool-result-untrusted）

- `tool-result-redact`：工具結果與錯誤文字送進模型前先遮機密，命中時寫 `redacted` 稽核
- `tool-result-size-limit`：遮蔽之後依 `ToolResultBytes` 截斷，先遮再截，不留機密片段
- `tool-result-untrusted`：工具結果包成 `<untrusted-data>`，內容無法偽造標記

## 工具對外連線（tool-egress-allowlist）

- `egress-https-allowlist`：`ToolCtx.HTTP` 只連 https 且主機在 `Policy.Hosts`；不跟轉址，回應有大小與逾時上限，不讀 proxy 環境變數

## local provider（local-provider-loopback-only）

- `local-provider-loopback-only`：`Privacy: local` 的 provider 只能連 loopback，`CreateProvider` 與撥號時都檢查；privacy 值無法辨識一律當 remote

## provider 串流錯誤（provider-stream-error）

- `provider-stream-error`：OpenAI 相容串流中途回報錯誤（頂層 `error` 或 `finish_reason: "error"`）時，回覆以 `error` 結束並帶錯誤訊息，不當成空的正常回答

## Apple fm 工具參數（provider-applefm-schema）

- `provider-applefm-schema`：`Compat.AppleFM` 送出的工具參數每個欄位都有型別；可空欄位取非 null 的型別，參數驗證仍用原 schema

## 系統提示詞外洩（output-prompt-leak）

- `output-prompt-leak-block`：訊息含系統提示詞的連續 20 字片段時整則攔下，工具呼叫不執行，這輪以 `blocked` 結束
- `new-rejects-short-leak-allow`：`LeakAllow` 有正規化後短於 20 字的句子時，`New` 回 error

## 不安全連結（output-unsafe-links）

- `output-unsafe-links`：每則進對話紀錄與畫面的訊息拿掉非 https 連結，markdown 圖片一律拿掉

## 數字核對（output-number-verify）

- `output-number-verify`：`NumberVerifier` 在最終回答標出對不到工具資料的數字；驗證器失敗時加註並寫 `output_flag`

## 稽核（audit-log）

- `audit-events`：工具呼叫（`tool`）、擋下（`block`、`cap`）、出口擋下（`egress_block`）、逾時（`timeout`）、輸出攔截（`output_block`）、遮蔽（`redacted`）、稽核中止（`audit_halt`）各寫一筆
- `audit-declared-args-only`：稽核只記工具 `AuditArgs` 列出的原始型別參數，字串先遮再截
- `audit-file-safe`：`NewFileAudit` 寫 JSON Lines，檔案 0600，symlink 與非一般檔案拒寫，寫入失敗會警告
