package entity

import (
	"encoding/json"
	"regexp"
)

// AuditEvent 是稽核事件。字串欄位空字串 = 不寫;
// Args 為 nil = 不寫;Count 為 nil = 不寫(0 也要寫成 count: 0,所以用指標)。
type AuditEvent struct {
	Kind   string         `json:"kind"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Host   string         `json:"host,omitempty"`
	Reason string         `json:"reason,omitempty"`
	// Count 是數量(如未核對數字個數),不記內容。
	Count *int   `json:"count,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// AuditSink:Write 回 false = 寫入失敗。
type AuditSink interface {
	Write(e AuditEvent) bool
}

type nullAudit struct{}

func (nullAudit) Write(AuditEvent) bool { return true }

// NullAudit 回什麼都不記的 AuditSink,永遠成功;測試與未設定時用。
func NullAudit() AuditSink { return nullAudit{} }

var ctrlRun = regexp.MustCompile(`[\x00-\x1f\x7f-\x9f]+`)

// OneLine 去控制字元(含換行、ESC),壓成單行。
// max > 0 時再截到 max 個 UTF-16 code unit(先壓單行再截);max ≤ 0 = 不截。
func OneLine(s string, max int) string {
	s = TrimJS(ctrlRun.ReplaceAllString(s, " "))
	if max > 0 {
		s = SliceUTF16(s, max)
	}
	return s
}

// FilterAuditArgs 只留 keys 內的原始型別值;字串先 redact 再 OneLine 並截 200 字(spec §12 H1)。物件/陣列丟棄。
// keys 為 nil 回空 map。數字收所有 Go 數值型別與 json.Number;
// NaN / ±Inf 照樣保留,寫檔端要自己處理 JSON 無法表示的值。
func FilterAuditArgs(args map[string]any, keys []string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		v, ok := args[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			out[k] = OneLine(Redact(x), 200)
		case bool, json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
			out[k] = x
		}
	}
	return out
}
