// Package repository 是對外 I/O 層:實作 usecase 定義的 port(LLM provider、HTTP client)、SSE 解析、帶重試的 HTTP。
package repository

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// ProviderConfig 是 provider 設定;Privacy 用指標區分「沒填」與「填了」,明填 null 用 PrivacyNull。
type ProviderConfig struct {
	Name    string
	API     string // anthropic-messages | openai-completions | google-generative-ai
	Model   string
	BaseURL string `json:"baseUrl"` // 鍵名用 baseUrl:UnmarshalJSON 把大小寫變體當無法辨識(裁決 G23),Marshal 要寫得回來
	APIKey  string
	Headers map[string]string `json:"headers"`           // 同上
	Privacy *entity.Privacy   `json:"privacy,omitempty"` // 鍵名用 privacy(UnmarshalJSON 只認這個鍵);omitempty:沒填不寫成 null
	// PrivacyNull = 明填 null(privacy: null):當無法辨識的值 → remote + privacy_override,優先於 Privacy(Ruling G4)。
	// encoding/json 把 "privacy": null 解碼成 nil(= 沒填);UnmarshalJSON 補上這個欄位(Ruling G14)。
	PrivacyNull   bool
	ContextWindow int // 0 = 預設
	MaxTokens     int // 0 = 預設
	Compat        Compat
}

// RedactKey 先把設定的 API key 本身換掉(它不一定符合任何遮蔽規則,例如自架服務的 key),再照規則遮。
// provider 錯誤訊息用,要在截斷前呼叫。key 短於 8 字不換:那不是可信的秘密(例如 "ollama"),換掉只會把錯誤訊息弄壞(審查 m8)。
func RedactKey(s, key string) string {
	if len(key) >= 8 {
		s = strings.ReplaceAll(s, key, "[REDACTED:api-key]")
	}
	return entity.Redact(s)
}

// UnmarshalJSON:privacy 以外的欄位照 encoding/json 預設解碼(欄位名不分大小寫,camelCase 鍵照樣對得上)。
// privacy 只看剛好叫 "privacy" 的鍵(Ruling G16;重複鍵最後一個生效):
// null → PrivacyNull;字串 → Privacy;其他 JSON 型別 → 無法辨識(remote + privacy_override)。
// 大小寫不同的 privacy、baseUrl、headers 鍵(Privacy、BASEURL、Headers…)不管值是什麼一律當無法辨識(不是忽略),
// 但不能讓 encoding/json 的不分大小寫比對拿它的值把 privacy 降成 local(G16、G23)。
// 沒有任何 privacy 鍵時 Privacy、PrivacyNull 保持原值(encoding/json 對沒出現的鍵的慣例)。
// 回 error 時 Privacy 一律是無法辨識(remote)。不支援 Decoder.DisallowUnknownFields:這裡自己呼叫 json.Unmarshal,
// 未知欄位照 encoding/json 預設忽略。
func (c *ProviderConfig) UnmarshalJSON(b []byte) error {
	type plain ProviderConfig // 沒有 UnmarshalJSON,避免遞迴
	aux := struct {
		*plain
		Privacy json.RawMessage `json:"privacy"` // 深度較淺,蓋過 plain.Privacy:所有大小寫的 privacy 鍵都進這裡,不會寫到 c.Privacy、也不會型別錯誤
	}{plain: (*plain)(c)}
	unrecognized := func() { // 空字串 = 無法辨識(ResolvePrivacy 當 remote 並寫稽核);Marshal 後再解碼結果不變
		u := entity.Privacy("")
		c.Privacy = &u
	}
	// 解碼失敗時其他欄位可能已寫了一半(例如 loopback 的 baseUrl):privacy 先設成無法辨識再回 error,
	// 呼叫端忽略 error 也只會得到 remote(審查 m9)
	if err := json.Unmarshal(b, &aux); err != nil {
		unrecognized()
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		unrecognized()
		return err
	}
	if raw, ok := keys["privacy"]; ok {
		var s *string
		if json.Unmarshal(raw, &s) != nil {
			unrecognized() // 數字、布林、物件、陣列
		} else if s == nil {
			c.Privacy, c.PrivacyNull = nil, true
		} else {
			p := entity.Privacy(*s)
			c.Privacy = &p
		}
	}
	// 判斷隱私只看 privacy、baseUrl、headers,而且只認剛好這樣拼的鍵。大小寫變體會讓 encoding/json 換掉這三個鍵的值
	// (例如 "Headers":null 清空 header、"BASEURL" 換成 loopback),可能把 remote 降成 local → 一律當無法辨識(G16、G23)
	for k := range keys {
		for _, exact := range []string{"privacy", "baseUrl", "headers"} {
			if k != exact && strings.EqualFold(k, exact) { // 與 encoding/json 的欄位名比對規則相同
				unrecognized()
			}
		}
	}
	return nil
}

// Compat 是 OpenAI 相容 provider 的相容選項。
type Compat struct {
	AppleFM            bool
	SupportsStrictMode bool
	MaxTokensField     string // "" = "max_tokens";或 "max_completion_tokens"
}

const (
	DefaultContextWindow = 200000
	DefaultMaxTokens     = 4096
)

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "" // 壞 URL:host 留空
	}
	return u.Hostname()
}

// ResolveProviderPrivacy 解析隱私等級;明填 local 但 baseURL 不是 loopback、或明填了無法辨識的值(當 remote)時寫 privacy_override 稽核。
// 第二個回傳值是 entity.ResolvePrivacy 的 override(明填 local 但 baseURL 不是 loopback;無法辨識的值不算),給 CreateProvider 回錯用。
func ResolveProviderPrivacy(cfg ProviderConfig, audit entity.AuditSink) (entity.Privacy, bool) {
	if audit == nil {
		audit = entity.NullAudit()
	}
	if cfg.PrivacyNull { // 明填 null 當成無法辨識的值(cfg 是複本,不改呼叫端)
		null := entity.Privacy("")
		cfg.Privacy = &null
	}
	privacy, override := entity.ResolvePrivacy(cfg.BaseURL, cfg.Privacy, cfg.Headers)
	if override {
		audit.Write(entity.AuditEvent{Kind: "privacy_override", Host: hostOf(cfg.BaseURL), Reason: "provider " + cfg.Name + " 明填 local 但 baseUrl 不是 loopback"})
	} else if cfg.Privacy != nil && *cfg.Privacy != entity.Local && *cfg.Privacy != entity.Remote {
		// 原值不寫進稽核(不是每個 sink 都會遮蔽)
		audit.Write(entity.AuditEvent{Kind: "privacy_override", Host: hostOf(cfg.BaseURL), Reason: "provider " + cfg.Name + " 的 privacy 值無法辨識,當 remote"})
	}
	return privacy, override
}

// createProvider 工廠不放這裡:repository 被 providers/openai 匯入,這裡再匯入 openai 會形成 import cycle
// (Go 不允許循環 import)。工廠在 providers.CreateProvider。
