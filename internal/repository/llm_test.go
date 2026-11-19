//go:build test

package repository

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// recAudit 沿用 http_test.go 的。

func priv(p entity.Privacy) *entity.Privacy { return &p }

func TestResolveProviderPrivacy(t *testing.T) {
	t.Run("明填 local 但非 loopback:回 local 並寫 privacy_override", func(t *testing.T) {
		a := &recAudit{}
		if p, o := ResolveProviderPrivacy(ProviderConfig{Name: "p", BaseURL: "https://api.example.com", Privacy: priv(entity.Local)}, a); p != entity.Local || !o {
			t.Fatal(p, o)
		}
		if len(a.events) != 1 || a.events[0].Kind != "privacy_override" || a.events[0].Host != "api.example.com" {
			t.Fatalf("%#v", a.events)
		}
	})
	t.Run("明填但無法辨識的值:當 remote 並寫 privacy_override", func(t *testing.T) {
		cfgs := map[string]ProviderConfig{}
		for _, bad := range []entity.Privacy{"LOCAL", "bogus"} {
			cfgs[string(bad)] = ProviderConfig{Name: "p", BaseURL: "http://127.0.0.1:1234", Privacy: priv(bad)}
		}
		// 明填 null:PrivacyNull(Ruling G4)
		cfgs["null"] = ProviderConfig{Name: "p", BaseURL: "http://127.0.0.1:1234", PrivacyNull: true}
		for name, cfg := range cfgs {
			a := &recAudit{}
			if p, o := ResolveProviderPrivacy(cfg, a); p != entity.Remote || o {
				t.Errorf("%s:%s %v", name, p, o)
			}
			if len(a.events) != 1 || a.events[0].Kind != "privacy_override" || a.events[0].Host != "127.0.0.1" {
				t.Errorf("%s:%#v", name, a.events)
			}
		}
	})
	t.Run("明填 remote 或 loopback 明填 local:不寫 audit", func(t *testing.T) {
		a := &recAudit{}
		if _, o := ResolveProviderPrivacy(ProviderConfig{Name: "p", BaseURL: "https://api.example.com", Privacy: priv(entity.Remote)}, a); o {
			t.Fatal("remote 不是 override")
		}
		if _, o := ResolveProviderPrivacy(ProviderConfig{Name: "p", BaseURL: "http://127.0.0.1", Privacy: priv(entity.Local)}, a); o {
			t.Fatal("loopback local 不是 override")
		}
		if len(a.events) != 0 {
			t.Fatalf("%#v", a.events)
		}
	})
	t.Run("未明填且非 loopback:remote,不寫 audit", func(t *testing.T) {
		a := &recAudit{}
		if p, o := ResolveProviderPrivacy(ProviderConfig{Name: "p", BaseURL: "https://api.example.com"}, a); p != entity.Remote || o {
			t.Fatal(p, o)
		}
		if len(a.events) != 0 {
			t.Fatalf("%#v", a.events)
		}
	})
}

// PrivacyNull 優先於 Privacy:同時填了 local 也當 remote(fail-closed),只寫一筆;audit 為 nil 不 panic。
func TestResolveProviderPrivacyNullWins(t *testing.T) {
	a := &recAudit{}
	cfg := ProviderConfig{Name: "p", BaseURL: "http://localhost:1234", Privacy: priv(entity.Local), PrivacyNull: true}
	if p, o := ResolveProviderPrivacy(cfg, a); p != entity.Remote || o {
		t.Fatal(p, o)
	}
	if len(a.events) != 1 || a.events[0].Reason != "provider p 的 privacy 值無法辨識,當 remote" {
		t.Fatalf("%#v", a.events)
	}
	if *cfg.Privacy != entity.Local {
		t.Fatal("不應改到呼叫端的 Privacy")
	}
	if p, o := ResolveProviderPrivacy(cfg, nil); p != entity.Remote || o {
		t.Fatal(p, o)
	}
}

