package repository

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"iter"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// SseEvent:Event 空字串 = 沒有 event 欄位。「沒有」與「event: (空值)」這裡不分;
// 目前沒有任何 provider 依賴這個差別。
type SseEvent struct {
	Event string
	Data  string
}

// splitLines 是 bufio.SplitFunc:行尾可為 \r\n、\n、\r。資料尾端落單的 \r 可能是 \r\n 被切開,等下一段再判斷。
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	i := bytes.IndexAny(data, "\r\n")
	if i < 0 {
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
	if data[i] == '\n' {
		return i + 1, data[:i], nil
	}
	if i+1 < len(data) {
		if data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return i + 1, data[:i], nil
	}
	return 0, nil, nil // 落單的 \r 在尾端:等更多資料
}

// ParseSSE 逐筆產生事件;讀取錯誤以 (零值, err) 產生一次後結束。行長不設上限(受記憶體限制)。
func ParseSSE(r io.Reader) iter.Seq2[SseEvent, error] {
	return func(yield func(SseEvent, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), math.MaxInt32)
		sc.Split(splitLines)
		var event string
		var data []string
		flush := func() (SseEvent, bool) {
			e, ok := SseEvent{Event: event, Data: strings.Join(data, "\n")}, len(data) > 0
			event, data = "", nil
			return e, ok
		}
		first := true
		for sc.Scan() {
			line := sc.Text()
			if first {
				line = strings.TrimPrefix(line, "\xef\xbb\xbf") // TextDecoder 會去掉開頭的 BOM
				first = false
			}
			switch {
			case line == "":
				if e, ok := flush(); ok && !yield(e, nil) {
					return
				}
			case strings.HasPrefix(line, ":"):
				// 註解行
			default:
				field, val, found := strings.Cut(line, ":")
				if found {
					val = strings.TrimPrefix(val, " ")
				}
				switch field {
				case "event":
					event = val
				case "data":
					data = append(data, val)
				}
			}
		}
		if err := sc.Err(); err != nil {
			yield(SseEvent{}, err)
			return
		}
		if e, ok := flush(); ok {
			yield(e, nil)
		}
	}
}

// FetchWithRetry:429 與 5xx 重試,最多 retries 次;退避 500ms×2^n,有 retry-after(秒)照用但上限 10s;
// ctx 取消立刻放棄並回 ctx.Err()。newReq 每次重試都要給新的 *http.Request(body 只能讀一次)。
func FetchWithRetry(ctx context.Context, do func(*http.Request) (*http.Response, error), newReq func() (*http.Request, error), retries int) (*http.Response, error) {
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		res, err := do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if res.StatusCode != 429 && res.StatusCode < 500 || n >= retries {
			return res, nil
		}
		res.Body.Close()
		ms := 500 * math.Pow(2, float64(n))
		if h, ok := res.Header["Retry-After"]; ok {
			// Number(header):照 JS 規則解析("" = 0、"1e1" = 10、"abc" = NaN)
			if ra := entity.JSNumber(strings.Join(h, ", ")); !math.IsNaN(ra) && !math.IsInf(ra, 0) && ra >= 0 {
				ms = ra * 1000
			}
		}
		t := time.NewTimer(time.Duration(min(ms, 10_000) * float64(time.Millisecond)))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
