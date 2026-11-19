//go:build test

package usecase

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

func vctx(mod func(*VerifyCtx)) VerifyCtx {
	vc := VerifyCtx{
		Final:        true,
		ToolData:     func() string { return "" },
		KnownNumbers: func() []float64 { return nil },
		Remote:       func() bool { return false },
		Mask:         func(t string) string { return t },
		Ask:          func(context.Context, string, string) (string, error) { return "[]", nil },
	}
	if mod != nil {
		mod(&vc)
	}
	return vc
}

func runV(t *testing.T, v Verifier, ctx context.Context, text string, vc VerifyCtx) string {
	t.Helper()
	out, err := v(ctx, text, vc)
	if err != nil {
		t.Fatalf("verifier error: %v", err)
	}
	return out
}

func TestNumberVerifier(t *testing.T) {
	bg := context.Background()

	t.Run("對得到的數字不標記,不複核", func(t *testing.T) {
		asked := 0
		out := runV(t, NumberVerifier(0), bg, "股價 512.5 元", vctx(func(c *VerifyCtx) {
			c.KnownNumbers = func() []float64 { return []float64{512.5} }
			c.Ask = func(context.Context, string, string) (string, error) { asked++; return "[]", nil }
		}))
		if out != "股價 512.5 元" || asked != 0 {
			t.Fatalf("out=%q asked=%d", out, asked)
		}
	})

	t.Run("複核認可 → 不標記", func(t *testing.T) {
		out := runV(t, NumberVerifier(0), bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { return `答案:["1025"]`, nil }
		}))
		if out != "合計 1025 元" {
			t.Fatalf("out=%q", out)
		}
	})

	t.Run("複核回 undefined(額度用完)→ 標記保留", func(t *testing.T) {
		out := runV(t, NumberVerifier(0), bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { return "", errors.New("額度不足") }
		}))
		if !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q", out)
		}
	})

	t.Run("非 final → 不複核,直接標記", func(t *testing.T) {
		asked := 0
		out := runV(t, NumberVerifier(0), bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Final = false
			c.Ask = func(context.Context, string, string) (string, error) { asked++; return `["1025"]`, nil }
		}))
		if asked != 0 || !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q asked=%d", out, asked)
		}
	})

	t.Run("複核逾時 → 標記保留", func(t *testing.T) {
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		out := runV(t, NumberVerifier(10*time.Millisecond), bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { <-block; return `["1025"]`, nil }
		}))
		if !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q", out)
		}
	})

	t.Run("remote 時 ask 收到的 data 與 answer 都已遮,被遮的數字不再送去複核", func(t *testing.T) {
		sent := ""
		out := runV(t, NumberVerifier(0), bg, "持股 1234.5 股,股價 777.7 元", vctx(func(c *VerifyCtx) {
			c.Remote = func() bool { return true }
			c.Mask = func(t string) string { return strings.ReplaceAll(t, "1234.5", entity.MASK) }
			c.ToolData = func() string { return `{"shares":1234.5,"price":777.7}` }
			c.Ask = func(_ context.Context, _, u string) (string, error) { sent = u; return `["777.7"]`, nil }
		}))
		if strings.Contains(sent, "1234.5") || !strings.Contains(sent, entity.MASK) || !strings.Contains(sent, `待核對數字:["777.7"]`) {
			t.Fatalf("sent=%q", sent)
		}
		// 被遮的數字無法核對,不算錯;原文照回
		if out != "持股 1234.5 股,股價 777.7 元" {
			t.Fatalf("out=%q", out)
		}
	})
}

