//go:build test

package entity

import (
	"encoding/json"
	"testing"
)

// DefaultLimits() 與 golden 逐欄相同(PerTool nil = {})。
func TestDefaultLimitsGolden(t *testing.T) {
	want := loadGolden(t, []goldenCase{{Fn: "defaultLimits"}})
	l := DefaultLimits()
	if l.PerTool != nil {
		t.Fatal("DefaultLimits().PerTool 應為 nil,避免複製後寫到共用的 map")
	}
	got, _ := json.Marshal(map[string]any{
		"perTurn": l.PerTurn, "perSession": l.PerSession, "perToolPerTurn": map[string]int{}, "turnTimeoutMs": l.TurnTimeoutMs,
		"maxOutputTokens": l.MaxOutputTokens, "toolResultBytes": l.ToolResultBytes, "maxModelCallsPerTurn": l.MaxModelCallsPerTurn,
	})
	if !reflectEqual(normalize(t, got), normalize(t, want[0])) {
		t.Fatalf("go: %s\nts: %s", got, want[0])
	}
}
