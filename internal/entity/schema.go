package entity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema 是工具參數的 JSON Schema。只支援框架用得到的子集(見 CompileSchema);
// 值用 Go literal(巢狀 Schema、[]string、int)或 encoding/json 解出來的都可以。
type Schema map[string]any

// ErrSchemaNotClosed:根層不是 additionalProperties:false。註冊端在前面接「工具 <名稱> 的 」,
// 就是完整的錯誤訊息。
var ErrSchemaNotClosed = errors.New("schema 必須是 additionalProperties:false")

// CompiledSchema 是註冊時編好的 schema。Check 的語意照 typebox 1.3.37 的 Value.Check。
type CompiledSchema struct{ root *node }

type node struct {
	typ                                string // "" = 不限型別
	props                              map[string]*node
	required                           []string
	closed                             bool // additionalProperties:false
	items                              *node
	enum                               []any
	hasEnum                            bool
	constV                             any
	hasConst                           bool
	anyOf                              []*node
	min, max, exMin                    *float64
	minLen, maxLen, minItems, maxItems int // -1 = 沒設
	pattern                            *regexp.Regexp
}

// CompileSchema 編譯 schema。只收這些關鍵字:type(object/string/number/integer/boolean/array/null)、properties、
// required、additionalProperties(只收 bool)、items(單一 schema)、enum、const、anyOf、minimum、maximum、
// exclusiveMinimum(只收數字)、minLength、maxLength、minItems、maxItems、
// pattern(JS 與 RE2 結果相同的子集)、description、title、default。其他關鍵字或值的型別不對 → error(fail closed;
// typebox 遇到不認得的 type 會直接放行,這裡不跟)。根層必須是 type:object 且 additionalProperties:false。
func CompileSchema(s Schema) (*CompiledSchema, error) {
	// 先 JSON 來回一次,各種 Go 寫法統一成 map[string]any / []any / float64;無法 JSON 化的值(func、NaN)在這裡就失敗。
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("schema 無法轉成 JSON:%w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("schema 無法轉成 JSON:%w", err)
	}
	if m["additionalProperties"] != false {
		return nil, ErrSchemaNotClosed
	}
	if m["type"] != "object" {
		return nil, errors.New("schema 根層必須是 type:object")
	}
	n, err := compileNode(m, "")
	if err != nil {
		return nil, err
	}
	return &CompiledSchema{n}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func compileNode(m map[string]any, at string) (*node, error) {
	where := at
	if where == "" {
		where = "根層"
	}
	n := &node{minLen: -1, maxLen: -1, minItems: -1, maxItems: -1}
	sub := func(v any, path string) (*node, error) {
		sm, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema %s 必須是物件", path)
		}
		return compileNode(sm, path)
	}
	for _, k := range sortedKeys(m) {
		v := m[k]
		// 出錯才格式化;值截到 80 字,大的 enum/const 不會讓訊息爆長
		bad := func() error { return fmt.Errorf("schema %s 的 %s 值不合法:%.80s", where, k, fmt.Sprint(v)) }
		switch k {
		case "type":
			s, _ := v.(string)
			switch s {
			case "object", "array", "string", "number", "integer", "boolean", "null":
				n.typ = s
			default:
				return nil, bad()
			}
		case "properties":
			ps, ok := v.(map[string]any)
			if !ok {
				return nil, bad()
			}
			n.props = map[string]*node{}
			for _, pk := range sortedKeys(ps) {
				c, err := sub(ps[pk], at+"/properties/"+pk)
				if err != nil {
					return nil, err
				}
				n.props[pk] = c
			}
		case "required":
			arr, ok := v.([]any)
			if !ok {
				return nil, bad()
			}
			for _, e := range arr {
				s, ok := e.(string)
				if !ok {
					return nil, bad()
				}
				n.required = append(n.required, s)
			}
		case "additionalProperties":
			b, ok := v.(bool)
			if !ok {
				return nil, bad()
			}
			n.closed = !b
		case "items":
			c, err := sub(v, at+"/items")
			if err != nil {
				return nil, err
			}
			n.items = c
		case "enum":
			arr, ok := v.([]any)
			if !ok {
				return nil, bad()
			}
			n.enum, n.hasEnum = arr, true
		case "const":
			n.constV, n.hasConst = v, true
		case "anyOf":
			arr, ok := v.([]any)
			if !ok || len(arr) == 0 {
				return nil, bad()
			}
			for i, e := range arr {
				c, err := sub(e, fmt.Sprintf("%s/anyOf/%d", at, i))
				if err != nil {
					return nil, err
				}
				n.anyOf = append(n.anyOf, c)
			}
		case "minimum", "maximum", "exclusiveMinimum":
			f, ok := v.(float64)
			if !ok {
				return nil, bad()
			}
			switch k {
			case "minimum":
				n.min = &f
			case "maximum":
				n.max = &f
			default:
				n.exMin = &f
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			f, ok := v.(float64)
			if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
				return nil, bad()
			}
			switch k {
			case "minLength":
				n.minLen = int(f)
			case "maxLength":
				n.maxLen = int(f)
			case "minItems":
				n.minItems = int(f)
			default:
				n.maxItems = int(f)
			}
		case "pattern":
			s, ok := v.(string)
			if !ok {
				return nil, bad()
			}
			re, err := jsPattern(s)
			if err != nil {
				return nil, fmt.Errorf("schema %s 的 pattern %q 不支援:%w", where, s, err)
			}
			n.pattern = re
		case "description", "title":
			if _, ok := v.(string); !ok {
				return nil, bad()
			}
		case "default":
		default:
			return nil, fmt.Errorf("schema %s 有不支援的關鍵字 %q", where, k)
		}
	}
	return n, nil
}

