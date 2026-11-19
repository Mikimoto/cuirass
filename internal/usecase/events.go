package usecase

import (
	"sync"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// Event.Type 的值。
const (
	EventTurnStart     = "turn_start"
	EventTurnEnd       = "turn_end"
	EventTextDelta     = "text_delta"
	EventThinkingDelta = "thinking_delta"
	EventToolStart     = "tool_start"
	EventToolBlocked   = "tool_blocked"
	EventToolEnd       = "tool_end"
	EventMessage       = "message"
	EventNotice        = "notice"
)

// Event 是 framework 對外事件,Type 決定哪些欄位有意義:
//
//	turn_start     Agent
//	turn_end       Agent、Reason(done | aborted | timeout | cap | error | blocked)
//	text_delta     Agent、Text
//	thinking_delta Agent、Text(保留給未來;turn 目前不轉發)
//	tool_start     Agent、ID、Name、Args
//	tool_blocked   Agent、ID、Name、Args、Reason
//	tool_end       Agent、ID、Name、Args、IsError、Details
//	message        Agent、Message(assistant)、Final
//	notice         Agent、Text
//
// listener 收到的是深複本,改了也影響不到 framework(spec §6.3)。
type Event struct {
	Type    string
	Agent   string
	Reason  string
	Text    string
	ID      string
	Name    string
	Args    map[string]any
	IsError bool
	Details any
	Message *entity.Message
	Final   bool
}

type Listener func(Event)

// Emitter 可併發使用:On / 取消 / Emit 都上鎖,listener 在鎖外呼叫(listener 裡再 On 不會死結)。零值可用。
type Emitter struct {
	mu        sync.Mutex
	listeners []*Listener // 指標當身分:同一個函式訂兩次也各算一次;切片保持訂閱順序
}

// On 回傳取消訂閱的函式(重複呼叫無害)。
func (em *Emitter) On(l Listener) (off func()) {
	w := &l
	em.mu.Lock()
	em.listeners = append(em.listeners, w)
	em.mu.Unlock()
	return func() {
		em.mu.Lock()
		defer em.mu.Unlock()
		for i, x := range em.listeners {
			if x == w {
				em.listeners = append(em.listeners[:i:i], em.listeners[i+1:]...)
				return
			}
		}
	}
}

// Emit:每個 listener 各拿一份深複本;複製失敗或 listener panic 都吞掉,不影響防護結果。nil Emitter 什麼都不做。
func (em *Emitter) Emit(e Event) {
	if em == nil {
		return
	}
	em.mu.Lock()
	ls := em.listeners // On/off 不改舊陣列內容(off 用三索引切片另配),這裡拿到的是不會變的快照
	em.mu.Unlock()
	for _, l := range ls {
		_ = try(func() error {
			c, err := cloneEvent(e)
			if err != nil {
				return err
			}
			(*l)(c)
			return nil
		})
	}
}

func cloneEvent(e Event) (Event, error) {
	var err error
	if e.Args != nil {
		if e.Args, err = entity.CloneJSON(e.Args); err != nil {
			return e, err
		}
	}
	if e.Details != nil {
		if e.Details, err = entity.CloneJSON(e.Details); err != nil {
			return e, err
		}
	}
	if e.Message != nil {
		m, err := entity.CloneJSON(*e.Message)
		if err != nil {
			return e, err
		}
		e.Message = &m
	}
	return e, nil
}
