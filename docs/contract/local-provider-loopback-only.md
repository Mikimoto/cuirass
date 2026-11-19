---
id: local-provider-loopback-only
tests: [TestCreateProviderLocalOverrideErrors, TestResolveProviderPrivacy, TestLoopbackDialRejectsRemoteIP, TestLoopbackDialAllows127, TestProviderClient, TestPrivacy, TestResolvePrivacyInvalidHosts, TestResolvePrivacyLaxURLParsing]
behaviors: [local-provider-loopback-only]
---
# `Privacy: local` 的 provider 只能連 loopback，建立與撥號時各檢查一次

## 義務

- provider 設定明填 `Privacy: local`，但 `BaseURL` 不是 loopback 時，`CreateProvider` 回傳錯誤、不回傳 provider。錯誤訊息含 provider 名稱與 `privacy: local 但 baseUrl 不是 loopback`。三種 `API`（`anthropic-messages`、`openai-completions`、`google-generative-ai`）行為相同。
- 這種情況回錯之前，先寫恰好一筆 `privacy_override` 稽核，`Host` 是 `BaseURL` 的 hostname，`Reason` 含 provider 名稱。
- loopback 只有這幾種：`localhost`、`127.0.0.0/8` 內的 IPv4、剛好是 `::1` 的 IPv6。scheme 只收 `http`、`https`。port 超過 65535、`127.1` 或 `0177.0.0.1` 這類寫法、`::1` 以外的 IPv6（含 zone、v4-mapped）、沒中括號的多冒號主機、壞 URL，一律不算 loopback。判斷不受 `GODEBUG=urlstrictcolons` 影響。
- `Privacy` 明填 `"local"`、`"remote"` 以外的值（打錯字、大小寫不對、JSON `null`）時，一律當 remote，不再看 `BaseURL`，並寫一筆 `privacy_override` 稽核。稽核不記原值。
- `ProviderConfig` 的 JSON 解碼裡，大小寫不同的 `privacy`、`baseUrl`、`headers` 鍵（`Privacy`、`BASEURL`…）不論值是什麼，都當無法辨識的 privacy（remote）。解碼出錯時 `Privacy` 也是無法辨識。
- 沒明填 `Privacy` 時：`BaseURL` 是 loopback 且沒有自訂 `Headers` 才是 local，其他一律 remote。
- local provider 沒有被注入 Transport 時（公開 API 都屬於這種），撥號時在 `net.Dialer.Control` 檢查實際要連的 IP：DNS 解析之後、connect 之前，不是 loopback 就回 `ErrNotLoopback`，連線不建立。位址解析不了也拒絕。`localhost` 被解析成外部位址時，同樣被這一關擋下。
- remote provider 不套用這個撥號限制。

## 原因

`Privacy: local` 的意思是「這個模型在本機，資料不出機器」，所以送給它的訊息不遮蔽數字。要是 `BaseURL` 其實指向遠端，卻仍標 local，未遮蔽的資料就會送到外部。為此要在兩個時間點各擋一次：建立時用 URL 字面判斷，擋設定錯誤或惡意設定；撥號時再看實際連到的 IP，擋 `localhost` 被劫持成外部位址、或判斷與連線兩次 DNS 解析不一致（DNS rebinding）。撥號檢查掛在 `Dialer.Control`，不能改成「先查 DNS 再撥」，因為查到的與實際撥的可能不同。

loopback 判斷寧可嚴。Go 的 resolver 會把 `127.1` 這類寫法當網域送去 DNS，被劫持就把 local 資料送出去。嚴格的結果只會多遮、不會少遮。明填的值無法辨識時往安全那邊當 remote，打錯字不會悄悄降成不遮蔽。
