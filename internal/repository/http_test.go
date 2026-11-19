//go:build test

package repository

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type recAudit struct{ events []entity.AuditEvent }

func (a *recAudit) Write(e entity.AuditEvent) bool { a.events = append(a.events, e); return true }

func resp(status int, body string, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// setup:注入的 transport 記錄每次呼叫。
func setupHTTP(t *testing.T, rt func(*http.Request) (*http.Response, error), mod func(*HTTPClientOptions)) (entity.HTTPClient, *recAudit, *[]*http.Request) {
	t.Helper()
	a := &recAudit{}
	var calls []*http.Request
	o := HTTPClientOptions{
		Hosts: []string{"api.example.com"},
		Audit: a,
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r)
			return rt(r)
		})},
	}
	if mod != nil {
		mod(&o)
	}
	return NewHTTPClient(o), a, &calls
}

func okResp(*http.Request) (*http.Response, error) { return resp(200, "hello", nil), nil }

func TestHTTPClient(t *testing.T) {
	ctx := context.Background()
	t.Run("白名單主機成功,且 redirect 為 manual", func(t *testing.T) {
		follow := func(*http.Request, []*http.Request) error { return nil }
		var injected *http.Client
		h, _, calls := setupHTTP(t, okResp, func(o *HTTPClientOptions) { o.HTTP.CheckRedirect = follow; injected = o.HTTP })
		r, err := h(ctx, "https://api.example.com/x", entity.HTTPOptions{})
		if err != nil || r.Status != 200 || r.Body != "hello" {
			t.Fatal(r, err)
		}
		if len(*calls) != 1 || (*calls)[0].URL.String() != "https://api.example.com/x" {
			t.Fatal(*calls)
		}
		// 不改呼叫端的 client(轉址不跟隨另見 TestHTTPRedirectNotFollowed)
		if injected.CheckRedirect == nil || injected.CheckRedirect(nil, nil) != nil {
			t.Fatal("呼叫端的 CheckRedirect 被改掉")
		}
	})
	for _, u := range []string{
		"https://evil.com/",
		"http://api.example.com/",
		"https://api.example.com.evil.com/",
		"https://api.example.com@evil.com/",
	} {
		t.Run("擋 "+u, func(t *testing.T) {
			h, a, calls := setupHTTP(t, okResp, nil)
			_, err := h(ctx, u, entity.HTTPOptions{})
			if err == nil || !strings.Contains(err.Error(), "blocked host") {
				t.Fatal(err)
			}
			if len(a.events) == 0 || a.events[0].Kind != "egress_block" {
				t.Fatal(a.events)
			}
			if len(*calls) != 0 {
				t.Fatal(len(*calls))
			}
		})
	}
	t.Run("3xx 丟錯", func(t *testing.T) {
		h, _, calls := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return resp(302, "", http.Header{"Location": {"https://evil.com"}}), nil
		}, nil)
		_, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if err == nil || err.Error() != "不跟隨轉址(302)" {
			t.Fatal(err)
		}
		if len(*calls) != 1 {
			t.Fatal("跟隨了轉址", len(*calls))
		}
	})
	t.Run("超過 maxBytes 丟錯", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return resp(200, strings.Repeat("x", 100), nil), nil
		}, func(o *HTTPClientOptions) { o.MaxBodyBytes = 10 })
		_, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if err == nil || err.Error() != "回應超過 10 bytes" {
			t.Fatal(err)
		}
	})
}

