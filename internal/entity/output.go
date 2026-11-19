package entity

import (
	"encoding/binary"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// 輸出檢查:純函式。回答進畫面/對話紀錄前用(LLM05 連結、LLM07 提示詞外洩、LLM09 數字核對)。
//
// 位置一律用「rune 索引」。用 UTF-16 索引就得依賴「全形轉半形等長」在原文與半形版之間共用位置;
// UTF-8 下全形數字 3 bytes、半形 1 byte,byte 位置會錯開,所以這裡改用 rune(全形 → 半形仍是 1 rune → 1 rune)。

// ── LLM05 連結 ──

const removedLink = "[連結已移除]"

// removedImage:markdown 圖片一律拿掉(https 也一樣)。畫面自動載入圖片時,網址本身就能把資料帶出去(LLM02 外洩)。
const removedImage = "[圖片已移除]"

// markdown 連結:[文字](URL);URL 允許一層括號(如 javascript:alert(1))。沒有 lookaround,RE2 的 leftmost-first 與 JS 選同一個匹配。
// 原式:/\[([^\]\n]*)\]\(((?:[^()\s]|\([^()\s]*\))*)\)/g(\s 換成 jsWS)
var mdLink = regexp.MustCompile(`\[([^\]\n]*)\]\(((?:[^()` + jsWS + `]|\([^()` + jsWS + `]*\))*)\)`)

// 裸 URL 的開頭(scheme)。原式:
// /(?<![a-z])(?:[a-z][a-z0-9+.-]*:\/\/|(?:javascript|data|vbscript|file|mailto|https?):(?=\S))(?:[^\s()]|\([^()\s]*\))*/gi
// i 旗標沒有 u:JS 只摺 ASCII,Go 的 (?i) 會把 ſ 當 s、K(開爾文)當 k,所以每個字母明列大小寫。
// lookbehind、句中的 (?=\S) 與尾巴在 StripUnsafeLinks 裡用 Go 處理。kw 組 = 走了關鍵字分支。
var bareHead = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://|(?P<kw>(?:` + asciiCI("javascript") + `|` + asciiCI("data") + `|` + asciiCI("vbscript") + `|` +
	asciiCI("file") + `|` + asciiCI("mailto") + `|` + asciiCI("http") + `[sS]?):)`)

var bareKW = bareHead.SubexpIndex("kw")

// asciiCI("ab") = "[aA][bB]"
func asciiCI(w string) string {
	var b strings.Builder
	for _, c := range w {
		b.WriteString("[" + string(c) + string(unicode.ToUpper(c)) + "]")
	}
	return b.String()
}

// isHTTPS 對應 /^https:\/\//i(只摺 ASCII)。
func isHTTPS(u string) bool {
	const p = "https://"
	if len(u) < len(p) {
		return false
	}
	for i := range len(p) {
		c := u[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != p[i] {
			return false
		}
	}
	return true
}

// bareTail 回傳 (?:[^\s()]|\([^()\s]*\))* 從 s 開頭吃掉的長度。每一步最多一個分支成立,貪婪的第一條路徑就是 JS 的結果。
func bareTail(s string) int {
	i := 0
	for i < len(s) {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == ')' || IsJSSpace(r):
			return i
		case r != '(':
			i += w
			continue
		}
		j := i + 1 // 括號組:( 非括號非空白* )
		for j < len(s) {
			r, w := utf8.DecodeRuneInString(s[j:])
			if r == '(' || r == ')' || IsJSSpace(r) {
				break
			}
			j += w
		}
		if j == len(s) || s[j] != ')' {
			return i
		}
		i = j + 1
	}
	return i
}

func isASCIILetterRune(r rune) bool { return r < utf8.RuneSelf && isASCIILetter(byte(r)) }

