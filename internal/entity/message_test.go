//go:build test

package entity

import (
	"strings"
	"testing"
)

// 審查 I3:JSON.stringify 不跳脫 U+2028/U+2029,Go 的 encoder 會。跳脫後 redact 的 \s 吃不到,秘密留在原文。
func TestStringifyLineSeparators(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"API_KEY=\u2028hunter2hunter2", `{"out":"API_KEY=` + "\u2028" + `[REDACTED:env]"}`},
		{"API_KEY=\u2029hunter2hunter2", `{"out":"API_KEY=` + "\u2029" + `[REDACTED:env]"}`},
		{"Authorization:\u2028Bearer abcdef123", `{"out":"Authorization:` + "\u2028" + `[REDACTED:auth-header]"}`},
	} {
		b, err := Stringify(map[string]string{"out": tc.in})
		if err != nil {
			t.Fatal(err)
		}
		if got, n := RedactCount(string(b), nil); got != tc.want || n != 1 {
			t.Errorf("%q:得到 %s(%d 筆),應為 %s", tc.in, got, n, tc.want)
		}
	}
}

func TestStringifyMatchesJSONStringify(t *testing.T) {
	// 字串裡原本就是 \u2028 這六個字(反斜線被跳脫成 \\)不能被換成原字元;<>& 不跳脫
	b, _ := Stringify(map[string]string{"s": "a\u2028b\u2029c" + `\u2028` + `\\u2029` + "<>&\x7f\b\f"})
	if want := `{"s":"a` + "\u2028b\u2029c" + `\\u2028\\\\u2029<>&` + "\x7f" + `\b\f"}`; string(b) != want {
		t.Fatalf("\n got: %s\nwant: %s", b, want)
	}
	// 巢狀:Message → Part 的 MarshalJSON 輸出經過外層 encoder 後也不能被再跳脫
	m := Message{Role: RoleUser, Content: []Part{{Type: PartText, Text: "x\u2028y"}}, Privacy: Local}
	b, _ = Stringify([]Message{m})
	if !strings.Contains(string(b), "x\u2028y") || strings.Contains(string(b), `\u2028`) {
		t.Fatalf("巢狀輸出仍跳脫:%s", b)
	}
	// CloneJSON 來回後內容不變
	c, err := CloneJSON(m)
	if err != nil || c.Content[0].Text != "x\u2028y" {
		t.Fatalf("CloneJSON:%v %q", err, c.Content[0].Text)
	}
}

func TestMessage(t *testing.T) {
	t.Run("privacyOf:缺值、亂值都當 local", func(t *testing.T) {
		if PrivacyOf(Message{}) != Local || PrivacyOf(Message{Privacy: "REMOTE"}) != Local || PrivacyOf(Message{Privacy: "remote"}) != Remote {
			t.Fatal()
		}
	})
	t.Run("textOf:只串 text part", func(t *testing.T) {
		m := Message{Role: RoleAssistant, Content: []Part{
			{Type: PartText, Text: "甲"}, {Type: PartThinking, Text: "想"}, {Type: PartToolCall, ID: "1", Name: "t", Args: map[string]any{}}, {Type: PartText, Text: "乙"},
		}, Privacy: Local, Provider: "p", StopReason: "stop"}
		if got := TextOf(m); got != "甲乙" {
			t.Fatal(got)
		}
	})
}
