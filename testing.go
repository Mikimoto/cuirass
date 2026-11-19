//go:build test

package cuirass

import "github.com/Mikimoto/cuirass/internal/usecase"

// 給 tests/internal/root 的外部測試用的未匯出成員。只在 test tag 建置時存在,不算公開 API
// (tests/api_test.go、nodatataps_test.go 用不帶 tag 的建置檢查根套件)。

// ToInternal 是 toInternal。
func (o AgentOptions) ToInternal() usecase.AgentOptions { return o.toInternal() }

// WrapProvider 把 internal provider 包成 *Provider(e2e 對照要包斷流 provider)。
func WrapProvider(p usecase.LLMProvider) *Provider { return &Provider{p} }