// StripUnsafeLinks 只放行 https:// 連結;其他協定換成 [連結已移除](markdown 形式保留文字)。
// markdown 圖片 ![文字](URL) 不論協定一律換成「文字 [圖片已移除]」。
func StripUnsafeLinks(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range mdLink.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > last && text[m[0]-1] == '!' {
			b.WriteString(text[last : m[0]-1])
			b.WriteString(text[m[2]:m[3]] + " " + removedImage)
			last = m[1]
			continue
		}
		b.WriteString(text[last:m[0]])
		if isHTTPS(text[m[4]:m[5]]) {
			b.WriteString(text[m[0]:m[1]])
		} else {
			b.WriteString(text[m[2]:m[3]] + " " + removedLink)
		}
		last = m[1]
	}
	b.WriteString(text[last:])
	s := b.String()

	b.Reset()
	last = 0
	for p := 0; p <= len(s); {
		loc := bareHead.FindStringSubmatchIndex(s[p:])
		if loc == nil {
			break
		}
		st, en := p+loc[0], p+loc[1]
		if r, _, ok := prevRune(s, st); ok && isASCIILetterRune(r) {
			// (?<![a-z]) 不成立:這段字母後面的位置前一字元都是字母,一樣不成立,整段跳過
			p = skipRun(s, st, isASCIILetterRune)
			continue
		}
		if loc[2*bareKW] >= 0 {
			// 關鍵字分支的 (?=\S):冒號後要有非空白。不成立時,這個起點 scheme 分支也不中(否則 RE2 會先選它),往後一個字元再找
			if r, _ := utf8.DecodeRuneInString(s[en:]); en == len(s) || IsJSSpace(r) {
				p = st + 1 // 起點一定是 ASCII 字母
				continue
			}
		}
		end := en + bareTail(s[en:])
		b.WriteString(s[last:st])
		if isHTTPS(s[st:end]) {
			b.WriteString(s[st:end])
		} else {
			b.WriteString(removedLink)
		}
		last, p = end, end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// ── LLM07 提示詞外洩 ──

// LeakMin:提示詞外洩判定的最短片段(NormalizeLeak 後的 UTF-16 code unit 數,同 JS 的 length)。
const LeakMin = 20

const cjkNum = `一二三四五六七八九十壹貳參肆伍陸柒捌玖拾`

// 行首清單符號(可連續,如「> a.」)。原式(gmu,沒有 i;此時文字已轉小寫):
// /^[ \t]*(?:(?:[-*+•‧·>|]|[一…拾]{1,3}[、.):]|\([一…拾]{1,3}\)|\(?(?:[a-z]|[ivx]{2,4})[.)、:])[ \t]*)+/gmu
// 本體沒有 lookaround,RE2 照 JS 的分支優先序選;多行 ^ 由 before 判斷(JS 在 \n \r U+2028 U+2029 之後都成立),
// 只在行首做錨定匹配,不中就跳到下一行(lookRule 的 anchored 模式)。
var listMarker = newAnchoredRule(`[ \t]*(?:(?:[-*+•‧·>|]|[`+cjkNum+`]{1,3}[、.):]|\([`+cjkNum+`]{1,3}\)|\(?(?:[a-z]|[ivx]{2,4})[.)、:])[ \t]*)+`,
	func(s string, i int) bool {
		r, _, ok := prevRune(s, i)
		return !ok || isLineTerm(r)
	}, func(r rune) bool { return !isLineTerm(r) })

func isLineTerm(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }

func dropIf(f func(rune) bool) func(rune) rune {
	return func(r rune) rune {
		if f(r) {
			return -1
		}
		return r
	}
}

// NormalizeLeak 是外洩比對前的正規化(回答與提示詞兩邊都套),步驟:
// 去 \p{No}\p{Nl}\p{S} → NFKC → 小寫 → 去 \p{Nd}\p{Cf}\p{M} 與隱形填充字母 → 去行首清單符號 → 去 \p{P}\p{S} → 去 JS 空白。
//
// 小寫用 unicode.ToLower 逐字轉:JS 的 toLowerCase 只有兩處不是逐字:İ → i + U+0307(下一步就去掉 \p{M},結果相同),
// 與字尾 Σ → ς(要看前後文)。這裡 ς 一律摺成 σ:結果 = JS 的結果把 ς 換成 σ,所以 JS 語意攔得到的這裡一定攔得到。
func NormalizeLeak(s string) string { return afterNFKC(norm.NFKC.String(preNFKC(s))) }

// preNFKC:NormalizeLeak 在 NFKC 之前的所有步驟(目前只有去 \p{No}\p{Nl}\p{S})。
// LeaksPrompt 的 CGJ 檢查(見 streamSafeInserted)用的就是它的輸出,也就是 NFKC 實際收到的字串;兩邊共用這一個函式,不會走岔。
func preNFKC(s string) string {
	return strings.Map(dropIf(func(r rune) bool { return unicode.In(r, unicode.No, unicode.Nl, unicode.S) }), s)
}

// afterNFKC:NFKC 之後的其餘步驟。
func afterNFKC(s string) string {
	s = strings.Map(func(r rune) rune {
		r = unicode.ToLower(r)
		if r == 'ς' {
			r = 'σ'
		}
		if unicode.In(r, unicode.Nd, unicode.Cf, unicode.M) || r == 0x3164 || r == 0x115f || r == 0x1160 || r == 0xffa0 {
			return -1
		}
		return r
	}, s)
	var b strings.Builder
	last := 0
	listMarker.each(s, func(m []int) {
		b.WriteString(s[last:m[0]])
		last = m[1]
	})
	b.WriteString(s[last:])
	return strings.Map(dropIf(func(r rune) bool { return unicode.In(r, unicode.P, unicode.S) || IsJSSpace(r) }), b.String())
}

// utf16Units:每個 UTF-16 code unit 兩個 byte(大端)。只在偶數位置取片段,所以等長片段相等 ⇔ code unit 序列相等。
func utf16Units(s string) []byte {
	b := make([]byte, 0, 2*len(s))
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

// streamSafeInserted:x/text 的 NFKC 照 Stream-Safe Text Format,連續超過 30 個非起始字元就插 U+034F(CGJ),
// 後面的組合字元組不回基底字;ICU 不插。例如 "e" + 30×U+0334 + U+0301:ICU 得 é、Go 得 e,
// 提示詞裡任何預組字母(é ü ñ…)都能這樣在 Go 繞過。所以 NFKC 一插 CGJ,LeaksPrompt 就直接當外洩(Go 獨有、只會多攔)。
//
// 直接看 NFKC 有沒有插,不自己數:x/text 數的是未匯出的 nLead(Hangul V/T 字母、U+0F73 這類 ccc 0 的字也算),
// 自己用 ccc 數會漏(fix round 2 的 fuzz:200,000 筆裡 13 筆插了 CGJ、ccc 掃描漏 8 筆)。
// 沒有字元分解成 CGJ,NFKC 也不刪 CGJ,所以輸出比輸入多出 CGJ ⇔ x/text 插了。pre 必須是 preNFKC 的輸出、n 是它的 NFKC。
func streamSafeInserted(pre, n string) bool {
	return strings.Count(n, "\u034f") > strings.Count(pre, "\u034f")
}

// LeaksPrompt:text 含 prompt(去掉 allow 句)任一長 LeakMin 的連續片段 → true。
// 每個提示詞片段各 includes 一次是 提示詞長 × 回答長;這裡先把提示詞的片段放進集合,回答只掃一次,結果相同。
// 片段長度照 UTF-16 code unit 算;用 rune 算的話 BMP 外的字要兩倍長才攔,比較鬆。
func LeaksPrompt(text, prompt string, allow []string) bool {
	pre := preNFKC(text)
	n := norm.NFKC.String(pre)
	if streamSafeInserted(pre, n) {
		return true // Go 獨有的 fail-closed,見 streamSafeInserted
	}
	// allow 句在原始提示詞上挖掉,再正規化各段(先正規化就對不到 allow 原句)。
	// allow 有空字串時,split("") 把提示詞拆成單字,永遠判不到外洩。
	parts := []string{prompt}
	for _, a := range allow {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, a)...)
		}
		parts = next
	}
	const w = 2 * LeakMin
	windows := map[string]struct{}{}
	for _, part := range parts {
		u := utf16Units(NormalizeLeak(part))
		for i := 0; i+w <= len(u); i += 2 {
			windows[string(u[i:i+w])] = struct{}{}
		}
	}
	if len(windows) == 0 {
		return false
	}
	t := utf16Units(afterNFKC(n))
	for i := 0; i+w <= len(t); i += 2 {
		if _, ok := windows[string(t[i:i+w])]; ok {
			return true
		}
	}
	return false
}

