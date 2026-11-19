package entity

import (
	"regexp"
	"unicode/utf8"
)

// lookRule 用 RE2 加一點 Go 程式碼,模擬 JS 正規式的 lookbehind 與結尾的 negative lookahead。
//
//   - 結尾的 (?!C) 改寫成會吃掉一個字元的具名組 (?P<la>[^C]|\z)。RE2 是 leftmost-first:在同一起點選的分支
//     與回溯式引擎(JS)相同,所以「本體在哪裡停」與 JS 一致;真正的結尾 = la 組的起點,下一次從那裡接著找。
//   - 開頭的 lookbehind 由 before(s, i) 在 Go 裡判斷。不成立時不能只跳一個字元(長數字串會變二次方),
//     改成跳過 skip 字元類的整段:呼叫端要保證這段裡每個位置的 lookbehind 也都不成立。
//
// 因為是在 s[p:] 上搜尋,本體開頭不能用 ^ 或 \b(切片會讓它們看不到前文);有需要的改由 before 處理。
type lookRule struct {
	re     *regexp.Regexp
	before func(s string, i int) bool // nil = 沒有 lookbehind
	skip   func(r rune) bool          // lookbehind 失敗時要整段跳過的字元類
	// skipAt 有設時取代 skip:回傳下一個可能成立的起點(> i)。給「整段跳過」要看前文才判斷得了的規則。
	skipAt func(s string, i int) int
	la     int // la 組的編號;-1 = 沒有 lookahead
	// anchored:不做 unanchored 搜尋,而是逐一走過 before 成立的起點、在該點做錨定匹配。
	// 給本體可能很長、而 before 能便宜地先排除的規則(env-secret:值可一路吃到字串尾,
	// 若先跑 RE2 再檢查關鍵字,每段名稱都會掃到字串尾 → 二次方)。
	anchored bool
}

// newAnchoredRule 見 lookRule.anchored。
func newAnchoredRule(expr string, before func(string, int) bool, skip func(rune) bool) lookRule {
	r := newLookRule(`^(?:`+expr+`)`, before, skip)
	r.anchored = true
	return r
}

func newLookRule(expr string, before func(string, int) bool, skip func(rune) bool) lookRule {
	re := regexp.MustCompile(expr)
	return lookRule{re: re, before: before, skip: skip, la: re.SubexpIndex("la")}
}

// each 依序回報每個不重疊的匹配(與 JS 的 g flag 相同),m 是相對 s 的組位置,m[1] 已是真正結尾。
func (r lookRule) each(s string, fn func(m []int)) {
	if r.before == nil && r.la < 0 {
		// 沒有 lookaround:整串交給 RE2,\b、^ 看得到完整前文
		for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
			fn(m)
		}
		return
	}
	if r.anchored {
		for p := 0; p < len(s); {
			var loc []int
			if r.before(s, p) {
				loc = r.re.FindStringSubmatchIndex(s[p:])
			}
			if loc == nil || loc[1] == 0 {
				p = skipRun(s, p, r.skip)
				continue
			}
			for i := range loc {
				if loc[i] >= 0 {
					loc[i] += p
				}
			}
			fn(loc)
			p = loc[1]
		}
		return
	}
	for p := 0; p <= len(s); {
		loc := r.re.FindStringSubmatchIndex(s[p:])
		if loc == nil {
			return
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += p
			}
		}
		start := loc[0]
		if r.before != nil && !r.before(s, start) {
			if r.skipAt != nil {
				p = r.skipAt(s, start)
			} else {
				p = skipRun(s, start, r.skip)
			}
			continue
		}
		if r.la >= 0 {
			loc[1] = loc[2*r.la]
			loc[2*r.la], loc[2*r.la+1] = -1, -1
		}
		fn(loc)
		switch {
		case loc[1] > start:
			p = loc[1]
		default: // 空匹配:往前一個字元,避免無窮迴圈
			_, w := utf8.DecodeRuneInString(s[start:])
			p = start + max(w, 1)
		}
	}
}

// skipRun 回傳 i 之後第一個「前一字元不在 class 裡」的位置:從 i 起整段 class 字元都跳過,再多跳一個。
func skipRun(s string, i int, class func(rune) bool) int {
	for i < len(s) {
		r, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if !class(r) {
			break
		}
	}
	return i
}

// prevRune 回傳 s[:i] 的最後一個字元;i == 0 時 ok = false。
func prevRune(s string, i int) (rune, int, bool) {
	if i == 0 {
		return 0, 0, false
	}
	r, w := utf8.DecodeLastRuneInString(s[:i])
	return r, w, true
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