// 複核卡住、不理會 ctx:時間到就放棄,標記保留,不等 Ask 回來。
func TestVerifierTimeout(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	var sawDeadline atomic.Bool
	start := time.Now()
	out := runV(t, NumberVerifier(20*time.Millisecond), context.Background(), "合計 1025 元", vctx(func(c *VerifyCtx) {
		c.Ask = func(ctx context.Context, _, _ string) (string, error) {
			_, ok := ctx.Deadline()
			sawDeadline.Store(ok)
			<-block
			return `["1025"]`, nil
		}
	}))
	if !strings.Contains(out, "⚠未核對") {
		t.Fatalf("out=%q", out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("逾時沒生效:%v", d)
	}
	if !sawDeadline.Load() {
		t.Fatal("Ask 拿到的 ctx 沒帶逾時")
	}
}

// 上層 ctx 取消:Ask 看得到取消;Ask 不理會也照樣返回,標記保留。
func TestVerifierAskCtxCancel(t *testing.T) {
	t.Run("Ask 收到取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var sawCancel atomic.Bool
		askDone := make(chan struct{})
		done := make(chan string, 1)
		go func() {
			out, _ := NumberVerifier(time.Minute)(ctx, "合計 1025 元", vctx(func(c *VerifyCtx) {
				c.Ask = func(ctx context.Context, _, _ string) (string, error) {
					defer close(askDone)
					cancel()
					<-ctx.Done()
					sawCancel.Store(true)
					return `["1025"]`, nil // 取消後才回的認可不算
				}
			}))
			done <- out
		}()
		select {
		case out := <-done:
			if !strings.Contains(out, "⚠未核對") {
				t.Fatalf("out=%q", out)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("取消後沒返回")
		}
		<-askDone
		if !sawCancel.Load() {
			t.Fatal("Ask 沒看到取消")
		}
	})

	t.Run("Ask 不理會取消也照樣返回", func(t *testing.T) {
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		out := runV(t, NumberVerifier(time.Minute), ctx, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { <-block; return `["1025"]`, nil }
		}))
		if !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q", out)
		}
	})

	t.Run("ctx 已取消:Ask 即使立刻認可也不算", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := make(chan struct{})
		out := runV(t, NumberVerifier(0), ctx, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { close(called); return `["1025"]`, nil }
		}))
		if !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q", out)
		}
		// Ask 在另一個 goroutine:等一下確定它真的沒被呼叫(不白吃額度)
		select {
		case <-called:
			t.Fatal("ctx 已取消仍呼叫 Ask")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// remote 時用真的 MaskText:送去 Ask 的工具資料與回答都看不到被遮的數字;local 時不呼叫 Mask。
func TestVerifierRemoteMask(t *testing.T) {
	set := entity.NewMaskSet(98765.4)
	t.Run("remote 遮", func(t *testing.T) {
		sent := ""
		runV(t, NumberVerifier(0), context.Background(), "存款 98,765.4 元,股價 777.7 元", vctx(func(c *VerifyCtx) {
			c.Remote = func() bool { return true }
			c.Mask = func(s string) string { return entity.MaskText(s, set) }
			c.ToolData = func() string { return `{"cash":98765.4,"price":777.8}` }
			c.Ask = func(_ context.Context, _, u string) (string, error) { sent = u; return "[]", nil }
		}))
		if sent == "" || strings.Contains(sent, "98765.4") || strings.Contains(sent, "98,765.4") {
			t.Fatalf("sent=%q", sent)
		}
		data, answer, _ := strings.Cut(sent, "\n\n回答:\n")
		if !strings.Contains(data, entity.MASK) || !strings.Contains(answer, entity.MASK) {
			t.Fatalf("data 或 answer 沒遮:%q", sent)
		}
	})
	t.Run("local 不呼叫 Mask", func(t *testing.T) {
		masked := 0
		sent := ""
		runV(t, NumberVerifier(0), context.Background(), "存款 98,765.4 元", vctx(func(c *VerifyCtx) {
			c.Mask = func(s string) string { masked++; return entity.MaskText(s, set) }
			c.ToolData = func() string { return `{"cash":1}` }
			c.Ask = func(_ context.Context, _, u string) (string, error) { sent = u; return "[]", nil }
		}))
		if masked != 0 || !strings.Contains(sent, "98,765.4") {
			t.Fatalf("masked=%d sent=%q", masked, sent)
		}
	})
}

// 任何一步 panic 都不能 crash,也不能讓數字變成「已核對」。
func TestVerifierFailClosed(t *testing.T) {
	bg := context.Background()
	marked := func(t *testing.T, mod func(*VerifyCtx)) {
		t.Helper()
		out := runV(t, NumberVerifier(time.Second), bg, "合計 1025 元", vctx(mod))
		if !strings.Contains(out, "⚠未核對") {
			t.Fatalf("out=%q", out)
		}
	}
	t.Run("Mask panic → 不呼叫 Ask,標記保留", func(t *testing.T) {
		asked := 0
		marked(t, func(c *VerifyCtx) {
			c.Remote = func() bool { return true }
			c.Mask = func(string) string { panic("snapshot 失敗") }
			c.Ask = func(context.Context, string, string) (string, error) { asked++; return `["1025"]`, nil }
		})
		if asked != 0 {
			t.Fatalf("asked=%d", asked)
		}
	})
	t.Run("ToolData panic → 標記保留", func(t *testing.T) {
		marked(t, func(c *VerifyCtx) { c.ToolData = func() string { panic("x") } })
	})
	t.Run("Remote panic → 標記保留", func(t *testing.T) {
		marked(t, func(c *VerifyCtx) { c.Remote = func() bool { panic("x") } })
	})
	t.Run("Ask panic(在另一個 goroutine)→ 標記保留", func(t *testing.T) {
		marked(t, func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { panic("x") }
		})
	})
	t.Run("Ask 是 nil → 標記保留", func(t *testing.T) {
		marked(t, func(c *VerifyCtx) { c.Ask = nil })
	})
	t.Run("回覆不是 JSON 陣列 → 標記保留", func(t *testing.T) {
		for _, r := range []string{"1025", "]1025[", `["1025"`, `"1025"]`, `["1025",]`, `[1025]`, `["1025"] ["1025"]`} {
			marked(t, func(c *VerifyCtx) {
				c.Ask = func(context.Context, string, string) (string, error) { return r, nil }
			})
		}
	})
	t.Run("陣列裡非字串元素略過、不讓整個解析失敗", func(t *testing.T) {
		out := runV(t, NumberVerifier(0), bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.Ask = func(context.Context, string, string) (string, error) { return `[1e999, null, "1025", {"x":1}]`, nil }
		}))
		if out != "合計 1025 元" {
			t.Fatalf("out=%q", out)
		}
	})
	t.Run("KnownNumbers panic → 回 error(整個 verifier 失敗)", func(t *testing.T) {
		_, err := NumberVerifier(0)(bg, "合計 1025 元", vctx(func(c *VerifyCtx) {
			c.KnownNumbers = func() []float64 { panic("x") }
		}))
		if err == nil {
			t.Fatal("want error")
		}
	})
}

