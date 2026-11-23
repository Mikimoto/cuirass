package main

import (
	"sync"

	"github.com/Mikimoto/cuirass"
)

// MemAudit 是記在記憶體的稽核,給 demo 與測試讀。
// 正式環境改用 cuirass.NewFileAudit,目錄放在只有執行者可寫的位置。
type MemAudit struct {
	mu     sync.Mutex
	events []cuirass.AuditEvent
}

// Write 記一筆,永遠成功。可能被不同 goroutine 同時呼叫。
func (m *MemAudit) Write(e cuirass.AuditEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return true
}

// Events 回目前所有紀錄的複本。
func (m *MemAudit) Events() []cuirass.AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cuirass.AuditEvent(nil), m.events...)
}