// ── LLM09 數字核對 ──

// NumToken:Index 是 Raw 在原文的起始 rune 索引。
type NumToken struct {
	Raw   string
	Value float64
	Base  float64
	Unit  string
	Line  int
	Index int
}

// Half 把全形數字/小數點/逗號轉半形(rune 數不變)。
func Half(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '．':
			return '.'
		case r == '，':
			return ','
		case r >= '０' && r <= '９':
			return r - 0xfee0
		}
		return r
	}, s)
}

func notDigitBefore(s string, i int) bool {
	r, _, ok := prevRune(s, i)
	return !ok || !isDigit(r)
}

// 不算「數字主張」的片段:網址、日期、時間、行首清單序號。先換成等長(rune 數)空白,位置不變。
// 每條後面是原式;lookbehind 改由 before 處理,結尾 lookahead 改成 la 組(見 lookaround.go)。
var skipRules = []lookRule{
	// /https?:\/\/\S+/g
	newLookRule(`https?://[^`+jsWS+`]+`, nil, nil),
	// /(?<!\d)(?:19|20)\d{2}\s*[-/]\s*\d{1,2}(?:\s*[-/]\s*\d{1,2})?(?![\d,.])/g
	newLookRule(`(?:19|20)\d{2}[`+jsWS+`]*[-/][`+jsWS+`]*\d{1,2}(?:[`+jsWS+`]*[-/][`+jsWS+`]*\d{1,2})?(?P<la>[^\d,.]|\z)`, notDigitBefore, isDigit),
	// /(?<!\d)\d{4}\s*年(?:\s*\d{1,2}\s*月(?:\s*\d{1,2}\s*日)?)?/g
	newLookRule(`\d{4}[`+jsWS+`]*年(?:[`+jsWS+`]*\d{1,2}[`+jsWS+`]*月(?:[`+jsWS+`]*\d{1,2}[`+jsWS+`]*日)?)?`, notDigitBefore, isDigit),
	// /(?<!\d)\d{1,2}\s*月(?:\s*\d{1,2}\s*日)?/g
	newLookRule(`\d{1,2}[`+jsWS+`]*月(?:[`+jsWS+`]*\d{1,2}[`+jsWS+`]*日)?`, notDigitBefore, isDigit),
	// /(?<!\d)(?<!\d[.,])\d{1,3}\s*(?:日|天|週|周|個月)/g
	newLookRule(`\d{1,3}[`+jsWS+`]*(?:日|天|週|周|個月)`, func(s string, i int) bool {
		r, w, ok := prevRune(s, i)
		if !ok {
			return true
		}
		if isDigit(r) {
			return false
		}
		if r == '.' || r == ',' {
			return notDigitBefore(s, i-w)
		}
		return true
	}, isDigit),
	// /(?<![\d.,])\d{1,2}[-/]\d{1,2}(?![\d.,])/g
	newLookRule(`\d{1,2}[-/]\d{1,2}(?P<la>[^\d.,]|\z)`, func(s string, i int) bool {
		r, _, ok := prevRune(s, i)
		return !ok || !(isDigit(r) || r == '.' || r == ',')
	}, isDigit),
	// /\d{1,2}:\d{2}(?::\d{2})?/g
	newLookRule(`\d{1,2}:\d{2}(?::\d{2})?`, nil, nil),
	// /^[ \t]*\d{1,2}[.、)](?!\d)/gm:JS 的多行 ^ 在 \n \r U+2028 U+2029 之後都成立,Go 的 (?m)^ 只認 \n,所以自己判斷。
	newLookRule(`[ \t]*\d{1,2}[.、)](?P<la>[^\d]|\z)`, func(s string, i int) bool {
		r, _, ok := prevRune(s, i)
		return !ok || r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
	}, func(r rune) bool { return r == ' ' || r == '\t' || isDigit(r) }),
}