// Check 回傳 v 是否符合 schema。任何意外(含 panic)都當不符(fail closed)。
// 遞迴深度只跟著 schema 走,v 自己參照自己也不會無限遞迴。
func (c *CompiledSchema) Check(v any) (ok bool) {
	if c == nil || c.root == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return c.root.check(v)
}

var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// number:JSON 數字是 float64(UseNumber 時是 json.Number)。isNum = 是數字型別;finite = 是有限數。
func number(v any) (f float64, isNum, finite bool) {
	switch x := v.(type) {
	case float64:
		return x, true, !math.IsNaN(x) && !math.IsInf(x, 0)
	case json.Number:
		// 先照 JSON 數字文法檢查:strconv 會收 "inf"、"0x1p4",json.Unmarshal 會把 "null" 當成 0 收下。超出 float64 範圍也算壞掉
		if !jsonNumber.MatchString(string(x)) {
			return 0, true, false
		}
		f, err := strconv.ParseFloat(string(x), 64)
		return f, true, err == nil
	}
	return 0, false, false
}

func (n *node) check(v any) bool {
	if isTypedNil(v) {
		return false
	}
	f, isNum, finite := number(v)
	if isNum && !finite {
		// typebox 的 IsNumber 是 Number.isFinite;JSON 產生不出這種值,一律擋
		return false
	}
	switch n.typ {
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return false
		}
	case "string":
		if _, ok := v.(string); !ok {
			return false
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return false
		}
	case "null":
		if v != nil {
			return false
		}
	case "number":
		if !isNum {
			return false
		}
	case "integer":
		if !isNum || f != math.Trunc(f) {
			return false
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range n.required {
			if _, ok := x[k]; !ok {
				return false
			}
		}
		for k, val := range x {
			p, ok := n.props[k]
			if !ok {
				if n.closed {
					return false
				}
				continue
			}
			if !p.check(val) {
				return false
			}
		}
	case []any:
		if n.minItems >= 0 && len(x) < n.minItems || n.maxItems >= 0 && len(x) > n.maxItems {
			return false
		}
		if n.items != nil {
			for _, e := range x {
				if !n.items.check(e) {
					return false
				}
			}
		}
	case string:
		// typebox 1.x 的 minLength/maxLength 以 code point 計(不是 JS 的 .length)
		if n.minLen >= 0 || n.maxLen >= 0 {
			l := utf8.RuneCountInString(x)
			if n.minLen >= 0 && l < n.minLen || n.maxLen >= 0 && l > n.maxLen {
				return false
			}
		}
		if n.pattern != nil && !n.pattern.MatchString(x) {
			return false
		}
	}
	if isNum {
		if n.exMin != nil && !(f > *n.exMin) || n.min != nil && !(f >= *n.min) || n.max != nil && !(f <= *n.max) {
			return false
		}
	}
	if n.hasConst && !jsonEqual(n.constV, v) {
		return false
	}
	if n.hasEnum && !slices.ContainsFunc(n.enum, func(e any) bool { return jsonEqual(e, v) }) {
		return false
	}
	if len(n.anyOf) > 0 && !slices.ContainsFunc(n.anyOf, func(a *node) bool { return a.check(v) }) {
		return false
	}
	return true
}