// Ruling G14/G16:privacy 只看剛好叫 "privacy" 的鍵;null → PrivacyNull,字串照填,
// 其他 JSON 型別當無法辨識;大小寫不同的 privacy 鍵一律當無法辨識(不讓 typo 降級)。
// 每案都解碼後丟進 ResolveProviderPrivacy 看結果與稽核筆數。
func TestProviderConfigUnmarshalPrivacy(t *testing.T) {
	const lb = `"baseUrl":"http://127.0.0.1:1"`
	cases := []struct {
		in     string
		want   entity.Privacy
		audits int
		null   bool
	}{
		{`{"privacy":null,` + lb + `}`, entity.Remote, 1, true},
		{`{` + lb + `}`, entity.Local, 0, false},
		{`{"privacy":"local",` + lb + `}`, entity.Local, 0, false},
		{`{"privacy":"bogus",` + lb + `}`, entity.Remote, 1, false},
		{`{"privacy":null,"privacy":"local",` + lb + `}`, entity.Local, 0, false}, // 重複鍵最後一個生效(同 JSON.parse)
		{`{"privacy":"local","privacy":null,` + lb + `}`, entity.Remote, 1, true},
		{`{"privacy":1,` + lb + `}`, entity.Remote, 1, false}, // 無法辨識 → remote + 稽核
		{`{"privacy":{},` + lb + `}`, entity.Remote, 1, false},
		{`{"privacy":true,` + lb + `}`, entity.Remote, 1, false},
		{`{"Privacy":null,` + lb + `}`, entity.Remote, 1, false}, // 大小寫不同的鍵 → 無法辨識
		{`{"privacy":"local","PRIVACY":null,` + lb + `}`, entity.Remote, 1, false},
		// 審查 C1 的三個設定
		{`{"Privacy":"local","baseUrl":"http://127.0.0.1:1","headers":{"a":"b"}}`, entity.Remote, 1, false},
		{`{"privacy":"remote","Privacy":"local",` + lb + `}`, entity.Remote, 1, false},
		{`{"PRIVACY":"local","baseUrl":"https://api.example.com"}`, entity.Remote, 1, false},
		// Marshal 寫出的 PrivacyNull 鍵讀得回來
		{`{"PrivacyNull":true,` + lb + `}`, entity.Remote, 1, true},
		// 無點 ı 不是 privacy 鍵(encoding/json 不認)→ 沒填
		{`{"prıvacy":"local",` + lb + `}`, entity.Local, 0, false},
	}
	for _, c := range cases {
		var got ProviderConfig
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Errorf("%s:%v", c.in, err)
			continue
		}
		if got.PrivacyNull != c.null {
			t.Errorf("%s:PrivacyNull=%v", c.in, got.PrivacyNull)
		}
		a := &recAudit{}
		if p, o := ResolveProviderPrivacy(got, a); p != c.want || o || len(a.events) != c.audits {
			t.Errorf("%s:%s,稽核 %d 筆", c.in, p, len(a.events))
		}
		if got.BaseURL == "" {
			t.Errorf("%s:baseUrl 沒解碼", c.in)
		}
	}
	// 壞 JSON、其他欄位型別不符照樣回 error
	for _, in := range []string{`{`, `[]`, `{"maxTokens":"x"}`, `{"privacy":"local","maxTokens":"x"}`} {
		var c ProviderConfig
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("%s:應回 error", in)
		}
	}
}

// privacy 以外的欄位與 encoding/json 預設解碼逐欄相同(含大小寫不同的鍵、重複鍵)。
func TestProviderConfigUnmarshalOtherFields(t *testing.T) {
	type plain ProviderConfig // 沒有 UnmarshalJSON 的對照組
	for _, in := range []string{
		`{"name":"p","api":"openai-completions","model":"m","apiKey":"k","headers":{"a":"b"},"contextWindow":5,"maxTokens":6,"compat":{"appleFm":true}}`,
		`{"baseUrl":"https://api.x","BASEURL":"http://127.0.0.1:1"}`, // encoding/json 取最後一個,所以整筆當無法辨識
		`{"privacy":"local","Name":"a","name":"b"}`,
		`{}`, `null`,
	} {
		var got ProviderConfig
		var ref plain
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(in), &ref); err != nil {
			t.Fatal(err)
		}
		got.Privacy, got.PrivacyNull, ref.Privacy, ref.PrivacyNull = nil, false, nil, false
		if !reflect.DeepEqual(got, ProviderConfig(ref)) {
			t.Errorf("%s:其他欄位不同\n%#v\n%#v", in, got, ref)
		}
	}
}

