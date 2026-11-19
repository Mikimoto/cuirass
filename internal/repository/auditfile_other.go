//go:build !unix

package repository

import "errors"

// 沒有 O_NOFOLLOW 的平台(Windows)擋不住 symlink,寧可拒寫。
func appendLog(string, []byte) error { return errors.New("此平台不支援 O_NOFOLLOW") }
