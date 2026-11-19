//go:build test

package entity

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Fatalf("%q 不應含 %q", s, sub)
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q 應含 %q", s, sub)
	}
}

func TestRedact(t *testing.T) {
	t.Run("遮蔽 Authorization header 值", func(t *testing.T) {
		out := Redact("Authorization: Bearer abc.def.ghi")
		mustNotContain(t, out, "abc.def.ghi")
		mustContain(t, out, "[REDACTED:")
	})
	t.Run("遮蔽連線字串的帳密", func(t *testing.T) {
		out := Redact("postgres://u:secretpw@h/db")
		mustNotContain(t, out, "secretpw")
		mustContain(t, out, "[REDACTED:")
	})
	t.Run("遮蔽 email", func(t *testing.T) {
		out := Redact("聯絡 a@b.com")
		mustNotContain(t, out, "a@b.com")
		mustContain(t, out, "[REDACTED:")
	})
	t.Run("不遮蔽識別字(欄位/表名)", func(t *testing.T) {
		if out := Redact("TABLE users (id, email_verified)"); out != "TABLE users (id, email_verified)" {
			t.Fatal(out)
		}
	})
	t.Run("redact 是純函式、不改原字串引用", func(t *testing.T) {
		// Go 字串不可變,「不改原字串」由語言保證;這裡只驗兩次結果相同。
		input := "Authorization: Bearer abc.def.ghi"
		if Redact(input) != Redact(input) || input != "Authorization: Bearer abc.def.ghi" {
			t.Fatal("結果不穩定")
		}
	})
	t.Run("DEFAULT_REDACT_PATTERNS 每條配替代標記", func(t *testing.T) {
		for _, p := range defaultRedactPatterns {
			mustContain(t, p.Replace, "[REDACTED:")
		}
	})
	t.Run("遮蔽 Google API key(AIza...)", func(t *testing.T) {
		key := "AIza" + strings.Repeat("x", 35)
		out := Redact("key " + key)
		mustNotContain(t, out, key)
		mustContain(t, out, "[REDACTED:gemini-key]")
	})
}

func TestRedactCount(t *testing.T) {
	t.Run("遮蔽私鑰、env 秘密、Anthropic key、JWT,並回傳筆數", func(t *testing.T) {
		src := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\nDB_PASSWORD=hunter2\nk=sk-ant-api03-AAAAAAAAAAAAAAAAAAAA\nt=eyJa.eyJb.sig"
		text, n := RedactCount(src, nil)
		if regexp.MustCompile(`hunter2|AAAAAAAA|abc\n|eyJa`).MatchString(text) {
			t.Fatal(text)
		}
		mustContain(t, text, "DB_PASSWORD=[REDACTED:env]")
		if n != 4 {
			t.Fatalf("count = %d", n)
		}
	})
	t.Run("一般文字不動:PATH=/usr/bin、email_verified 欄名", func(t *testing.T) {
		if _, n := RedactCount("PATH=/usr/bin\nemail_verified: true", nil); n != 0 {
			t.Fatalf("count = %d", n)
		}
	})
	t.Run("slack token", func(t *testing.T) {
		text, n := RedactCount("x=xoxb-1234567890-abcdef", nil)
		mustNotContain(t, text, "xoxb-")
		if n != 1 {
			t.Fatalf("count = %d", n)
		}
	})
	t.Run(`對 JSON.stringify 後的字串也有效(私鑰 \n 跳脫、env 值)`, func(t *testing.T) {
		b, _ := Stringify(map[string]string{
			"k": "-----BEGIN RSA PRIVATE KEY-----\nAAA\n-----END RSA PRIVATE KEY-----",
			"e": "DB_PASSWORD=hunter2",
		})
		text, _ := RedactCount(string(b), nil)
		if regexp.MustCompile(`AAA|hunter2`).MatchString(text) {
			t.Fatal(text)
		}
		if !json.Valid([]byte(text)) {
			t.Fatalf("不是合法 JSON:%s", text)
		}
	})
}

// within 斷言 f 在上限時間內跑完。
// -race 下絕對時間不可靠,直接略過;TestAdversarialLinearTime 的比值檢查在 -race 下涵蓋同樣的輸入形狀,
// 絕對上限由非 -race 執行涵蓋。
func within(t *testing.T, limit time.Duration, f func()) time.Duration {
	t.Helper()
	if raceEnabled {
		t.Skip("-race 下略過絕對時間斷言(非 race 執行與 TestAdversarialLinearTime 比值檢查涵蓋)")
	}
	t0 := time.Now()
	f()
	d := time.Since(t0)
	if d > limit {
		t.Fatalf("花了 %v,超過 %v", d, limit)
	}
	return d
}

