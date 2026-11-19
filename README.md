# cuirass

讓 AI agent 能動手做事、又不會闖禍的 Go framework。

安全靠開發者自己加,忘了加或加錯就出事。cuirass 把防護做成 agent 迴圈裡固定的步驟(對照 OWASP LLM Top 10 2025),核心防護沒有開關可以關。你只寫工具與規則,其他交給 framework。

**給開發者**

- 少寫很多程式:agent 迴圈、工具呼叫、串流都寫好了。
- 換模型只改 provider 設定,工具與規則不用動:Anthropic、OpenAI、Google 的 API,或用 Ollama 跑本機模型(OpenAI 相容介面)。
- 設定錯了當場知道:工具 schema 沒封閉、工具白名單列了沒註冊的工具,`New` 直接回 error,不會等到上線才出事。
- 依賴很少:只有標準函式庫與 `golang.org/x/text`。

**給你的使用者**

- agent 只做你允許的事:只能呼叫開放的工具,參數不合格式就不執行,呼叫次數有上限。
- 連網有白名單:工具用 framework 給的 `tc.HTTP` 連線,只連得到白名單上的 https 主機,不跟轉址。工具自己用 `net/http` 目前擋不到,要等沙箱。
- 機密先遮再送:工具結果先遮掉機密才給模型看;回答照抄系統提示詞時,最後那則訊息整則攔下;送去雲端模型前,可以先遮掉敏感數字。
- 數字可以核對:加上 `NumberVerifier`,回答裡對不到工具資料的數字會標出來。
- 出事查得到:設了稽核,工具呼叫、被規則或上限擋下、連線被擋、輸出被攔,都會寫一筆。

還沒做的:沙箱(讀寫檔案、執行指令的工具現在一律拒絕註冊)與 session 存檔。細節看下方「七層對照」。想直接跑看看,跳到下方「最小範例」。

## 看一個例子:數字核對

模型常把數字算錯或講錯。cuirass 會核對最終回答裡的數字:對不到工具資料的,先請模型複核,複核不認可就在那一行標上 `⚠未核對`,對得上的不動。下面用假的模型(stub,照劇本回答、不連網路)示範:它查到匯率 32.15,卻把 100 美元算成 3150 新台幣:

```go
script := []cuirass.StubStep{
	{ToolCall: &cuirass.StubCall{Name: "get_rate", Args: map[string]any{}}},
	{Text: "匯率 32.15\n100 美元約 3150 新台幣"},
	{Text: "[]"}, // 複核回覆:這裡寫死不認可(真的模型會依工具資料判斷)
}
provider := cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: &script})

getRate := cuirass.ToolDef{
	Name:        "get_rate",
	Description: "查美元兌新台幣匯率",
	Parameters: cuirass.Schema{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{},
	},
	Effects: cuirass.EffectsNone,
	Execute: func(_ context.Context, _ map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
		rate := map[string]any{"usd_twd": 32.15}
		// 數字核對只認 Details 裡的數字
		return cuirass.ToolOutput{Content: rate, Details: rate}, nil
	},
}

agent, err := cuirass.New(cuirass.AgentOptions{
	Providers: []*cuirass.Provider{provider},
	System:    func() string { return "你是匯率助理。" },
	Tools:     []cuirass.ToolDef{getRate},
	Verifiers: []cuirass.Verifier{cuirass.NumberVerifier(0)},
})
if err != nil {
	panic(err)
}
defer agent.Dispose()
// 印出事件的寫法同下方「最小範例」
agent.Prompt(context.Background(), "100 美元換多少新台幣?")
```

輸出:

```
工具: get_rate 錯誤: false
回答: 匯率 32.15
100 美元約 3150 新台幣 ⚠未核對

有 1 個數字無法對到工具資料
結束: done
```

完整程式在 `example/example_test.go` 的 `ExampleNumberVerifier`,`go test` 會核對這段輸出。

## 安裝

需要 Go 1.27 以上。依賴只有標準函式庫與 `golang.org/x/text`。

```sh
go get github.com/Mikimoto/cuirass@vX.Y.Z
```

只要 import 根套件 `github.com/Mikimoto/cuirass` 一個就夠。其他程式碼都在 `internal/`,外部 import 不到。

## 開發

