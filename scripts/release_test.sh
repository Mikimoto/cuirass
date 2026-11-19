#!/usr/bin/env bash
# 在暫存目錄建 git repo + 假 origin(bare repo),驗證 scripts/release.sh 的檢查。
# 只碰暫存目錄,不碰本 repo 與真正的遠端。只跑 dry-run(不加 --yes)。
set -euo pipefail

release="$(cd "$(dirname "$0")" && pwd)/release.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export MISE_TRUSTED_CONFIG_PATHS="$tmp"
unset GIT_DIR GIT_WORK_TREE

git init -q --bare -b main "$tmp/origin.git"
git init -q -b main "$tmp/work"
cd "$tmp/work"
git remote add origin "$tmp/origin.git"
printf 'module github.com/Mikimoto/cuirass\n\ngo 1.27\n' > go.mod
# release 會跑 mise run test / vuln;暫存 repo 用空的 task 代替。
printf '[tasks.test]\nrun = "true"\n\n[tasks.vuln]\nrun = "true"\n' > mise.toml
git add . && git commit -qm init && git push -q origin main
git tag archive-final && git push -q origin archive-final

fails=0
# expect <期望結束碼:0 或 fail> <訊息片段> <版本...>
expect() {
  local want="$1" msg="$2"; shift 2
  local out code=0
  out="$("$release" "$@" 2>&1)" || code=$?
  if { [ "$want" = 0 ] && [ "$code" = 0 ]; } || { [ "$want" = fail ] && [ "$code" != 0 ]; }; then
    if grep -qF -- "$msg" <<<"$out"; then echo "ok   $* → $code:$msg"; return; fi
  fi
  echo "FAIL $*(結束碼 $code,期望 $want 與「$msg」)輸出:"; echo "$out" | sed 's/^/     /'
  fails=$((fails + 1))
}

# archive-final 不算前一個版本:v0.1.0 全部檢查通過,dry-run 不建 tag
expect 0 "dry-run,沒有加 --yes" v0.1.0
git rev-parse -q --verify refs/tags/v0.1.0 >/dev/null && { echo "FAIL dry-run 建了 tag"; fails=$((fails + 1)); }
[ -z "$(git ls-remote --tags origin refs/tags/v0.1.0)" ] || { echo "FAIL dry-run 推了 tag"; fails=$((fails + 1)); }

expect fail "要 ≥ v0.1.0" v0.0.9
expect fail "缺版本"
expect fail "不是 semver" 0.1.0
expect fail "不是 semver" v1.2
expect fail "不認得的參數" v0.1.0 --force

# 工作區不乾淨(未追蹤檔)
touch stray.txt
expect fail "工作區不乾淨" v0.1.0
rm stray.txt

# HEAD 不等於 origin/main
git commit -q --allow-empty -m local
expect fail "不等於 origin/main" v0.1.0
git reset -q --hard origin/main

# 版本不大於上一個 v* tag;同名 tag 已存在(本機與遠端)
git tag -a v0.2.0 -m v0.2.0 && git push -q origin v0.2.0
expect fail "沒有大於上一個 tag v0.2.0" v0.1.0
expect fail "已有 tag v0.2.0" v0.2.0
git tag -d v0.2.0 >/dev/null
expect fail "已有 tag v0.2.0" v0.2.0 # 只在遠端:fetch --tags 後照樣擋下
expect 0 "上一個版本:v0.2.0" v0.10.0 # semver 比較不是字串比較

# module 名稱不對
printf 'module example.com/other\n\ngo 1.27\n' > go.mod
git commit -qam other && git push -q origin main
expect fail "不是 github.com/Mikimoto/cuirass" v0.3.0

# fetch 成功但 ls-remote 失敗:要擋下(fail-closed),不能當成「遠端沒有同名 tag」
mkdir "$tmp/bin"
printf '#!/bin/sh\n[ "$1" = ls-remote ] && exit 128\nexec %s "$@"\n' "$(command -v git)" > "$tmp/bin/git"
chmod +x "$tmp/bin/git"
PATH="$tmp/bin:$PATH" expect fail "git ls-remote origin 失敗" v0.4.0

# origin 連不到:fetch / ls-remote 失敗要擋下,不能當成「遠端沒有同名 tag」
git remote set-url origin "$tmp/missing.git"
expect fail "git fetch origin 失敗" v0.4.0

[ "$fails" = 0 ] || { echo "$fails 個失敗"; exit 1; }
echo "release 檢查全部通過"
