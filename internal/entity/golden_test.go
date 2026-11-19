//go:build test

package entity

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// 凍結的 golden 答案放在 testdata/golden/<測試名>.json.gz(gzip 壓縮,減少 go get 下載量):每行一筆 [輸入, 預期輸出]。
// 輸入由測試用固定 seed 產生,讀檔時逐筆比對輸入沒變,再把預期輸出交給原本的比對邏輯。

// goldenCases 是各 golden 測試的案例數(len(cases));golden 檔只能由審查過的 commit 手動更新。
var goldenCases = map[string]int{
	"TestAuditGolden":         4610,
	"TestDefaultLimitsGolden": 1,
	"TestDifferentialGolden":  18076,
	"TestGateGolden":          2000,
	"TestPrivacyGolden":       3064,
}

func loadGolden(t *testing.T, cases []goldenCase) []json.RawMessage {
	t.Helper()
	path := filepath.Join("testdata", "golden", t.Name()+".json.gz")
	raw := readGzip(t, path)
	var rows [][2]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("%s:%v", path, err)
	}
	if n, ok := goldenCases[t.Name()]; !ok || len(cases) != n || len(rows) != n {
		t.Fatalf("案例數:測試產生 %d、golden %d,預期 %d(有登記:%v)", len(cases), len(rows), n, ok)
	}
	want := make([]json.RawMessage, len(rows))
	for i, c := range cases {
		b, _ := json.Marshal(c)
		if !bytes.Equal(b, rows[i][0]) {
			t.Fatalf("第 %d 筆輸入與 golden 不同(產生器或 seed 變了):\n  測試:%s\n  golden:%s", i, b, rows[i][0])
		}
		want[i] = rows[i][1]
	}
	return want
}

// readGzip 讀 gzip 壓縮的 golden 檔。
func readGzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s:%v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s:%v", path, err)
	}
	return b
}
