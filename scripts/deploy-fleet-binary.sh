#!/usr/bin/env bash
# deploy-fleet-binary.sh — fleet 用の bridge-claude2 binary を main から build して配置する (issue #285)
#
# 使い方:
#   scripts/deploy-fleet-binary.sh
#
# 処理:
#   1. build 用の clone (作業ディレクトリとは別) を origin/main に合わせる。
#      git worktree は使わない (vcs 情報が binary に付かないため)。
#   2. agent-hub-sdk を ci.yml の AGENT_HUB_SDK_REF で sibling に clone する
#      (bridge-claude2/go.mod の replace ../../agent-hub-sdk/go 用。CI と同じ ref)。
#   3. 配置先と同じディレクトリの一時ファイルに build する。
#   4. vcs.revision が main の HEAD と一致し、vcs.modified=false であることを確認する。
#   5. rename で置き換え、配置先・sha256・vcs.revision を出力する。
#
# env (fleet.env / ~/.bashrc) の変更と bridge の restart は行わない (operator が行う)。
#
# 環境変数:
#   FLEET_BIN_DIR      配置先ディレクトリ (既定: ~/.agent-hub/bin)。PATH には入れない。
#   FLEET_BUILD_ROOT   build 用 clone の置き場所 (既定: ~/.agent-hub/build)
#   BRIDGES_REPO_URL   agent-hub-bridges の clone 元 (既定: GitHub の https URL)
#   SDK_REPO_URL       agent-hub-sdk の clone 元 (既定: GitHub の https URL)

set -euo pipefail

BINARY=bridge-claude2
FLEET_BIN_DIR="${FLEET_BIN_DIR:-$HOME/.agent-hub/bin}"
FLEET_BUILD_ROOT="${FLEET_BUILD_ROOT:-$HOME/.agent-hub/build}"
BRIDGES_REPO_URL="${BRIDGES_REPO_URL:-https://github.com/kishibashi3/agent-hub-bridges.git}"
SDK_REPO_URL="${SDK_REPO_URL:-https://github.com/kishibashi3/agent-hub-sdk.git}"

bridges_dir="$FLEET_BUILD_ROOT/agent-hub-bridges"
sdk_dir="$FLEET_BUILD_ROOT/agent-hub-sdk"
dest="$FLEET_BIN_DIR/$BINARY"

log() { echo "[deploy-fleet-binary] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

mkdir -p "$FLEET_BIN_DIR" "$FLEET_BUILD_ROOT"

# 同時実行すると build 用 clone を取り合うので排他する。
exec 9>"$FLEET_BUILD_ROOT/.lock"
flock -n 9 || die "別の deploy が実行中です ($FLEET_BUILD_ROOT/.lock)"

# ---------------------------------------------------------------------------
# 1. agent-hub-bridges を origin/main に合わせる
# ---------------------------------------------------------------------------
if [[ ! -d "$bridges_dir/.git" ]]; then
    log "clone $BRIDGES_REPO_URL -> $bridges_dir"
    git clone -q "$BRIDGES_REPO_URL" "$bridges_dir"
fi
git -C "$bridges_dir" fetch -q origin main
git -C "$bridges_dir" checkout -q --force --detach origin/main
git -C "$bridges_dir" clean -q -fdx
revision=$(git -C "$bridges_dir" rev-parse HEAD)
log "agent-hub-bridges main = $revision"

# ---------------------------------------------------------------------------
# 2. agent-hub-sdk を CI と同じ ref で置く
# ---------------------------------------------------------------------------
sdk_ref=$(sed -n 's/^[[:space:]]*AGENT_HUB_SDK_REF:[[:space:]]*\([0-9a-f]\{40\}\)[[:space:]]*$/\1/p' \
    "$bridges_dir/.github/workflows/ci.yml")
[[ -n "$sdk_ref" ]] || die "ci.yml から AGENT_HUB_SDK_REF を取得できません"
if [[ ! -d "$sdk_dir/.git" ]]; then
    git init -q "$sdk_dir"
fi
git -C "$sdk_dir" fetch -q --depth 1 "$SDK_REPO_URL" "$sdk_ref"
git -C "$sdk_dir" checkout -q --force --detach FETCH_HEAD
git -C "$sdk_dir" clean -q -fdx
[[ "$(git -C "$sdk_dir" rev-parse HEAD)" == "$sdk_ref" ]] || die "agent-hub-sdk を $sdk_ref に合わせられません"
log "agent-hub-sdk = $sdk_ref"

# ---------------------------------------------------------------------------
# 3. 配置先と同じディレクトリの一時ファイルに build する
# ---------------------------------------------------------------------------
tmp=$(mktemp "$FLEET_BIN_DIR/.$BINARY.XXXXXX")
trap 'rm -f "$tmp"' EXIT
log "build -> $tmp"
(cd "$bridges_dir/$BINARY" && go build -o "$tmp" ./cmd/bridge)

# ---------------------------------------------------------------------------
# 4. vcs 情報を確認する
# ---------------------------------------------------------------------------
buildinfo=$(go version -m "$tmp")
vcs_revision=$(awk '$1 == "build" && $2 ~ /^vcs\.revision=/ { sub(/^vcs\.revision=/, "", $2); print $2 }' <<<"$buildinfo")
vcs_modified=$(awk '$1 == "build" && $2 ~ /^vcs\.modified=/ { sub(/^vcs\.modified=/, "", $2); print $2 }' <<<"$buildinfo")
[[ "$vcs_revision" == "$revision" ]] || die "vcs.revision ($vcs_revision) が main ($revision) と一致しません"
[[ "$vcs_modified" == "false" ]] || die "vcs.modified=$vcs_modified です (build 用 clone に変更があります)"

# ---------------------------------------------------------------------------
# 5. rename で置き換える
# ---------------------------------------------------------------------------
sha256=$(sha256sum "$tmp" | awk '{print $1}')
prev_sha256="(なし)"
if [[ -e "$dest" ]]; then
    prev_sha256=$(sha256sum "$dest" | awk '{print $1}')
fi
chmod 755 "$tmp"
mv -f -T "$tmp" "$dest"
trap - EXIT

log "配置しました。env の変更と restart は operator が行います。"
cat <<EOF
path:           $dest
sha256:         $sha256
vcs.revision:   $vcs_revision
vcs.modified:   $vcs_modified
sdk ref:        $sdk_ref
prev sha256:    $prev_sha256
EOF
