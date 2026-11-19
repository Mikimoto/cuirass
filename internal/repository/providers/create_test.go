//go:build test

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

// 本檔是 package providers(內部測試);tests/internal/repository/providers/providers_test.go 是 package providers_test,兩者分開編譯。

type recAudit struct{ ev []entity.AuditEvent }

func (r *recAudit) Write(e entity.AuditEvent) bool { r.ev = append(r.ev, e); return true }

var apis = []string{"anthropic-messages", "openai-completions", "google-generative-ai"}

func cfg(api, baseURL string) repository.ProviderConfig {
	return repository.ProviderConfig{Name: "p", API: api, Model: "m", BaseURL: baseURL, APIKey: "k"}
}

func mustCreate(t *testing.T, c repository.ProviderConfig, d CreateDeps) usecase.LLMProvider {
	t.Helper()
	p, err := CreateProvider(c, d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateProvider(t *testing.T) {
	for _, api := range apis {
		t.Run(api+":名稱、模型、隱私、預設值", func(t *testing.T) {
			p := mustCreate(t, cfg(api, "https://api.example.com"), CreateDeps{})
			if p.Name() != "p" || p.Model() != "m" || p.Privacy() != entity.Remote || p.ContextWindow() != 200000 || p.MaxTokens() != 4096 {
				t.Fatal(p.Name(), p.Model(), p.Privacy(), p.ContextWindow(), p.MaxTokens())
			}
		})
	}
	t.Run("loopback 預設 local", func(t *testing.T) {
		if p := mustCreate(t, cfg("openai-completions", "http://localhost:1234"), CreateDeps{}); p.Privacy() != entity.Local {
			t.Fatal(p.Privacy())
		}
	})
}

// 明填 local + 非 loopback → CreateProvider 回錯(不照設定當 local);稽核照寫,恰一筆。
// loopback 明填 local 照常;無法辨識的值照舊 remote + 一筆 privacy_override。
func TestCreateProviderLocalOverrideErrors(t *testing.T) {
	l, bogus := entity.Local, entity.Privacy("bogus")
	for _, api := range apis {
		a := &recAudit{}
		c := cfg(api, "https://api.example.com")
		c.Privacy = &l
		p, err := CreateProvider(c, CreateDeps{Audit: a})
		if err == nil || p != nil || !strings.Contains(err.Error(), "provider p") || !strings.Contains(err.Error(), "privacy: local 但 baseUrl 不是 loopback") {
			t.Errorf("%s:%v %v", api, p, err)
		}
		if len(a.ev) != 1 || a.ev[0].Kind != "privacy_override" || a.ev[0].Host != "api.example.com" {
			t.Errorf("%s:%#v", api, a.ev)
		}

		a = &recAudit{}
		c = cfg(api, "http://127.0.0.1:11434")
		c.Privacy = &l
		if p := mustCreate(t, c, CreateDeps{Audit: a}); p.Privacy() != entity.Local || len(a.ev) != 0 {
			t.Errorf("%s:loopback %s %#v", api, p.Privacy(), a.ev)
		}

		a = &recAudit{}
		c = cfg(api, "http://127.0.0.1:11434")
		c.Privacy = &bogus
		if p := mustCreate(t, c, CreateDeps{Audit: a}); p.Privacy() != entity.Remote || len(a.ev) != 1 || a.ev[0].Kind != "privacy_override" {
			t.Errorf("%s:無法辨識 %s %#v", api, p.Privacy(), a.ev)
		}
	}
}

// entity.ResolvePrivacy 不動:local + 遠端照舊回 (Local, true),回錯只在 CreateProvider(privacy golden 鎖住 ResolvePrivacy 的行為)。
func TestResolvePrivacyUnchanged(t *testing.T) {
	l := entity.Local
	if p, override := entity.ResolvePrivacy("https://api.example.com", &l, nil); p != entity.Local || !override {
		t.Fatal(p, override)
	}
}

// CreateProvider 複製 Headers:建立後呼叫端改 map,送出的 header 不變。
func TestCreateProviderCopiesHeaders(t *testing.T) {
	for _, api := range apis {
		var got http.Header
		rt := rtFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Clone()
			return nil, errors.New("stop")
		})
		c := cfg(api, "https://api.example.com")
		h := map[string]string{"x-a": "1"}
		c.Headers = h
		p := mustCreate(t, c, CreateDeps{HTTP: &http.Client{Transport: rt}})
		h["x-a"] = "2"
		h["x-b"] = "3"
		for range p.Stream(context.Background(), usecase.LLMRequest{}) {
		}
		if got == nil || got.Get("x-a") != "1" || got.Get("x-b") != "" {
			t.Errorf("%s:送出的 header %v", api, got)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Ruling G4:明填 null(PrivacyNull)即使 baseUrl 是 loopback 也要 remote,並恰寫一筆 privacy_override。
func TestCreateProviderPrivacyNullIsRemote(t *testing.T) {
	for _, api := range apis {
		a := &recAudit{}
		c := cfg(api, "http://127.0.0.1:1234")
		c.PrivacyNull = true
		p := mustCreate(t, c, CreateDeps{Audit: a})
		if p.Privacy() != entity.Remote {
			t.Error(api, p.Privacy())
		}
		if len(a.ev) != 1 || a.ev[0].Kind != "privacy_override" || a.ev[0].Host != "127.0.0.1" {
			t.Errorf("%s:%#v", api, a.ev)
		}
	}
	// 對照:同一個 loopback 設定沒明填 = local,證明上面不是 baseUrl 造成的 remote
	if p := mustCreate(t, cfg("openai-completions", "http://127.0.0.1:1234"), CreateDeps{}); p.Privacy() != entity.Local {
		t.Fatal(p.Privacy())
	}
}

func TestCreateProviderUnknownAPI(t *testing.T) {
	p, err := CreateProvider(repository.ProviderConfig{Name: "p", API: "x", Model: "m", BaseURL: "http://a", APIKey: "k"}, CreateDeps{})
	if err == nil || p != nil {
		t.Fatal(p, err)
	}
	if err.Error() != `provider p:不支援的 api "x"` {
		t.Fatal(err)
	}
}

// Ruling G14/G16:從 JSON 解碼的設定,明填 null、大小寫不同的 privacy 鍵、非字串值走到 CreateProvider 都是 remote + 恰一筆 privacy_override;沒填 + loopback = local。
func hostOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Hostname()
}

func TestCreateProviderFromJSONPrivacyNull(t *testing.T) {
	for in, want := range map[string]entity.Privacy{
		`{"name":"p","api":"openai-completions","model":"m","apiKey":"k","privacy":null,"baseUrl":"http://127.0.0.1:1"}`: entity.Remote,
		`{"name":"p","api":"openai-completions","model":"m","apiKey":"k","baseUrl":"http://127.0.0.1:1"}`:                entity.Local,
		// 審查 C1:大小寫不同的 privacy 鍵 → remote + 一筆稽核(Ruling G16)
		`{"api":"openai-completions","Privacy":"local","baseUrl":"http://127.0.0.1:1","headers":{"a":"b"}}`: entity.Remote,
		`{"api":"openai-completions","privacy":"remote","Privacy":"local","baseUrl":"http://127.0.0.1:1"}`:  entity.Remote,
		`{"api":"openai-completions","PRIVACY":"local","baseUrl":"https://api.example.com"}`:                entity.Remote,
		`{"api":"openai-completions","privacy":1,"baseUrl":"http://127.0.0.1:1"}`:                           entity.Remote,
	} {
		var c repository.ProviderConfig
		if err := json.Unmarshal([]byte(in), &c); err != nil {
			t.Fatal(err)
		}
		a := &recAudit{}
		p := mustCreate(t, c, CreateDeps{Audit: a})
		if p.Privacy() != want {
			t.Errorf("%s:%s", in, p.Privacy())
		}
		wantEv := 0
		if want == entity.Remote {
			wantEv = 1
		}
		if len(a.ev) != wantEv || wantEv == 1 && (a.ev[0].Kind != "privacy_override" || a.ev[0].Host != hostOf(c.BaseURL)) {
			t.Errorf("%s:%#v", in, a.ev)
		}
	}
}
