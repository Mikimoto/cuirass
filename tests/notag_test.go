//go:build !test

package cuirass_test

import "testing"

func TestRequiresTestTag(t *testing.T) {
	t.Fatal("要帶 -tags test:go test -tags test ./...")
}
