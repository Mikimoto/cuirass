//go:build test && !unix

package repository

import (
	"testing"

	"github.com/Mikimoto/cuirass/internal/entity"
)

// 沒有 O_NOFOLLOW 的平台(Windows):一律拒寫並只 warn 一次。
func TestAuditFileNoNofollowRefuses(t *testing.T) {
	var msgs []string
	a := NewFileAudit(t.TempDir(), func(m string) { msgs = append(msgs, m) })
	if a.Write(entity.AuditEvent{Kind: "tool"}) || a.Write(entity.AuditEvent{Kind: "tool"}) {
		t.Fatal("應拒寫")
	}
	if len(msgs) != 1 || msgs[0] != "audit 寫入失敗:此平台不支援 O_NOFOLLOW" {
		t.Fatal(msgs)
	}
}
