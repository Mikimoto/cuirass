package entity

import (
	"slices"
	"strings"
)

// Best-effort 字串遮蔽:擋掉私鑰、env 秘密、連線字串帳密、Authorization header、API token、email 這類「值」(不動識別字)。

// RedactPattern 是一條遮蔽規則。Replace 可用 $1..$9 引用捕獲組(未參與的組展開成空字串)。
// 每條規則是一個正規式;Go 的 RE2 沒有 lookaround,有用到的規則改由 rule 補上 Go 程式碼。
type RedactPattern struct {
	Name    string
	Replace string
	rule    lookRule
}

// NewRedactPattern 建立沒有 lookaround 的規則(呼叫端自訂規則用;expr 是 RE2 語法)。
// RE2 的 \s 只認 ASCII 空白,JS 的 \s 還含 Unicode 空白:要照 JS 語意,空白要自己列成字元集
// `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`(內建規則用的 jsWS)。
func NewRedactPattern(name, expr, replace string) RedactPattern {
	return RedactPattern{Name: name, Replace: replace, rule: newLookRule(expr, nil, nil)}
}

func isEnvName(r rune) bool { return r >= 'A' && r <= 'Z' || isDigit(r) || r == '_' }

func isSchemeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || isDigit(r) || r == '+' || r == '.' || r == '-'
}

var envKeywords = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD"}

