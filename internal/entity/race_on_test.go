//go:build test && race

package entity

// -race 會讓程式慢 5–10 倍且受機器負載影響,絕對時間門檻不可靠;改只看線性比值。
const raceEnabled = true
