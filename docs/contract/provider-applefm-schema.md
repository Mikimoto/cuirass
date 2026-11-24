---
id: provider-applefm-schema
tests: [TestFmSchema, TestFmSchemaAnyOfKeepsType, TestFmPayload]
behaviors: [provider-applefm-schema]
---
# 送給 Apple fm 的工具參數，每個欄位都帶 fm 認得的型別

## 義務

- `Compat.AppleFM` 開著時，工具參數送出前改寫成 fm 接受的 schema，只留 `type`、`enum`、`description`、`required`、`properties`、`items`。
- `anyOf` 的選項全是 `const` 時，改成 `enum`，型別照下一條的規則決定。
- 其他 `anyOf` 取第一個不是 `{"type":"null"}` 的選項，遞迴改寫後當作這個欄位的 schema；外層的 `description` 等欄位優先。例如可空字串 `anyOf:[string, null]` 送出成 `{"type":"string"}`。
- 改寫後仍沒有 `type` 的欄位，依 `enum` 的值決定型別：全是數字給 `number`，全是布林給 `boolean`，全是字串給 `string`。沒有 `enum`（任意型別 `{}`、只有 null），或列舉值混了不同型別、null、陣列、物件時，拿掉 `enum`、給 `{"type":"string"}`，避免型別和列舉值互相矛盾，沒有任何合法值。
- 已有 `type`（外層寫的，或取自 `anyOf` 分支）但和 `enum` 值的型別衝突時（`integer` 配數字不算衝突），拿掉 `enum`、保留 `type`。
- 這只改送給 fm 的提示，工具參數驗證仍用原 schema：模型傳 `null` 照樣合法。

## 原因

fm 拒收沒有 `type` 的參數，整個請求回 400「The data couldn’t be read because it isn’t in the correct format」，不會只略過那個欄位。舊版把非 const 的 `anyOf` 整個拿掉，只要有一個可空參數，所有對話都失敗。
