// Package entity 是框架的純資料與純函式層:訊息、隱私、遮蔽、稽核事件。
package entity

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Privacy 只有 "local" / "remote" 兩個有效值;其他值(含空字串 = 沒標記)一律由 PrivacyOf 當 local。
type Privacy string

const (
	Local  Privacy = "local"
	Remote Privacy = "remote"
)

// 角色與 part 類型。Go 沒有 sum type,用字串標籤 + 扁平 struct 表示 union。
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"

	PartText     = "text"
	PartThinking = "thinking"
	PartToolCall = "toolCall"
)

// Part 對應 TextPart | ThinkingPart | ToolCallPart,依 Type 決定哪些欄位有意義。
// Signature:provider 不透明的簽章(Gemini thoughtSignature),同一 provider 重送時要原樣帶回。
type Part struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Args      map[string]any `json:"args"`
	Signature string         `json:"signature"`
}

// Usage 是 token 用量。
type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// Message 對應 UserMessage | AssistantMessage | ToolResultMessage;Role 決定哪些欄位有意義。
type Message struct {
	Role    string  `json:"role"`
	Content []Part  `json:"content"`
	Privacy Privacy `json:"privacy"`
	Ts      int64   `json:"ts"`
	// assistant
	Provider     string `json:"provider"`
	StopReason   string `json:"stopReason"` // stop | toolUse | length | error | aborted
	ErrorMessage string `json:"errorMessage"`
	Usage        *Usage `json:"usage"`
	// toolResult
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
}

// PrivacyOf:隱私標記缺值或無法辨識一律當 local(spec §12 M2);只有剛好 "remote" 才回 remote。
func PrivacyOf(m Message) Privacy {
	if m.Privacy == Remote {
		return Remote
	}
	return Local
}

// TextOf 只串 text part(不含 thinking、toolCall)。
func TextOf(m Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// MarshalJSON 依 type 只輸出該型別有的欄位,鍵的順序固定(估 token、比對內容時要一致)。
func (p Part) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case PartText:
		return Stringify(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{p.Type, p.Text})
	case PartThinking:
		return Stringify(struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Signature string `json:"signature,omitempty"`
		}{p.Type, p.Text, p.Signature})
	case PartToolCall:
		args := p.Args
		if args == nil {
			args = map[string]any{}
		}
		return Stringify(struct {
			Type      string         `json:"type"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Args      map[string]any `json:"args"`
			Signature string         `json:"signature,omitempty"`
		}{p.Type, p.ID, p.Name, args, p.Signature})
	}
	// 無法辨識的 part:只留 type(其餘欄位不外帶)
	return Stringify(struct {
		Type string `json:"type"`
	}{p.Type})
}

// MarshalJSON 依角色白名單輸出欄位;沒標記的 privacy 不輸出。
func (m Message) MarshalJSON() ([]byte, error) {
	content := m.Content
	if content == nil {
		content = []Part{}
	}
	switch m.Role {
	case RoleAssistant:
		return Stringify(struct {
			Role         string  `json:"role"`
			Content      []Part  `json:"content"`
			Privacy      Privacy `json:"privacy,omitempty"`
			Ts           int64   `json:"ts"`
			Provider     string  `json:"provider"`
			StopReason   string  `json:"stopReason"`
			Usage        *Usage  `json:"usage,omitempty"`
			ErrorMessage string  `json:"errorMessage,omitempty"`
		}{m.Role, content, m.Privacy, m.Ts, m.Provider, m.StopReason, m.Usage, m.ErrorMessage})
	case RoleToolResult:
		return Stringify(struct {
			Role       string  `json:"role"`
			ToolCallID string  `json:"toolCallId"`
			ToolName   string  `json:"toolName"`
			Content    []Part  `json:"content"`
			IsError    bool    `json:"isError"`
			Privacy    Privacy `json:"privacy,omitempty"`
			Ts         int64   `json:"ts"`
		}{m.Role, m.ToolCallID, m.ToolName, content, m.IsError, m.Privacy, m.Ts})
	}
	return Stringify(struct {
		Role    string  `json:"role"`
		Content []Part  `json:"content"`
		Privacy Privacy `json:"privacy,omitempty"`
		Ts      int64   `json:"ts"`
	}{m.Role, content, m.Privacy, m.Ts})
}

// Stringify 是最接近 JSON.stringify 的輸出:不跳脫 <>&、U+2028/U+2029 保留原字元、結尾不帶換行。
// 差異:無效 UTF-8 換成 U+FFFD;map 的鍵會排序(JS 照插入順序)。
// U+2028 不能留跳脫:redact 的 jsWS 吃得到原字元,吃不到跳脫後的六個字,秘密會遮不到(審查 I3)。
func Stringify(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return unescapeLineSeps(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// unescapeLineSeps 把 encoding/json 一定會輸出的 U+2028、U+2029 跳脫序列換回原字元。
// 遇到反斜線就連同下一個位元組一起處理,所以使用者原文裡的反斜線加 u2028(輸出時反斜線變兩個)不會被誤換。
// 外層 encoder 收到 MarshalJSON 的輸出時,SetEscapeHTML(false) 下不會再跳脫,所以巢狀的 Part 也成立。
func unescapeLineSeps(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 == len(b) {
			out = append(out, b[i])
			continue
		}
		switch string(b[i+1 : min(i+6, len(b))]) {
		case "u2028":
			out = append(out, "\u2028"...)
			i += 5
		case "u2029":
			out = append(out, "\u2029"...)
			i += 5
		default:
			out = append(out, b[i], b[i+1])
			i++
		}
	}
	return out
}

// CloneJSON 用 JSON 來回做深複製兼正規化(相當於 JS 的 structuredClone + JSON 來回):
// 結果只剩 JSON 型別,任何外部參考、func、未匯出欄位都不會留下;無法序列化就回錯誤。
func CloneJSON[T any](v T) (T, error) {
	var out T
	b, err := Stringify(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}
