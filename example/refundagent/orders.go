package main

import (
	"fmt"
	"sync"
)

// Order 是一筆訂單;Amount 單位是新台幣元。
type Order struct {
	ID       string
	Amount   int
	Refunded bool
}

// Store 是記憶體裡的訂單表與工單表。
// 工具是一個一個執行的,但逾時被放棄的 Execute goroutine 可能還在跑,所以用 mutex 保護。
type Store struct {
	mu      sync.Mutex
	orders  map[string]*Order
	tickets []string
}

// NewStore 建立四筆假訂單:A 1200、B 5000、C 800、D 2500。
func NewStore() *Store {
	s := &Store{orders: map[string]*Order{}}
	for _, o := range []Order{{ID: "A", Amount: 1200}, {ID: "B", Amount: 5000}, {ID: "C", Amount: 800}, {ID: "D", Amount: 2500}} {
		s.orders[o.ID] = &o
	}
	return s
}

// Get 查訂單。
func (s *Store) Get(id string) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Refund 全額退款;找不到或已退過回 error。
func (s *Store) Refund(id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, fmt.Errorf("找不到訂單 %s", id)
	}
	if o.Refunded {
		return Order{}, fmt.Errorf("訂單 %s 已退過", id)
	}
	o.Refunded = true
	return *o, nil
}

// CreateTicket 開一張轉專人審核的工單,回單號(T-001 起遞增;固定格式,離線劇本才能事先寫好)。
func (s *Store) CreateTicket(orderID, reason string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("T-%03d", len(s.tickets)+1)
	s.tickets = append(s.tickets, id)
	return id
}

// Tickets 回已開的單號。
func (s *Store) Tickets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tickets...)
}
