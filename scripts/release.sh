#!/usr/bin/env bash
# 發版:scripts/release.sh <vX.Y.Z> [--yes]
# 預設 dry-run:跑完所有檢查,只印出要做的動作。加 --yes 才建 tag、推 tag、建 GitHub release。
# 執行 --yes 前要先問過使用者。
set -euo pipefail

MODULE="github.com/Mikimoto/cuirass"
BRANCH="main"

die() { echo "release 失敗:$*" >&2; exit 1; }
step() { echo "==> $*"; }

version="" yes=0
for arg in "$@"; do
  case "$arg" in
    --yes) yes=1 ;;
    -*) die "不認得的參數 $arg(用法:mise run release <vX.Y.Z> [--yes])" ;;
    *) [ -z "$version" ] || die "只能給一個版本"; version="$arg" ;;
  esac
done
[ -n "$version" ] || die "缺版本(用法:mise run release <vX.Y.Z> [--yes])"

step "檢查版本格式"
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
  || die "版本 $version 不是 semver(格式 vX.Y.Z)"

step "檢查工作區乾淨"
dirty="$(git status --porcelain)"
[ -z "$dirty" ] || die "工作區不乾淨(含未追蹤檔):
$dirty"

step "git fetch origin"
git fetch --quiet --tags origin || die "git fetch origin 失敗(連不到遠端?)"

step "檢查 HEAD 等於 origin/$BRANCH"
head="$(git rev-parse HEAD)"
remote="$(git rev-parse "origin/$BRANCH")" || die "找不到 origin/$BRANCH"
[ "$head" = "$remote" ] || die "HEAD($head)不等於 origin/$BRANCH($remote)"

step "檢查同名 tag 不存在"
! git rev-parse -q --verify "refs/tags/$version" >/dev/null || die "本機已有 tag $version"
remote_tag="$(git ls-remote --tags origin "refs/tags/$version")" || die "git ls-remote origin 失敗,無法確認遠端沒有 $version"
[ -z "$remote_tag" ] || die "遠端已有 tag $version"

step "檢查版本大於上一個 v* tag"
# 只看 vX.Y.Z 形式的 tag;archive-final 之類的不算。
prev="$(git tag -l 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
if [ -n "$prev" ]; then
  top="$(printf '%s\n%s\n' "$prev" "$version" | sort -V | tail -1)"
  [ "$top" = "$version" ] || die "版本 $version 沒有大於上一個 tag $prev"
  echo "上一個版本:$prev"
else
  [ "$(printf '%s\n%s\n' v0.1.0 "$version" | sort -V | head -1)" = v0.1.0 ] || die "沒有前一個 tag 時版本要 ≥ v0.1.0,收到 $version"
  echo "上一個版本:(沒有)"
fi

step "檢查 go.mod"
[ -f go.mod ] || die "根目錄沒有 go.mod"
mod="$(go list -m)"
[ "$mod" = "$MODULE" ] || die "module 是 $mod,不是 $MODULE"

step "檢查 go mod tidy -diff"
go mod tidy -diff || die "go mod tidy 有差異,先跑 go mod tidy 再 commit"

step "mise run test"
mise run test
step "mise run vuln"
mise run vuln

actions=(
  "git tag -a $version -m 'cuirass $version'"
  "git push origin refs/tags/$version"
  "gh release create $version --verify-tag --generate-notes --title $version"
  "在空白暫存 module 用 GOPRIVATE=$MODULE 跑 go list -m $MODULE@$version"
)
if [ "$yes" = 0 ]; then
  echo "檢查全過。dry-run,沒有加 --yes,以下動作不執行:"
  printf '  %s\n' "${actions[@]}"
  exit 0
fi

step "${actions[0]}"
git tag -a "$version" -m "cuirass $version"
step "${actions[1]}"
git push origin "refs/tags/$version"
step "${actions[2]}"
gh release create "$version" --verify-tag --generate-notes --title "$version" \
  || die "gh release create 失敗;tag $version 已推到 origin,修好後手動跑:gh release create $version --verify-tag --generate-notes --title $version"
step "${actions[3]}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
(cd "$tmp" && go mod init example.com/releasecheck >/dev/null 2>&1 \
  && GOPRIVATE="$MODULE" GOFLAGS=-mod=mod go list -m "$MODULE@$version") \
  || die "go list 抓不到 $MODULE@$version"
echo "已發佈 $version"
