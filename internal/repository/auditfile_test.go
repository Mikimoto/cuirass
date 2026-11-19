//go:build test && unix

package repository

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

const auditKey = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"

func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func logLines(t *testing.T, dir string) []string {
	return strings.Split(strings.TrimRight(readLog(t, dir), "\n"), "\n")
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// notDir 用「目錄路徑其實是檔案」製造失敗(寫入時會把目錄 chmod 回 0700)。
func notDir(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileAudit(t *testing.T) {
	t.Run("write 成功回 true,每筆一行 JSON 含 ts", func(t *testing.T) {
		dir := t.TempDir()
		a := NewFileAudit(dir, nil)
		if !a.Write(entity.AuditEvent{Kind: "tool", Tool: "quote"}) || !a.Write(entity.AuditEvent{Kind: "block", Reason: "x"}) {
			t.Fatal("write 失敗")
		}
		ls := logLines(t, dir)
		if len(ls) != 2 {
			t.Fatal(ls)
		}
		iso := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
		for _, l := range ls {
			var rec map[string]any
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				t.Fatal(err)
			}
			if ts, ok := rec["ts"].(string); !ok || !iso.MatchString(ts) {
				t.Fatal(rec["ts"])
			}
		}
	})

	t.Run("寫入失敗回 false,warn 只呼叫一次,不丟例外", func(t *testing.T) {
		var msgs []string
		a := NewFileAudit(notDir(t), func(m string) { msgs = append(msgs, m) })
		if a.Write(entity.AuditEvent{Kind: "tool"}) || a.Write(entity.AuditEvent{Kind: "tool"}) {
			t.Fatal("應回 false")
		}
		if len(msgs) != 1 || !strings.HasPrefix(msgs[0], "audit 寫入失敗:") {
			t.Fatal(msgs)
		}
	})

	t.Run("warn 為 nil 時寫到標準 log,不會沒聲音", func(t *testing.T) {
		var buf strings.Builder
		log.SetOutput(&buf)
		defer log.SetOutput(os.Stderr)
		NewFileAudit(notDir(t), nil).Write(entity.AuditEvent{Kind: "tool"})
		if !strings.Contains(buf.String(), "cuirass audit: audit 寫入失敗:") {
			t.Fatalf("%q", buf.String())
		}
	})

	t.Run("warn 丟例外也不外洩", func(t *testing.T) {
		a := NewFileAudit(notDir(t), func(string) { panic("boom") })
		if a.Write(entity.AuditEvent{Kind: "tool"}) {
			t.Fatal("應回 false")
		}
	})

	t.Run("reason 與 args 的控制字元壓成一行", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "block", Reason: "a\x1b[31mb\nc", Args: map[string]any{"q": "q\n\x1b[0m"}})
		raw := readLog(t, dir)
		if len(strings.Split(strings.TrimRight(raw, "\n"), "\n")) != 1 || strings.Contains(raw, "\x1b") || strings.Contains(raw, `\u001b`) {
			t.Fatal(raw)
		}
	})

	t.Run("reason 內的 key 先遮蔽再寫入", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "block", Reason: auditKey})
		if strings.Contains(readLog(t, dir), auditKey) {
			t.Fatal(readLog(t, dir))
		}
	})

	t.Run("host/tool/from/to/agent 也先遮蔽再壓單行", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "egress_block", Host: auditKey + "\n", Tool: auditKey, From: auditKey, To: auditKey, Agent: "a\x1b" + auditKey})
		raw := readLog(t, dir)
		if strings.Contains(raw, auditKey) || strings.Contains(raw, `\u001b`) || len(logLines(t, dir)) != 1 {
			t.Fatal(raw)
		}
		var rec map[string]any
		_ = json.Unmarshal([]byte(raw), &rec)
		for _, k := range []string{"host", "tool", "from", "to", "agent"} {
			if s, _ := rec[k].(string); !strings.Contains(s, "[REDACTED") {
				t.Fatal(k, rec)
			}
		}
	})

	t.Run("目錄已存在且權限太寬 → 收回 0700;既有檔案收回 0600", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "wide")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "audit.log"), nil, 0o644)
		_ = os.Chmod(filepath.Join(dir, "audit.log"), 0o644)
		if !NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool"}) {
			t.Fatal("write 失敗")
		}
		if perm(t, dir) != 0o700 || perm(t, filepath.Join(dir, "audit.log")) != 0o600 {
			t.Fatal(perm(t, dir), perm(t, filepath.Join(dir, "audit.log")))
		}
	})
}

