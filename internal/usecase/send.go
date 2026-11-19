// Package usecase 是流程層。send.go 是唯一送出管線(spec §11.6):所有送進 provider 的請求都先經過這裡;
// 遮蔽之後沒有任何開發者 hook。
package usecase

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// ContentRewrite 只拿到 content 的副本;回 error、panic 或回 nil 切片 → 該則用原內容。
// 回 []Part{}(非 nil 的空切片)= 內容清空。nil = 回傳值無效。
type ContentRewrite func(parts []entity.Part, role string) ([]entity.Part, error)

// Masking 是遮蔽相關的 hook。Snapshot 回 error 或 panic → 遮蔽失敗(fail-closed)。
// Collect:local 輪成功的工具結果,回傳要加進遮蔽集合的數字;回 error 或 panic → 忽略
// (送出時 Snapshot 再補)。details 是工具原始的 Details(未 redact)。
type Masking struct {
	Collect    func(tool string, details any) ([]float64, error)
	Snapshot   func() ([]float64, error)
	HideResult func(tool string) (string, bool) // 傳給 MaskMessages
}

// PrepareInput 對應 prepareRequest 的參數物件;signal 不在這裡(Go 由 Stream 的 ctx 帶)。
type PrepareInput struct {
	Provider  LLMProvider
	System    string
	Messages  []entity.Message
	Tools     []ToolSpec
	MaxTokens int
	Rewrite   ContentRewrite
	MaskSet   *entity.MaskSet
	Masking   Masking
	Audit     entity.AuditSink
}

// try 執行 f,panic 轉成 error(guard 步驟一律 fail-closed)。
func try(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f()
}

// rewriteOne:rewrite 只拿到 content;role、privacy、ts、id 一律保留原值(spec §12 M6)。任何失敗都回原訊息。
func rewriteOne(m entity.Message, rewrite ContentRewrite) entity.Message {
	out := m
	err := try(func() error {
		before, err := entity.Stringify(m.Content)
		if err != nil {
			return err
		}
		// rewrite 拿到副本,回傳值也再 JSON 來回一次:任何 hook 手上的參考(切片、map)都碰不到我們的訊息
		in, err := entity.CloneJSON(m.Content)
		if err != nil {
			return err
		}
		parts, err := rewrite(in, m.Role)
		if err != nil {
			return err
		}
		if parts == nil {
			return errors.New("rewrite 回 nil")
		}
		// JSON 來回正規化:請求裡放的就是被比對的內容(未匯出欄位、func、自訂型別都會被洗掉或讓序列化失敗)
		j, err := entity.Stringify(parts)
		if err != nil {
			return err
		}
		var copied []entity.Part
		if err := json.Unmarshal(j, &copied); err != nil {
			return err
		}
		out.Content = copied
		// 擴充點只能收緊:remote 訊息被改動(可能夾帶 local 內容)→ 降為 local,讓它被遮
		if entity.PrivacyOf(m) == entity.Remote && string(j) != string(before) {
			out.Privacy = entity.Local
		}
		return nil
	})
	if err != nil {
		return m
	}
	return out
}

// PrepareRequest 組出送給 provider 的請求。只有初次複製失敗(訊息裡有無法序列化的值)會回 error,
// 深複製失敗(例如含 function):遮蔽步驟的失敗不回 error,而是 fail-closed 丟掉 local 訊息。
func PrepareRequest(o PrepareInput) (LLMRequest, error) {
	// ① 複製:之後的改動不影響呼叫端
	msgs, err := entity.CloneJSON(o.Messages)
	if err != nil {
		return LLMRequest{}, fmt.Errorf("複製訊息失敗:%w", err)
	}
	// ②
	if o.Rewrite != nil {
		for i, m := range msgs {
			msgs[i] = rewriteOne(m, o.Rewrite)
		}
	}
	// ③ 只有明確 local 才不遮(空值、typo 都當 remote);任何一步失敗 → 丟掉 local 訊息。此後不碰內容。
	if o.Provider.Privacy() != entity.Local {
		var masked []entity.Message
		err := try(func() error {
			if o.Masking.Snapshot != nil {
				snap, err := o.Masking.Snapshot()
				if err != nil {
					return err
				}
				o.MaskSet.Add(snap)
			}
			masked = entity.MaskMessages(msgs, o.MaskSet, o.Masking.HideResult)
			return nil
		})
		if err != nil {
			masked = entity.DropLocal(msgs)
			// 稽核寫不進去(含 panic)也不能送出未遮的內容
			_ = try(func() error {
				if o.Audit != nil {
					o.Audit.Write(entity.AuditEvent{Kind: "mask_fallback", Reason: "遮蔽失敗,已丟掉所有 local 訊息"})
				}
				return nil
			})
		}
		msgs = masked
	}
	// 最後再複製一次,請求不持有任何外部參考
	msgs, err = entity.CloneJSON(msgs)
	if err != nil {
		return LLMRequest{}, err
	}
	// ④
	return LLMRequest{System: o.System, Messages: msgs, Tools: o.Tools, MaxTokens: o.MaxTokens}, nil
}
