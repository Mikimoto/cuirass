# refundagent:會動手退款、但有煞車的客服 agent

這是教學範例。agent 可以真的呼叫退款工具,但 cuirass 幫它裝了三道煞車:規則、次數上限、稽核。被規則擋下時,agent 不會卡死,會改走允許的路(開單轉專人)。

## 內容

- `orders.go`:記憶體裡的訂單表(A 1200、B 5000、C 800、D 2500 元)與工單表。
- `agent.go`:兩個工具 `refund`、`create_ticket`,3000 元的規則,每輪上限,以及 `NewAgent`。
- `audit.go`:記在記憶體的稽核,demo 最後印出來。
- `main.go`:演三段對話,印出工具事件、回答與稽核紀錄。
- `agent_test.go`、`orders_test.go`:用 stub provider 驗證行為。

## 執行

```sh
go run ./example/refundagent                         # 離線,stub 照劇本演
ANTHROPIC_API_KEY=sk-... go run ./example/refundagent  # 改用真的 Claude
go test -tags test ./example/refundagent/            # 跑測試
```

## 三段對話,各秀一道煞車

| 客人說 | 發生什麼 | 哪道煞車 |
|---|---|---|
| 我要退訂單 A | 1200 元,規則放行,退款成功 | 規則(放行) |
| 我要退訂單 B | 5000 元,規則擋下;agent 收到原因,改呼叫 `create_ticket` 開單,回客人單號 T-001 | 規則(擋下,可恢復) |
| 訂單 C 和 D 都退 | C 退成功;D 是這輪第 2 次退款,碰到每輪 1 次的上限,這輪直接結束 | 次數上限(硬停) |

最後印出稽核紀錄:每次放行、規則擋下、碰到上限都有紀錄(參數不符 schema 的呼叫在更前面就被拒,不會寫稽核)。

## 防護層在哪裡起作用

- **規則看真的金額**:`refund` 只收 `order_id`,金額一律從訂單表拿。`Policy.Rules` 用訂單編號查真金額,超過 3000 元就擋。模型沒有參數能謊報金額;多塞一個 `amount` 會被封閉 schema 直接拒絕。3000 元的規則**沒有**寫在 system prompt 裡,煞車靠框架,不靠模型自律。
- **規則擋下 vs 上限**:兩種不一樣。
  - 規則擋下是「可恢復的」:這輪繼續,模型會收到原因,可以換一條路(例如開單)。
  - 上限是「硬停」:這輪直接結束(結束原因 `cap`),模型不會再說話,所以 `main.go` 自己印一行告訴客人。上限每輪重置,下一則訊息又能退 1 次。
- **被擋的呼叫也算次數**:規則擋下的 `refund` 也算進每輪 1 次。所以第 2 段被擋之後,這輪不能再退款,只能開單。
- **稽核**:`AuditArgs` 只列 `order_id`,`create_ticket` 的 `reason` 是自由文字,不進稽核。`AuditFailure: "halt"`:稽核寫不進去就不執行工具,退款和開單都不會做(`EffectsNone` 的工具預設只警告,動到錢的工具要設 halt)。`NewAgent` 不收 nil 的稽核,否則 halt 形同虛設。
  - 稽核的 `tool` 紀錄代表「允許執行」,不代表「執行成功」(例如退一筆已退過的訂單,一樣有 `tool` 紀錄,工具再回錯誤)。
  - 碰到上限會寫兩筆:`block` 和 `cap`。
- **業務錯誤回給模型**:訂單不存在或已退過,工具回 `IsError: true`,不回 Go error。注意這次呼叫已經用掉這輪唯一的退款次數:模型這輪不能再退,要等客人下一則訊息。
- **輸出攔截**:回答照抄 system prompt 連續 20 字以上,會被當成外洩攔下,整則換成「(已攔下:回答含系統指令內容)」;工具參數(例如 `create_ticket` 的 `reason`)照抄的話,這輪直接結束,單也不會開。所以不要把希望 agent 原樣說出口的句子寫進 system prompt;真的需要時用 `AgentOptions.LeakAllow` 列出允許照抄的句子。

## 離線劇本與真的模型

離線版用 stub provider 照劇本演,它不會判斷:「被擋之後改開單」是劇本寫好的。這樣 `go test` 每次結果都一樣。

接上真的 Claude 時,是模型自己決定要不要改開單,通常會照 system prompt 做,但不保證。例如模型被擋後又重試 `refund`,就會碰到上限,這輪結束、沒開到單;或是回答照抄了 system prompt,被輸出攔截換掉。煞車一樣有效,只是故事收尾不同。

## 換成正式環境

- 訂單表換成你的資料庫,`refund` 換成真的金流 API。會連網的工具要改宣告 `EffectsNetwork` 並設 `Policy.Hosts`,用 `tc.HTTP` 連線。
- 稽核改用 `cuirass.NewFileAudit(dir, warn)`,`dir` 放在只有執行者可寫的位置(不要放 `/tmp` 這類共用目錄;Windows 上檔案稽核一律拒寫)。
- 3000 元的門檻、每輪次數,照你的退款政策調。
