package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	maxPathLen  = 200
	maxFileSize = 8 << 10 // 單檔上限 8 KiB
	maxFiles    = 50
)

// Workspace 是記憶體裡的假檔案系統:path -> 內容。
//
// 為什麼不直接讀寫磁碟?cuirass 目前在註冊時就拒絕 Effects 為 fs / exec 的工具
// (L4 sandbox 還沒實作)。所以教學版的工具只碰這個 map,並誠實宣告 EffectsNone。
// 等 sandbox 實作後,把這裡換成真的檔案操作、Effects 改成 EffectsFS 即可,其他程式不用動。
type Workspace struct {
	mu    sync.Mutex
	files map[string]string
}

// NewWorkspace 建立並放入兩個範例 Go 檔。
func NewWorkspace() *Workspace {
	return &Workspace{files: map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tprintln(Add(1, 2))\n}\n",
		"add.go":  "package main\n\n// Add 回傳兩數之和。\nfunc Add(a, b int) int {\n\treturn a - b // BUG: 應該是加法\n}\n",
	}}
}

func validPath(p string) error {
	if p == "" || len(p) > maxPathLen {
		return fmt.Errorf("路徑長度需介於 1 到 %d", maxPathLen)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\\") {
		return fmt.Errorf("路徑只能是工作區內的相對路徑:%q", p)
	}
	return nil
}

// List 回排序後的檔名。
func (w *Workspace) List() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.names()
}

// names 假設呼叫端已持鎖。
func (w *Workspace) names() []string {
	names := make([]string, 0, len(w.files))
	for k := range w.files {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Read 讀檔;不存在回 error。
func (w *Workspace) Read(path string) (string, error) {
	if err := validPath(path); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.files[path]
	if !ok {
		return "", fmt.Errorf("檔案不存在:%s", path)
	}
	return c, nil
}

// Write 寫檔(新增或覆蓋),檢查大小與檔案數上限。
func (w *Workspace) Write(path, content string) error {
	if err := validPath(path); err != nil {
		return err
	}
	if len(content) > maxFileSize {
		return fmt.Errorf("內容超過 %d bytes", maxFileSize)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.files[path]; !ok && len(w.files) >= maxFiles {
		return fmt.Errorf("檔案數已達上限 %d", maxFiles)
	}
	w.files[path] = content
	return nil
}

// Search 逐行找子字串,回 "path:行號: 內容"。
func (w *Workspace) Search(query string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var hits []string
	for _, name := range w.names() {
		for i, line := range strings.Split(w.files[name], "\n") {
			if strings.Contains(line, query) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", name, i+1, line))
			}
		}
	}
	return hits
}
