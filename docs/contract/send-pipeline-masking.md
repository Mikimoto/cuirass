---
id: send-pipeline-masking
tests: [TestPrepareRequest, TestMask, TestPublicAPINoDataTaps, TestCollectAPICatchesBypasses, TestTurnLateWritesRaceClean, TestE2EGolden, TestPrepareRequestNoMaskingNoMask]
behaviors: [single-send-pipeline, remote-masking]
---
# 送進 provider 的請求只走一條送出管線，送 remote 前遮掉 local 輪的敏感數字

## 義務

- 框架送給 provider 的每一個請求都先經過送出管線，包含主對話與驗證器的 `VerifyCtx.Ask`。管線沒有第二個入口。
- 公開 API 沒有能看到或改走請求的管道。根套件匯出的欄位、參數與回傳值裡，型別是 func 或介面的，都要列在允許清單並寫理由。不得匯出 `LLMRequest`，不得有 `Stream` 方法，不得有 `net` 型別，不得有回傳訊息內容的函式（`Agent.Messages` 例外）。多一個少一個，`TestPublicAPINoDataTaps` 都失敗。
- 管線依序做：
  - 複製訊息，之後的改動不影響呼叫端。
  - 套用 `AgentOptions.Rewrite`（有設才做）。
  - 對 remote provider 遮蔽。
  - 再複製一次，請求不持有外部參考。
- `Rewrite` 只拿到 content 的副本。它改不到 `role`、`privacy`、`ts`、`id`。它回 `error`、panic 或回 `nil` 切片時，該則用原內容。回非 `nil` 的空切片代表內容清空。
- `Rewrite` 改動了 `privacy` 為 remote 的訊息時，該則降為 local，讓它被遮。遮蔽在 `Rewrite` 之後，`Rewrite` 塞回去的原數字仍被遮。
- 只有 provider 的 `Privacy` 明確是 `local` 才不遮。`remote`、空值、打錯字都當 remote 遮。
- 遮蔽時先呼叫 `Masking.Snapshot`，回傳的數字併進遮蔽集合，再遮：
  - 遮蔽集合裡的數字，在 local 訊息的 text、thinking、toolCall 參數（字串與數字）與 `ErrorMessage` 裡換成 `▇▇`。
  - `privacy` 為 remote 的訊息不遮。沒有隱私標記的訊息當 local，會被遮。
  - `Masking.HideResult(tool)` 回 `ok` 時，該工具的 local 結果整則換成它回的那一行。
  - local 訊息用欄位白名單重建，多餘欄位不外帶。無法辨識的 part 與 role 丟掉。
  - 命中不看格式：千分位、全形數字、小數取整後的值、`K`、`M`、`B` 後綴都會遮。個位整數只在集合裡剛好有它時才遮。
- 遮蔽任何一步失敗（`Snapshot` 回 error 或 panic、`HideResult` panic、遮蔽本身 panic），fail-closed：丟掉所有 local 訊息，只送 remote 訊息，並寫一筆 `Kind == "mask_fallback"` 的稽核。稽核寫不進去（含 panic）也照樣不送未遮的內容。
- 訊息裡有無法序列化的值時，`PrepareRequest` 回 error，不送請求。
- 沒設 `Masking`（`Snapshot`、`Collect`、`HideResult` 都是 `nil`）時，遮蔽集合是空的，不遮任何內容。框架不會自己猜哪些數字敏感。
- `System()` 回傳的提示詞與 `VerifyCtx.Ask` 的 `system` 參數不經遮蔽，原樣送出。
- provider 是 local 時，所有訊息都不遮。

## 原因

OWASP LLM 防護的核心是：local 輪看過的敏感資料，切到雲端 provider 之後不能跟著出去。要守住這點，只能有一條出口，而且遮蔽放在最後面。如果 `Rewrite` 之後還有別的 hook 能碰內容，或公開 API 讓開發者拿得到請求，任何一個疏忽都會繞過遮蔽。所以管線在遮蔽之後不碰內容，公開 API 也不交出請求，由 `TestPublicAPINoDataTaps` 逐一列舉鎖住。

遮蔽是 fail-closed：寧可丟掉 local 訊息，讓模型少看一點上下文，也不送出未遮的資料。`Masking` 由開發者決定要遮什麼，框架不內建敏感判斷，所以沒設就不遮。`System()` 是開發者自己寫的提示詞，不是 local 輪產生的資料，不在遮蔽範圍。