用 [mise](https://mise.jdx.dev/) 跑:

| 指令 | 做什麼 |
|---|---|
| `mise run build` | 編譯 + `go vet` |
| `mise run test` | gofmt 檢查、`-race` 測試、不開 race 的覆蓋率測試、覆蓋率下限 |
| `mise run vuln` | `govulncheck` 弱點掃描(需要網路) |
| `mise run release <vX.Y.Z>` | 發版前檢查;預設只印出要做的事,加 `--yes` 才建 tag 與 release |
| `mise run test:release` | 在暫存 repo 驗證 release 的檢查 |

測試檔都帶 `test` build tag,直接跑要用 `go test -tags test ./...`;gopls 要設 `"buildFlags": ["-tags=test"]`。白箱測試留在 `internal/` 各套件目錄,其餘測試都放 `tests/`:

| 目錄 | 放什麼 |
|---|---|
| `tests/` | 使用者角度的測試,只能用公開 API(`layers_test.go` 會擋 import internal);掃原始碼的守門測試(分層、對外連線、公開 API 形狀) |
| `tests/internal/<路徑>` | 需要 internal 的外部測試,路徑對應原始碼;`tests/internal/root` 是根套件,透過 `testing.go`(只在 test tag 建置存在)拿未匯出成員 |
| `example/` | 教學範例,也是測試:`ExampleNew` 與自己寫一個 coding agent(`example/codingagent`) |

### 行為契約(IMP)

對外行為先寫成契約再實作。`docs/contract/behaviors.md` 是行為清單,每個行為對到一份 `docs/contract/<id>.md`,裡面寫義務、原因,以及守住它的測試名稱。改對外行為時先改契約,新測試也要加進契約的 `tests`。檢查每個行為都有契約、契約列的測試都存在:

```sh
python3 scripts/imp-check.py --root .
```

## 最小範例

用 stub provider,不碰網路。這段就是 `example/example_test.go` 的 `ExampleNew`,`go test` 會核對輸出:

```go
script := []cuirass.StubStep{
	{ToolCall: &cuirass.StubCall{Name: "get_price", Args: map[string]any{"symbol": "2330"}}},
	{Text: "台積電收盤價 1000 元"},
}
provider := cuirass.NewStubProvider(cuirass.StubOptions{Name: "stub", Script: &script})

getPrice := cuirass.ToolDef{
	Name:        "get_price",
	Description: "查股價",
	Parameters: cuirass.Schema{
		"type":                 "object",
		"additionalProperties": false, // 必填:參數只收列出的欄位
		"properties":           map[string]any{"symbol": map[string]any{"type": "string"}},
		"required":             []string{"symbol"},
	},
	Effects: cuirass.EffectsNone,
	Execute: func(_ context.Context, args map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
		return cuirass.ToolOutput{Content: map[string]any{"symbol": args["symbol"], "close": 1000}}, nil
	},
}

agent, err := cuirass.New(cuirass.AgentOptions{
	Providers: []*cuirass.Provider{provider},
	System:    func() string { return "你是台股助理。" },
	Tools:     []cuirass.ToolDef{getPrice},
})
if err != nil {
	panic(err)
}
defer agent.Dispose()

agent.On(func(e cuirass.Event) {
	switch e.Type {
	case cuirass.EventToolEnd:
		fmt.Println("工具:", e.Name, "錯誤:", e.IsError)
	case cuirass.EventMessage:
		if e.Final {
			fmt.Println("回答:", e.Message.Content[0].Text)
		}
	case cuirass.EventTurnEnd:
		fmt.Println("結束:", e.Reason)
	}
})
if err := agent.Prompt(context.Background(), "台積電多少?"); err != nil {
	panic(err)
}
fmt.Println("額度已用:", agent.Budget().Session)
// 工具: get_price 錯誤: false
// 回答: 台積電收盤價 1000 元
// 結束: done
// 額度已用: 1
```

接真的模型(其他廠商與本機模型見下一節「接真的模型」):

```go
audit := cuirass.NewFileAudit(".agent-audit", func(msg string) { log.Println(msg) })
claude, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "claude", API: "anthropic-messages", Model: "claude-sonnet-4-5",
	BaseURL: "https://api.anthropic.com", APIKey: os.Getenv("ANTHROPIC_API_KEY"),
}, cuirass.CreateDeps{Audit: audit}) // 傳 Audit:privacy override 才會寫進稽核
if err != nil {
	return err
}

agent, err := cuirass.New(cuirass.AgentOptions{
	Providers: []*cuirass.Provider{claude},
	System:    func() string { return "你是台股助理。" },
	Tools: []cuirass.ToolDef{{
		Name:        "quote",
		Description: "查股價",
		Parameters: cuirass.Schema{ // 必須封閉
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"code": map[string]any{"type": "string"}},
			"required":   []string{"code"},
		},
		Effects:   cuirass.EffectsNetwork,
		AuditArgs: []string{"code"},
		// 一定要用 tc.HTTP:只有它受主機白名單管
		Execute: func(ctx context.Context, args map[string]any, tc cuirass.ToolCtx) (cuirass.ToolOutput, error) {
			r, err := tc.HTTP(ctx, "https://api.example.com/quote/"+url.PathEscape(args["code"].(string)), cuirass.HTTPOptions{})
			if err != nil {
				return cuirass.ToolOutput{}, err
			}
			return cuirass.ToolOutput{Content: r.Body}, nil
		},
	}},
	Policy: cuirass.PolicyOptions{Hosts: []string{"api.example.com"}},
	Audit:  audit,
})
if err != nil {
	return err
}
defer agent.Dispose()

agent.On(func(e cuirass.Event) {
	if e.Type == cuirass.EventMessage && e.Final {
		fmt.Println(e.Message.Content)
	}
})
err = agent.Prompt(ctx, "2330 現在多少?")
```

- `Prompt` 會排隊:同時只跑一輪,其餘依呼叫順序等待,阻塞到自己那輪結束。只有設定或 hook 出錯、或排隊中被取消 / `Dispose` 才回 `error`;這輪怎麼結束看 `turn_end` 事件的 `Reason`(done、aborted、timeout、cap、error、blocked)。
- `Abort()` 只中止目前這輪,排隊中的照跑,之後可以繼續用。`Prompt` 的 `ctx` 取消也只中止那一輪。還在排隊(沒輪到)時 `ctx` 被取消,那個 `Prompt` 直接回 `ctx.Err()`,不跑也不留紀錄;排隊中 `Dispose()` 則回「agent 已停用」。
- `SetProvider()` 在輪與輪之間才生效:輪中呼叫時,這輪與之前已排隊的輪仍用舊的,`Provider()` 也回舊的。
- `Provider()` 只回 `Name`、`Model`、`Privacy`、`ContextWindow`、`MaxTokens`,不交出 provider 本身。`Messages()` 回深複本。

## 接真的模型

`CreateProvider` 用 `API` 決定走哪一種協定。支援三種,涵蓋下面五類模型:

| 模型 | `API` | `BaseURL` | 金鑰 |
|---|---|---|---|
| Anthropic Claude | `anthropic-messages` | `https://api.anthropic.com` | `x-api-key` header |
| OpenAI | `openai-completions` | `https://api.openai.com/v1` | `Authorization: Bearer` |
| Google Gemini | `google-generative-ai` | `https://generativelanguage.googleapis.com` | `x-goog-api-key` header |
| Ollama(本機) | `openai-completions` | `http://localhost:11434/v1` | 不需要 |
| Apple Foundation Models(`fm serve`) | `openai-completions` + `Compat.AppleFM` | `http://127.0.0.1:1976/v1` | 不需要 |

`BaseURL` 只寫到上表那一層,framework 會自己接路徑:Anthropic 接 `/v1/messages`,OpenAI 相容接 `/chat/completions`,Gemini 接 `/v1beta/models/<Model>:streamGenerateContent`。所以 OpenAI 相容的 `BaseURL` 要包含 `/v1`。三種都走串流。

```go
audit := cuirass.NewFileAudit(".agent-audit", func(msg string) { log.Println(msg) })
deps := cuirass.CreateDeps{Audit: audit}

// Anthropic Claude
claude, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "claude", API: "anthropic-messages", Model: "claude-sonnet-4-5",
	BaseURL: "https://api.anthropic.com", APIKey: os.Getenv("ANTHROPIC_API_KEY"),
}, deps)

// OpenAI(其他 OpenAI 相容的雲端服務也一樣,換 BaseURL 就好)
gpt, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "gpt", API: "openai-completions", Model: "gpt-4.1",
	BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY"),
	// 新模型不收 max_tokens 時改用 max_completion_tokens
	Compat: cuirass.Compat{MaxTokensField: "max_completion_tokens"},
}, deps)

// Google Gemini
gemini, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "gemini", API: "google-generative-ai", Model: "gemini-2.5-flash",
	BaseURL: "https://generativelanguage.googleapis.com", APIKey: os.Getenv("GEMINI_API_KEY"),
}, deps)

// Ollama:先 `ollama pull llama3.1`,`ollama serve` 預設聽 11434
local := cuirass.Local
ollama, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "ollama", API: "openai-completions", Model: "llama3.1",
	BaseURL: "http://localhost:11434/v1",
	Privacy: &local, // 明填 local;不是 loopback 位址時 CreateProvider 會回錯
}, deps)

// Apple Foundation Models:先跑 `fm serve`,預設聽 127.0.0.1:1976(`--port` 可改)
// Model 填 "system"(在本機跑)
fm, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "fm", API: "openai-completions", Model: "system",
	BaseURL: "http://127.0.0.1:1976/v1", Privacy: &local,
	Compat: cuirass.Compat{AppleFM: true}, // 送出前把請求轉成 fm serve 收得下的格式
}, deps)

// Model 填 "pcc" 時跑在 Apple 的 Private Cloud Compute,資料會離開本機。
// 位址雖然是 loopback,要明填 remote,送出前才會遮蔽
remote := cuirass.Remote
pcc, err := cuirass.CreateProvider(cuirass.ProviderConfig{
	Name: "pcc", API: "openai-completions", Model: "pcc",
	BaseURL: "http://127.0.0.1:1976/v1", Privacy: &remote,
	Compat: cuirass.Compat{AppleFM: true},
}, deps)
```

每個 `err` 都要檢查,上面為了短省略了。

幾個要注意的地方:

- **金鑰不要寫死**:用環境變數或 secret manager。provider 的錯誤訊息會先把這把金鑰遮掉才回傳。
- **本機模型是 `local`,雲端是 `remote`**:沒填 `Privacy` 時,loopback 位址(localhost、127.x、::1)而且沒有自訂 `Headers` 的算 `local`,其他一律 `remote`。有設 `Masking` 時,`local` 輪的敏感數字送去 `remote` provider 之前會遮掉。明填 `Privacy: &local` 可以多一道保險:位址不是 loopback 就直接回錯,撥號時也只准連 loopback IP。反過來,本機轉發到雲端的服務(例如 `fm serve` 的 `pcc` 模型)位址是 loopback,要明填 `Privacy: &remote`。
- **同時接好幾家**:全部放進 `Providers`,`InitialProvider` 指定一開始用哪個(空字串 = 第一個),之後用 `agent.SetProvider("<Name>")` 切換,在輪與輪之間生效;名字找不到時回 `false`。

```go
agent, err := cuirass.New(cuirass.AgentOptions{
	Providers:       []*cuirass.Provider{ollama, claude},
	InitialProvider: "ollama", // 平常用本機模型
	System:          func() string { return "你是天氣助理。" },
})
// 需要比較強的模型時
agent.SetProvider("claude")
```

## 自定義工具

工具是 agent 能做的事。每個工具是一個 `cuirass.ToolDef`,放進 `AgentOptions.Tools`:

| 欄位 | 說明 |
|---|---|
| `Name` | 模型呼叫時用的名字,符合 `^[a-zA-Z0-9_-]{1,64}$`,不能重複 |
| `Description` | 告訴模型這個工具做什麼、什麼時候用 |
| `Parameters` | 參數的 JSON Schema。根層必須是 `"type": "object"` 加 `"additionalProperties": false` |
| `Effects` | 工具會碰到什麼:`EffectsNone`(只在記憶體裡算)或 `EffectsNetwork`(會連網)。讀寫檔案、執行指令的工具(`fs`/`exec`)要等沙箱完成,現在註冊會失敗 |
| `Execute` | 真正做事的函式 |
| `AuditArgs` | 哪些參數可以寫進稽核紀錄;沒列的不記 |

下面是一個查天氣的工具,也示範怎麼用規則和次數上限管住它:

```go
getWeather := cuirass.ToolDef{
	Name:        "get_weather",
	Description: "查城市目前的氣溫與天氣",
	Parameters: cuirass.Schema{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"city": map[string]any{"type": "string", "minLength": 1, "maxLength": 40},
		},
		"required": []string{"city"},
	},
	Effects:   cuirass.EffectsNetwork,
	AuditArgs: []string{"city"},
	Execute: func(ctx context.Context, args map[string]any, tc cuirass.ToolCtx) (cuirass.ToolOutput, error) {
		city := args["city"].(string) // schema 已檢查過型別,這裡可以直接轉
		// 連網一律用 tc.HTTP:只連得到 Policy.Hosts 列的 https 主機
		r, err := tc.HTTP(ctx, "https://api.example.com/weather?city="+url.QueryEscape(city), cuirass.HTTPOptions{})
		if err != nil {
			return cuirass.ToolOutput{}, err
		}
		if r.Status != 200 {
			// 模型能自己修正的錯誤(例如城市名打錯)回 IsError,讓它看到原因再試
			return cuirass.ToolOutput{Content: "查不到這個城市", IsError: true}, nil
		}
		var w struct {
			TempC float64 `json:"temp_c"`
			Desc  string  `json:"desc"`
		}
		if err := json.Unmarshal([]byte(r.Body), &w); err != nil {
			return cuirass.ToolOutput{}, err
		}
		return cuirass.ToolOutput{
			Content: w,                                 // 給模型看的內容
			Details: map[string]any{"temp_c": w.TempC}, // 數字核對只認這裡的數字
		}, nil
	},
}

perTurn := 5
agent, err := cuirass.New(cuirass.AgentOptions{
	Providers: []*cuirass.Provider{claude},
	System:    func() string { return "你是天氣助理。" },
	Tools:     []cuirass.ToolDef{getWeather},
	Verifiers: []cuirass.Verifier{cuirass.NumberVerifier(0)},
	Policy: cuirass.PolicyOptions{
		Tools: []string{"get_weather"},     // 開放給模型的工具;nil = 全部已註冊的
		Hosts: []string{"api.example.com"}, // tc.HTTP 能連的主機
		Limits: cuirass.LimitsOptions{
			PerTurn: &perTurn,                         // 每輪最多呼叫 5 次工具
			PerTool: map[string]int{"get_weather": 3}, // get_weather 每輪最多 3 次
		},
		Rules: []cuirass.ToolRule{
			// 第一個回 block=true 的規則擋下;規則 panic 也算擋下
			func(name string, args map[string]any, _ cuirass.RuleCtx) (string, bool) {
				if name == "get_weather" && args["city"] == "測試" {
					return "不查測試城市", true
				}
				return "", false
			},
		},
	},
	Audit: audit,
})
```

framework 在工具前後替你做的事:

- **呼叫前**:工具不在 `Policy.Tools`、參數不符 schema、被規則擋下,工具都不會執行,模型看到的是被擋的原因。超過次數上限時也不會執行,而且這輪直接以 `cap` 結束。
- **呼叫後**:結果先遮掉機密、再依 `ToolResultBytes` 截斷,最後包成不可信資料才交給模型。所以工具回傳的內容就算夾帶「忽略前面的指示」這類文字,模型也知道那只是資料。
- **出錯時**:`Execute` 回 Go `error` 或 panic,這次呼叫算失敗,模型看到遮蔽過的錯誤訊息。模型能自己修正的錯誤,建議回 `IsError: true` 並寫清楚原因。

寫工具時的原則:

- `Parameters` 只用支援的關鍵字:`type`、`properties`、`required`、`additionalProperties`、`items`、`enum`、`const`、`anyOf`、`minimum`、`maximum`、`exclusiveMinimum`、`minLength`、`maxLength`、`minItems`、`maxItems`、`pattern`、`description`、`title`、`default`。用了其他的(例如 `$ref`、`oneOf`、`format`),`New` 會回 error。
- 參數盡量收窄:能用 `enum` 就不要開放任意字串,字串加 `maxLength`,數字加 `minimum`/`maximum`。
- `Effects` 照實宣告。會連網的工具不要標成 `EffectsNone`,讀寫檔案的工具不要為了過關改標別的。
- 不要在工具裡自己用 `net/http` 連網,那樣白名單擋不到。

完整的多工具範例看 `example/codingagent`。

## 併發與時序契約(寫 hook 前必讀)

**下面這些 hook 必須能併發呼叫**:

- `AgentOptions.Audit`(`AuditSink.Write`)
- `Masking.Snapshot`
- `AgentOptions.Rewrite`(`ContentRewrite`)
- `Masking.HideResult`(保守起見):它也交給複核 `Ask` 的送出管線;目前複核請求不含工具結果,實際不會在那裡被呼叫

`NewFileAudit` 已上鎖。自己寫的實作要自己上鎖,或不碰共用狀態。

**中止或逾時之後,還有被放棄的 goroutine 在跑**。`Abort()`、`Prompt` 的 `ctx` 取消、`Dispose()` 或本輪逾時時,`Prompt` 馬上返回,不等不理會 `ctx` 的工作。這時:

- 工具的 `Execute`、驗證器(`Verifiers`)與它呼叫的 `Ask`,如果還沒回來,就成了被放棄的 goroutine,會一直跑到它們自己返回。
- 它們在 `Prompt` 返回**之後**(甚至下一輪進行中)仍可能呼叫上面那些 hook:例如 `Ask` 會呼叫 `Masking.Snapshot` 與 `Rewrite`,被放棄的驗證器可能觸發稽核寫入。
- 它們的結果一律丟掉,寫不進對話紀錄、遮蔽集合與數字帳本。`Prompt` 返回之後,被放棄的 `Ask` 也不會再扣額度(還沒返回時扣到的那次算在這一輪)。
- 所以工具與驗證器要尊重 `ctx`:取消後盡快返回,不要在取消後還做有副作用的事。

其他 hook(`System`、`LeakSources`、`Masking.Collect`、`Policy.Rules`、`Policy.PublicReason`、listener、`TurnTimeout`)只在這輪自己的 goroutine 上同步呼叫,不會併發。它們卡住,這輪就卡住,中止也救不回來。

**listener 與這些 hook 不可以同步呼叫 `Prompt`。** 新的那輪會排在自己這輪後面,兩邊互等,會卡死。要在事件裡接著問,用 goroutine:

```go
agent.On(func(e cuirass.Event) {
	if e.Type == cuirass.EventTurnEnd && e.Reason == "done" {
		go agent.Prompt(ctx, "接著問") // 不要同步呼叫
	}
})
```

萬一同步呼叫了,只有那個 `Prompt` 的 `ctx` 被取消(或逾時)、或另一個 goroutine 呼叫 `Dispose()`,才放得開。

## 額度存檔與還原

```go
st := agent.Budget()          // 存檔:可序列化成 JSON
b, _ := json.Marshal(st)
// …下次啟動…
var restored cuirass.BudgetState
_ = json.Unmarshal(b, &restored) // 壞值不回錯,而是標成用完
agent, err := cuirass.New(cuirass.AgentOptions{ /* … */ Restore: &restored })
```

還原的狀態不可信:任何壞值(負數、非整數、缺欄位、未知欄位、JSON 語法錯而留下零值)都讓**整個 session 的額度用完**,不會悄悄從 0 開始。

## 建立時的檢查

`New` 遇到下面任一情況回 `error`,不會帶著壞設定跑:

- 工具名不符 `^[a-zA-Z0-9_-]{1,64}$`、名稱重複、`effects` 是 `fs`/`exec`(專案 1 不支援)或無法辨識。
- 工具 schema 不是根層 `type: object` 且 `additionalProperties: false`,或用了不支援的關鍵字(fail closed)。
- `Policy.Tools` 含未註冊的工具。`nil` = 全部已註冊工具;空切片 = 一個都不開。
- `Policy.Limits` 有負數或超過 2^53-1。沒填(`nil`)用預設值;`0` 是有效值。
- `Policy.AuditFailure` 不是 `""`、`"halt"`、`"warn"`(打錯字不會悄悄變成 warn)。
- `LeakAllow` 有正規化後短於 20 字的句子(空字串或短句會讓外洩偵測失效)。
- 沒有 provider、provider 是 nil 或名稱重複、`InitialProvider` 找不到、`System` 是 nil。

## 七層對照

| 層 | 做什麼 | OWASP |
|---|---|---|
| L1 Agent Core | 只認註冊的工具,沒有內建危險工具;系統提示詞每輪重設,前面加固定安全前言 | LLM06 |
| L2 授權 gate | 工具白名單、schema 驗證、開發者規則、每輪/每 session 次數上限;預設擋 | LLM06、LLM01、LLM10 |
| L3 工具層 | 封閉 schema;工具結果(含錯誤文字)先遮機密,再包成不可信資料;大小上限 | LLM05、LLM01、LLM02、LLM10 |
| L4 沙箱 | 專案 1 未實作:`Effects` 為 `fs`/`exec` 的工具註冊時直接拒絕 | LLM06 |
| L5 狀態層 | 專案 2(session 存檔)才有 | LLM04 |
| L6 稽核 | 工具呼叫、擋下、出口擋下、逾時、輸出攔截寫 JSON Lines(0600);只記宣告過的參數 | 可究責 |
| L7 執行範圍 | 可用工具、可連主機、上限在專案 1 強制;可寫路徑與使用者同意等專案 3 | LLM06 |

另外:有設 `Masking` 時,送到 remote provider 前,`local` 輪的敏感數字會依 `Masking` 遮掉(沒設就不遮);回答含系統提示詞會整則攔下。

## 注意

- **出口白名單的範圍**:`Policy.Hosts` 只管 `ToolCtx.HTTP`;provider 的請求只送到它設定的 `BaseURL`。工具自己用 `net/http` 等**擋不到**,要等專案 3 的 L4 沙箱才真正強制。工具請一律用 `tc.HTTP`。framework 自己的程式碼由 `TestNetworkSites` 鎖住:只有 `internal/repository/http.go`、`internal/repository/sse.go` 與 `internal/repository/providers/**` 可以連線。
- **保證範圍**:
  - 框架送給非 loopback provider 的請求,一定先經過送出管線;公開 API 沒有能看到或改走請求的管道。
  - 送給 remote 時遮什麼由 `Masking` 決定;沒設就不遮任何內容。`System()` 與 `Ask` 的 system 參數不經遮蔽。
  - 工具對外連線由 `Policy.Hosts` 與 `Rules` 管(工具拿到的參數可能含 local 輪的內容);開發者自己的程式連網,不在保證內。
- **連線一律由框架建**:公開 API 沒有注入 `http.Client`、`Transport` 的地方(`CreateDeps` 只有 `Audit`;`AgentOptions` 沒有 `Transport`;`StubOptions` 沒有看請求的 hook)。框架的所有對外連線(工具的 `tc.HTTP`、三家 provider)都**不讀** `HTTPS_PROXY` 等環境變數,傳輸也是全新建立,不複製 `http.DefaultTransport`:別的套件改過全域的 `DialContext`、`TLSClientConfig` 或整個換掉,都不影響框架。目前不支援 proxy。
- **privacy: local 只給 loopback**:provider 設定明填 `Privacy: local` 但 `BaseURL` 不是 loopback(localhost、127.x、::1)時,`CreateProvider` **回錯**(錯誤訊息含 provider 名稱),並寫一筆 `privacy_override` 稽核。local provider 撥號時還會再檢查一次實際連到的 IP:不是 loopback 就拒絕(例如 `localhost` 被解析成外部位址)。`privacy` 明填了 `"local"`、`"remote"` 以外的值(打錯字、大小寫不對、`null`)一律當 remote,也寫一筆 `privacy_override` 稽核。
- **檔案稽核(`NewFileAudit`)的平台與目錄前提**:開檔用 `O_NOFOLLOW`,`audit.log` 是 symlink 就拒寫。非 unix 平台(Windows)擋不住 symlink,所以**一律拒寫**(`Write` 回 `false`),這些平台上不會有稽核檔。寫入失敗時呼叫 `warn`;`warn` 傳 `nil` 就寫到標準 log(stderr),不會沒聲音。稽核目錄本身是不是 symlink、上層目錄有沒有被換成 symlink **不檢查**:目錄要放在只有執行者可寫的位置,不要放在 `/tmp` 這類共用可寫的地方。
- **輸出的連結與圖片**:每則送出的 `message`(包含出錯、中止的半截回答與帶工具呼叫的中間訊息)都會拿掉非 https 連結;markdown 圖片 `![…](…)` 不論協定一律拿掉,因為畫面自動載入圖片時,網址本身就能把資料帶出去。
- **JSON 鍵值的機密**:工具結果裡鍵名以 key、token、secret、password、passwd、pwd 結尾(不分大小寫)的字串值會被遮(例如 `"apiKey"`、`"DB_PASSWORD"`);`max_tokens`、`keyboard` 這類鍵不遮。數字值不遮。
- **`text_delta` 在輸出檢查之前送出**。畫面要用最後的 `message` 事件(`Final: true`)內容取代串流文字:那則可能已被攔下或改寫(例如換成「已攔下」、拿掉不安全連結、加上未核對標記)。
- **`TurnTimeout`** 回 ≤ 0 代表「用 `Policy.Limits.TurnTimeoutMs`」。

## 授權

Apache-2.0,全文見 `LICENSE`。
