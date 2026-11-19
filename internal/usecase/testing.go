//go:build test

package usecase

// 給外部測試(package usecase_test)用的未匯出成員。

var (
	MsDuration = msDuration
	ErrStopped = errStopped
)

// QueueLen 回排隊中加執行中的輪數。
func (a *Agent) QueueLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue)
}
