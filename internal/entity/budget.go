package entity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sync"
)

// 還原的計數只認 0 ≤ n ≤ 2^53-1 的整數(JS 的 Number.isSafeInteger 範圍);上限也讓 ++ 不可能溢位(假設 64 位元 int)。
const maxSafeInteger = 1<<53 - 1

// TurnState 是一個 agent 本輪的計數。
type TurnState struct {
	Calls   int            `json:"calls"`
	PerTool map[string]int `json:"perTool"`
}

// BudgetState 是 Budget 的存檔,JSON 形狀:{"session":n,"turn":{agent:{"calls":n,"perTool":{tool:n}}}}。
// Exhausted = session 用完;JSON 寫成 "session":null(Infinity 經 JSON.stringify 變 null)。
// 零值(Turn 為 nil)還原後當成用完(turn 缺值)。
// Exhausted 為 true 時 Session 沒有意義(Snapshot 填 0),不要拿它計算剩餘額度。
type BudgetState struct {
	Session   int
	Turn      map[string]TurnState
	Exhausted bool
}

func (s BudgetState) MarshalJSON() ([]byte, error) {
	var session any = s.Session
	if s.Exhausted {
		session = nil
	}
	turn := s.Turn
	if turn == nil {
		turn = map[string]TurnState{}
	}
	return json.Marshal(struct {
		Session any                  `json:"session"`
		Turn    map[string]TurnState `json:"turn"`
	}{session, turn})
}

// UnmarshalJSON 不回錯:還原的 state 來自檔案或應用層,不可信,任何壞值(型別不對、負數、非整數、null、缺欄位、未知欄位)
// 一律標成用完,不讓「解碼失敗就從 0 開始」變成放行。壞掉的 JSON 語法由 encoding/json 先擋下並回錯,
// 此時 s 保持原值;呼叫端用零值 BudgetState 接,還原後同樣當成用完。
func (s *BudgetState) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		*s = BudgetState{Exhausted: true}
		return nil
	}
	*s = budgetStateFrom(v)
	return nil
}

func onlyKeys(m map[string]any, a, b string) bool {
	for k := range m {
		if k != a && k != b {
			return false
		}
	}
	return true
}

func jsonCount(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f < 0 || f > maxSafeInteger {
		return 0, false
	}
	return int(f), true
}

func budgetStateFrom(v any) BudgetState {
	top, ok := v.(map[string]any)
	turn, ok2 := top["turn"].(map[string]any)
	if !ok || !ok2 {
		return BudgetState{Exhausted: true}
	}
	session, ok := jsonCount(top["session"])
	bad := !ok || !onlyKeys(top, "session", "turn")
	st := BudgetState{Session: session, Turn: map[string]TurnState{}}
	for agent, tv := range turn {
		t, ok := tv.(map[string]any)
		perTool, ok2 := t["perTool"].(map[string]any)
		if !ok || !ok2 {
			bad = true
			continue
		}
		calls, ok := jsonCount(t["calls"])
		bad = bad || !ok || !onlyKeys(t, "calls", "perTool")
		ts := TurnState{Calls: calls, PerTool: map[string]int{}}
		for tool, n := range perTool {
			c, ok := jsonCount(n)
			bad = bad || !ok
			ts.PerTool[tool] = c
		}
		st.Turn[agent] = ts
	}
	st.Exhausted = bad
	return st
}

func validCount(n int) bool { return n >= 0 && int64(n) <= maxSafeInteger }

// Budget 是工具呼叫額度。所有方法都上鎖,可以併發呼叫(例如 Agent 輪中取 snapshot)。
type Budget struct {
	mu     sync.Mutex
	limits Limits
	// exhausted:session 用完;之後任何呼叫都擋,ResetTurn 不清。
	exhausted bool
	session   int
	turn      map[string]*TurnState
}

// NewBudget 從 0 開始,或從 restore 還原。restore 不可信:任何壞值(含單輪計數)都讓整個 session 用完,
// 因為單輪計數會被 ResetTurn 清掉,不能只靠它擋。restore 會被深複製。
func NewBudget(l Limits, restore *BudgetState) *Budget {
	b := &Budget{limits: l, turn: map[string]*TurnState{}}
	if restore == nil {
		return b
	}
	bad := restore.Exhausted || restore.Turn == nil || !validCount(restore.Session)
	b.session = restore.Session
	for agent, t := range restore.Turn {
		if t.PerTool == nil {
			bad = true
			continue
		}
		bad = bad || !validCount(t.Calls)
		for _, n := range t.PerTool {
			bad = bad || !validCount(n)
		}
		b.turn[agent] = &TurnState{Calls: t.Calls, PerTool: maps.Clone(t.PerTool)}
	}
	if bad {
		b.exhausted, b.session = true, 0
	}
	return b
}

// ResetTurn 每輪開始清單輪計數;session 計數與用完狀態不清。
func (b *Budget) ResetTurn(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.turn[agentID] = &TurnState{PerTool: map[string]int{}}
}

// Reserve 先計數再比(含被擋的)。
func (b *Budget) Reserve(agentID, tool string) GateResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.turn[agentID]
	if t == nil {
		t = &TurnState{PerTool: map[string]int{}}
		b.turn[agentID] = t
	}
	t.PerTool[tool]++
	n := t.PerTool[tool]
	t.Calls++
	l := b.limits
	if b.exhausted {
		return GateResult{Reason: fmt.Sprintf("本次對話工具呼叫已達上限(%d 次),請重啟", l.PerSession), Cap: true}
	}
	b.session++
	if b.session > l.PerSession {
		return GateResult{Reason: fmt.Sprintf("本次對話工具呼叫已達上限(%d 次),請重啟", l.PerSession), Cap: true}
	}
	if t.Calls > l.PerTurn {
		return GateResult{Reason: fmt.Sprintf("工具呼叫已達上限(%d 次)", l.PerTurn), Cap: true}
	}
	if max, ok := l.PerTool[tool]; ok && n > max {
		return GateResult{Reason: fmt.Sprintf("%s 已達上限(%d 次)", tool, max), Cap: true}
	}
	return GateResult{OK: true}
}

// Spend 是非工具的模型呼叫(複核),吃 session 額度;超過回 false。
func (b *Budget) Spend() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted {
		return false
	}
	b.session++
	return b.session <= b.limits.PerSession
}

func (b *Budget) TurnCount(agentID, tool string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.turn[agentID]; t != nil {
		return t.PerTool[tool]
	}
	return 0
}

// Snapshot 回深複本。
func (b *Budget) Snapshot() BudgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BudgetState{Session: b.session, Turn: map[string]TurnState{}, Exhausted: b.exhausted}
	for agent, t := range b.turn {
		st.Turn[agent] = TurnState{Calls: t.Calls, PerTool: maps.Clone(t.PerTool)}
	}
	return st
}
