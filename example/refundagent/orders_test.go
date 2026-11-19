//go:build test

package main

import (
	"strings"
	"testing"

	"github.com/Mikimoto/cuirass"
)

func TestRefundOnce(t *testing.T) {
	o, err := NewStore().Refund("A")
	if err != nil || o != (Order{ID: "A", Amount: 1200, Refunded: true}) {
		t.Fatalf("got %+v, %v", o, err)
	}
}

func TestRefundTwiceFails(t *testing.T) {
	s := NewStore()
	if _, err := s.Refund("A"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refund("A"); err == nil || !strings.Contains(err.Error(), "已退過") {
		t.Fatalf("第二次退款應失敗:%v", err)
	}
}

func TestRefundUnknown(t *testing.T) {
	if _, err := NewStore().Refund("Z"); err == nil || !strings.Contains(err.Error(), "找不到") {
		t.Fatalf("不存在的訂單應失敗:%v", err)
	}
}

func TestTicketIDs(t *testing.T) {
	s := NewStore()
	if a, b := s.CreateTicket("B", "x"), s.CreateTicket("B", "y"); a != "T-001" || b != "T-002" {
		t.Fatalf("單號 %q %q", a, b)
	}
	if got := s.Tickets(); len(got) != 2 {
		t.Fatalf("Tickets() = %v", got)
	}
}

func TestMemAudit(t *testing.T) {
	var m MemAudit
	m.Write(cuirass.AuditEvent{Kind: "tool"})
	m.Write(cuirass.AuditEvent{Kind: "block"})
	if ev := m.Events(); len(ev) != 2 || ev[0].Kind != "tool" || ev[1].Kind != "block" {
		t.Fatalf("Events() = %+v", ev)
	}
}