func TestHTTPClientGoExtras(t *testing.T) {
	ctx := context.Background()
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"

	t.Run("剛好 maxBytes 放行", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return resp(200, strings.Repeat("x", 10), nil), nil
		}, func(o *HTTPClientOptions) { o.MaxBodyBytes = 10 })
		if r, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{}); err != nil || len(r.Body) != 10 {
			t.Fatal(r, err)
		}
	})
	t.Run("主機與 scheme 不分大小寫、白名單也不分", func(t *testing.T) {
		h, _, calls := setupHTTP(t, okResp, func(o *HTTPClientOptions) { o.Hosts = []string{"API.Example.com"} })
		if _, err := h(ctx, "HTTPS://api.EXAMPLE.com/", entity.HTTPOptions{}); err != nil || len(*calls) != 1 {
			t.Fatal(err)
		}
	})
	for _, u := range []string{
		"https://api.example.com./",                 // 結尾點:hostname 保留,不相符
		"https://api.example.com\\@evil.com/",       // Go 解析失敗 → 擋
		"https://evil.com?@api.example.com",         // 查詢字串不是 userinfo
		"https:///api.example.com",                  // 空主機
		"https://api.%E2%84%AAexample.com/",         // %-編碼的非 ASCII 主機:解碼後被非 ASCII 檢查擋
		"https://Kapi.example.com/",                 // 非 ASCII 主機一律擋(Kelvin 符號 ToLower 會變 k)
		"ftp://api.example.com/",                    // 非 https
		"https://api.example.com%00.evil.com/",      // 解析失敗
		"::not a url",                               // 解析失敗
		"https://api.example.com:443@evil.com:443/", // userinfo 帶冒號
		"https://user:pass@api.example.com/",        // 帶帳密:fetch 拒絕,Go 擋
	} {
		t.Run("擋 "+u, func(t *testing.T) {
			h, a, calls := setupHTTP(t, okResp, func(o *HTTPClientOptions) { o.Hosts = []string{"api.example.com", "kapi.example.com"} })
			if _, err := h(ctx, u, entity.HTTPOptions{}); err == nil || err.Error() != "blocked host" {
				t.Fatal(err)
			}
			if len(a.events) != 1 || a.events[0].Kind != "egress_block" || len(*calls) != 0 {
				t.Fatal(a.events, len(*calls))
			}
		})
	}
	t.Run("5xx 當一般回應回傳,本文原樣不截", func(t *testing.T) {
		body := "err " + key + strings.Repeat("y", 1000)
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) { return resp(500, body, nil), nil }, nil)
		r, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if err != nil || r.Status != 500 || r.Body != body {
			t.Fatal(r.Status, err)
		}
	})
	t.Run("擋下時 audit 記 hostname(小寫)", func(t *testing.T) {
		h, a, _ := setupHTTP(t, okResp, nil)
		_, _ = h(ctx, "https://EVIL.com:8443/x", entity.HTTPOptions{})
		if a.events[0].Host != "evil.com" {
			t.Fatal(a.events)
		}
	})
	t.Run("IPv6 白名單寫成 [::1]", func(t *testing.T) {
		h, _, calls := setupHTTP(t, okResp, func(o *HTTPClientOptions) { o.Hosts = []string{"[::1]"} })
		if _, err := h(ctx, "https://[::1]:8443/", entity.HTTPOptions{}); err != nil || len(*calls) != 1 {
			t.Fatal(err)
		}
	})
	t.Run("method、headers、body 照送", func(t *testing.T) {
		h, _, calls := setupHTTP(t, okResp, nil)
		_, err := h(ctx, "https://api.example.com/p", entity.HTTPOptions{Method: "POST", Headers: map[string]string{"X-A": "1"}, Body: "b"})
		if err != nil {
			t.Fatal(err)
		}
		r := (*calls)[0]
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("x-a") != "1" || string(b) != "b" {
			t.Fatal(r.Method, r.Header, string(b))
		}
	})
	t.Run("回應 header key 小寫、多值以逗號接", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return resp(200, "", http.Header{"X-Multi": {"a", "b"}, "Content-Type": {"text/plain"}}), nil
		}, nil)
		r, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if err != nil || r.Headers["x-multi"] != "a, b" || r.Headers["content-type"] != "text/plain" {
			t.Fatal(r.Headers, err)
		}
	})
	t.Run("連線錯誤:不帶 URL(查詢字串的 key)、內層訊息先遮蔽", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return nil, errors.New("tls 失敗 " + key + strings.Repeat(" x", 400))
		}, nil)
		_, err := h(ctx, "https://api.example.com/q?api_key="+key+"&token=SECRETTOKEN", entity.HTTPOptions{})
		if err == nil || strings.Contains(err.Error(), "sk-ant") || strings.Contains(err.Error(), "SECRETTOKEN") || !strings.Contains(err.Error(), "[REDACTED") {
			t.Fatal(err)
		}
	})
	t.Run("讀 body 錯誤也先遮蔽", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(io.MultiReader(strings.NewReader("x"), errReader{errors.New("讀取失敗 " + key)}))}, nil
		}, nil)
		_, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if err == nil || strings.Contains(err.Error(), "sk-ant") {
			t.Fatal(err)
		}
	})
	t.Run("逾時(含讀 body)", func(t *testing.T) {
		h, _, _ := setupHTTP(t, func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(ctxReader{r.Context()})}, nil
		}, func(o *HTTPClientOptions) { o.TimeoutMs = 30 })
		start := time.Now()
		_, err := h(ctx, "https://api.example.com/", entity.HTTPOptions{})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatal(err)
		}
	})
	t.Run("呼叫端取消", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		h, _, _ := setupHTTP(t, func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }, nil)
		if _, err := h(c, "https://api.example.com/", entity.HTTPOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("Audit 為 nil 也照擋、不 panic", func(t *testing.T) {
		h := NewHTTPClient(HTTPClientOptions{Hosts: []string{"api.example.com"}})
		if _, err := h(ctx, "https://evil.com/", entity.HTTPOptions{}); err == nil || err.Error() != "blocked host" {
			t.Fatal(err)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type ctxReader struct{ ctx context.Context }

func (c ctxReader) Read([]byte) (int, error) { <-c.ctx.Done(); return 0, c.ctx.Err() }

// TestHTTPRedirectNotFollowed:真的 TLS 伺服器。白名單主機 api.example.com 回 302,
// 目標(同主機的 /target 與非白名單的 evil.example)都記錄被打的次數,必須是 0。
func TestHTTPRedirectNotFollowed(t *testing.T) {
	var targetHits, evilHits atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/evil":
			http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "https://api.example.com/target", http.StatusMovedPermanently)
		case "/target":
			targetHits.Add(1)
		}
	}))
	defer api.Close()

	// 依主機名撥號:兩台都在 127.0.0.1,不這樣做 evil 也會落在白名單主機上
	addr := map[string]string{"api.example.com:443": api.Listener.Addr().String(), "evil.example:443": evil.Listener.Addr().String()}
	// httptest 的憑證含 example.com;固定 ServerName 讓兩台都驗得過,evil 被跟隨時 handler 才真的會被打到
	tlsCfg := api.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = "example.com"
	var evilDials atomic.Int32
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			to, ok := addr[a]
			if !ok {
				return nil, errors.New("unexpected dial " + a)
			}
			if a == "evil.example:443" {
				evilDials.Add(1)
			}
			return (&net.Dialer{}).DialContext(ctx, network, to)
		},
	}
	defer tr.CloseIdleConnections()
	// 注入的 client 用預設 CheckRedirect(會跟隨);NewHTTPClient 必須蓋掉
	h := NewHTTPClient(HTTPClientOptions{Hosts: []string{"api.example.com"}, HTTP: &http.Client{Transport: tr}, Audit: &recAudit{}})

	// 前提:目標本身可達(否則 0 次證明不了什麼)
	if r, err := h(context.Background(), "https://api.example.com/target", entity.HTTPOptions{}); err != nil || r.Status != 200 || targetHits.Load() != 1 {
		t.Fatal("前提失敗", r, err)
	}
	targetHits.Store(0)

	for _, c := range []struct{ path, msg string }{{"/evil", "不跟隨轉址(302)"}, {"/same", "不跟隨轉址(301)"}} {
		_, err := h(context.Background(), "https://api.example.com"+c.path, entity.HTTPOptions{})
		if err == nil || err.Error() != c.msg {
			t.Error(c.path, err)
		}
	}
	if n, d := evilHits.Load(), evilDials.Load(); n != 0 || d != 0 {
		t.Fatalf("非白名單主機被連了:handler %d 次、撥號 %d 次", n, d)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("同主機轉址被跟隨了 %d 次", n)
	}
}