// /(?<![A-Za-z\d.])(\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?)(?![A-Za-z\d])\s*(%|％|億|萬|千元)?/g
// 後面的 \s*(單位)? 不影響本體怎麼停(單位字元都不是英數),在 Go 裡手動接。
var numRule = newLookRule(`(\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?)(?P<la>[^A-Za-z\d]|\z)`, func(s string, i int) bool {
	r, _, ok := prevRune(s, i)
	return !ok || !(isDigit(r) || r == '.' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
}, isDigit)

var (
	units    = []string{"%", "％", "億", "萬", "千元"}
	unitMult = map[string]float64{"億": 1e8, "萬": 1e4, "千元": 1e3}
)

// replaceRule 把每個匹配換成等量(rune 數)空白。
func replaceRule(s string, r lookRule) string {
	var b strings.Builder
	last := 0
	r.each(s, func(m []int) {
		b.WriteString(s[last:m[0]])
		b.WriteString(strings.Repeat(" ", utf8.RuneCountInString(s[m[0]:m[1]])))
		last = m[1]
	})
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// runeIndexer 把遞增的 byte 位置換成 rune 索引(單趟、線性)。
type runeIndexer struct {
	s          string
	byteAt, at int
}

func (x *runeIndexer) index(b int) int {
	x.at += utf8.RuneCountInString(x.s[x.byteAt:b])
	x.byteAt = b
	return x.at
}

// ExtractNumbers 抓回答裡的數字主張。個位整數(無小數、無單位)不算,避免「近 5 日」這類雜訊。
func ExtractNumbers(text string) []NumToken {
	work := Half(text)
	for _, r := range skipRules {
		work = replaceRule(work, r)
	}
	runes := []rune(text)
	idx := runeIndexer{s: work}
	var out []NumToken
	line, lineAt := 0, 0
	numRule.each(work, func(m []int) {
		digits := work[m[2]:m[3]]
		rest := strings.TrimLeftFunc(work[m[1]:], IsJSSpace)
		unit := ""
		for _, u := range units {
			if strings.HasPrefix(rest, u) {
				unit = u
				break
			}
		}
		base := JSNumber(strings.ReplaceAll(digits, ",", ""))
		if math.IsInf(base, 0) || math.IsNaN(base) {
			return
		}
		if unit == "" && !strings.Contains(digits, ".") && base < 10 {
			return
		}
		at := idx.index(m[0])
		mult, ok := unitMult[unit]
		if !ok {
			mult = 1
		}
		for _, r := range runes[lineAt:at] { // 行號也遞增計算,避免每個數字都從頭掃(二次方)
			if r == '\n' {
				line++
			}
		}
		lineAt = at
		out = append(out, NumToken{Raw: string(runes[at : at+len(digits)]), Value: base * mult, Base: base, Unit: unit, Line: line, Index: at})
	})
	return out
}

// decimals 對應 (String(n).split(".")[1] ?? "").length,包含 JS 指數表示法的怪結果(1.5e-7 → "5e-7" → 4)。
func decimals(n float64) int {
	_, frac, _ := strings.Cut(JSNumberString(n), ".")
	return len(frac)
}

// Matches:依 value 的小數位數四捨五入相等,或相對誤差 ≤ 0.5%(正負號不論)。
func Matches(value float64, known []float64) bool {
	v := math.Abs(value)
	d := min(decimals(v), 10)
	for _, k0 := range known {
		k := math.Abs(k0)
		if v != 0 && math.Abs(k-v)/v <= 0.005 {
			return true
		}
		// toFixed(d) 最多把 k 移動 0.5(再加 v 的半個 ulp,v < 2^53 時 ≤ 0.5;k ≥ 2^53 時 toFixed 不改值):
		// |k−v| > 1 時 Number(k.toFixed(d)) 不可能等於 v,省掉 math/big。寫成 !(… > 1),NaN、Inf 照舊走原判斷。
		if !(math.Abs(k-v) > 1) && JSToFixedNumber(k, d) == v {
			return true
		}
	}
	return false
}

// CollectNumbers 遞迴收工具資料裡的數字(含字串裡的數字)。
// 不認得的 Go 型別(int、struct、json.Number…)先 JSON 正規化再收;轉不了就不收(只會多標記未核對)。
// map 依鍵排序走訪(JS 照插入順序;Go map 沒有順序),NumberBook 丟最舊時才固定。
func CollectNumbers(v any) []float64 { return collectNumbers(v, nil) }

func collectNumbers(v any, out []float64) []float64 {
	switch x := v.(type) {
	case nil, bool:
		return out
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			out = append(out, x)
		}
		return out
	case string:
		for _, t := range ExtractNumbers(x) {
			out = append(out, t.Base, t.Value)
		}
		if n := JSNumber(strings.ReplaceAll(x, ",", "")); TrimJS(x) != "" && !math.IsNaN(n) && !math.IsInf(n, 0) {
			out = append(out, n)
		}
		return out
	case []any:
		for _, e := range x {
			out = collectNumbers(e, out)
		}
		return out
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out = collectNumbers(x[k], out)
		}
		return out
	}
	n, err := CloneJSON[any](v)
	if err != nil {
		return out
	}
	return collectNumbers(n, out)
}

