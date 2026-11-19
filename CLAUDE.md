# cuirass

Go agent framework，防護層是 agent 迴圈裡固定的步驟，使用者關不掉。使用說明與開發指令看 `README.md`。

## 守則

- 文件、註解、commit message 用台灣繁體中文。
- 分層方向：`internal/entity` ← `internal/usecase` ← `internal/repository` ← 根套件。usecase 定義介面（例如 `internal/usecase/llm.go`），repository 實作。`tests/layers_test.go` 會擋反向 import。
- 不要加讓防護可關閉的選項或旁路。所有送進 provider 的請求都走同一條送出管線。
- 測試檔開頭要有 `//go:build test`；`example/example_test.go` 例外，讓 `go test ./...` 不帶 tag 也能跑範例。少了 tag 時 `tests/notag_test.go` 會失敗。
- 白箱測試放套件目錄；只用公開 API 的測試放 `tests/`；需要 internal 的外部測試放 `tests/internal/<路徑>`。
- 新增套件要在 `scripts/coverage-floor.sh` 加覆蓋率下限。

## 改對外行為

1. 先改 `docs/contract/` 的契約與 `behaviors.md`。
2. 實作，把新測試名稱加進契約的 `tests`。
3. 跑 `mise run test` 和 `python3 scripts/imp-check.py --root .`，兩個都要過。

## 分支與發版

- git-flow：feature 從 `dev` 開，用 PR 合回 `dev`；release 從 `dev` 開，合進 `main` 並打 tag。
- repo 是 public。ruleset `copilot-review-dev` 讓 Copilot 自動 review 進 `dev` 的 PR，收尾時要跑 Copilot review；`protect-main-dev` 禁止刪除與 force push `main`、`dev`。
- 版本 tag 用 `vX.Y.Z`。
