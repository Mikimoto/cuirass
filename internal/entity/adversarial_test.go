//go:build test

package entity

import (
	"strings"
	"testing"
	"time"
)

// 對抗輸入:每條用 Go 程式碼補 lookaround 的規則(env-secret、conn-string、數字類)都餵 ~100KB 與 ~400KB,
// 驗證線性時間:100KB 要在 200ms 內,400KB 與 100KB 的耗時比不能像二次方(16 倍)。

func best(f func()) time.Duration {
	d := time.Duration(1 << 62)
	for range 3 {
		t0 := time.Now()
		f()
		d = min(d, time.Since(t0))
	}
	return d
}

func TestAdversarialLinearTime(t *testing.T) {
	rep := func(s string, size int) string { return strings.Repeat(s, max(1, size/len(s))) }
	shapes := []struct {
		name string
		gen  func(size int) string
		run  func(string)
	}{
		{"redact: A×n", func(n int) string { return rep("A", n) }, nil},
		{"redact: KEY×n", func(n int) string { return rep("KEY", n) }, nil},
		{"redact: _KEY×n", func(n int) string { return rep("_KEY", n) }, nil},
		{"redact: A×n+KEY", func(n int) string { return rep("A", n) + "KEY" }, nil},
		{"redact: KEY= + 空白×n", func(n int) string { return "KEY=" + rep(" ", n) }, nil},
		{"redact: A_KEY=x 重複(大量命中)", func(n int) string { return rep("A_KEY=x ", n) }, nil},
		{"redact: PATH=FOO= 重複(關鍵字不中)", func(n int) string { return rep("PATH=FOO=", n) }, nil},
		{"redact: 1+a×n+://(lookbehind 不中)", func(n int) string { return "1" + rep("a", n) + "://u:p@h" }, nil},
		{"redact: a:// 重複", func(n int) string { return rep("a://", n) }, nil},
		{"redact: BEGIN PRIVATE KEY 重複", func(n int) string { return rep("-----BEGIN RSA PRIVATE KEY-----", n) }, nil},
		{"redact: sk-+a×n+-", func(n int) string { return "sk-" + rep("a", n) + "-" }, nil},
		{"redact: a@ 重複", func(n int) string { return rep("a@", n) }, nil},
		{"mask: 1×n", func(n int) string { return rep("1", n) }, mask},
		{"mask: a+1×n(lookbehind 不中)", func(n int) string { return "a" + rep("1", n) }, mask},
		{"mask: 1. 重複", func(n int) string { return rep("1.", n) }, mask},
		{"mask: 1, 重複", func(n int) string { return rep("1,", n) }, mask},
		{"mask: 1,000 重複", func(n int) string { return rep("1,000", n) }, mask},
		{"mask: 12345 + ,234 重複(逗號 lookbehind 不中)", func(n int) string { return "12345" + rep(",234", n) }, mask},
		{"mask: 1000,200,300 CSV", func(n int) string { return rep("1000,200,300\n", n) }, mask},
		{"mask: 2024-1 重複", func(n int) string { return rep("2024-1", n) }, mask},
		{"mask: 換行+ 1. 重複", func(n int) string { return rep("\n 1.", n) }, mask},
		{"mask: 1000 加換行重複(大量 token)", func(n int) string { return rep("1000\n", n) }, mask},
		{"mask: 全形數字×n", func(n int) string { return rep("１", n) }, mask},
		{"mask: 1×n+K", func(n int) string { return rep("1", n) + "K" }, mask},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			run := s.run
			if run == nil {
				run = func(x string) { RedactCount(x, nil) }
			}
			// -race 下縮小輸入(50KB/200KB,仍是 4 倍;25KB 在 -race 下有固定開銷偏差(實測非線性),不可用)並略過絕對時間;比值檢查照舊,二次方仍會失敗。
			// 絕對上限由非 -race 的執行涵蓋。
			sSize, lSize := 100_000, 400_000
			if raceEnabled {
				sSize, lSize = 50_000, 200_000
			}
			small, large := s.gen(sSize), s.gen(lSize)
			ts, tl := best(func() { run(small) }), best(func() { run(large) })
			t.Logf("%dKB %v / %dKB %v", sSize/1000, ts, lSize/1000, tl)
			if !raceEnabled && ts > 200*time.Millisecond {
				t.Fatalf("100KB 花了 %v", ts)
			}
			// 太快時比值只是雜訊;夠大才看比值
			if tl > 20*time.Millisecond && tl > 10*ts {
				t.Fatalf("400KB/100KB = %.1f 倍,疑似二次方", float64(tl)/float64(ts))
			}
		})
	}
}

func mask(s string) { MaskText(s, NewMaskSet(1000, 1, 2024, 111)) }
