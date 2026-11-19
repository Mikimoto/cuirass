//go:build test

package entity

import (
	"encoding/json"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func ptr(p Privacy) *Privacy { return &p }

// 審查 I1:不合法的 IPv4、超出範圍的 port、IPv6 zone / v4-mapped,new URL 都會丟例外或不算 loopback → remote。
// Go 的 url.Parse 照收,原本會把它們當 loopback → local → 不遮就送出。
func TestResolvePrivacyInvalidHosts(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.256:11434/v1",
		"http://127.999.999.999/",
		"http://127.0.0.08/",
		"http://127.0.0.1:99999/",
		"http://[::1%25lo0]/",
		"http://[::ffff:127.0.0.1]/",
		// 審查 N1:url.Parse 只對 http/https 拒收沒中括號的 IPv6 加 port,ws/wss/ftp 照收,Hostname() 拿到 ::1
		"ws://::1:11434", "wss://0:0::1:0", "ftp://u@::1:11434", "ws://::1", "ftp://0:0::1:21/x",
		// 審查 m1:http/https 也一樣。go 1.27 的 url.Parse 預設就拒收;GODEBUG=urlstrictcolons=0 時照收,要自己擋
		"http://::1:11434", "https://0:0::1:443/", "HTTP://::1:0", "http://0:0::1:11434", "http://u@::1:11434/v1",
	} {
		if p, o := ResolvePrivacy(u, nil, nil); p != Remote || o {
			t.Errorf("%s:得到 %s/%v,應為 remote", u, p, o)
		}
	}
	if HostIsLoopback("127.0.0.256") {
		t.Error("127.0.0.256 不是合法 IPv4,不能當 loopback")
	}
	// 中括號裡只能是 IPv6(new URL 對 [127.0.0.1] 丟例外)
	if HostIsLoopback("[127.0.0.1]") || HostIsLoopback("[localhost]") {
		t.Error("中括號裡的 IPv4 / 網域不能當 loopback")
	}
}

// 審查 m1:隱私判斷不能靠 go.mod 的 go 版本或 GODEBUG。在子行程用 GODEBUG=urlstrictcolons=0 重跑上面的測試。
func TestResolvePrivacyLaxURLParsing(t *testing.T) {
	if os.Getenv("CUIRASS_LAX_URL_CHILD") == "1" {
		return // 子行程只跑 TestResolvePrivacyInvalidHosts
	}
	cmd := exec.Command(os.Args[0], "-test.run=^(TestResolvePrivacyInvalidHosts|TestResolvePrivacyLaxURLParsing)$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "CUIRASS_LAX_URL_CHILD=1", "GODEBUG=urlstrictcolons=0")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestResolvePrivacyInvalidHosts") {
		t.Fatal(err, string(out))
	}
	// 對照:子行程裡 url.Parse 真的照收沒中括號的 IPv6 加 port,否則這個測試什麼也沒驗到
	probe := exec.Command(os.Args[0], "-test.run=^TestLaxURLProbe$", "-test.count=1", "-test.v")
	probe.Env = append(os.Environ(), "CUIRASS_LAX_URL_CHILD=1", "GODEBUG=urlstrictcolons=0")
	if out, err := probe.CombinedOutput(); err != nil || !strings.Contains(string(out), "lax-parse-ok") {
		t.Fatal("GODEBUG=urlstrictcolons=0 沒讓 url.Parse 放寬,子行程測試沒有意義:", err, string(out))
	}
}

func TestLaxURLProbe(t *testing.T) {
	if os.Getenv("CUIRASS_LAX_URL_CHILD") != "1" {
		t.Skip("只在 TestResolvePrivacyLaxURLParsing 的子行程裡跑")
	}
	if _, err := url.Parse("http://::1:11434"); err == nil {
		t.Log("lax-parse-ok")
	}
}

