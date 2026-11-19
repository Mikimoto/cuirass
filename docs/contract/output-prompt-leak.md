---
id: output-prompt-leak
tests: [TestRunTurn, TestLeaksPrompt, TestLeaksPromptBypass, TestLeaksPromptGo, TestNormalizeLeakGo, TestTurnLeakAllowValidation, TestTurnLeakAllowBoundary, TestTurnLeakInThinkingAndError]
behaviors: [output-prompt-leak-block, new-rejects-short-leak-allow]
---
# 回答含系統提示詞時整則攔下，不留任何原內容

## 義務

- 模型每回一則訊息，`Agent` 在放行前先比對外洩。比對的內容是這則訊息裡的全部 text、thinking、toolCall 參數裡的字串值（巢狀的 map 與陣列也收），加上 `ErrorMessage`，以換行接成一段。
- 比對來源由 `AgentOptions.LeakSources` 提供。`LeakSources` 為 `nil` 時，來源是 `System()` 回傳的系統提示詞。任一來源判定外洩，整則攔下。
- 判定規則：回答與來源各自正規化後，回答含有來源任一段連續 20 個 UTF-16 code unit（`LeakMin`），就算外洩。正規化會去掉數字符號、標點、空白、組合符號、行首清單符號，並轉小寫、套 NFKC。所以改大小寫、插空白、插標點、全形半形都躲不掉。
- 回答只含 19 個 code unit 的片段不算外洩。
- 外洩時：
  - 這則訊息的內容整個換成一個 text part：`(已攔下:回答含系統指令內容)`。原本的 thinking、toolCall、`ErrorMessage` 都不保留，`StopReason` 設為 `stop`。
  - 這則訊息裡的工具呼叫一個都不執行。
  - 寫一筆稽核 `output_block`。
  - 送出 `Final: true` 的 `message` 事件，內容是換掉後的訊息，不送非 final 的版本。
  - 這一輪以 `blocked` 結束（`turn_end` 的 `Reason` 與 `TurnResult.StopReason`）。換掉後的訊息進對話紀錄。
- 比對本身 panic（例如 `LeakSources` 丟例外）時，當作外洩攔下。
- 有驗證器時，驗證器改完的最終回答再比對一次。驗證器貼進系統提示詞，一樣整則攔下。
- 允許句不算外洩：`AgentOptions.LeakAllow` 的每一句，加上內建的安全前言 `SecurityPreamble`，會先從來源挖掉再比對。
- `LeakAllow` 每一句正規化後要有至少 20 個 UTF-16 code unit（用 code unit 算，不是 rune）。空字串或短句會讓比對失效，所以 `cuirass.New` 直接回傳 `error`，指出是第幾句、實際字數與最低字數。
- 正規化讓 NFKC 插入 U+034F（Stream-Safe 處理）時，直接當外洩，只會多攔，不會少攔。
- 不做：不比對使用者訊息與工具結果；不做模糊或語意比對，只比對連續片段。

## 原因

系統提示詞常帶有業務規則、內部欄位名稱，甚至憑證。提示詞注入最常見的目標就是叫模型把它吐出來（OWASP LLM07）。在出口攔，不靠模型自己守口，模型被說服時也守得住。

比對的內容不只 text。攻擊者可以叫模型把提示詞塞進 thinking、工具參數或錯誤訊息，那些路徑會被畫面顯示、寫進紀錄，或送到外部工具，一樣是外洩。攔下時整則換掉，不做部分遮蔽，因為部分遮蔽會留下可拼回原文的片段，也可能留下帶外洩參數的工具呼叫。

標點、空白、大小寫都正規化掉，是為了擋「每個字中間插一個符號」這類逐字改寫。`SecurityPreamble` 與 `LeakAllow` 要例外，是因為模型被允許照抄那些句子（例如免責聲明）。允許句太短會把來源切碎，讓偵測形同關閉，所以建立 agent 時就拒絕。