// envBefore 取代原式的 (?<![A-Z0-9_])(?=[A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD)):
// 起點前一字元不能是名稱字元(所以起點一定是整段名稱的開頭);lookahead 裡的關鍵字字元本身也是名稱字元,
// 所以「這段名稱(到第一個非名稱字元為止)含關鍵字」與原 lookahead 等價。每段名稱只掃一次;
// 規則用錨定模式,關鍵字不中就不跑 RE2(見 lookRule.anchored)。
func envBefore(s string, i int) bool {
	if r, _, ok := prevRune(s, i); ok && isEnvName(r) {
		return false
	}
	end := i
	for end < len(s) && isEnvName(rune(s[end])) {
		end++
	}
	name := s[i:end]
	for _, k := range envKeywords {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}

// connBefore 取代 (?<![a-zA-Z0-9+.-]):scheme 只能從整段 scheme 字元的開頭算起,避免從每個字母重掃(二次方)。
func connBefore(s string, i int) bool {
	r, _, ok := prevRune(s, i)
	return !ok || !isSchemeChar(r)
}

// DefaultRedactPatterns 回預設規則的複本。
func DefaultRedactPatterns() []RedactPattern { return slices.Clone(defaultRedactPatterns) }

// defaultRedactPatterns:起始保守集合。順序有意義:先處理整段結構(私鑰、env、auth header、連線字串),
// 再處理單值(token、email),避免後者把前者切半。
// 工具結果是 JSON 序列化後才遮蔽,所以 private-key、env-secret 要對得到跳脫後的字串(\n 字面、引號)。
// 凡是原式寫 \s 的地方都換成 jsWS(見 js.go);(?s:.) 等同 JS 的 [\s\S]。
var defaultRedactPatterns = []RedactPattern{
	// 中間可含真換行或 JSON 跳脫的 \n;沒有 END(輸出被截斷)就從 BEGIN 遮到字串尾
	NewRedactPattern("private-key",
		`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:(?s:.)*?-----END [A-Z ]*PRIVATE KEY-----|(?s:.)*$)`,
		"[REDACTED:private-key]"),
	{
		// NAME=value:保留名稱,只遮值。lookbehind 與 lookahead 由 envBefore 處理(見上)。
		// 值可為單引號整段、雙引號包住(原樣或 JSON 跳脫的 \"),或未加引號(在空白、引號、反斜線處結束,
		// 但引號後緊接字母數字視為值的一部分,如 ab"cd)。
		Name:    "env-secret",
		Replace: "$1[REDACTED:env]",
		rule: newAnchoredRule(`([A-Z0-9_]+[`+jsWS+`]*=[`+jsWS+`]*)(?:'[^']*'|\\?"[^"\\]*\\?"|[^`+jsWS+`"\\]+(?:"[A-Za-z0-9_][^`+jsWS+`"\\]*)*)`,
			envBefore, isEnvName),
	},
	// JSON 鍵值:鍵名(不分大小寫)以 key / token / secret / password / passwd / pwd 結尾,值是字串 → 保留鍵名,只遮值。
	// 工具結果先 JSON 序列化才遮蔽,{"password":"x"}、{"apiKey":"x"} 這種 env-secret(NAME=值)對不到。
	// 只認結尾,max_tokens、keyboard 這類鍵不誤遮。引號可帶反斜線:工具回的字串裡又包 JSON 時會再跳脫一次。
	NewRedactPattern("json-secret",
		`(?i)(\\?"[A-Za-z0-9_.-]*(?:key|token|secret|password|passwd|pwd)\\?"[ \t]*:[ \t]*\\?")(?:[^"\\]|\\[^"])+`,
		"$1[REDACTED:json-secret]"),
	// 兼容 JSON 物件形式("Authorization":"Bearer x");值在引號、反斜線處結束,第二段只吃同行空白。
	// 注意 Go 的 (?i) 用 Unicode 摺疊(k 會配到 K 開爾文符號、s 配到 ſ),JS 非 u 模式只摺 ASCII;
	// 這兩個標頭名稱沒有 k、s,結果相同。
	NewRedactPattern("auth-header",
		`(?i)\b(Authorization|Proxy-Authorization)(["']?[`+jsWS+`]*:[`+jsWS+`]*["']?)[^`+jsWS+`"'\\]+(?:[ \t]+[^`+jsWS+`"'\\]+)?`,
		"$1$2[REDACTED:auth-header]"),
	{
		// scheme://user:pass@host → 保留 scheme 與 host,只遮帳密
		Name:    "conn-string-credentials",
		Replace: "$1[REDACTED:credentials]@",
		rule:    newLookRule(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^`+jsWS+`:/@]+:[^`+jsWS+`:/@]+@`, connBefore, isSchemeChar),
	},
	NewRedactPattern("jwt", `eyJ[\w-]+\.[\w-]+\.[\w-]+`, "[REDACTED:jwt]"),
	NewRedactPattern("slack-token", `xox[abprs]-[\w-]{10,}`, "[REDACTED:slack-token]"),
	// 必須在 openai-key 之前,否則 sk-ant- 會被當 openai 風格吃掉
	NewRedactPattern("anthropic-key", `sk-ant-[\w-]{16,}`, "[REDACTED:api-key]"),
	// OpenAI 風格 sk-/sk-proj- token
	NewRedactPattern("openai-key", `\bsk-(?:proj-)?[A-Za-z0-9_-]{16,}\b`, "[REDACTED:api-key]"),
	// GitHub token:ghp_/gho_/ghu_/ghs_/ghr_ + 36+ 字元
	NewRedactPattern("github-token", `\bgh[pousr]_[A-Za-z0-9]{36,}\b`, "[REDACTED:api-key]"),
	NewRedactPattern("aws-access-key-id", `\bAKIA[0-9A-Z]{16}\b`, "[REDACTED:api-key]"),
	NewRedactPattern("gemini-key", `AIza[0-9A-Za-z_-]{35}`, "[REDACTED:gemini-key]"),
	// email:需有 @ 與帶 TLD 的 domain,故 email_verified 這種欄名不會命中
	NewRedactPattern("email", `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, "[REDACTED:email]"),
}

// RedactCount 是純函式:依序套用 patterns(nil = 預設)後回傳新字串與命中筆數。
func RedactCount(text string, patterns []RedactPattern) (string, int) {
	if patterns == nil {
		patterns = defaultRedactPatterns
	}
	out, count := text, 0
	for _, p := range patterns {
		var b strings.Builder
		last, hit := 0, false
		src := out
		p.rule.each(src, func(m []int) {
			count++
			hit = true
			b.WriteString(src[last:m[0]])
			b.WriteString(expand(p.Replace, src, m))
			last = m[1]
		})
		if hit {
			b.WriteString(src[last:])
			out = b.String()
		}
	}
	return out, count
}

// Redact 只回字串。
func Redact(text string) string {
	s, _ := RedactCount(text, nil)
	return s
}

// expand 展開 $1..$9(只認一位數)。
func expand(tmpl, src string, m []int) string {
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] == '$' && i+1 < len(tmpl) && isDigit(rune(tmpl[i+1])) {
			n := int(tmpl[i+1] - '0')
			if 2*n+1 < len(m) && m[2*n] >= 0 {
				b.WriteString(src[m[2*n]:m[2*n+1]])
			}
			i++
			continue
		}
		b.WriteByte(tmpl[i])
	}
	return b.String()
}
