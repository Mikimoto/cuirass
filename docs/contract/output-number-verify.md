---
id: output-number-verify
tests: [TestNumberVerifier, TestVerifierTimeout, TestVerifierAskCtxCancel, TestVerifierRemoteMask, TestVerifierFailClosed, TestVerifierPrompt, TestNumberVerifierFullWidth, TestNumbers, TestRunTurn, TestTurnVerifierHangTimesOut, TestTurnHideResultNotInReview]
behaviors: [output-number-verify]
---
# NumberVerifier 標出對不到工具資料的數字，核對失敗時保留標記

## 義務

- `NumberVerifier(timeout)` 回傳一個 `Verifier`，放進 `AgentOptions.Verifiers` 使用。`timeout <= 0` 用預設 30 秒，不會無上限。
- 它只在最終回答（不帶工具呼叫的那則）上跑。`error`、`aborted` 的訊息與帶工具呼叫的中間訊息不跑驗證器。
- 數字主張的抽取規則：
  - 帶小數點、帶單位，或整數部分大於等於 10 的數字才算。個位整數且無小數、無單位的不算，例如「近 5 日」。
  - 單位有 `%`、`％`、`億`、`萬`、`千元`。全形數字、小數點、逗號先轉半形。
  - 時間（`13:30`）與行首的清單編號（`1.`）不算數字。
- 已知數字來自本輪使用者訊息，以及整段對話中成功工具結果的 `details`（遞迴收，含字串裡的數字）。失敗的工具結果不收。`NumberBook` 上限 5000 個，超過丟最舊的。
- 比對規則：依回答數字的小數位數四捨五入後相等，或相對誤差在 0.5% 以內，正負號不論。帶單位的另試原數與換算後的值（`億`、`萬`、`千元`），`%` 另試比率（除以 100）。
- 回答裡有對不到的數字時，final 階段先用 `VerifyCtx.Ask` 請模型複核一次，只認複核回覆裡以 JSON 字串陣列列出的數字原文。複核認可的不標記。
- 仍對不到的數字，在所在行尾加 ` ⚠未核對`，結尾補空行與 `有 N 個數字無法對到工具資料`。沒有未核對數字時，文字原樣不動。
- 複核失敗、額度用完、逾時（`timeout`）、取消、回覆不是 JSON 陣列、`Ask` panic，都保留標記，不放行。
- provider 是 remote 時：送去複核的工具資料與回答先套遮蔽。被遮掉的數字無法核對，不算錯，不標記也不送出。複核資料每段先遮再截，截斷處不會留下半個沒遮的數字。`hideResult` 的工具資料不進複核資料。
- 驗證器本身失敗或 panic（例如 `KnownNumbers` 失敗）時，`RunTurn` 保留原回答，在結尾加 `⚠ 這則回答未能完成核對`（只加一次），並寫稽核 `output_flag`，`Reason` 為 `驗證器失敗`。
- 驗證器卡住時，這一輪不會被拖住。受本輪逾時管轄，逾時就中止。
- 這個標記只給畫面看，不影響其他防護。

## 原因

模型常把工具查到的數字算錯、記錯，或乾脆編一個（OWASP LLM09）。使用者看到有模有樣的數字就會信。把對不到工具資料的數字標出來，使用者才知道哪些要自己再確認。

數字對不到不一定是錯，可能是模型做了合理的四則運算或單位換算，所以先請模型複核一次，認可的才放過。複核失敗時保留標記而不放行，是因為「核對不了」不能當成「核對過了」。複核請求會把資料送出去，所以 remote 時一律先遮蔽，並且遮蔽要在截斷之前，否則截斷處剩下的半個數字會逃過遮罩。
