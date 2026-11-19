//go:build test

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
	. "github.com/Mikimoto/cuirass/internal/usecase"
)

// stub 只需要 privacy,用最小的假實作。
type stub struct{ privacy entity.Privacy }

func (s stub) Name() string            { return "stub" }
func (s stub) Model() string           { return "stub" }
func (s stub) Privacy() entity.Privacy { return s.privacy }
func (s stub) ContextWindow() int      { return 200000 }
func (s stub) MaxTokens() int          { return 4096 }
func (s stub) Stream(context.Context, LLMRequest) iter.Seq[LLMEvent] {
	return func(func(LLMEvent) bool) {}
}

var (
	remoteStub = stub{entity.Remote}
	localStub  = stub{entity.Local}
)

type spyAudit struct {
	kinds []string
	panic bool
}

func (s *spyAudit) Write(e entity.AuditEvent) bool {
	if s.panic {
		panic("disk")
	}
	s.kinds = append(s.kinds, e.Kind)
	return true
}

func msg(text string, privacy entity.Privacy) entity.Message {
	return entity.Message{Role: entity.RoleUser, Content: []entity.Part{{Type: entity.PartText, Text: text}}, Privacy: privacy, Ts: 1}
}

func mk(p LLMProvider) PrepareInput {
	return PrepareInput{
		Provider: p, System: "s", MaxTokens: 100,
		Messages: []entity.Message{msg("價格 12345 元", entity.Local), msg("公開 12345", entity.Remote)},
		MaskSet:  entity.NewMaskSet(12345),
		Audit:    &spyAudit{},
	}
}

func text(m entity.Message) string { return m.Content[0].Text }