// TestHTTPDefaultClientIgnoresProxyEnv(裁決 G12):沒注入 client(或注入的 client 沒 Transport)時不讀 proxy 環境變數,
// ProxyFromEnvironment 每個行程只讀一次環境變數,所以在子行程裡跑。
// 子行程另用一個會讀環境變數的 client 打 port 1 當對照組,證明環境變數在子行程裡確實有效。
func TestHTTPDefaultClientIgnoresProxyEnv(t *testing.T) {
	if os.Getenv("CUIRASS_PROXY_CHILD") == "1" {
		ctx := context.Background()
		// 0.0.0.0 不是 loopback,ProxyFromEnvironment 不會自動略過;直連會被本機拒絕,不出網
		opts := HTTPClientOptions{Hosts: []string{"0.0.0.0"}, TimeoutMs: 2000}
		_, _ = NewHTTPClient(opts)(ctx, "https://0.0.0.0:443/", entity.HTTPOptions{})
		opts.HTTP = &http.Client{}
		_, _ = NewHTTPClient(opts)(ctx, "https://0.0.0.0:444/", entity.HTTPOptions{})
		opts.HTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
		_, _ = NewHTTPClient(opts)(ctx, "https://0.0.0.0:1/", entity.HTTPOptions{})
		return
	}
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPDefaultClientIgnoresProxyEnv$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CUIRASS_PROXY_CHILD=1", "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY="+proxy.URL,
		"https_proxy="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "CONNECT 0.0.0.0:1" {
		t.Fatalf("proxy 只該看到對照組的 CONNECT 0.0.0.0:1,實際 %q", seen)
	}
}