// TestAuditFilePerms:目錄 0700、檔案 0600。
func TestAuditFilePerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	if !NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool"}) {
		t.Fatal("write 失敗")
	}
	if perm(t, dir) != 0o700 || perm(t, filepath.Join(dir, "audit.log")) != 0o600 {
		t.Fatal(perm(t, dir), perm(t, filepath.Join(dir, "audit.log")))
	}
}

// TestAuditFileSymlinkRefused:audit.log 是 symlink → 拒寫回 false,不寫到連結目標。
func TestAuditFileSymlinkRefused(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(target, 0o644)
	dir := filepath.Join(tmp, "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "audit.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool"}) {
		t.Fatal("symlink 應拒寫")
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Fatal(string(b))
	}
	if perm(t, target) != 0o644 {
		t.Fatal(perm(t, target))
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink 被換掉", err)
	}
}

// TestAuditFileFiltersArgs:args 在寫入端也套 FilterAuditArgs 規則;kind 也先遮蔽再壓單行。
func TestAuditFileFiltersArgs(t *testing.T) {
	t.Run("args 在寫入端也套 filterAuditArgs 規則:字串遮蔽、壓單行、截 200;物件/陣列丟棄", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool", Args: map[string]any{
			"k": auditKey, "long": "a\n" + strings.Repeat("x", 500), "o": map[string]any{"secret": auditKey}, "arr": []any{auditKey}, "n": 5, "b": false,
		}})
		raw := readLog(t, dir)
		if strings.Contains(raw, auditKey) {
			t.Fatal(raw)
		}
		var rec struct{ Args map[string]any }
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatal(err)
		}
		if len(rec.Args) != 4 || rec.Args["n"] != 5.0 || rec.Args["b"] != false {
			t.Fatal(rec.Args)
		}
		if k, _ := rec.Args["k"].(string); !strings.Contains(k, "[REDACTED") {
			t.Fatal(rec.Args["k"])
		}
		if l, _ := rec.Args["long"].(string); entity.UTF16Len(l) != 200 || strings.Contains(l, "\n") {
			t.Fatal(rec.Args["long"])
		}
	})
	t.Run("kind 也先遮蔽再壓單行", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "x " + auditKey + "\n\x1b"})
		raw := readLog(t, dir)
		if strings.Contains(raw, auditKey) || strings.Contains(raw, `\u001b`) || len(logLines(t, dir)) != 1 {
			t.Fatal(raw)
		}
	})
	t.Run("先遮蔽再截:key 跨在第 200 字上也不留片段", func(t *testing.T) {
		dir := t.TempDir()
		NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool", Args: map[string]any{"q": strings.Repeat("x", 190) + auditKey}})
		raw := readLog(t, dir)
		if strings.Contains(raw, "sk-ant") || !strings.Contains(raw, "[RED") {
			t.Fatal(raw)
		}
	})
}

// JSON 寫不出來的數字照 JSON.stringify 的規則寫成 null,不丟事件。
func TestAuditFileNonFiniteNumbers(t *testing.T) {
	dir := t.TempDir()
	ok := NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool", Args: map[string]any{
		"nan": math.NaN(), "inf": math.Inf(1), "ninf": math.Inf(-1), "f32": float32(math.Inf(1)),
		"bad": json.Number("abc"), "good": json.Number("1.5"), "n": 2,
	}})
	if !ok {
		t.Fatal("NaN/Inf 不該讓寫入失敗")
	}
	var rec struct{ Args map[string]any }
	if err := json.Unmarshal([]byte(readLog(t, dir)), &rec); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nan": nil, "inf": nil, "ninf": nil, "f32": nil, "bad": nil, "good": 1.5, "n": 2.0}
	for k, v := range want {
		if got, has := rec.Args[k]; !has || got != v {
			t.Fatal(k, got, has)
		}
	}
}

