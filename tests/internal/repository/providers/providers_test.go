//go:build test

// Package providers_test 放三家 provider 共同的防護測試(本目錄只有測試檔)。
package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
	"github.com/Mikimoto/cuirass/internal/repository"
	"github.com/Mikimoto/cuirass/internal/repository/providers/anthropic"
	"github.com/Mikimoto/cuirass/internal/repository/providers/google"
	"github.com/Mikimoto/cuirass/internal/repository/providers/openai"
	"github.com/Mikimoto/cuirass/internal/usecase"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func clientFor(status int, body string) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

type provider struct {
	name    string
	fixture string // 完整的成功串流,截斷用
	cutAt   string // 切在最後一個以此開頭的 data 行的 JSON 中間
	mk      func(*http.Client) usecase.LLMProvider
}

func providers() []provider {
	c := func(api string) repository.ProviderConfig {
		return repository.ProviderConfig{Name: api, API: api, Model: "m", BaseURL: "https://api.example.com", APIKey: "k"}
	}
	return []provider{
		{"openai", "openai_tool.sse", "data: {", func(h *http.Client) usecase.LLMProvider {
			return openai.New(c("openai-completions"), openai.Deps{HTTP: h})
		}},
		{"anthropic", "anthropic_tool.sse", `data: {"type":"message_delta"`, func(h *http.Client) usecase.LLMProvider {
			return anthropic.New(c("anthropic-messages"), anthropic.Deps{HTTP: h})
		}},
		{"google", "google_tool.sse", "data: {", func(h *http.Client) usecase.LLMProvider {
			return google.New(c("google-generative-ai"), google.Deps{HTTP: h})
		}},
	}
}

func done(t *testing.T, p usecase.LLMProvider) *entity.Message {
	t.Helper()
	var last usecase.LLMEvent
	req := usecase.LLMRequest{System: "s", Messages: []entity.Message{{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: "hi"}}}}, MaxTokens: 10}
	for e := range p.Stream(context.Background(), req) {
		last = e
	}
	if last.Type != usecase.LLMEventDone || last.Message == nil {
		t.Fatalf("最後一個事件不是 done:%#v", last)
	}
	return last.Message
}

// 錯誤本文先遮再截:照 UTF-16 數,秘密橫跨第 500 個 code unit(前綴用代理對字元)。驗的是「先遮再截」的順序;
// 截斷本身照 UTF-16 數由 entity.SliceUTF16 的測試負責,這裡改成照 byte 或 rune 截也會過。
func TestProviderErrorBodyRedactBeforeTruncate(t *testing.T) {
	const key = "sk-abcdefghijklmnopqrstuvwxyz0123"
	prefix := strings.Repeat("😀", 245) + " " // 491 個 code unit,key 佔第 492–524 個
	if n := entity.UTF16Len(prefix); n >= 500 || n+len(key) <= 500 {
		t.Fatalf("key 沒有跨第 500 個 code unit:%d", n)
	}
	for _, p := range providers() {
		t.Run(p.name, func(t *testing.T) {
			msg := done(t, p.mk(clientFor(400, prefix+key)))
			if msg.StopReason != "error" {
				t.Fatal(msg.StopReason)
			}
			for _, half := range []string{"sk-", "abcdefgh", "0123"} {
				if strings.Contains(msg.ErrorMessage, half) {
					t.Fatalf("錯誤訊息留下秘密片段 %q:%s", half, msg.ErrorMessage)
				}
			}
			if !strings.Contains(msg.ErrorMessage, "400") || !strings.Contains(msg.ErrorMessage, "😀") {
				t.Fatal(msg.ErrorMessage)
			}
		})
	}
}

// 成功回應的串流在 JSON 中間被切斷:三家都要回 stopReason=error,不 panic。
func TestProviderMalformedSSE(t *testing.T) {
	for _, p := range providers() {
		t.Run(p.name, func(t *testing.T) {
			b, err := os.ReadFile("../../../../testdata/" + p.fixture)
			if err != nil {
				t.Fatal(err)
			}
			full := string(b)
			// 切在 JSON 中間(沒有結尾的 } 與空行),之後的內容全部不見。
			// anthropic 略過壞 JSON 事件,所以切在帶 stop_reason 的 message_delta,靠「沒收到 stop_reason」判錯。
			last := strings.LastIndex(full, p.cutAt)
			if last < 0 {
				t.Fatal("fixture 沒有 " + p.cutAt)
			}
			cut := full[:last+len(p.cutAt)+10]
			msg := done(t, p.mk(clientFor(200, cut)))
			if msg.StopReason != "error" || msg.ErrorMessage == "" {
				t.Fatalf("stopReason=%q errorMessage=%q", msg.StopReason, msg.ErrorMessage)
			}
		})
	}
}

