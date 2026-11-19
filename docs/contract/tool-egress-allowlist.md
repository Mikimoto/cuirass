---
id: tool-egress-allowlist
tests: [TestHTTPClient, TestHTTPClientGoExtras, TestHTTPRedirectNotFollowed, TestHTTPDefaultClientIgnoresProxyEnv, TestHTTPDefaultTransportReplaced, TestFrameworkIgnoresGlobalTransportSettings, TestEgressBlockAuditedViaRoot, TestAgentEgressWithInjectedTransport, TestNetworkSites]
behaviors: [egress-https-allowlist]
---
# 工具的 `ToolCtx.HTTP` 只連 https 且主機在 `Policy.Hosts` 裡，不跟轉址

## 義務

- 工具透過 `ToolCtx.HTTP` 發出的請求，只在 scheme 是 `https` 且主機（`URL.Hostname()`）完全等於 `Policy.Hosts` 其中一項時才送出。主機與 scheme 不分大小寫，白名單項目也不分大小寫。
- 下列請求一律不送出，回傳錯誤 `blocked host`，並寫一筆 `egress_block` 稽核（`Host` 欄為小寫 hostname，IPv6 帶中括號）：
  - scheme 是 `http` 或其他非 `https`。
  - 主機不在白名單，包含 `api.example.com.evil.com` 這類字尾相同的主機。
  - URL 帶帳密（`user:pass@host`、`api.example.com@evil.com`）。
  - 主機含非 ASCII 字元（包含 `%`-編碼解出來的非 ASCII）。
  - URL 無法解析。
- 白名單比對不看 port。
- 不跟隨轉址。回應狀態碼 3xx 時回傳錯誤 `不跟隨轉址(<狀態碼>)`，不對 `Location` 發出任何請求，即使目標在白名單內。呼叫端自己給的 `CheckRedirect` 不會被採用，也不會被改動。
- 回應 body 預設上限 2 MiB。實際讀到超過上限（不信 `Content-Length`）時回傳錯誤 `回應超過 <N> bytes`；剛好等於上限時放行。
- 每次請求的逾時預設 8000 ms，涵蓋讀 body。逾時或呼叫端取消時，回傳的錯誤 `errors.Is` 認得 `context.DeadlineExceeded` 或 `context.Canceled`。
- 其他連線錯誤的訊息不含請求 URL（查詢字串可能帶 key），並先經過機密遮蔽。
- 回應 header 的鍵一律轉小寫。
- 出口 client 與三家 provider 的連線不讀 `HTTPS_PROXY` 等環境變數。傳輸由框架全新建立，不複製 `http.DefaultTransport`，其他套件改過全域的 `DialContext`、`TLSClientConfig` 或換掉 `DefaultTransport`，都不影響框架。公開 API 沒有注入 `http.Client` 或 `Transport` 的地方。
- 框架自己的程式碼只有 `internal/repository/http.go`、`internal/repository/sse.go` 與 `internal/repository/providers/**` 可以連線，由 `TestNetworkSites` 鎖住。
- `Policy.Hosts` 只管 `ToolCtx.HTTP`。provider 的請求只送到它設定的 `BaseURL`，不受 `Policy.Hosts` 影響。工具自己用 `net/http` 發的請求擋不到。

## 原因

工具的輸入來自模型，模型又可能被提示注入控制。要是工具能連任意主機，攻擊者就能叫模型把對話或機密塞進 URL 送出去。出口白名單把「能連哪裡」收斂成開發者明列的少數主機。

白名單只擋第一跳不夠：白名單內的主機回一個 302 指到攻擊者的網站，client 一跟隨就繞過了，所以轉址一律不跟。檢查對象是實際要撥號的那個已解析 URL，而且帶帳密、非 ASCII 主機直接擋，因為 Go 與 WHATWG URL 對這些寫法的解讀不同（例如 Kelvin 符號 `K` 小寫化後變成 `k`），差一個字元就能讓檢查看到 A 主機、連線連到 B 主機。大小與逾時上限避免單次請求拖垮記憶體或卡住一輪。

不讀 proxy 環境變數，是因為環境變數不在開發者的稽核範圍內，一旦有人設了 `HTTPS_PROXY`，請求就悄悄繞經別處。連線一律由框架建，才能保證上面這些檢查是唯一的出口。`TestNetworkSites` 把「哪些檔案能連線」鎖在靜態檢查，新增一個對外連線的檔案會讓測試失敗，逼人重新審視這份義務。
