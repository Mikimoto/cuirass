package entity

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// 移植 JS 語意時要補的小工具。Go 與 JS 在這些地方行為不同,不補就會悄悄出現不同結果。

// jsWS 是 JS 正規式 \s 的字元集內容(Unicode 空白 + BOM)。Go RE2 的 \s 只有 ASCII [\t\n\f\r ],連 \v 都沒有,
// 中文常見的全形空白 U+3000 也不算,所以凡是要照 JS \s / \S 語意的地方都要換成這個。
const jsWS = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// IsJSSpace 對應 JS 的 \s 與 String.prototype.trim 的空白定義。
func IsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// TrimJS 對應 JS 的 String.prototype.trim(strings.TrimSpace 認 U+0085、不認 U+FEFF,不一樣)。
func TrimJS(s string) string { return strings.TrimFunc(s, IsJSSpace) }

// JSNumberString 對應 JS 的 String(n)(ES6 Number::toString;與 encoding/json 的浮點輸出規則相同)。
func JSNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0" // 含 -0
	}
	abs := math.Abs(f)
	if abs < 1e-6 || abs >= 1e21 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Go 輸出 e-07,JS 是 e-7
		if i := strings.IndexByte(s, 'e'); i >= 0 {
			exp := s[i+2:]
			exp = strings.TrimLeft(exp, "0")
			s = s[:i+2] + exp
		}
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

var pow10 = [...]float64{1, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10}

// JSToFixedNumber 對應 Number(x.toFixed(d)),x ≥ 0、0 ≤ d ≤ 10。
// JS toFixed 對「剛好一半」往大的進位,Go 的 FormatFloat 是四捨六入五成雙(2.5 → "2"),所以要用精確值自己算。
func JSToFixedNumber(x float64, d int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x >= 1e21 {
		return x // toFixed 回 String(x),Number 再轉回來不變
	}
	if r, ok := jsToFixedFast(x, d); ok {
		return r
	}
	return jsToFixedBig(x, d)
}

// jsToFixedFast:不用 math/big 算 n = floor(x·10^d + 0.5)(審查 m7:math/big 是 Matches 最壞情況的瓶頸)。
// p = x·10^d 的浮點結果,精確值 = p + e(e 用 FMA 精確算出;x ≥ 1e-11 時不會下溢)。p < 2^52 時 u = ulp(p) ≤ 0.5、|e| ≤ u/2:
//   - f = floor(p)、a = p − f 都精確,而且 a、0.5 都是 u 的倍數。
//   - a ≠ 0.5 時 |a − 0.5| ≥ u > |e|,精確值的小數部分與 a 落在 0.5 的同一邊:a > 0.5 才進位(a = 0 時 n = f)。
//   - a = 0.5 時精確值是 f + 0.5 + e:e ≥ 0 進位(e = 0 是真的一半,JS 往大的進位),e < 0 不進位。
//
// n < 2^53,最後的除法與 jsToFixedBig 的快速分支完全相同。p ≥ 2^52 時 ok=false,交給 jsToFixedBig。
func jsToFixedFast(x float64, d int) (float64, bool) {
	if x < 1e-11 {
		return 0, true // x·10^d < 0.1(d ≤ 10):toFixed 一定是 0
	}
	p := float64(x * pow10[d]) // 明確轉型:禁止編譯器把乘法與後面的減法融合成 FMA(Go 規格允許),p 一定是捨入後的值
	if !(p < 0x1p52) {
		return 0, false
	}
	f := math.Floor(p)
	switch a := p - f; {
	case a > 0.5:
		f++
	case a == 0.5 && math.FMA(x, pow10[d], -p) >= 0:
		f++
	}
	return f / pow10[d], true
}

// jsToFixedBig:用 256 位元精確值算(原本的做法;快速路徑判斷不了時用,也是測試的對照)。
func jsToFixedBig(x float64, d int) float64 {
	p := new(big.Float).SetPrec(256).SetFloat64(x)
	p.Mul(p, new(big.Float).SetPrec(256).SetFloat64(pow10[d]))
	p.Add(p, big.NewFloat(0.5))
	n, _ := p.Int(nil) // 正數:Int 即 floor
	if n.IsInt64() && n.Int64() < 1<<53 {
		// 兩個精確值相除,IEEE 正確捨入 = 解析十進位字串的結果
		return float64(n.Int64()) / pow10[d]
	}
	s := n.String()
	if d > 0 {
		s = s[:len(s)-d] + "." + s[len(s)-d:]
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

var jsDecimal = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)

// JSNumber 對應 JS 的 Number(string):去 JS 空白、空字串 = 0、支援 0x/0o/0b 與 Infinity,其餘不合法 = NaN。
// strconv.ParseFloat 會收 "inf"、"nan"、"1_000"、十六進位浮點,JS 都不收,所以先用正規式擋。
func JSNumber(s string) float64 {
	s = TrimJS(s)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
		if base != 0 {
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok || strings.ContainsRune(s[2:], '_') {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	if !jsDecimal.MatchString(s) {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s, 64) // 溢位時回 ±Inf,與 JS 相同
	return f
}

// UTF16Len 是 JS 的 string.length(UTF-16 code unit 數)。
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// SliceUTF16 對應 JS 的 s.slice(0, n)。若第 n 個 code unit 落在代理對中間,JS 會留下孤立的高代理;
// Go 字串表示不了,這裡直接不收那個字元(少一個字元,不會多出內容)。
func SliceUTF16(s string, n int) string {
	used := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if used+w > n {
			return s[:i]
		}
		used += w
	}
	return s
}