// 送出的 user 文字與模板逐字相同。
func TestVerifierPrompt(t *testing.T) {
	var sys, user string
	runV(t, NumberVerifier(0), context.Background(), "合計 1025 元", vctx(func(c *VerifyCtx) {
		c.ToolData = func() string { return "<x>" }
		c.Ask = func(_ context.Context, s, u string) (string, error) { sys, user = s, u; return "[]", nil }
	}))
	if sys != reviewSystem {
		t.Fatalf("sys=%q", sys)
	}
	want := "工具資料:\n" + entity.WrapUntrusted("tools", "<x>") + "\n\n回答:\n合計 1025 元\n\n待核對數字:[\"1025\"]"
	if user != want {
		t.Fatalf("user=%q\nwant=%q", user, want)
	}
}

// 審查 m16:全形數字、全形小數點與 U+3000 全形空白。對得到的不標記也不複核;對不到的照樣標記(不因全形而漏掉)。
func TestNumberVerifierFullWidth(t *testing.T) {
	bg := context.Background()
	var reviews atomic.Int32
	known := vctx(func(c *VerifyCtx) {
		c.KnownNumbers = func() []float64 { return []float64{512.5, 1025} }
		c.Ask = func(context.Context, string, string) (string, error) { reviews.Add(1); return "[]", nil }
	})
	for _, s := range []string{"股價　５１２．５　元", "合計　１，０２５　元", "股價　512.5　元"} {
		if out := runV(t, NumberVerifier(0), bg, s, known); out != s || reviews.Load() != 0 {
			t.Fatalf("%q → %q(複核 %d 次)", s, out, reviews.Load())
		}
	}
	asked := ""
	out := runV(t, NumberVerifier(0), bg, "股價　６００．９　元", vctx(func(c *VerifyCtx) {
		c.KnownNumbers = func() []float64 { return []float64{512.5} }
		c.Ask = func(_ context.Context, _, u string) (string, error) { asked = u; return "[]", nil }
	}))
	if !strings.Contains(out, "⚠未核對") || !strings.Contains(asked, "６００．９") {
		t.Fatalf("out=%q asked=%q", out, asked)
	}
}
