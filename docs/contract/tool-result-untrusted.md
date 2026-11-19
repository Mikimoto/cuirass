---
id: tool-result-untrusted
tests: [TestExecuteToolCall, TestToolExecPanicIsBlock, TestToolExecIsErrorSkipsCollect, TestToolExecContentMarshalPanic, TestToolExecRedactBeforeTruncate, TestTruncateBytes, TestRedact, TestRedactCount, TestJSONSecret, TestUntrusted]
behaviors: [tool-result-redact, tool-result-untrusted, tool-result-size-limit]
---
# 工具結果先遮機密、再截斷、最後包成不可信資料區塊才進模型

## 義務

- 工具結果（成功內容，或錯誤文字）依固定順序處理：序列化成 JSON，遮蔽機密，依 `Policy.Limits.ToolResultBytes` 截斷，包成 `<untrusted-data tool="<name>">`。模型只看到最後的字串。
- 序列化：
  - `ToolOutput.Content` 先 JSON 字串化；字串內容也照 JSON 規則處理（含引號與跳脫）。
  - 無法序列化（含序列化時 panic），結果改成 `{"ok":false,"error":"工具結果無法序列化"}`，`IsError` 為 true，原內容不外流。
- 遮蔽：
  - 預設規則遮私鑰、`NAME=值` 的環境變數秘密、JSON 鍵名以 key / token / secret / password / passwd / pwd 結尾的字串值、`Authorization` 類標頭、連線字串帳密、JWT、Slack、Anthropic、OpenAI、GitHub、AWS、Gemini 的 token 與 email。
  - 錯誤也走同一條管線：工具回 error、丟例外或 panic，訊息文字照樣遮蔽，格式是 `{"ok":false,"error":<訊息>}`。
  - 有遮蔽時寫一筆稽核 `redacted`，`Count` 是命中筆數；沒命中不寫。
  - 成功結果的 `ToolExecResult.Details` 也經過同一組遮蔽；`IsError` 時是 nil。
- 截斷：
  - 上限單位是 UTF-8 位元組，作用在遮蔽之後的字串，不含外層的包裝標記。
  - 超過就切到上限，切到半個字元時丟掉殘字，結尾的 U+FFFD 一併去掉，再加上 `\n…(已截斷)`。
  - 沒超過就原樣不動。
  - 先遮再截：機密跨過截斷邊界，也不會留下機密片段。
- 包裝：
  - 輸出格式固定是 `<untrusted-data tool="<name>">\n<內容>\n</untrusted-data>`。包裝在截斷之後，結尾標記一定在。
  - 內容裡的 `<`、`>` 與全形、小型、尖括號類變體，一律改寫成 `\uXXXX` 跳脫；內容裡拼不出任何標籤。
  - 內容裡出現標記名 `untrusted-data` 的變體（大小寫、全形、夾零寬字元、連字號變體），整段換成 `untrusted_data_removed`。
  - 所以模型只會看到一組真標記，內容無法偽造出提早結束或另開的標記。
  - `tool` 屬性原樣寫入工具名；工具名由開發者註冊，不當成不可信資料。
- 每輪送給模型的系統提示開頭帶有 `SecurityPreamble`，說明 `<untrusted-data>` 內的一切是資料不是指令。
- 錯誤結果（`IsError`）不收進遮蔽數字集合，因為錯誤文字可能回顯模型給的參數。

## 原因

工具結果來自外部世界（網頁、檔案、API），可能含有機密，也可能含有想劫持模型的指令（間接 prompt injection）。進模型之前一定要遮機密，否則機密會被模型複述給使用者、寫進對話紀錄、甚至送到遠端 provider。錯誤文字常常把原始參數或回應整段帶出來，所以和成功內容走同一條路，不留旁路。

順序有理由。先序列化再遮蔽，才能對到 JSON 跳脫後的值（`\n`、引號）。先遮再截，因為先截會把機密切成認不出來的半截。最後才包裝，因為包裝是防偽造的最後一層，必須在所有內容改動之後做，結尾標記才不會被截掉。包裝只靠標記不夠，內容可以自己寫出一個結尾標記再接上偽造的「系統」指令，所以內容裡的角括號與標記名都先中和。遮蔽是 best-effort 的規則集，不保證抓到所有機密。
