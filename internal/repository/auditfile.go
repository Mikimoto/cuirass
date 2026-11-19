package repository

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// clean:寫入端一律先遮蔽再壓單行(呼叫端給的欄位可能帶參數原文或控制字元)。
func clean(s string) string { return entity.OneLine(entity.Redact(s), 0) }

// auditRecord 的欄位順序固定;字串空值與 nil 不寫。
type auditRecord struct {
	Ts     string `json:"ts"`
	Kind   string `json:"kind"`
	Tool   string `json:"tool,omitempty"`
	Args   any    `json:"args,omitempty"` // any:空 map 仍寫 {}
	Host   string `json:"host,omitempty"`
	Reason string `json:"reason,omitempty"`
	Count  *int   `json:"count,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// fileAudit 是 append-only 稽核日誌(JSON Lines)。建立時不碰檔案系統;寫入失敗回 false、只 warn 一次、永不 panic。
type fileAudit struct {
	dir    string
	warn   func(string)
	mu     sync.Mutex
	warned bool
}

// NewFileAudit 建立寫檔的稽核。warn 為 nil 時寫到標準 log(stderr):稽核寫不進去不能沒有聲音。
func NewFileAudit(dir string, warn func(string)) entity.AuditSink {
	if warn == nil {
		warn = func(msg string) { log.Println("cuirass audit:", msg) }
	}
	return &fileAudit{dir: dir, warn: warn}
}

func (a *fileAudit) Write(e entity.AuditEvent) bool {
	msg, ok := a.write(e)
	if msg != "" && a.warn != nil {
		// 鎖外呼叫:warn 回頭寫同一個 sink 也不會死鎖;warn panic 也不外洩
		func() {
			defer func() { _ = recover() }()
			a.warn(msg)
		}()
	}
	return ok
}

// write 持鎖寫一行;失敗且還沒 warn 過時回傳要 warn 的訊息。
func (a *fileAudit) write(e entity.AuditEvent) (msg string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			msg, ok = a.fail(fmt.Errorf("%v", r)), false
		}
	}()
	line, err := encodeRecord(e)
	if err == nil {
		err = appendLog(a.dir, line)
	}
	if err != nil {
		return a.fail(err), false
	}
	return "", true
}

// fail 只在第一次失敗回傳 warn 訊息(呼叫端持鎖)。
func (a *fileAudit) fail(err error) string {
	if a.warned {
		return ""
	}
	a.warned = true
	return "audit 寫入失敗:" + entity.OneLine(entity.Redact(err.Error()), 0)
}

func encodeRecord(e entity.AuditEvent) ([]byte, error) {
	rec := auditRecord{
		Ts:     time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Kind:   clean(e.Kind),
		Tool:   clean(e.Tool),
		Host:   clean(e.Host),
		Reason: clean(e.Reason),
		Count:  e.Count,
		From:   clean(e.From),
		To:     clean(e.To),
		Agent:  clean(e.Agent),
	}
	if e.Args != nil {
		// 呼叫端不一定先過濾過;寫入端再套同一套規則(只留原始型別、字串遮蔽截斷)
		keys := make([]string, 0, len(e.Args))
		for k := range e.Args {
			keys = append(keys, k)
		}
		args := entity.FilterAuditArgs(e.Args, keys)
		for k, v := range args {
			args[k] = jsonSafe(v)
		}
		rec.Args = args
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 與 JSON.stringify 一樣不轉 < > &
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil // Encode 已附 "\n"
}

// jsonSafe:JSON 寫不出來的值(NaN、±Inf、不合法的 json.Number)照 JSON.stringify 的規則寫成 null,不丟事件。
func jsonSafe(v any) any {
	if _, err := json.Marshal(v); err != nil {
		return nil
	}
	return v
}