// I2(裁決 G22,同 G12):沒注入 client(或注入的 client 沒 Transport)時,三家 provider 都不讀 proxy 環境變數。
// 用 http:// 的 baseUrl(明文情況:proxy 看得到 API key 與內容)。ProxyFromEnvironment 每個行程只讀一次環境變數,所以在子行程裡跑。
// 子行程另用一個會讀環境變數的 client 打 port 1 當對照組,證明環境變數在子行程裡確實有效。
func TestProvidersIgnoreProxyEnv(t *testing.T) {
	if os.Getenv("CUIRASS_PROXY_CHILD") == "1" {
		// 0.0.0.0 不是 loopback,ProxyFromEnvironment 不會自動略過;直連會被本機拒絕,不出網
		cfg := func(api, port string) repository.ProviderConfig {
			return repository.ProviderConfig{Name: api, API: api, Model: "m", BaseURL: "http://0.0.0.0:" + port, APIKey: "k"}
		}
		run := func(p usecase.LLMProvider) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // 限住重試
			defer cancel()
			req := usecase.LLMRequest{System: "s", Messages: []entity.Message{{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: "hi"}}}}, MaxTokens: 10}
			for range p.Stream(ctx, req) { // Stream 是惰性的,不 range 就不會送出請求
			}
		}
		for _, h := range []*http.Client{nil, {}} {
			run(openai.New(cfg("openai-completions", "2"), openai.Deps{HTTP: h}))
			run(anthropic.New(cfg("anthropic-messages", "3"), anthropic.Deps{HTTP: h}))
			run(google.New(cfg("google-generative-ai", "4"), google.Deps{HTTP: h}))
		}
		run(openai.New(cfg("openai-completions", "1"), openai.Deps{HTTP: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}}))
		return
	}
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProvidersIgnoreProxyEnv$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CUIRASS_PROXY_CHILD=1", "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY="+proxy.URL,
		"https_proxy="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("對照組沒經過 proxy:環境變數在子行程裡沒生效,這個測試證明不了什麼")
	}
	for _, h := range seen {
		if h != "0.0.0.0:1" {
			t.Fatalf("proxy 只該看到對照組的 0.0.0.0:1,實際 %q", seen)
		}
	}
}

// 審查 m8:錯誤本文回顯設定的 API key 本身時要遮掉,即使它不符合任何遮蔽規則(自架服務的 key);
// 而且要在截斷前遮(key 跨第 500 個 code unit 時不留半截)。短於 8 字的 key 不算可信的秘密,不換(否則錯誤訊息全毀)。
func TestProviderErrorBodyRedactsConfiguredKey(t *testing.T) {
	const key = "local-model-secret-9f3a7c" // 不符合任何內建遮蔽規則
	if entity.Redact(key) != key {
		t.Fatal("前提:這把 key 不該被規則遮到")
	}
	prefix := strings.Repeat("😀", 245) + " " // key 跨第 500 個 code unit
	mk := []struct {
		name string
		mk   func(*http.Client, string) usecase.LLMProvider
	}{
		{"openai", func(h *http.Client, k string) usecase.LLMProvider {
			return openai.New(repository.ProviderConfig{Name: "o", Model: "m", BaseURL: "https://api.example.com", APIKey: k}, openai.Deps{HTTP: h})
		}},
		{"anthropic", func(h *http.Client, k string) usecase.LLMProvider {
			return anthropic.New(repository.ProviderConfig{Name: "a", Model: "m", BaseURL: "https://api.example.com", APIKey: k}, anthropic.Deps{HTTP: h})
		}},
		{"google", func(h *http.Client, k string) usecase.LLMProvider {
			return google.New(repository.ProviderConfig{Name: "g", Model: "m", BaseURL: "https://api.example.com", APIKey: k}, google.Deps{HTTP: h})
		}},
	}
	for _, p := range mk {
		t.Run(p.name, func(t *testing.T) {
			for _, body := range []string{"bad key " + key, prefix + key} {
				msg := done(t, p.mk(clientFor(401, body), key))
				for _, frag := range []string{key, "local-model", "9f3a7c"} {
					if strings.Contains(msg.ErrorMessage, frag) {
						t.Fatalf("錯誤訊息留下 key 片段 %q:%s", frag, msg.ErrorMessage)
					}
				}
			}
			short := done(t, p.mk(clientFor(401, "keyword k"), "k"))
			if !strings.Contains(short.ErrorMessage, "keyword k") {
				t.Fatal("短 key 不該被替換:", short.ErrorMessage)
			}
		})
	}
}
