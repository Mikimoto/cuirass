//go:build test

package entity

import (
	"encoding/json"
	"strings"
	"testing"
)

// restoreJSON 照應用層的寫法從 JSON 還原;錯誤刻意不看(壞掉的 JSON 也必須擋)。
func restoreJSON(s string) *BudgetState {
	var st BudgetState
	_ = json.Unmarshal([]byte(s), &st)
	return &st
}

func limitsWith(f func(*Limits)) Limits {
	l := DefaultLimits()
	f(&l)
	return l
}

func wantCap(t *testing.T, r GateResult, reason, msg string) {
	t.Helper()
	if r.OK || !r.Cap || (reason != "" && r.Reason != reason) {
		t.Errorf("%s:預期 cap 擋(%q),得到 %+v", msg, reason, r)
	}
}

func TestBudget(t *testing.T) {
	L := DefaultLimits()

	t.Run("第 21 次呼叫被擋且 cap", func(t *testing.T) {
		b := NewBudget(L, nil)
		b.ResetTurn("main")
		for i := range 20 {
			if r := b.Reserve("main", "t"); r != (GateResult{OK: true}) {
				t.Fatalf("第 %d 次:%+v", i+1, r)
			}
		}
		wantCap(t, b.Reserve("main", "t"), "工具呼叫已達上限(20 次)", "第 21 次")
	})

	t.Run("session 額度跨輪累積;resetTurn 不清", func(t *testing.T) {
		b := NewBudget(limitsWith(func(l *Limits) { l.PerSession, l.PerTurn = 3, 2 }), nil)
		b.ResetTurn("main")
		if !b.Reserve("main", "t").OK || !b.Reserve("main", "t").OK {
			t.Fatal("前兩次應放行")
		}
		b.ResetTurn("main")
		if !b.Reserve("main", "t").OK {
			t.Fatal("第三次應放行")
		}
		wantCap(t, b.Reserve("main", "t"), "本次對話工具呼叫已達上限(3 次),請重啟", "第四次")
	})

	t.Run("perToolPerTurn:web_search 第 4 次擋", func(t *testing.T) {
		b := NewBudget(limitsWith(func(l *Limits) { l.PerTool = map[string]int{"web_search": 3} }), nil)
		b.ResetTurn("main")
		for range 3 {
			if !b.Reserve("main", "web_search").OK {
				t.Fatal("前三次應放行")
			}
		}
		wantCap(t, b.Reserve("main", "web_search"), "web_search 已達上限(3 次)", "第四次")
		if !b.Reserve("main", "other").OK {
			t.Fatal("其他工具不受影響")
		}
	})

	t.Run("被擋的呼叫也計數;spend 吃 session 額度", func(t *testing.T) {
		b := NewBudget(limitsWith(func(l *Limits) { l.PerSession = 2 }), nil)
		b.Reserve("main", "t")
		b.Reserve("main", "t")
		if b.Reserve("main", "t").OK {
			t.Fatal("第三次應擋")
		}
		if n := b.TurnCount("main", "t"); n != 3 {
			t.Fatalf("turnCount=%d", n)
		}
		if b.Spend() {
			t.Fatal("spend 應回 false")
		}
	})

	t.Run("snapshot 後還原,額度延續", func(t *testing.T) {
		limits := limitsWith(func(l *Limits) { l.PerSession = 3 })
		b := NewBudget(limits, nil)
		b.Reserve("main", "t")
		b.Reserve("main", "t")
		raw, err := json.Marshal(b.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		b2 := NewBudget(limits, restoreJSON(string(raw)))
		if !b2.Reserve("main", "t").OK || b2.Reserve("main", "t").OK {
			t.Fatal("還原後應剩一次")
		}
		if s := b.Snapshot().Session; s != 2 {
			t.Fatalf("原 budget 不受影響,session=%d", s)
		}
	})

	t.Run("還原的 session 壞值(NaN、負數、Infinity、字串、null)→ 當成用完,呼叫一律擋", func(t *testing.T) {
		// NaN / ±Infinity 經 JSON.stringify 都變 null;NaN 原樣寫進檔案是壞 JSON
		for _, bad := range []string{"null", "-5", "1.5", `"0"`, "{}", "[]", "true", "1e999", "NaN", "Infinity"} {
			b := NewBudget(L, restoreJSON(`{"session":`+bad+`,"turn":{}}`))
			wantCap(t, b.Reserve("main", "t"), "", bad)
			if b.Spend() {
				t.Errorf("%s:spend 應回 false", bad)
			}
		}
		b := NewBudget(L, restoreJSON(`{"turn":{}}`)) // undefined
		wantCap(t, b.Reserve("main", "t"), "", "session 缺")
	})

	t.Run("還原的 turn 計數壞值 → 當成用完;turn 結構壞掉 → 整個 session 當成用完", func(t *testing.T) {
		st := func(turn string) *BudgetState { return restoreJSON(`{"session":0,"turn":` + turn + `}`) }
		wantCap(t, NewBudget(L, st(`{"main":{"calls":null,"perTool":{}}}`)).Reserve("main", "t"), "", "calls null")
		wantCap(t, NewBudget(L, st(`{"main":{"calls":-100,"perTool":{}}}`)).Reserve("main", "t"), "", "calls -100")
		lim := limitsWith(func(l *Limits) { l.PerTool = map[string]int{"t": 3} })
		wantCap(t, NewBudget(lim, st(`{"main":{"calls":0,"perTool":{"t":-10}}}`)).Reserve("main", "t"), "", "perTool -10")
		wantCap(t, NewBudget(lim, st(`{"main":{"calls":0,"perTool":{"t":"1"}}}`)).Reserve("main", "t"), "", `perTool "1"`)
		for _, bad := range []string{"null", `"x"`, "[]", `{"main":null}`, `{"main":{"calls":0,"perTool":null}}`, `{"main":{"calls":0,"perTool":[]}}`} {
			wantCap(t, NewBudget(L, st(bad)).Reserve("other", "t"), "", bad)
		}
		wantCap(t, NewBudget(L, restoreJSON(`"x"`)).Reserve("main", "t"), "", "state 不是物件")
	})

	t.Run("還原的 turn 計數壞值 → 整個 session 用完,resetTurn 清不掉", func(t *testing.T) {
		lim := limitsWith(func(l *Limits) { l.PerTool = map[string]int{"t": 3} })
		for _, main := range []string{`{"calls":null,"perTool":{}}`, `{"calls":0,"perTool":{"t":-1}}`, `{"calls":"0","perTool":{}}`} {
			b := NewBudget(lim, restoreJSON(`{"session":0,"turn":{"main":`+main+`}}`))
			b.ResetTurn("main")
			wantCap(t, b.Reserve("main", "t"), "", main)
			if b.Spend() {
				t.Errorf("%s:spend 應回 false", main)
			}
		}
	})

	t.Run("用完的額度經 JSON 存檔(Infinity → null)再還原仍然擋", func(t *testing.T) {
		b := NewBudget(L, restoreJSON(`{"session":null,"turn":{}}`))
		raw, err := json.Marshal(b.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"session":null`) {
			t.Fatalf("用完應存成 null:%s", raw)
		}
		if NewBudget(L, restoreJSON(string(raw))).Reserve("main", "t").OK {
			t.Fatal("還原後應擋")
		}
	})

	t.Run("正常還原的值照用;沒給 state 從 0 開始", func(t *testing.T) {
		b := NewBudget(limitsWith(func(l *Limits) { l.PerTurn = 2 }), restoreJSON(`{"session":1,"turn":{"main":{"calls":1,"perTool":{"t":1}}}}`))
		if n := b.TurnCount("main", "t"); n != 1 {
			t.Fatalf("turnCount=%d", n)
		}
		if !b.Reserve("main", "t").OK || b.Reserve("main", "t").OK {
			t.Fatal("應剩一次")
		}
		raw, _ := json.Marshal(NewBudget(L, nil).Snapshot())
		if string(raw) != `{"session":0,"turn":{}}` {
			t.Fatalf("初始 snapshot:%s", raw)
		}
	})
}

// 還原的 state 不可信:任何壞值都讓整個 session 用完,resetTurn 清不掉。
func TestBudgetRestoreInvalidExhausts(t *testing.T) {
	jsonCases := []string{
		`{"session":-1,"turn":{}}`,
		`{"session":null,"turn":{}}`,
		`{"session":"NaN","turn":{}}`,
		`{"session":NaN,"turn":{}}`,
		`{"session":9007199254740992,"turn":{}}`, // 超過 2^53-1
		`{"session":9223372036854775807,"turn":{}}`,
		`{"session":0,"turn":{},"extra":1}`,                               // 未知欄位
		`{"session":0,"turn":{"main":{"calls":0,"perTool":{},"x":1}}}`,    // turn 裡的未知欄位
		`{"session":0,"turn":{"main":{"perTool":{}}}}`,                    // calls 缺
		`{"session":0,"turn":{"main":{"calls":-1,"perTool":{}}}}`,         // 單輪計數壞值
		`{"session":0,"turn":{"main":{"calls":0,"perTool":{"t":1.5}}}}`,   // 單一工具計數壞值
		`{"session":0,"turn":{"other":{"calls":0,"perTool":{"t":null}}}}`, // 別的 agent 壞掉也一樣
		`{"session":0}`, `null`, `[]`, `0`, `""`, `{`, ``,
	}
	type tc struct {
		name string
		st   *BudgetState
	}
	var cases []tc
	for _, s := range jsonCases {
		cases = append(cases, tc{s, restoreJSON(s)})
	}
	cases = append(cases,
		tc{"struct 負 session", &BudgetState{Session: -1, Turn: map[string]TurnState{}}},
		tc{"struct session 超過 2^53-1", &BudgetState{Session: 1 << 53, Turn: map[string]TurnState{}}},
		tc{"struct 負 calls", &BudgetState{Turn: map[string]TurnState{"main": {Calls: -1, PerTool: map[string]int{}}}}},
		tc{"struct 負 perTool", &BudgetState{Turn: map[string]TurnState{"main": {PerTool: map[string]int{"t": -1}}}}},
		tc{"struct perTool nil", &BudgetState{Turn: map[string]TurnState{"main": {}}}},
		tc{"struct turn nil(零值)", &BudgetState{}},
		tc{"struct Exhausted", &BudgetState{Turn: map[string]TurnState{}, Exhausted: true}},
	)
	for _, c := range cases {
		b := NewBudget(DefaultLimits(), c.st)
		b.ResetTurn("main")
		b.ResetTurn("other")
		wantCap(t, b.Reserve("main", "t"), "本次對話工具呼叫已達上限(200 次),請重啟", c.name)
		if b.Spend() {
			t.Errorf("%s:spend 應回 false", c.name)
		}
		if !b.Snapshot().Exhausted {
			t.Errorf("%s:snapshot 應標用完", c.name)
		}
	}
	// 上限邊界的合法值照用
	b := NewBudget(DefaultLimits(), restoreJSON(`{"session":-0,"turn":{"main":{"calls":1e1,"perTool":{"t":2.0}}}}`))
	if r := b.Reserve("main", "t"); !r.OK || b.TurnCount("main", "t") != 3 {
		t.Fatalf("合法值應照用:%+v turnCount=%d", r, b.TurnCount("main", "t"))
	}
}

// 工具名或 agent 是 JS 原型鍵時,單一工具上限照常生效(JS 物件查表有這個漏洞;Go 用 map 自然正確,這裡鎖住)。
func TestBudgetPrototypeKeyTool(t *testing.T) {
	keys := []string{"constructor", "__proto__", "toString", "hasOwnProperty", "valueOf"}
	per := map[string]int{}
	for _, k := range keys {
		per[k] = 2
	}
	l := limitsWith(func(l *Limits) { l.PerTool = per })
	for _, agent := range []string{"main", "__proto__", "constructor"} {
		b := NewBudget(l, nil)
		b.ResetTurn(agent)
		for _, k := range keys {
			if !b.Reserve(agent, k).OK || !b.Reserve(agent, k).OK {
				t.Fatalf("%s/%s:前兩次應放行", agent, k)
			}
			wantCap(t, b.Reserve(agent, k), k+" 已達上限(2 次)", agent+"/"+k)
			if n := b.TurnCount(agent, k); n != 3 {
				t.Errorf("%s/%s:turnCount=%d", agent, k, n)
			}
		}
	}
	// 還原的 state 裡有原型鍵,也照一般鍵處理
	b := NewBudget(l, restoreJSON(`{"session":0,"turn":{"__proto__":{"calls":0,"perTool":{"__proto__":2,"constructor":1}}}}`))
	wantCap(t, b.Reserve("__proto__", "__proto__"), "__proto__ 已達上限(2 次)", "還原的 __proto__")
	if !b.Reserve("__proto__", "constructor").OK || b.Reserve("__proto__", "constructor").OK {
		t.Fatal("還原的 constructor 應剩一次")
	}
}

// snapshot 與還原都是深複本:改呼叫端的 map 動不到 budget。
func TestBudgetSnapshotIsCopy(t *testing.T) {
	st := &BudgetState{Turn: map[string]TurnState{"main": {Calls: 0, PerTool: map[string]int{}}}}
	b := NewBudget(limitsWith(func(l *Limits) { l.PerTool = map[string]int{"t": 1} }), st)
	st.Turn["main"].PerTool["t"] = 99
	if !b.Reserve("main", "t").OK {
		t.Fatal("改還原來源不應影響 budget")
	}
	snap := b.Snapshot()
	snap.Turn["main"].PerTool["t"] = -5
	snap.Turn["x"] = TurnState{}
	if n := b.TurnCount("main", "t"); n != 1 {
		t.Fatalf("改 snapshot 不應影響 budget,turnCount=%d", n)
	}
}

// 併發的 Reserve / Snapshot 不會 race,也不會超發(配合 -race)。
func TestBudgetConcurrent(t *testing.T) {
	b := NewBudget(limitsWith(func(l *Limits) { l.PerTurn, l.PerSession = 1000, 50 }), nil)
	done := make(chan int)
	for range 8 {
		go func() {
			ok := 0
			for range 20 {
				if b.Reserve("main", "t").OK {
					ok++
				}
				b.Snapshot()
			}
			done <- ok
		}()
	}
	total := 0
	for range 8 {
		total += <-done
	}
	if total != 50 {
		t.Fatalf("放行 %d 次,上限 50", total)
	}
}
