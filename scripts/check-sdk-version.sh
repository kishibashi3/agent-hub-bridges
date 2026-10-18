#!/usr/bin/env bash
# agent-hub-sdk を require している Go module の SDK の版が全て同じかを検査する
# (issue #357)。1 module だけ上げ忘れる取り残し (#321 型) を CI で止める。
#
# 対象 module は repo 内の go.mod を走査して SDK を require しているものを拾う
# (= module 追加時に一覧の書き忘れが起きない)。go.mod は `go mod edit -json`
# で読むので network も module cache も要らない。
#
# usage: scripts/check-sdk-version.sh   (repo root で実行)
set -euo pipefail

SDK=github.com/kishibashi3/agent-hub-sdk/go

declare -A versions=()
while IFS= read -r gomod; do
  dir=$(dirname "$gomod")
  v=$(cd "$dir" && go mod edit -json | jq -r --arg p "$SDK" '.Require[]? | select(.Path == $p) | .Version')
  [ -n "$v" ] && versions[$dir]=$v
done < <(git ls-files '*go.mod' | grep -E '(^|/)go\.mod$')

if [ ${#versions[@]} -eq 0 ]; then
  echo "::error::$SDK を require している go.mod が見つからない"
  exit 1
fi

for dir in $(printf '%s\n' "${!versions[@]}" | sort); do
  echo "$dir: ${versions[$dir]}"
done

uniq_count=$(printf '%s\n' "${versions[@]}" | sort -u | wc -l)
if [ "$uniq_count" -ne 1 ]; then
  echo "::error::$SDK の版が module 間で揃っていない (${uniq_count} 種類)。全 module で同じ版に go get + go mod tidy すること"
  exit 1
fi
echo "OK: ${#versions[@]} module とも同じ版"
