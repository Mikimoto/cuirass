// Package providers 放 CreateProvider 工廠。不放在 repository:子套件匯入 repository,反過來會 import cycle。
package providers

import (
	"fmt"
	"maps"
	"net/http"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/repository/providers/anthropic"
	"github.com/Mikimoto/cuirass/internal/repository/providers/google"
	"github.com/Mikimoto/cuirass/internal/repository/providers/openai"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// CreateDeps:HTTP 照 repository.ProviderClient 處理(nil 時框架自建、不讀 proxy 環境變數,local 只准撥 loopback);
// Audit 為 nil = NullAudit()。HTTP 只給 internal 測試換傳輸;根套件的 CreateDeps 沒有這欄。
type CreateDeps struct {
	HTTP  *http.Client
	Audit entity.AuditSink
}

// CreateProvider 依 cfg.API 建 provider。privacy 只在這裡解析一次(privacy_override 至多寫一筆),再傳給各家。
// 明填 local 但 baseUrl 不是 loopback 回錯(不照設定當 local,否則送出前不遮蔽);稽核在回錯前照寫。
// Headers 先複製:建立後呼叫端再改 map 也不影響送出。
func CreateProvider(cfg repository.ProviderConfig, deps CreateDeps) (usecase.LLMProvider, error) {
	cfg.Headers = maps.Clone(cfg.Headers)
	privacy, override := repository.ResolveProviderPrivacy(cfg, deps.Audit)
	if override {
		return nil, fmt.Errorf("provider %s:privacy: local 但 baseUrl 不是 loopback", cfg.Name)
	}
	switch cfg.API {
	case "anthropic-messages":
		return anthropic.New(cfg, anthropic.Deps{HTTP: deps.HTTP, Privacy: privacy}), nil
	case "openai-completions":
		return openai.New(cfg, openai.Deps{HTTP: deps.HTTP, Privacy: privacy}), nil
	case "google-generative-ai":
		return google.New(cfg, google.Deps{HTTP: deps.HTTP, Privacy: privacy}), nil
	default:
		return nil, fmt.Errorf(`provider %s:不支援的 api "%s"`, cfg.Name, cfg.API)
	}
}