// 設定鍵(camelCase)逐欄對到 Go 欄位;Marshal 後再 Unmarshal 全部欄位不變。
func TestProviderConfigJSONRoundTrip(t *testing.T) {
	ts := `{"name":"p","api":"openai-completions","model":"m","baseUrl":"https://api.example.com/v1","apiKey":"k",
		"headers":{"x-a":"1"},"privacy":"remote","contextWindow":1000,"maxTokens":200,
		"compat":{"appleFm":true,"supportsStrictMode":true,"maxTokensField":"max_completion_tokens"}}`
	want := ProviderConfig{Name: "p", API: "openai-completions", Model: "m", BaseURL: "https://api.example.com/v1", APIKey: "k",
		Headers: map[string]string{"x-a": "1"}, Privacy: priv(entity.Remote), ContextWindow: 1000, MaxTokens: 200,
		Compat: Compat{AppleFM: true, SupportsStrictMode: true, MaxTokensField: "max_completion_tokens"}}
	var got ProviderConfig
	if err := json.Unmarshal([]byte(ts), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("camelCase 鍵解碼不同\n%#v\n%#v", got, want)
	}
	for _, c := range []ProviderConfig{want, {Name: "q", PrivacyNull: true}, {Privacy: priv("")}, {Privacy: priv(entity.Local)}, {}} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var back ProviderConfig
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, c) {
			t.Errorf("round-trip 不同\n%#v\n%#v", back, c)
		}
	}
}

// 審查 m9:其他欄位型別錯誤時 UnmarshalJSON 回 error;呼叫端若忽略 error,留下的半成品也不能被判成 local
// (privacy 寫 remote、baseUrl 是 loopback,privacy 卻還沒解析 → 原本會落到 loopback 預設的 local)。
func TestProviderConfigUnmarshalErrorFailsClosed(t *testing.T) {
	var c ProviderConfig
	err := json.Unmarshal([]byte(`{"privacy":"remote","baseUrl":"http://127.0.0.1:1","contextWindow":"x"}`), &c)
	if err == nil {
		t.Fatal("型別錯誤應該回 error")
	}
	if p, o := ResolveProviderPrivacy(c, nil); p != entity.Remote || o {
		t.Fatalf("半成品被判成 %s %v", p, o)
	}
}

// 裁決 G23(同 G16):判斷隱私只看 privacy、baseUrl、headers 三個鍵,而且只認剛好這樣拼的鍵。
// 大小寫不同的 headers / baseUrl 鍵會讓 encoding/json 換掉這三個鍵的值,可能把 remote 降成 local,
// 所以一律當無法辨識:remote + 恰一筆 privacy_override。剛好叫 headers / baseUrl 的鍵照常解碼。
func TestProviderConfigCaseVariantKeys(t *testing.T) {
	cases := []struct {
		in     string
		want   entity.Privacy
		audits int
	}{
		// 最終審查 concern 1 實測的情況:Headers:null 把 header 清空,本來 headers 會判 remote
		{`{"baseUrl":"http://127.0.0.1:1","headers":{"a":"b"},"Headers":null}`, entity.Remote, 1},
		{`{"baseUrl":"http://127.0.0.1:1","Headers":{"a":"b"}}`, entity.Remote, 1},
		{`{"baseUrl":"http://127.0.0.1:1","HEADERS":{}}`, entity.Remote, 1},
		{`{"privacy":"local","baseUrl":"http://127.0.0.1:1","headerS":null}`, entity.Remote, 1},
		// baseUrl 的大小寫變體:encoding/json 取最後一個(loopback),蓋掉 baseUrl(remote 主機或沒填)
		{`{"baseUrl":"https://api.example.com","BASEURL":"http://127.0.0.1:1"}`, entity.Remote, 1},
		{`{"BaseURL":"http://127.0.0.1:1"}`, entity.Remote, 1},
		{`{"baseurl":"http://127.0.0.1:1"}`, entity.Remote, 1},
		// 剛好的鍵照舊
		{`{"baseUrl":"http://127.0.0.1:1","headers":null}`, entity.Local, 0},
		{`{"baseUrl":"http://127.0.0.1:1","headers":{"a":"b"}}`, entity.Remote, 0},
		{`{"baseUrl":"http://127.0.0.1:1"}`, entity.Local, 0},
	}
	for _, c := range cases {
		var got ProviderConfig
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Errorf("%s:%v", c.in, err)
			continue
		}
		a := &recAudit{}
		if p, o := ResolveProviderPrivacy(got, a); p != c.want || o || len(a.events) != c.audits {
			t.Errorf("%s:%s,稽核 %d 筆(應為 %s、%d 筆)", c.in, p, len(a.events), c.want, c.audits)
		}
	}
}