// FindUnverified:回答中對不到 known 的數字。帶單位的另試原數與千元換算;% 另試比率。
func FindUnverified(text string, known []float64) []NumToken {
	var out []NumToken
	for _, t := range ExtractNumbers(text) {
		cands := []float64{t.Value, t.Base}
		if _, ok := unitMult[t.Unit]; ok {
			cands = append(cands, t.Value/1e3)
		}
		if t.Unit == "%" || t.Unit == "％" {
			cands = append(cands, t.Base/100)
		}
		if !slices.ContainsFunc(cands, func(c float64) bool { return Matches(c, known) }) {
			out = append(out, t)
		}
	}
	return out
}

// MarkUnverified:對應行尾加 ⚠未核對,結尾補一行總數。只給畫面用。
func MarkUnverified(text string, unverified []NumToken) string {
	if len(unverified) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	marked := make([]bool, len(lines))
	for _, u := range unverified {
		if u.Line >= 0 && u.Line < len(lines) && !marked[u.Line] {
			marked[u.Line] = true
			lines[u.Line] += " ⚠未核對"
		}
	}
	return strings.Join(lines, "\n") + "\n\n有 " + strconv.Itoa(len(unverified)) + " 個數字無法對到工具資料"
}

const contextMax = 8000

// NumberBook:整段對話累積的已知數字(上限丟最舊)與最近工具資料(給複核)。長度一律是 UTF-16 code unit。
// 沒有上鎖,不可併發使用(Agent 一輪一輪排隊呼叫)。
type NumberBook struct {
	max  int
	nums []float64
	data []string
}

