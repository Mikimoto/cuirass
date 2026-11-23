---
id: provider-applefm-schema
tests: [TestFmSchema, TestFmSchemaAnyOfKeepsType, TestFmPayload]
behaviors: [provider-applefm-schema]
---
# 送給 Apple fm 的工具參數，每個欄位都帶 fm 認得的型別

## 義務

- `Compat.AppleFM` 開著時，工具參數送出前改寫成 fm 接受的 schema，只留 `type`、`enum`、`description`、`required`、`properties`、`items`。
- `anyOf` 的選項全是 `const` 時，改成 `{"type":"string","enum":[...]}`。
- 其他 `anyOf` 取第一個不是 `{"type":"null"}` 的選項，遞迴改寫後當作這個欄位的 schema；外層的 `description` 等欄位優先。例如可空字串 `anyOf:[string, null]` 送出成 `{"type":"string"}`。
- 改寫後仍沒有 `type` 的欄位（原本是任意型別 `{}`，或只有 null）一律給 `{"type":"string"}`。
- 這只改送給 fm 的提示，工具參數驗證仍用原 schema：模型傳 `null` 照樣合法。

## 原因

fm 拒收沒有 `type` 的參數，整個請求回 400「The data couldn’t be read because it isn’t in the correct format」，不會只略過那個欄位。舊版把非 const 的 `anyOf` 整個拿掉，只要有一個可空參數，所有對話都失敗。