func prepare(t *testing.T, o PrepareInput) LLMRequest {
	t.Helper()
	r, err := PrepareRequest(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := entity.Stringify(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func want(t *testing.T, got, w string) {
	t.Helper()
	if got != w {
		t.Fatalf("got %q, want %q", got, w)
	}
}

const masked = "價格 " + entity.MASK + " 元"

func TestPrepareRequest(t *testing.T) {
	t.Run("remote:local 訊息的數字被遮", func(t *testing.T) {
		r := prepare(t, mk(remoteStub))
		want(t, text(r.Messages[0]), masked)
		want(t, text(r.Messages[1]), "公開 12345")
	})
	t.Run("local provider:不遮", func(t *testing.T) {
		want(t, text(prepare(t, mk(localStub)).Messages[0]), "價格 12345 元")
	})
	t.Run("snapshot 丟例外 → 丟掉所有 local 訊息並寫 mask_fallback", func(t *testing.T) {
		// Go 有兩種「丟例外」:回 error 與 panic,兩種都要 fail-closed
		for _, snap := range []func() ([]float64, error){
			func() ([]float64, error) { return nil, errors.New("x") },
			func() ([]float64, error) { panic("x") },
		} {
			o := mk(remoteStub)
			audit := &spyAudit{}
			o.Audit, o.Masking.Snapshot = audit, snap
			r := prepare(t, o)
			for _, m := range r.Messages {
				if m.Privacy != entity.Remote {
					t.Fatal("留下了 local 訊息")
				}
			}
			if !strings.Contains(strings.Join(audit.kinds, ","), "mask_fallback") {
				t.Fatal(audit.kinds)
			}
		}
	})
	t.Run("audit 寫入丟例外也不外洩 local", func(t *testing.T) {
		o := mk(remoteStub)
		o.Audit = &spyAudit{panic: true}
		o.Masking.Snapshot = func() ([]float64, error) { return nil, errors.New("x") }
		r := prepare(t, o)
		if len(r.Messages) != 1 || r.Messages[0].Privacy != entity.Remote {
			t.Fatal(r.Messages)
		}
	})
	t.Run("snapshot 的數字會加進遮蔽集合", func(t *testing.T) {
		o := mk(remoteStub)
		o.MaskSet = entity.NewMaskSet()
		o.Masking.Snapshot = func() ([]float64, error) { return []float64{12345}, nil }
		want(t, text(prepare(t, o).Messages[0]), masked)
	})
	t.Run("rewrite 想改 privacy/role 無效,內容照樣被遮", func(t *testing.T) {
		// 調整:Go 的 Part 沒有 privacy/role 欄位,rewrite 的簽章也碰不到 Message,型別上就改不了;
		// 保留「回傳後再改 hook 手上的參考也無效」這半。
		var captured []entity.Part
		o := mk(remoteStub)
		o.Rewrite = func(parts []entity.Part, role string) ([]entity.Part, error) {
			if role != entity.RoleUser {
				return parts, nil
			}
			captured = append([]entity.Part{{Type: entity.PartText, Text: "x 12345"}}, parts[1:]...)
			return captured, nil
		}
		r := prepare(t, o)
		captured[0].Text = "事後改 12345"
		m := r.Messages[0]
		if m.Privacy != entity.Local || m.Role != entity.RoleUser || m.Ts != 1 {
			t.Fatal(m)
		}
		want(t, text(m), "x "+entity.MASK)
	})
	t.Run("provider privacy 缺值或 cloud → 仍遮", func(t *testing.T) {
		for _, p := range []entity.Privacy{"", "cloud"} {
			want(t, text(prepare(t, mk(stub{p})).Messages[0]), masked)
		}
	})
	t.Run("rewrite 把 local 內容抄進 remote 訊息 → 送出的請求沒有原數字", func(t *testing.T) {
		i := 0
		o := mk(remoteStub)
		o.Rewrite = func(parts []entity.Part, _ string) ([]entity.Part, error) {
			i++
			if i == 2 {
				return []entity.Part{{Type: entity.PartText, Text: "抄 12345"}}, nil
			}
			return parts, nil
		}
		if strings.Contains(jsonOf(t, prepare(t, o).Messages), "12345") {
			t.Fatal("外洩")
		}
	})
	t.Run("rewrite 塞回原數字 → 仍被遮(遮蔽在 rewrite 之後)", func(t *testing.T) {
		o := mk(remoteStub)
		o.Rewrite = func(parts []entity.Part, _ string) ([]entity.Part, error) {
			return append(parts, entity.Part{Type: entity.PartText, Text: " 又 12345"}), nil
		}
		r := prepare(t, o)
		if strings.Contains(entity.TextOf(r.Messages[0]), "12345") || !strings.Contains(jsonOf(t, r.Messages[0]), " 又 ") {
			t.Fatal(r.Messages[0])
		}
	})
	t.Run("rewrite 丟例外或回非陣列 → 用原內容", func(t *testing.T) {
		// 「丟例外」= 回 error 或 panic;「非陣列」= 回 nil
		for _, rw := range []ContentRewrite{
			func([]entity.Part, string) ([]entity.Part, error) { return nil, errors.New("x") },
			func([]entity.Part, string) ([]entity.Part, error) { panic("x") },
			func([]entity.Part, string) ([]entity.Part, error) { return nil, nil },
		} {
			o := mk(localStub)
			o.Rewrite = rw
			want(t, text(prepare(t, o).Messages[0]), "價格 12345 元")
		}
	})
	t.Run("原 messages 陣列不被改", func(t *testing.T) {
		o := mk(remoteStub)
		before := jsonOf(t, o.Messages)
		o.Rewrite = func(p []entity.Part, _ string) ([]entity.Part, error) { p[0].Text = "改"; return p, nil }
		prepare(t, o)
		want(t, jsonOf(t, o.Messages), before)
	})
	t.Run("原地改 remote 訊息 text 與 toolCall.args(回傳同一陣列)→ 請求沒有原數字", func(t *testing.T) {
		o := mk(remoteStub)
		o.Messages = append(o.Messages, entity.Message{
			Role: entity.RoleAssistant, Privacy: entity.Remote, Ts: 2, Provider: "p", StopReason: "toolUse",
			Content: []entity.Part{{Type: entity.PartToolCall, ID: "c", Name: "t", Args: map[string]any{"q": "a"}}},
		})
		o.Rewrite = func(parts []entity.Part, _ string) ([]entity.Part, error) {
			if parts[0].Type == entity.PartText {
				parts[0].Text = "抄 12345"
			} else {
				parts[0].Args["q"] = "抄 12345"
			}
			return parts, nil
		}
		if strings.Contains(jsonOf(t, prepare(t, o).Messages), "12345") {
			t.Fatal("外洩")
		}
	})
	t.Run("保留參考、之後在別的呼叫中改 → 不影響請求;送出後再改也不影響", func(t *testing.T) {
		o := mk(remoteStub)
		var kept [][]entity.Part
		o.Rewrite = func(parts []entity.Part, _ string) ([]entity.Part, error) {
			for _, k := range kept {
				k[0].Text = "抄 12345"
			}
			kept = append(kept, parts)
			return parts, nil
		}
		r := prepare(t, o)
		want(t, text(r.Messages[0]), masked)
		want(t, text(r.Messages[1]), "公開 12345")
		snap := jsonOf(t, r.Messages)
		for _, k := range kept {
			k[0].Text = "事後 12345"
		}
		want(t, jsonOf(t, r.Messages), snap)
	})
	t.Run("part 帶 toJSON → 退回原訊息,不被騙過", func(t *testing.T) {
		// 調整:Go 的對應是自訂 MarshalJSON。會回錯誤的 → 序列化失敗 → 退回原訊息;
		// 會「說謊」的 MarshalJSON 騙不了:請求裡放的就是它序列化出來、拿去比對的那份。
		o := mk(remoteStub)
		o.Rewrite = func([]entity.Part, string) ([]entity.Part, error) {
			return []entity.Part{{Type: entity.PartText, Text: "抄 12345"}, {Type: entity.PartToolCall, Args: map[string]any{"x": badJSON{}}}}, nil
		}
		r := prepare(t, o)
		want(t, text(r.Messages[0]), masked)
		want(t, text(r.Messages[1]), "公開 12345")
	})
	t.Run("rewrite 回傳 Map/Error/陣列具名屬性夾帶 local 數字 → 請求任何地方都沒有", func(t *testing.T) {
		// 調整:Go 的對應變體。陣列具名屬性在 Go 不存在(N/A);改用 error 值、未匯出欄位、非字串鍵 map、func。
		type hidden struct{ secret string }
		variants := []func(p []entity.Part) []entity.Part{
			func(p []entity.Part) []entity.Part {
				p[0].Args = map[string]any{"q": map[int]string{1: "local 12345"}}
				return p
			},
			func(p []entity.Part) []entity.Part {
				p[0].Args = map[string]any{"q": errors.New("local 12345")}
				return p
			},
			func(p []entity.Part) []entity.Part { p[0].Args = map[string]any{"q": hidden{"local 12345"}}; return p },
			func(p []entity.Part) []entity.Part {
				p[0].Args = map[string]any{"q": "local 12345", "f": func() {}}
				return p
			},
		}
		for i, v := range variants {
			o := mk(remoteStub)
			o.Messages = []entity.Message{{
				Role: entity.RoleAssistant, Privacy: entity.Remote, Ts: 2, Provider: "p", StopReason: "toolUse",
				Content: []entity.Part{{Type: entity.PartToolCall, ID: "c", Name: "t", Args: map[string]any{}}},
			}}
			o.Rewrite = func(p []entity.Part, _ string) ([]entity.Part, error) { return v(p), nil }
			if s := fmt.Sprintf("%#v", prepare(t, o)); strings.Contains(s, "12345") {
				t.Fatalf("變體 %d 外洩:%s", i, s)
			}
		}
	})
	// 深複製遇到 function 失敗 → PrepareRequest 回 error。
	t.Run("訊息含無法序列化的值 → 回 error", func(t *testing.T) {
		o := mk(remoteStub)
		o.Messages[0].Content = []entity.Part{{Type: entity.PartToolCall, Args: map[string]any{"f": func() {}}}}
		if _, err := PrepareRequest(o); err == nil {
			t.Fatal("應回 error")
		}
	})
	// hideResult panic 也算遮蔽失敗
	t.Run("hideResult panic → 丟掉 local", func(t *testing.T) {
		o := mk(remoteStub)
		o.Messages = append(o.Messages, entity.Message{Role: entity.RoleToolResult, ToolName: "x", Privacy: entity.Local, Content: []entity.Part{{Type: entity.PartText, Text: "1"}}})
		o.Masking.HideResult = func(string) (string, bool) { panic("boom") }
		r := prepare(t, o)
		if len(r.Messages) != 1 || r.Messages[0].Privacy != entity.Remote {
			t.Fatal(r.Messages)
		}
	})
}

type badJSON struct{}

func (badJSON) MarshalJSON() ([]byte, error) { return nil, errors.New("no") }

// 沒設 Masking(Snapshot、Collect、HideResult 都是 nil)且遮蔽集合是空的:送到 remote 也不遮任何內容,框架不自己猜。
// system 提示詞不論集合裡有什麼都原樣送出。
func TestPrepareRequestNoMaskingNoMask(t *testing.T) {
	r := prepare(t, PrepareInput{
		Provider: remoteStub, System: "系統 12345", MaxTokens: 100,
		Messages: []entity.Message{msg("價格 12345 元,持有 98765 股", entity.Local)},
		MaskSet:  entity.NewMaskSet(),
		Audit:    &spyAudit{},
	})
	if got := text(r.Messages[0]); got != "價格 12345 元,持有 98765 股" {
		t.Fatalf("沒設 Masking 不該遮:%q", got)
	}
	// 集合裡有 12345:訊息被遮,system 照樣原文
	r = prepare(t, PrepareInput{
		Provider: remoteStub, System: "系統 12345", MaxTokens: 100,
		Messages: []entity.Message{msg("價格 12345 元", entity.Local)},
		MaskSet:  entity.NewMaskSet(12345),
		Audit:    &spyAudit{},
	})
	if strings.Contains(text(r.Messages[0]), "12345") {
		t.Fatalf("訊息該遮:%q", text(r.Messages[0]))
	}
	if r.System != "系統 12345" {
		t.Fatalf("system 不經遮蔽,應原樣:%q", r.System)
	}
}
