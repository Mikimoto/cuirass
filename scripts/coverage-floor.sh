#!/usr/bin/env bash
# 覆蓋率下限:讀 coverage.out(go test -coverpkg=./... 產生),
# 每個套件不得低於下限,總體不得低於 80%。低於就非 0 結束。
# 下限 = 2026-11-12 重量值減 1(取整數);改程式讓覆蓋率上升後可以調高,不要調低。
set -euo pipefail
profile="${1:-coverage.out}"
[ -f "$profile" ] || { echo "找不到 $profile" >&2; exit 1; }

floors='github.com/Mikimoto/cuirass 99;github.com/Mikimoto/cuirass/internal/entity 96;github.com/Mikimoto/cuirass/internal/repository 93;github.com/Mikimoto/cuirass/internal/repository/providers 99;github.com/Mikimoto/cuirass/internal/repository/providers/anthropic 88;github.com/Mikimoto/cuirass/internal/repository/providers/google 89;github.com/Mikimoto/cuirass/internal/repository/providers/openai 90;github.com/Mikimoto/cuirass/internal/repository/providers/stub 99;github.com/Mikimoto/cuirass/internal/usecase 94;github.com/Mikimoto/cuirass/example/codingagent 86;TOTAL 80'

# 多個測試執行檔會重複同一個區塊:以 檔案:位置 去重,任一次有執行就算覆蓋。
awk -v floors="$floors" '
/^mode:/ { next }
{
  split($1, a, ":"); file = a[1]
  pkg = file; sub(/\/[^\/]*$/, "", pkg)
  stmts[$1] = $2; pkgof[$1] = pkg
  if ($3 > 0) hit[$1] = 1
}
END {
  n = split(floors, lines, ";")
  for (i = 1; i <= n; i++) if (split(lines[i], f, " ") == 2) floor[f[1]] = f[2]
  for (b in stmts) {
    p = pkgof[b]; total[p] += stmts[b]; all += stmts[b]
    if (b in hit) { cov[p] += stmts[b]; allcov += stmts[b] }
  }
  fail = 0
  printf "%-62s %7s %6s\n", "套件", "實測", "下限"
  for (p in total) {
    pct = 100 * cov[p] / total[p]
    if (!(p in floor)) { printf "%-62s %6.1f%%  沒有下限(新套件?請加進 scripts/coverage-floor.sh)\n", p, pct; fail = 1; continue }
    bad = pct < floor[p]
    printf "%-62s %6.1f%% %5d%% %s\n", p, pct, floor[p], bad ? "低於下限" : ""
    if (bad) fail = 1
  }
  pct = 100 * allcov / all
  bad = pct < floor["TOTAL"]
  printf "%-62s %6.1f%% %5d%% %s\n", "總體", pct, floor["TOTAL"], bad ? "低於下限" : ""
  if (bad) fail = 1
  exit fail
}' "$profile"
