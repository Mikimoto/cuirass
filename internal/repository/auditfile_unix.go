//go:build unix

package repository

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// appendLog:目錄 0700、檔案 0600;mode 會被 umask 削弱、既有目錄或檔案可能權限太寬,故一律再 chmod。
// O_NOFOLLOW:audit.log 是 symlink 就開檔失敗(拒寫),不會寫到連結目標。
// O_NONBLOCK + 只收一般檔案:audit.log 是 FIFO 等特殊檔時立刻拒寫,不會在持鎖時卡住。
func appendLog(dir string, line []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errors.New("audit.log 不是一般檔案")
		}
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