// DefaultNumberBookMax 是 NumberBook 的預設上限(Agent 建 NumberBook 時用)。
const DefaultNumberBookMax = 5000

// NewNumberBook:max 是已知數字的上限(預設 5000,即 DefaultNumberBookMax);≤ 0 什麼都不留。
func NewNumberBook(max int) *NumberBook { return &NumberBook{max: max} }

func (b *NumberBook) Add(ns []float64) {
	b.nums = append(b.nums, ns...)
	if len(b.nums) > b.max {
		b.nums = b.nums[len(b.nums)-max(b.max, 0):]
	}
}

// AddData 存原文、取用時才截:Context(mask) 要先套 mask 再截,截斷處才不會留半個數字。
func (b *NumberBook) AddData(s string) {
	b.data = append(b.data, s)
	size := func(d string) int { return min(UTF16Len(d), contextMax) + 1 }
	total := 0
	for _, d := range b.data {
		total += size(d)
	}
	for total > contextMax && len(b.data) > 1 {
		total -= size(b.data[0])
		b.data = b.data[1:]
	}
}

// Values 回已知數字的複本。
func (b *NumberBook) Values() []float64 { return slices.Clone(b.nums) }

// Clone 回獨立的複本(驗證器在別的 goroutine 讀複本,turn 之後改原本的互不影響)。
func (b *NumberBook) Clone() *NumberBook {
	return &NumberBook{max: b.max, nums: slices.Clone(b.nums), data: slices.Clone(b.data)}
}

// Context:每筆資料先套 mask(nil = 原樣)再截前 8000,接起來取最後 8000。
// 順序不能反:先截再遮,截斷處可能剩半個數字(600000 → 60000),遮罩對不到就外流。
func (b *NumberBook) Context(mask func(string) string) string {
	parts := make([]string, len(b.data))
	for i, d := range b.data {
		if mask != nil {
			d = mask(d)
		}
		parts[i] = SliceUTF16(d, contextMax)
	}
	return tailUTF16(strings.Join(parts, "\n"), contextMax)
}

// tailUTF16 對應 JS 的 s.slice(-n)。起點落在代理對中間時 JS 留孤立的低代理;這裡同 SliceUTF16,不收那個字元。
func tailUTF16(s string, n int) string {
	used := 0
	for i := len(s); i > 0; {
		r, w := utf8.DecodeLastRuneInString(s[:i])
		if used+utf16.RuneLen(r) > n {
			return s[i:]
		}
		used += utf16.RuneLen(r)
		i -= w
	}
	return s
}