// isTypedNil:typed nil 的 map/slice。encoding/json 產生不出來(JSON null 是 nil interface);
// 當成 {} / [] 會放行 typebox 眼中的 null,所以一律不過。
func isTypedNil(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return x == nil
	case []any:
		return x == nil
	}
	return false
}

// jsonEqual 對應 typebox 的 const/enum 比較(值用 ===,陣列/物件逐項)。want 來自 schema(JSON 正規化過),
// 遞迴只跟著 want 走;不用 == 比 any,兩邊都是 map/slice 時會 panic。
func jsonEqual(want, v any) bool {
	if isTypedNil(v) {
		return false
	}
	switch w := want.(type) {
	case nil:
		return v == nil
	case bool:
		b, ok := v.(bool)
		return ok && b == w
	case string:
		s, ok := v.(string)
		return ok && s == w
	case float64:
		f, isNum, finite := number(v)
		return isNum && finite && f == w
	case []any:
		a, ok := v.([]any)
		if !ok || len(a) != len(w) {
			return false
		}
		for i := range w {
			if !jsonEqual(w[i], a[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		m, ok := v.(map[string]any)
		if !ok || len(m) != len(w) {
			return false
		}
		for k, wv := range w {
			mv, ok := m[k]
			if !ok || !jsonEqual(wv, mv) {
				return false
			}
		}
		return true
	}
	return false
}

// jsSyntax 是 u 旗標下可以用 \ 跳脫成字面值的字元(ECMAScript SyntaxCharacter 加 /)。
const jsSyntax = `^$\.*+?()[]{}|/`

// jsEscape 是 \d \w \s 與其否定在 JS(u 旗標、無 i 旗標)的意思,寫成 RE2 字元類別內容。
var jsEscape = map[rune]string{
	'd': `0-9`, 'D': `^0-9`, 'w': `0-9A-Za-z_`, 'W': `^0-9A-Za-z_`, 's': jsWS, 'S': `^` + jsWS,
}

var braceQuant = regexp.MustCompile(`^[0-9]+(,[0-9]*)?$`)

// jsPattern 把 typebox 的 pattern(JS new RegExp(p, "u"))轉成結果相同的 RE2。只收這個子集:字面字元、.、
// 字元類別(範圍、否定、類別裡的 \d \w \s)、跳脫的語法字元、\d \D \w \W \s \S、( ) 與 (?: )、|、^ $、
// 量詞 * + ? {n} {n,} {n,m}(可加 ? 變 lazy)。lookaround、反向參照、具名群組、旗標、\b、\p、\u 等一律拒絕。
// 字面字元一律輸出成 \x{..},不會被 RE2 當成語法;只看「有沒有匹配」,所以貪婪/lazy、捕獲與否不影響結果。
func jsPattern(p string) (*regexp.Regexp, error) {
	rs := []rune(p)
	var b strings.Builder
	depth := 0
	canRepeat := false // 前一個是可以接量詞的東西(JS u 模式下 ^* 、a** 都是語法錯誤)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '^', '$', '|':
			// 沒有 m 旗標:^ $ 只在整段開頭/結尾,RE2 預設相同
			b.WriteRune(r)
			canRepeat = false
		case '(':
			if i+1 < len(rs) && rs[i+1] == '?' {
				if i+2 >= len(rs) || rs[i+2] != ':' {
					return nil, errors.New("不支援 (? 開頭的群組(lookaround、具名群組、旗標)")
				}
				i += 2
			}
			b.WriteString("(?:")
			depth++
			canRepeat = false
		case ')':
			if depth == 0 {
				return nil, errors.New("括號不成對")
			}
			depth--
			b.WriteByte(')')
			canRepeat = true
		case '*', '+', '?', '{':
			if !canRepeat {
				return nil, errors.New("量詞前面沒有可重複的東西")
			}
			if r == '{' {
				j := i + 1
				for j < len(rs) && rs[j] != '}' {
					j++
				}
				if j >= len(rs) || !braceQuant.MatchString(string(rs[i+1:j])) {
					return nil, errors.New("{ 不是合法的量詞")
				}
				b.WriteString(string(rs[i : j+1]))
				i = j
			} else {
				b.WriteRune(r)
			}
			if i+1 < len(rs) && rs[i+1] == '?' {
				b.WriteByte('?')
				i++
			}
			canRepeat = false
		case '}', ']':
			return nil, fmt.Errorf("單獨的 %c", r)
		case '.':
			// JS 的 . 不吃四種行結束字元;RE2 只排除 \n
			b.WriteString(`[^\n\r\x{2028}\x{2029}]`)
			canRepeat = true
		case '[':
			j, err := jsClass(rs, i, &b)
			if err != nil {
				return nil, err
			}
			i = j
			canRepeat = true
		case '\\':
			if i+1 >= len(rs) {
				return nil, errors.New(`結尾是單獨的 \`)
			}
			i++
			if set, ok := jsEscape[rs[i]]; ok {
				b.WriteString("[" + set + "]")
			} else if strings.ContainsRune(jsSyntax, rs[i]) {
				fmt.Fprintf(&b, `\x{%x}`, rs[i])
			} else {
				return nil, fmt.Errorf(`不支援的跳脫 \%c`, rs[i])
			}
			canRepeat = true
		default:
			fmt.Fprintf(&b, `\x{%x}`, r)
			canRepeat = true
		}
	}
	if depth != 0 {
		return nil, errors.New("括號不成對")
	}
	// 範圍 n > m、次數超過 1000 等,RE2 自己會拒絕
	return regexp.Compile(b.String())
}

// jsClass 轉換從 rs[i](即 [)開始的字元類別,回傳結尾 ] 的位置。
func jsClass(rs []rune, i int, b *strings.Builder) (int, error) {
	i++
	b.WriteByte('[')
	if i < len(rs) && rs[i] == '^' {
		b.WriteByte('^')
		i++
	}
	if i < len(rs) && rs[i] == ']' {
		// JS 的 [] 什麼都不配、[^] 什麼都配;RE2 會把 ] 當字面值,直接拒絕
		return 0, errors.New("不支援空的字元類別")
	}
	for i < len(rs) && rs[i] != ']' {
		lo, set, n, err := classAtom(rs, i)
		if err != nil {
			return 0, err
		}
		i += n
		if i+1 < len(rs) && rs[i] == '-' && rs[i+1] != ']' {
			hi, set2, n2, err := classAtom(rs, i+1)
			if err != nil {
				return 0, err
			}
			if set != "" || set2 != "" || lo > hi {
				return 0, errors.New("字元類別的範圍不合法")
			}
			fmt.Fprintf(b, `\x{%x}-\x{%x}`, lo, hi)
			i += 1 + n2
			continue
		}
		if set != "" {
			b.WriteString(set)
		} else {
			fmt.Fprintf(b, `\x{%x}`, lo)
		}
	}
	if i >= len(rs) {
		return 0, errors.New("字元類別沒有結尾的 ]")
	}
	b.WriteByte(']')
	return i, nil
}

// classAtom 讀字元類別裡的一個元素:一般字元回 r;\d \w \s 回 set(類別裡不收否定的 \D \W \S)。n 是用掉幾個 rune。
func classAtom(rs []rune, i int) (r rune, set string, n int, err error) {
	if rs[i] != '\\' {
		return rs[i], "", 1, nil
	}
	if i+1 >= len(rs) {
		return 0, "", 0, errors.New(`結尾是單獨的 \`)
	}
	switch e := rs[i+1]; {
	case e == 'd' || e == 'w' || e == 's':
		return 0, jsEscape[e], 2, nil
	case e == '-' || strings.ContainsRune(jsSyntax, e):
		return e, "", 2, nil
	default:
		return 0, "", 0, fmt.Errorf(`字元類別裡不支援的跳脫 \%c`, e)
	}
}
