---
id: provider-stream-error
tests: [TestOpenAIStreamError]
behaviors: [provider-stream-error]
---
# provider 在串流中途回報錯誤時，這則回覆以 error 結束，不當成正常回答

## 義務

- OpenAI 相容 provider 的串流 chunk 帶頂層 `error`，或 `finish_reason` 是 `"error"` 時：
  - 回覆的 `StopReason` 是 `"error"`，不是 `"stop"`。
  - `ErrorMessage` 用 `error.message`；沒有就用 `error`。訊息先遮 API key，再截到 500 字。
  - 這則回覆不帶已收到的文字與工具呼叫，工具不執行。
- 已經中止（`ctx` 取消）時照舊以 `"aborted"` 結束。

## 原因

有些本機服務（例如 MTPLX）在 HTTP 200 的串流裡回報錯誤，例如記憶體不足。當成正常結束的話，agent 拿到一則空回答，開發者看不到錯誤，也無法重試。Anthropic provider 已經把串流裡的 `error` 事件當錯誤；OpenAI 相容 provider 要一致。