// Go 會照原字串連線(不像 JS 的 fetch 先正規化),所以只有 netip 認得的 IP 或 localhost 才算 loopback。
// 下列 golden 判 local、Go 刻意判 remote:Go 的 resolver 不把它們當 IP,會改走 DNS(I1 的外送路徑)。
var privacyFailClosed = []string{
	"http://0x7f.0.0.1/", "http://0177.0.0.1/", "http://2130706433/", "http://127.1/", "http://0x7f000001/",
	"http://127.0.0.1./", "http://127.0.0.0x1/", "http://%31%32%37.0.0.1/", "http://１２７.０.０.１/",
	"http:127.0.0.1", "http:///127.0.0.1", " http://127.0.0.1 ", "http://127.0.0.1\t/", "file://127.0.0.1/", "foo://127.0.0.256/",
	"file://[::1]/", `http://127.0.0.1\@evil.com/`, "ws://127.0.0.1/", "wss://localhost/", "ftp://[::1]:21/",
}

// 這些必須與 golden 逐字相同。
var privacyMustMatch = []string{
	"http://[::1]:1/", "http://[::1]/", "http://[0:0::1]/", "http://[::0.0.0.1]/", "http://[0:0:0:0:0:0:0:1]:8080/v1",
	"http://localhost./", "http://LOCALHOST/", "http://LocalHost:11434", "http://a@127.0.0.1/", "http://u:p@localhost/",
	"http://127.0.0.1:65535/", "http://127.0.0.1:/", "http://127.255.255.255/", "HTTP://127.0.0.1/",
	"http://127.0.0.1@evil.com/", "http://127.0.0.1.evil.com/", "http://localhost.evil.com/", "http://4294967295/",
	"http://127.0.0.256:11434/v1", "http://127.999.999.999/", "http://127.0.0.08/", "http://127.0.0.09/", "http://1.2.3.4.5/",
	"http://127.0.0.1:99999/", "http://[::1%25lo0]/", "http://[::ffff:127.0.0.1]/", "http://127.0.0.1../",
	"http://0x/", "http://0/", "localhost:11434", "not a url", "", "https://api.anthropic.com", "http://128.0.0.1/",
	"http://[127.0.0.1]/", "http://[localhost]/", `http://evil.com\@127.0.0.1/`, "http://[::127.0.0.1]/", "http://[::1]]/",
	"ws://::1:11434", "wss://0:0::1:0", "ftp://u@::1:11434", "http://::1:11434", "https://0:0::1:443/",
}

func privacyCase(u string) map[string]any {
	p, o := ResolvePrivacy(u, nil, nil)
	return map[string]any{"privacy": string(p), "override": o}
}

