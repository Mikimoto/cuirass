//go:build test

package usecase

import (
	"reflect"
	"sync"
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// listener panic 吞掉,其他 listener 照收;每個 listener 拿各自的深複本。
func TestEmitterListenerPanicSwallowed(t *testing.T) {
	em := &Emitter{}
	var got []Event
	em.On(func(Event) { panic("listener 壞掉") })
	em.On(func(e Event) {
		e.Args["q"] = "hacked"
		e.Message.Content[0].Text = "hacked"
		e.Details.(map[string]any)["n"] = 0
	})
	em.On(func(e Event) { got = append(got, e) })
	args := map[string]any{"q": "x"}
	m := &entity.Message{Role: entity.RoleAssistant, Content: []entity.Part{{Type: entity.PartText, Text: "hi"}}}
	details := map[string]any{"n": 1.0}
	em.Emit(Event{Type: "tool_end", Agent: "main", Args: args, Message: m, Details: details})
	if len(got) != 1 || got[0].Args["q"] != "x" || got[0].Message.Content[0].Text != "hi" || got[0].Details.(map[string]any)["n"] != 1.0 {
		t.Fatalf("%+v", got)
	}
	if args["q"] != "x" || m.Content[0].Text != "hi" || details["n"] != 1.0 {
		t.Fatal("listener 改到 framework 的值")
	}
}

func TestEmitterOnOff(t *testing.T) {
	em := &Emitter{}
	n := 0
	l := func(Event) { n++ }
	off1 := em.On(l)
	em.On(l) // 同一個函式訂兩次也各算一次
	em.Emit(Event{Type: "notice"})
	off1()
	off1() // 重複取消無害
	em.Emit(Event{Type: "notice"})
	if n != 3 {
		t.Fatal(n)
	}
	var nilEm *Emitter
	nilEm.Emit(Event{Type: "notice"}) // nil 不 panic
}

// 輪中訂閱/取消與 Emit 併發(Agent.On)→ -race 無警告。
func TestEmitterConcurrent(t *testing.T) {
	em := &Emitter{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			off := em.On(func(Event) {})
			off()
		}()
		go func() {
			defer wg.Done()
			em.Emit(Event{Type: "text_delta", Text: "x"})
		}()
	}
	wg.Wait()
}

func TestEmitterKeepsOrder(t *testing.T) {
	em := &Emitter{}
	var order []int
	for i := range 5 {
		em.On(func(Event) { order = append(order, i) })
	}
	em.Emit(Event{Type: "turn_start"})
	if !reflect.DeepEqual(order, []int{0, 1, 2, 3, 4}) {
		t.Fatal(order)
	}
}
