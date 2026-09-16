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
#   6. fleet.env の AGENT_HUB_BRIDGE_CLAUDE2_BIN が配置先を指しているかを確認する。
#      指していなければ WARNING を出し、結果を出力したあと exit 1 で終了する
#      (配置は済んでいるが、fleet が起動しない binary なので deploy としては失敗)。
#
# env (fleet.env / ~/.bashrc) の変更と bridge の restart は行わない (operator が行う)。
#
# 環境変数:
#   FLEET_BIN_DIR      配置先ディレクトリ (既定: ~/.agent-hub/bin)。PATH には入れない。
#   FLEET_BUILD_ROOT   build 用 clone の置き場所 (既定: ~/.agent-hub/build)
#   BRIDGES_REPO_URL   agent-hub-bridges の clone 元 (既定: GitHub の https URL)
#   SDK_REPO_URL       agent-hub-sdk の clone 元 (既定: GitHub の https URL)
#   FLEET_ENV_FILE     確認する fleet.env (既定: ~/.agent-hub/fleet.env)。読むだけで書き換えない。

set -euo pipefail

BINARY=bridge-claude2
FLEET_BIN_DIR="${FLEET_BIN_DIR:-$HOME/.agent-hub/bin}"
FLEET_BUILD_ROOT="${FLEET_BUILD_ROOT:-$HOME/.agent-hub/build}"
BRIDGES_REPO_URL="${BRIDGES_REPO_URL:-https://github.com/kishibashi3/agent-hub-bridges.git}"
SDK_REPO_URL="${SDK_REPO_URL:-https://github.com/kishibashi3/agent-hub-sdk.git}"
FLEET_ENV_FILE="${FLEET_ENV_FILE:-$HOME/.agent-hub/fleet.env}"
BIN_ENV_KEY=AGENT_HUB_BRIDGE_CLAUDE2_BIN

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

# ---------------------------------------------------------------------------
# 6. fleet.env の *_BIN が配置先を指しているかを確認する (書き換えはしない)
# ---------------------------------------------------------------------------
# 指していない場合、fleet は別の binary (作業ディレクトリや PATH 上のもの) を起動し続ける。
# 配置 (rename) は済ませたうえで、WARNING と出力の fleet.env 行で知らせ、exit 1 で終了する。
fleet_env_status="OK"
if [[ ! -f "$FLEET_ENV_FILE" ]]; then
    fleet_env_status="NG ($FLEET_ENV_FILE がありません)"
else
    # 最後の定義を採り、値を囲む引用符を外す (systemd の EnvironmentFile と同じ扱い)。
    env_value=$(sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$BIN_ENV_KEY=//p" "$FLEET_ENV_FILE" | tail -n 1)
    if [[ "$env_value" =~ ^\"(.*)\"$ || "$env_value" =~ ^\'(.*)\'$ ]]; then
        env_value="${BASH_REMATCH[1]}"
    fi
    if [[ -z "$env_value" ]]; then
        fleet_env_status="NG ($BIN_ENV_KEY が $FLEET_ENV_FILE にありません)"
    elif [[ "$(realpath -m -- "$env_value")" != "$(realpath -m -- "$dest")" ]]; then
        fleet_env_status="NG ($BIN_ENV_KEY=$env_value は配置先ではありません)"
    fi
fi
if [[ "$fleet_env_status" != "OK" ]]; then
    log "WARNING: fleet.env: $fleet_env_status"
    log "WARNING: fleet は今回配置した binary を起動しません。$BIN_ENV_KEY=$dest への変更を operator に依頼してください。"
fi

cat <<EOF
path:           $dest
sha256:         $sha256
vcs.revision:   $vcs_revision
vcs.modified:   $vcs_modified
sdk ref:        $sdk_ref
prev sha256:    $prev_sha256
fleet.env:      $fleet_env_status
EOF

if [[ "$fleet_env_status" != "OK" ]]; then
    log "ERROR: fleet.env の確認が NG のため失敗として終了します (binary は配置済み)"
    exit 1
fi
