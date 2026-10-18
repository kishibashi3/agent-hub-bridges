#!/usr/bin/env bash
# agent-hub-sdk を require している Go module の SDK の版が全て同じかを検査する
# (issue #357)。1 module だけ上げ忘れる取り残し (#321 型) を CI で止める。
#
# 対象 module は repo 内の go.mod を走査して SDK を require しているものを拾う
# (= module 追加時に一覧の書き忘れが起きない)。go.mod は `go mod edit -json`
# で読むので network も module cache も要らない。
# SDK を replace している module があれば、require の版が揃っていても
# build は別の SDK に対して行われるので落とす (issue #365)。
#
# usage: scripts/check-sdk-version.sh   (repo 内のどこから実行してもよい)
set -euo pipefail

SDK=github.com/kishibashi3/agent-hub-sdk/go

# git ls-files は cwd 配下しか列挙しないので、repo root から走査する (issue #365)。
cd "$(git rev-parse --show-toplevel)"

declare -A versions=()
replaced=()
while IFS= read -r gomod; do
  dir=$(dirname "$gomod")
  json=$(cd "$dir" && go mod edit -json)
  v=$(jq -r --arg p "$SDK" '.Require[]? | select(.Path == $p) | .Version' <<<"$json")
  if [ -n "$v" ]; then
    versions[$dir]=$v
  fi
  r=$(jq -r --arg p "$SDK" '.Replace[]? | select(.Old.Path == $p) | "\(.Old.Path)\(if .Old.Version then "@" + .Old.Version else "" end) => \(.New.Path)\(if .New.Version then " " + .New.Version else "" end)"' <<<"$json")
  if [ -n "$r" ]; then
    while IFS= read -r line; do
      replaced+=("$dir: replace $line")
    done <<<"$r"
  fi
done < <(git ls-files '*go.mod' | grep -E '(^|/)go\.mod$')

if [ ${#replaced[@]} -gt 0 ]; then
  for line in "${replaced[@]}"; do
    echo "::error::$line"
  done
  echo "::error::$SDK を replace している go.mod がある。require の版と別の SDK で build されるので replace を外すこと"
  exit 1
fi

if [ ${#versions[@]} -eq 0 ]; then
  echo "::error::$SDK を require している go.mod が見つからない"
  exit 1
fi

while IFS= read -r dir; do
  echo "$dir: ${versions[$dir]}"
done < <(printf '%s\n' "${!versions[@]}" | sort)

uniq_count=$(printf '%s\n' "${versions[@]}" | sort -u | wc -l)
if [ "$uniq_count" -ne 1 ]; then
  echo "::error::$SDK の版が module 間で揃っていない (${uniq_count} 種類)。全 module で同じ版に go get + go mod tidy すること"
  exit 1
fi
echo "OK: ${#versions[@]} module とも同じ版"
