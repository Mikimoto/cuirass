package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// HTTPClientOptions 是 NewHTTPClient 的選項。HTTP 照 CloneClient 處理:為 nil(或其 Transport 為 nil)時
// 不讀 HTTPS_PROXY 等環境變數;呼叫端自己給的 Transport 照用(含它的 proxy 設定)。
// MaxBodyBytes / TimeoutMs ≤ 0 用預設(2 MiB、8000ms)。Audit 為 nil 不記錄(照樣擋)。
type HTTPClientOptions struct {
	Hosts        []string
	HTTP         *http.Client
	Audit        entity.AuditSink
	MaxBodyBytes int
	TimeoutMs    int
}

// CloneClient 複製呼叫端的 client(nil = 零值 client;不改原本那個),一律不跟隨轉址。
// Transport 為 nil 時換成框架全新建立、不讀 HTTPS_PROXY 等環境變數的傳輸(裁決 G12、G22):
// 出口白名單 client 與三家 provider 共用,沒有任何對外連線會悄悄經過環境變數指定的 proxy。
// 呼叫端自己給的 Transport 照用(含它的 proxy 設定);只有 internal 測試會注入,公開 API 沒有注入的管道。
func CloneClient(h *http.Client) *http.Client {
	c := http.Client{}
	if h != nil {
		c = *h
	}
	if c.Transport == nil {
		c.Transport = newTransport(nil)
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// ProviderClient 是三家 provider 的 client:同 CloneClient,但 local provider 沒注入 Transport 時用 LoopbackOnlyTransport。
func ProviderClient(h *http.Client, privacy entity.Privacy) *http.Client {
	if privacy == entity.Local && (h == nil || h.Transport == nil) {
		c := http.Client{}
		if h != nil {
			c = *h
		}
		c.Transport = LoopbackOnlyTransport()
		h = &c
	}
	return CloneClient(h)
}

// ErrNotLoopback:local provider 的連線撥號時,實際要連的 IP 不是 loopback。
var ErrNotLoopback = errors.New("local provider 只能連 loopback 位址,拒絕撥號")

// LoopbackOnlyTransport 是 local provider 的傳輸:撥號時在 net.Dialer.Control 檢查實際要連的 IP(DNS 解析之後、connect 之前),
// 不是 loopback 就回 ErrNotLoopback。localhost 被解析成外部位址、判斷隱私與送出時兩次解析不一致,都擋在這裡。
// 不可改成先查 DNS 再撥號(查到的與撥的可能不同)。
func LoopbackOnlyTransport() *http.Transport {
	return newTransport(func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !ap.Addr().Unmap().IsLoopback() {
			return ErrNotLoopback // 解析不了也拒絕(fail-closed)
		}
		return nil
	})
}

// newTransport 全新建立傳輸,設定同標準函式庫預設值但 Proxy 為 nil;不複製 http.DefaultTransport:
// 別的套件改過的全域 DialContext、DialTLSContext、TLSClientConfig(httpmock、instrumentation)不能被帶進來。
// control 非 nil 時掛在 net.Dialer.Control。沒設 DialTLSContext,https 也走這個 DialContext。
func newTransport(control func(network, address string, c syscall.RawConn) error) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: control}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewHTTPClient 是出口白名單 http client(spec §7):只放行 https 且主機完全相符;不跟轉址;限制大小與逾時。
func NewHTTPClient(o HTTPClientOptions) entity.HTTPClient {
	c := CloneClient(o.HTTP)
	maxBytes := o.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = 2 * 1024 * 1024
	}
	timeout := time.Duration(o.TimeoutMs) * time.Millisecond
	if o.TimeoutMs <= 0 {
		timeout = 8000 * time.Millisecond
	}
	audit := o.Audit
	if audit == nil {
		audit = entity.NullAudit()
	}
	hosts := map[string]bool{}
	for _, h := range o.Hosts {
		hosts[strings.ToLower(h)] = true
	}

	return func(ctx context.Context, rawURL string, opts entity.HTTPOptions) (entity.HTTPResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout) // 涵蓋讀 body
		defer cancel()
		var body io.Reader
		if opts.Body != "" {
			body = strings.NewReader(opts.Body)
		}
		method := opts.Method
		if method == "" {
			method = http.MethodGet
		}
		// 檢查的就是要撥號的那個 URL:只解析一次,檢查 req.URL
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		host, ok := "", false
		if err == nil {
			host, ok = hostOK(req.URL, hosts)
		}
		if !ok {
			audit.Write(entity.AuditEvent{Kind: "egress_block", Host: host})
			return entity.HTTPResponse{}, errors.New("blocked host")
		}
		for k, v := range opts.Headers {
			req.Header.Set(k, v)
		}
		res, err := c.Do(req)
		if err != nil {
			return entity.HTTPResponse{}, httpErr(ctx, err)
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			return entity.HTTPResponse{}, fmt.Errorf("不跟隨轉址(%d)", res.StatusCode)
		}
		headers := map[string]string{}
		for k, v := range res.Header {
			headers[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		// 不信 Content-Length:多讀 1 byte 判斷是否超過
		b, err := io.ReadAll(io.LimitReader(res.Body, int64(maxBytes)+1))
		if err != nil {
			return entity.HTTPResponse{}, httpErr(ctx, err)
		}
		if len(b) > maxBytes {
			return entity.HTTPResponse{}, fmt.Errorf("回應超過 %d bytes", maxBytes)
		}
		return entity.HTTPResponse{Status: res.StatusCode, Headers: headers, Body: string(b)}, nil
	}
}

// hostOK 回傳要記進稽核的 hostname(對應 WHATWG URL.hostname:小寫、IPv6 帶中括號)與是否放行。
// 非 ASCII 主機一律擋:WHATWG 會先轉 punycode,Go 不會;strings.ToLower 還會把 U+212A(Kelvin)變成 k。
func hostOK(u *url.URL, hosts map[string]bool) (string, bool) {
	h := u.Hostname()
	for i := range len(h) {
		if h[i] >= 0x80 {
			return h, false
		}
	}
	h = strings.ToLower(h)
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	// 帶帳密的 URL:fetch 直接拒絕(不送出);Go 會改成 Basic auth 送出,所以擋
	return h, u.Scheme == "https" && u.User == nil && h != "" && hosts[h]
}

// httpErr:逾時或取消回 ctx 的錯誤(errors.Is 可判斷);其他錯誤丟掉 *url.Error 外層(會帶完整 URL,
// 查詢字串可能有 key),內層訊息先遮蔽。
func httpErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return errors.New(entity.Redact(err.Error()))
}