func TestPrivacyGolden(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	nums := []string{"127", "0", "1", "255", "256", "0177", "0x7f", "0X7F", "08", "00", "0x", "2130706433", "16777215", "4294967296", "1e2", ""}
	schemes := []string{"http://", "https://", "HTTP://", "ws://", "ftp://", "file://", "foo://", "http:", "http:///"}
	extras := []string{"", "@", "a@", ":11434", ":99999", ":0", "/v1", ".", "..", ".evil.com", "#@x", "?q", "%2f"}
	var urls []string
	urls = append(urls, privacyFailClosed...)
	urls = append(urls, privacyMustMatch...)
	for range 3000 {
		host := ""
		for i := range 1 + r.IntN(5) {
			if i > 0 {
				host += "."
			}
			host += nums[r.IntN(len(nums))]
		}
		switch r.IntN(6) {
		case 0:
			host = []string{"localhost", "LOCALHOST", "[::1]", "[::ffff:7f00:1]", "[0::1]", "[::2]", "localhost.", "::1", "0:0::1", "::ffff:127.0.0.1"}[r.IntN(10)]
		case 1:
			host = extras[r.IntN(len(extras))] + host
		}
		urls = append(urls, schemes[r.IntN(len(schemes))]+host+extras[r.IntN(len(extras))])
	}
	cases := make([]goldenCase, len(urls))
	for i, u := range urls {
		cases[i] = goldenCase{Fn: "privacy", Text: u}
	}
	want := loadGolden(t, cases)
	goldenLocal := map[string]bool{}
	closed := 0
	for i, u := range urls {
		var gold struct{ Privacy string }
		if err := json.Unmarshal(want[i], &gold); err != nil {
			t.Fatal(err)
		}
		goldenLocal[u] = gold.Privacy == "local"
		got := privacyCase(u)
		if got["privacy"] == "local" && gold.Privacy != "local" {
			t.Errorf("fail-open:%q Go local、golden %s", u, gold.Privacy)
		} else if got["privacy"] != gold.Privacy {
			closed++
		}
	}
	for _, u := range privacyMustMatch {
		got, _ := json.Marshal(privacyCase(u))
		var gold any
		_ = json.Unmarshal(want[indexOf(urls, u)], &gold)
		if !reflectEqual(normalize(t, got), gold) {
			t.Errorf("%q:go %s、golden %v", u, got, gold)
		}
	}
	for _, u := range privacyFailClosed {
		if !goldenLocal[u] || privacyCase(u)["privacy"] != "remote" {
			t.Errorf("%q:預期 golden local、Go remote(刻意的 fail-closed 差異),實際 golden local=%v、Go %v", u, goldenLocal[u], privacyCase(u)["privacy"])
		}
	}
	t.Logf("隱私 golden 測試 %d 筆;golden local、Go remote(fail-closed)%d 筆", len(urls), closed)
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func TestPrivacy(t *testing.T) {
	type res struct {
		p Privacy
		o bool
	}
	rp := func(u string, p *Privacy, h map[string]string) res { a, b := ResolvePrivacy(u, p, h); return res{a, b} }
	t.Run("hostIsLoopback", func(t *testing.T) {
		for _, h := range []string{"localhost", "LOCALHOST", "::1", "[::1]", "127.0.0.1", "127.1.2.3"} {
			if !HostIsLoopback(h) {
				t.Error(h)
			}
		}
		for _, h := range []string{"example.com", "10.0.0.5", "128.0.0.1", "127.0.0.1.evil.com", "localhost.evil.com"} {
			if HostIsLoopback(h) {
				t.Error(h)
			}
		}
	})
	t.Run("resolvePrivacy:明填優先;有 header 一律 remote;loopback 才 local", func(t *testing.T) {
		if r := rp("https://api.anthropic.com", nil, nil); r != (res{Remote, false}) {
			t.Error(r)
		}
		if r := rp("http://127.0.0.1:11434", nil, nil); r != (res{Local, false}) {
			t.Error(r)
		}
		if r := rp("http://localhost:11434", nil, map[string]string{"CF-Access-Client-Id": "x"}); r.p != Remote {
			t.Error(r)
		}
		if r := rp("https://llm.example.com", ptr(Local), nil); r != (res{Local, true}) {
			t.Error(r)
		}
	})
	t.Run("resolvePrivacy:壞 URL 當 remote", func(t *testing.T) {
		if r := rp("not a url", nil, nil); r != (res{Remote, false}) {
			t.Error(r)
		}
	})
	t.Run("resolvePrivacy:只認 local/remote 確切值;明填了但無法辨識一律 remote(loopback 也是)", func(t *testing.T) {
		if r := rp("https://x.com", ptr("REMOTE"), nil); r.p != Remote {
			t.Error(r)
		}
		if r := rp("https://x.com", ptr("LOCAL"), nil); r != (res{Remote, false}) {
			t.Error(r)
		}
		// null、1 在這個函式的型別上不存在(Privacy 是字串);null 由 repository.ProviderConfig.PrivacyNull 表示(Ruling G4)
		for _, bad := range []Privacy{"bogus", "LOCAL", "Local", ""} {
			if r := rp("http://127.0.0.1", ptr(bad), nil); r != (res{Remote, false}) {
				t.Errorf("%q:%v", bad, r)
			}
		}
		// 沒明填(nil)仍照 baseURL 判斷
		if r := rp("http://127.0.0.1", nil, nil); r.p != Local {
			t.Error(r)
		}
	})
}
