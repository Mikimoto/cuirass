# codingagent:用 cuirass 寫自己的 coding agent

這是教學範例。它示範怎麼用 cuirass 的公開 API 組一個小型 coding agent。

## 內容

- `workspace.go`:記憶體工作區(path 對應內容),放兩個範例 Go 檔,其中 `add.go` 有個 bug。
- `agent.go`:四個工具 `list_files`、`read_file`、`write_file`、`search`,以及 `NewAgent`。
- `main.go`:示範程式,印出工具事件與最後回答。
- `agent_test.go`:用 stub provider 驗證行為。

## 執行

```sh
go run ./example/codingagent                        # 離線,stub 照劇本演:list、read、write、回答
ANTHROPIC_API_KEY=sk-... go run ./example/codingagent  # 改用真的 Claude
go test -tags test ./example/...                    # 跑測試
```

## 防護層在哪裡起作用

- **封閉 schema**:每個工具都是 `additionalProperties: false`。模型多塞參數(例如 `write_file` 加 `mode`),
  在工具執行前就被拒,工具不會跑。這是 cuirass 註冊工具的硬性要求。
- **工具白名單**:`Policy.Tools` 只列這四個,模型叫不動別的工具。
- **稽核最小化**:`AuditArgs` 只列 `path`、`query`;檔案內容 `content` 不會進稽核檔。
- **輸入驗證**:路徑不可為絕對路徑或含 `..`,單檔上限 8 KiB,最多 50 個檔案。
  工具失敗時回 `IsError: true` 的結果給模型,讓它自己修正,而不是回 Go error。
- **輸出處理**:工具結果會被遮蔽機密、截斷、標成不可信內容再交給模型(框架自動做)。

## 為什麼檔案在記憶體裡

cuirass 目前在註冊時會**直接拒絕** `Effects` 為 `fs` 或 `exec` 的工具,因為 L4 sandbox 還沒實作。
所以這裡的工具只操作記憶體的 map,並誠實宣告 `cuirass.EffectsNone`。不要為了過關把真的寫檔工具標成 `EffectsNone`。

等 sandbox 實作後:把 `Workspace` 換成真的檔案操作,工具的 `Effects` 改成 `EffectsFS`
(要跑指令的工具用 `EffectsExec`),其他程式碼不用改。
