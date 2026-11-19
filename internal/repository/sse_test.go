//go:build test

package repository

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// chunks 讓每次 Read 只回一段,模擬跨 chunk。
type chunks struct{ parts []string }

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts[0] = c.parts[0][n:]
	if c.parts[0] == "" {
		c.parts = c.parts[1:]
	}
	return n, nil
}

func collect(t *testing.T, parts ...string) []SseEvent {
	t.Helper()
	out := []SseEvent{}
	for e, err := range ParseSSE(&chunks{parts}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestParseSSE(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		want  []SseEvent
	}{
		{"跨 chunk 的事件", []string{"event: a\nda", "ta: 1\n\ndata: 2\n", "\n"}, []SseEvent{{Event: "a", Data: "1"}, {Data: "2"}}},
		{`\r\n 與多行 data`, []string{"data: x\r\ndata: y\r\n\r\n"}, []SseEvent{{Data: "x\ny"}}},
		{"註解行略過", []string{": ping\n\ndata: ok\n\n"}, []SseEvent{{Data: "ok"}}},
		// \r\n 剛好被切在兩個 chunk 之間,不能多出一個空行事件
		{`\r\n 被切在 chunk 邊界`, []string{"data: x\r", "\ndata: y\r\n\r\n"}, []SseEvent{{Data: "x\ny"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collect(t, c.parts...); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v", got)
			}
		})
	}
	t.Run("讀取錯誤會回報", func(t *testing.T) {
		boom := errors.New("boom")
		var got error
		for _, err := range ParseSSE(iotest.ErrReader(boom)) {
			got = err
		}
		if !errors.Is(got, boom) {
			t.Fatal(got)
		}
	})
}

func res(status int, h map[string]string) *http.Response {
	r := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("x"))}
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func newReq() (*http.Request, error) { return http.NewRequest("GET", "http://x", nil) }

func TestFetchWithRetry(t *testing.T) {
	ctx := context.Background()
	t.Run("兩次 429 後 200", func(t *testing.T) {
		seq, n := []int{429, 429, 200}, 0
		do := func(*http.Request) (*http.Response, error) {
			n++
			return res(seq[n-1], map[string]string{"retry-after": "0"}), nil
		}
		r, err := FetchWithRetry(ctx, do, newReq, 2)
		if err != nil || r.StatusCode != 200 || n != 3 {
			t.Fatal(r, err, n)
		}
	})
	t.Run("三次 429 回最後一個 429", func(t *testing.T) {
		n := 0
		do := func(*http.Request) (*http.Response, error) {
			n++
			return res(429, map[string]string{"retry-after": "0"}), nil
		}
		r, err := FetchWithRetry(ctx, do, newReq, 2)
		if err != nil || r.StatusCode != 429 || n != 3 {
			t.Fatal(r, err, n)
		}
	})
	t.Run("abort 時立即丟 AbortError", func(t *testing.T) {
		// AbortError → context.Canceled;500 後會等 500ms 退避,取消要立刻打斷
		ctx, cancel := context.WithCancel(ctx)
		do := func(*http.Request) (*http.Response, error) { cancel(); return res(500, nil), nil }
		t0 := time.Now()
		_, err := FetchWithRetry(ctx, do, newReq, 2)
		if !errors.Is(err, context.Canceled) || time.Since(t0) > 100*time.Millisecond {
			t.Fatal(err, time.Since(t0))
		}
	})
	t.Run("404 不重試", func(t *testing.T) {
		n := 0
		do := func(*http.Request) (*http.Response, error) { n++; return res(404, nil), nil }
		r, _ := FetchWithRetry(ctx, do, newReq, 2)
		if r.StatusCode != 404 || n != 1 {
			t.Fatal(r.StatusCode, n)
		}
	})
}