// I3:http.DefaultTransport 被換掉(httpmock、instrumentation)時不 panic,也不改用被換上的那個(它可能有 proxy 或錄下流量);
// 改用自己建的傳輸,同樣不讀 proxy 環境變數。本套件沒有 t.Parallel,改全域變數安全。
func TestHTTPDefaultTransportReplaced(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	var hits atomic.Int32
	http.DefaultTransport = rtFunc(func(*http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("swapped transport used")
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()

	tr, ok := CloneClient(nil).Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil || tr.TLSHandshakeTimeout == 0 {
		t.Fatalf("後備傳輸:%#v", tr)
	}
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig // 信任測試憑證
	res, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	// NewHTTPClient 也走同一個 helper:不 panic、不碰被換上的傳輸
	_, _ = NewHTTPClient(HTTPClientOptions{Hosts: []string{"0.0.0.0"}, TimeoutMs: 500})(context.Background(), "https://0.0.0.0:1/", entity.HTTPOptions{})
	if n := hits.Load(); n != 0 {
		t.Fatalf("被換上的 DefaultTransport 被用了 %d 次", n)
	}
}

// CloneClient 不改呼叫端的 client;一律不跟隨轉址;呼叫端給的 Transport 照用。
func TestCloneClient(t *testing.T) {
	mine := rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })
	in := &http.Client{Transport: mine, Timeout: time.Second}
	out := CloneClient(in)
	if out == in || in.CheckRedirect != nil || out.CheckRedirect == nil || out.Timeout != time.Second {
		t.Fatal("要回複本並設 CheckRedirect,不改原 client")
	}
	if _, ok := out.Transport.(rtFunc); !ok {
		t.Fatal("呼叫端的 Transport 要照用")
	}
	if out.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("不跟隨轉址")
	}
	if tr, ok := CloneClient(&http.Client{}).Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("沒 Transport 時要用不讀 proxy 的預設傳輸")
	}
}

// 別的套件把 http.DefaultTransport 換成另一個 *http.Transport(自訂 DialContext、TLSClientConfig…)時,框架建的 client
// 不能帶進它的任何設定:transport 一律全新建立。TestHTTPDefaultTransportReplaced 只測換成非 *http.Transport 的情況。
func TestFrameworkIgnoresGlobalTransportSettings(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	var dials atomic.Int32
	http.DefaultTransport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	for name, c := range map[string]*http.Client{
		"CloneClient(nil)":                   CloneClient(nil),
		"ProviderClient(nil, remote)":        ProviderClient(nil, entity.Remote),
		"ProviderClient(nil, local)":         ProviderClient(nil, entity.Local),
		"LoopbackOnlyTransport":              {Transport: LoopbackOnlyTransport()},
		"ProviderClient(無 Transport, local)": ProviderClient(&http.Client{}, entity.Local),
	} {
		res, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(name, err)
		}
		_ = res.Body.Close()
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("全域 DefaultTransport 的 DialContext 被呼叫了 %d 次", n)
	}
}

// local provider 的撥號檢查放在 net.Dialer.Control:看的是實際要連的 IP,不是先查 DNS 再撥。
// 用 IP 字面值:自訂 resolver 改不了 localhost 的解析(系統先讀 hosts 檔)。錯誤要是 ErrNotLoopback 本身,
// 不能只比對 IP 字串(一般連線失敗的訊息也含 IP)。
func TestLoopbackDialRejectsRemoteIP(t *testing.T) {
	c := &http.Client{Transport: LoopbackOnlyTransport()}
	for _, u := range []string{"http://192.0.2.1:9/", "http://[2001:db8::1]:9/", "http://[::ffff:192.0.2.1]:9/", "http://0.0.0.0:9/", "https://192.0.2.1:9/"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		res, err := c.Do(req)
		cancel()
		if err == nil {
			_ = res.Body.Close()
			t.Errorf("%s:應被拒絕", u)
			continue
		}
		if !errors.Is(err, ErrNotLoopback) {
			t.Errorf("%s:錯誤不是 ErrNotLoopback:%v", u, err)
		}
	}
}

func TestLoopbackDialAllows127(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	for _, u := range []string{srv.URL, "http://localhost:" + port} {
		res, err := ProviderClient(nil, entity.Local).Get(u)
		if err != nil {
			t.Fatal(u, err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if string(b) != "ok" {
			t.Fatal(u, string(b))
		}
	}
}

// 只有 local 且沒注入 Transport 才換成 loopback-only;注入的照用(只有 internal 測試會注入)。
func TestProviderClient(t *testing.T) {
	mine := rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })
	if _, ok := ProviderClient(&http.Client{Transport: mine}, entity.Local).Transport.(rtFunc); !ok {
		t.Fatal("注入的 Transport 要照用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // 192.0.2.1 是文件用位址,連不上:等逾時
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://192.0.2.1:9/", nil)
	if _, err := ProviderClient(nil, entity.Remote).Do(req); err == nil || errors.Is(err, ErrNotLoopback) {
		t.Fatal("remote 不做 loopback 檢查", err)
	}
	if c := ProviderClient(nil, entity.Local); c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("一律不跟隨轉址")
	}
}