// count 有值(含 0)才寫;空 args map 寫 {};nil args 不寫;欄位順序固定。
func TestAuditFileOptionalFields(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	a := NewFileAudit(dir, nil)
	a.Write(entity.AuditEvent{Kind: "k", Count: &zero, Args: map[string]any{}})
	a.Write(entity.AuditEvent{Kind: "k"})
	ls := logLines(t, dir)
	if !regexp.MustCompile(`^\{"ts":"[^"]+","kind":"k","args":\{\},"count":0\}$`).MatchString(ls[0]) {
		t.Fatal(ls[0])
	}
	if !regexp.MustCompile(`^\{"ts":"[^"]+","kind":"k"\}$`).MatchString(ls[1]) {
		t.Fatal(ls[1])
	}
	a.Write(entity.AuditEvent{Kind: "k", Tool: "t", Args: map[string]any{"a": 1}, Host: "h", Reason: "r", Count: &zero, From: "f", To: "o", Agent: "g"})
	if !regexp.MustCompile(`^\{"ts":"[^"]+","kind":"k","tool":"t","args":\{"a":1\},"host":"h","reason":"r","count":0,"from":"f","to":"o","agent":"g"\}$`).MatchString(logLines(t, dir)[2]) {
		t.Fatal(logLines(t, dir)[2])
	}
}

// 併發寫入不交錯、不漏(-race 下跑)。
func TestAuditFileConcurrent(t *testing.T) {
	dir := t.TempDir()
	a := NewFileAudit(dir, nil)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if !a.Write(entity.AuditEvent{Kind: "tool", Reason: strings.Repeat("r", 5000)}) {
				t.Error("write 失敗")
			}
		})
	}
	wg.Wait()
	ls := logLines(t, dir)
	if len(ls) != 50 {
		t.Fatal(len(ls))
	}
	for _, l := range ls {
		if !json.Valid([]byte(l)) {
			t.Fatal(l)
		}
	}
}

// 對應 describe("nullAudit")。
func TestNullAudit(t *testing.T) {
	t.Run("永遠回 true", func(t *testing.T) {
		if !entity.NullAudit().Write(entity.AuditEvent{Kind: "tool"}) {
			t.Fatal("應回 true")
		}
	})
}

// audit.log 是沒有讀端的 FIFO → 立刻拒寫(不在持鎖時卡住),之後的寫入也不會卡。
func TestAuditFileFIFORefused(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "audit.log"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewFileAudit(dir, nil)
	done := make(chan bool, 1)
	go func() { done <- a.Write(entity.AuditEvent{Kind: "tool"}) || a.Write(entity.AuditEvent{Kind: "tool"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("FIFO 應拒寫")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("寫 FIFO 卡住")
	}
}

// FIFO 有讀端時開檔會成功,靠 fstat 只收一般檔案擋下,讀端什麼都收不到。
func TestAuditFileFIFOWithReaderRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if NewFileAudit(dir, nil).Write(entity.AuditEvent{Kind: "tool"}) {
		t.Fatal("FIFO 應拒寫")
	}
	if n, _ := r.Read(make([]byte, 64)); n != 0 {
		t.Fatal("FIFO 收到資料", n)
	}
}

// warn 回頭寫同一個 sink 不會死鎖(warn 在鎖外呼叫)。
func TestAuditFileWarnReentrant(t *testing.T) {
	var a entity.AuditSink
	calls := 0
	a = NewFileAudit(notDir(t), func(string) { calls++; a.Write(entity.AuditEvent{Kind: "audit_halt"}) })
	done := make(chan bool, 1)
	go func() { done <- a.Write(entity.AuditEvent{Kind: "tool"}) }()
	select {
	case ok := <-done:
		if ok || calls != 1 {
			t.Fatal(ok, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warn 重入死鎖")
	}
}
