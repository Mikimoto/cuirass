package entity

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// HostIsLoopback:localhost、::1、127.0.0.0/8。
// 比 JS 的 new URL 嚴格:那邊的 hostname 已經過正規化(不合法的 IPv4 在那一步就丟例外),Go 拿到的是原字串,
// 而且 Go 會照原字串連線。所以 IP 一定要 netip 認得:127.0.0.256、0177.0.0.1、127.1 這類不算,
// 否則 Go 的 resolver 會把它們當網域走 DNS(被劫持就把未遮的 local 資料送出去)。
// IPv6 要剛好是 ::1(不收 zone、不收 v4-mapped,與 new URL 正規化後的結果相同)。
func HostIsLoopback(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" {
		return true
	}
	bracketed := strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]")
	if bracketed {
		h = h[1 : len(h)-1] // URL.hostname 保留中括號
	}
	a, err := netip.ParseAddr(h)
	if err != nil || bracketed && !a.Is6() { // 中括號裡只能是 IPv6([127.0.0.1] 在 new URL 會丟例外)
		return false
	}
	return a == netip.IPv6Loopback() || a.Is4() && a.As4()[0] == 127
}

// 只有 http(s) 實際送得出去,其餘 scheme 一律不當 loopback(fail-closed)。
// 不收 ws/wss/ftp:Go 的 url.Parse 只對 http/https 拒收沒中括號的 IPv6 加 port(ws://::1:11434),
// 其他 scheme 照收,Hostname() 會拿到 ::1,new URL 則丟例外(審查 N1)。
var specialSchemes = map[string]bool{"http": true, "https": true}

// ResolvePrivacy 判斷 provider 的隱私等級:明填優先(無法辨識的值當 remote);沒明填時,
// 有自訂 header(可能是 proxy 的憑證)或非 loopback 一律 remote。
// override = 明填 local 但 baseURL 不是 loopback(呼叫端應警告)。
// privacy 用指標區分「沒填」(nil)與「填了空字串」(undefined vs "")。
func ResolvePrivacy(baseURL string, privacy *Privacy, headers map[string]string) (Privacy, bool) {
	loopback := false
	// 壞 URL 視為非 loopback。Go 的 Hostname() 會拿掉 IPv6 中括號,所以 [::1] 與 ::1 都認得。
	// port 超出 65535 時 new URL 丟例外,Go 的 url.Parse 照收,要自己擋(ParseUint 的 16 位元上限)。
	// 沒中括號的 host 有兩個以上冒號(http://::1:11434)一律不算 loopback:new URL 丟例外;
	// Go 1.27 的 url.Parse 預設也拒收,但 GODEBUG=urlstrictcolons=0 時照收,不能讓判斷跟著環境變數走(審查 m1)。
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" && specialSchemes[u.Scheme] &&
		(strings.HasPrefix(u.Host, "[") || strings.Count(u.Host, ":") <= 1) {
		_, perr := strconv.ParseUint(u.Port(), 10, 16)
		loopback = (u.Port() == "" || perr == nil) && HostIsLoopback(u.Hostname())
	}
	if privacy != nil {
		if *privacy == Local || *privacy == Remote {
			return *privacy, *privacy == Local && !loopback
		}
		// 明填了但無法辨識(typo、大小寫錯)→ 往安全那邊當 remote,不再看 baseURL
		return Remote, false
	}
	if loopback && len(headers) == 0 {
		return Local, false
	}
	return Remote, false
}