func TestRedactCountFixes(t *testing.T) {
	t.Run("env-secret 不是二次方", func(t *testing.T) {
		within(t, 200*time.Millisecond, func() { RedactCount(strings.Repeat("A", 100000), nil) })
	})
	t.Run("env-secret 雙引號值(原樣與 JSON 跳脫)", func(t *testing.T) {
		if got, _ := RedactCount(`API_KEY="abc123"`, nil); got != "API_KEY=[REDACTED:env]" {
			t.Fatal(got)
		}
		b, _ := Stringify(map[string]string{"e": `API_KEY="abc123" ok`})
		r, _ := RedactCount(string(b), nil)
		mustNotContain(t, r, "abc123")
		var j map[string]string
		if err := json.Unmarshal([]byte(r), &j); err != nil {
			t.Fatal(err)
		}
		mustContain(t, j["e"], "API_KEY=[REDACTED:env]")
	})
	t.Run("env-secret 值內含引號不外漏", func(t *testing.T) {
		if got, _ := RedactCount(`PW_SECRET=ab"cd`, nil); got != "PW_SECRET=[REDACTED:env]" {
			t.Fatal(got)
		}
	})
	t.Run("auth-header JSON 物件形式,後面的跳脫文字保留", func(t *testing.T) {
		// 鍵要依插入順序(headers 在前);Go map 會排序,用 struct 保持順序。
		b, _ := Stringify(struct {
			Headers map[string]string `json:"headers"`
			N       string            `json:"n"`
		}{map[string]string{"Authorization": "Bearer abcdef123"}, "x\nNext: y"})
		r, _ := RedactCount(string(b), nil)
		mustNotContain(t, r, "abcdef123")
		mustContain(t, r, "Next: y")
		var j struct{ Headers map[string]string }
		if err := json.Unmarshal([]byte(r), &j); err != nil {
			t.Fatal(err)
		}
		mustContain(t, j.Headers["Authorization"], "[REDACTED:auth-header]")
	})
	t.Run("截斷的私鑰(無 END)從 BEGIN 遮到尾", func(t *testing.T) {
		text, n := RedactCount("head -----BEGIN RSA PRIVATE KEY-----\nLEAKBODY\nmore", nil)
		mustNotContain(t, text, "LEAKBODY")
		if !strings.HasPrefix(text, "head ") || n != 1 {
			t.Fatalf("%q %d", text, n)
		}
	})
}

func TestEnvSecretBacktrackAndSingleQuote(t *testing.T) {
	for i, s := range []string{strings.Repeat("KEY", 40000), strings.Repeat("_KEY", 30000)} {
		t.Run("重複關鍵字不是二次方/"+string(rune('0'+i)), func(t *testing.T) {
			within(t, 200*time.Millisecond, func() { RedactCount(s, nil) })
		})
	}
	t.Run("單引號值含空白整段遮蔽", func(t *testing.T) {
		if got, _ := RedactCount("KEY='a b' rest", nil); got != "KEY=[REDACTED:env] rest" {
			t.Fatal(got)
		}
	})
}

func TestEnvSecretLongName(t *testing.T) {
	cases := [][2]string{
		{strings.Repeat("A", 65) + "KEY=secret", strings.Repeat("A", 65) + "KEY=[REDACTED:env]"},
		{"KEY" + strings.Repeat("A", 65) + "=secret", "KEY" + strings.Repeat("A", 65) + "=[REDACTED:env]"},
		{
			"AZURE_STORAGE_ACCOUNT_PRIMARY_REGION_BACKUP_REPLICATION_SERVICE_PRINCIPAL_CLIENT_SECRET=abc",
			"AZURE_STORAGE_ACCOUNT_PRIMARY_REGION_BACKUP_REPLICATION_SERVICE_PRINCIPAL_CLIENT_SECRET=[REDACTED:env]",
		},
	}
	for i, c := range cases {
		t.Run("長名稱仍遮蔽/"+string(rune('0'+i)), func(t *testing.T) {
			if got, _ := RedactCount(c[0], nil); got != c[1] {
				t.Fatal(got)
			}
		})
	}
	t.Run("超長名稱無 = 不是二次方", func(t *testing.T) {
		within(t, 200*time.Millisecond, func() { RedactCount(strings.Repeat("A", 200000)+"KEY", nil) })
	})
}

func TestJSONSecret(t *testing.T) {
	for _, c := range [][2]string{
		{`{"password":"hunter2","apiKey":"abc123","DB_PASSWORD":"x1"}`,
			`{"password":"[REDACTED:json-secret]","apiKey":"[REDACTED:json-secret]","DB_PASSWORD":"[REDACTED:json-secret]"}`},
		{`{"access_token": "t-1", "user":"amy"}`, `{"access_token": "[REDACTED:json-secret]", "user":"amy"}`},
		// 工具回字串、裡面又是 JSON:序列化後引號帶反斜線
		{`"{\"secret\":\"s3\",\"n\":1}"`, `"{\"secret\":\"[REDACTED:json-secret]\",\"n\":1}"`},
		// 只認結尾:用量、鍵盤這類鍵不遮
		{`{"max_tokens":"100","keyboard":"jis","tokenizer":"bpe"}`, `{"max_tokens":"100","keyboard":"jis","tokenizer":"bpe"}`},
	} {
		if got := Redact(c[0]); got != c[1] {
			t.Errorf("Redact(%s)\n got %s\nwant %s", c[0], got, c[1])
		}
	}
}
