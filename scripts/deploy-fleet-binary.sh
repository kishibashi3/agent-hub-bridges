#!/usr/bin/env bash
# deploy-fleet-binary.sh — fleet 用の bridge binary を main から build して配置する (issue #285 / #349)
#
# 使い方:
#   scripts/deploy-fleet-binary.sh [--activate] [--revision <commit>] [bridge ...]
#
#   bridge を省略すると bridge-claude2 / bridge-codex2 / bridge-tmux の 3 つを対象にする。
#   --activate を付けたときだけ symlink を張り替える。付けなければ実体を置くだけで、
#   fleet が起動する binary は変わらない。
#   --revision には試した版の commit (7〜40 桁の 16 進) を渡す。origin/main の HEAD が
#   その commit でなければ、build も張り替えもせずに失敗する (#351)。--activate のときは
#   --revision が必須 (試していない版に張り替えないため)。
#
# 配置の形:
#   $FLEET_BIN_DIR/<bridge>@<revision 先頭 12 桁>   バージョン付きの実体 (上書きしない)
#   $FLEET_BIN_DIR/<bridge> -> <bridge>@<...>      symlink (--activate のときだけ張り替える)
#
#   実体の名前も "bridge-" で始まるので、/proc/<pid>/exe が実体を指しても
#   agenthubctl の bridge 判定 (exe の basename が "bridge-" で始まること) を通る。
#   前の版に戻すときは、残っている実体を指すように symlink を張り直す:
#     ln -sfn '<bridge>@<revision>' "$FLEET_BIN_DIR/<bridge>"
#
# 処理:
#   1. build 用の clone (作業ディレクトリとは別) を origin/main に合わせる。
#      git worktree は使わない (vcs 情報が binary に付かないため)。
#      --revision があれば、origin/main の HEAD がその commit であることを確かめる。
#   2. agent-hub-sdk を ci.yml の AGENT_HUB_SDK_REF で sibling に clone する
#      (bridge-claude2/go.mod の replace ../../agent-hub-sdk/go 用。CI と同じ ref)。
#      bridge-codex2 / bridge-tmux は replace を持たないので go.mod の require 版を使う。
#   3〜5 は対象の bridge すべてに行い、すべて通ってから 6 に進む (#351)。途中の bridge で
#   失敗すると、symlink は 1 つも張り替えない (置いた実体は残るが、起動する binary は変わらない)。
#   3. bridge ごとに、配置先と同じディレクトリの一時ファイルに build する。
#   4. vcs.revision が main の HEAD と一致し、vcs.modified=false であることを確認する。
#   5. rename で <bridge>@<revision> に置く。同じ名前の実体がすでにあり sha256 が同じなら
#      そのまま使う。sha256 が違えば上書きせずに失敗する (版の名前と中身を 1 対 1 に保つため)。
#   6. --activate のときだけ、symlink を rename で <bridge>@<revision> に張り替える。
#      一時 symlink を先に全部作ってから、rename だけを続けて行う。rename 自体が途中で
#      失敗したとき (ディスクや権限の異常) は、それより前の bridge だけ張り替わる。
#      3 つまとめての原子性は無い。
#   7. bridge ごとに、実体・sha256・vcs.revision・symlink の向き先・fleet.env の *_BIN を出力する。
#      fleet.env は読むだけで、symlink を指していなくても失敗にはしない
#      (env をやめて symlink を参照する方針 (#348)。向け直しは operator が行う)。
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

ALL_BRIDGES=(bridge-claude2 bridge-codex2 bridge-tmux)
FLEET_BIN_DIR="${FLEET_BIN_DIR:-$HOME/.agent-hub/bin}"
FLEET_BUILD_ROOT="${FLEET_BUILD_ROOT:-$HOME/.agent-hub/build}"
BRIDGES_REPO_URL="${BRIDGES_REPO_URL:-https://github.com/kishibashi3/agent-hub-bridges.git}"
SDK_REPO_URL="${SDK_REPO_URL:-https://github.com/kishibashi3/agent-hub-sdk.git}"
FLEET_ENV_FILE="${FLEET_ENV_FILE:-$HOME/.agent-hub/fleet.env}"

bridges_dir="$FLEET_BUILD_ROOT/agent-hub-bridges"
sdk_dir="$FLEET_BUILD_ROOT/agent-hub-sdk"

log() { echo "[deploy-fleet-binary] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

usage="$0 [--activate] [--revision <commit>] [bridge-claude2|bridge-codex2|bridge-tmux ...]"
activate=false
want_revision=""
bridges=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --activate) activate=true ;;
        --revision)
            [[ $# -ge 2 ]] || die "--revision に commit がありません (使い方: $usage)"
            want_revision=$2
            shift
            ;;
        --revision=*) want_revision=${1#--revision=} ;;
        bridge-claude2 | bridge-codex2 | bridge-tmux) bridges+=("$1") ;;
        *) die "知らない引数です: $1 (使い方: $usage)" ;;
    esac
    shift
done
if [[ -n "$want_revision" && ! "$want_revision" =~ ^[0-9a-f]{7,40}$ ]]; then
    die "--revision は 7〜40 桁の 16 進 (小文字) の commit で指定してください: $want_revision"
fi
if [[ "$activate" == true && -z "$want_revision" ]]; then
    die "--activate には --revision <試した版の commit> が必要です (試していない版に張り替えないため)"
fi
if [[ ${#bridges[@]} -eq 0 ]]; then
    bridges=("${ALL_BRIDGES[@]}")
fi

mkdir -p "$FLEET_BIN_DIR" "$FLEET_BUILD_ROOT"

# 同時実行すると build 用 clone を取り合うので排他する。
exec 9>"$FLEET_BUILD_ROOT/.lock"
flock -n 9 || die "別の deploy が実行中です ($FLEET_BUILD_ROOT/.lock)"

tmp_files=()
cleanup() { if [[ ${#tmp_files[@]} -gt 0 ]]; then rm -f "${tmp_files[@]}"; fi; }
trap cleanup EXIT

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
version="${revision:0:12}"
log "agent-hub-bridges main = $revision"
if [[ -n "$want_revision" && "$revision" != "$want_revision"* ]]; then
    die "origin/main ($revision) が指定の revision ($want_revision) と違います。build も symlink の張り替えもしません"
fi

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

# fleet.env の <key> の値 (最後の定義、値を囲む引用符を外す。systemd の EnvironmentFile と同じ扱い)。
fleet_env_value() {
    local key=$1 value
    [[ -f "$FLEET_ENV_FILE" ]] || return 0
    value=$(sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$key=//p" "$FLEET_ENV_FILE" | tail -n 1)
    if [[ "$value" =~ ^\"(.*)\"$ || "$value" =~ ^\'(.*)\'$ ]]; then
        value="${BASH_REMATCH[1]}"
    fi
    printf '%s' "$value"
}

# 3〜5 の結果。6・7 で使う (添字は bridges と同じ)。
sha256s=()
vcs_revisions=()
vcs_modifieds=()

for bridge in "${bridges[@]}"; do
    entity="$bridge@$version"
    dest="$FLEET_BIN_DIR/$entity"

    # -----------------------------------------------------------------------
    # 3. 配置先と同じディレクトリの一時ファイルに build する
    # -----------------------------------------------------------------------
    tmp=$(mktemp "$FLEET_BIN_DIR/.$bridge.XXXXXX")
    tmp_files+=("$tmp")
    log "$bridge: build -> $tmp"
    (cd "$bridges_dir/$bridge" && go build -o "$tmp" ./cmd/bridge)

    # -----------------------------------------------------------------------
    # 4. vcs 情報を確認する
    # -----------------------------------------------------------------------
    buildinfo=$(go version -m "$tmp")
    vcs_revision=$(awk '$1 == "build" && $2 ~ /^vcs\.revision=/ { sub(/^vcs\.revision=/, "", $2); print $2 }' <<<"$buildinfo")
    vcs_modified=$(awk '$1 == "build" && $2 ~ /^vcs\.modified=/ { sub(/^vcs\.modified=/, "", $2); print $2 }' <<<"$buildinfo")
    [[ "$vcs_revision" == "$revision" ]] || die "$bridge: vcs.revision ($vcs_revision) が main ($revision) と一致しません"
    [[ "$vcs_modified" == "false" ]] || die "$bridge: vcs.modified=$vcs_modified です (build 用 clone に変更があります)"

    # -----------------------------------------------------------------------
    # 5. バージョン付きの実体を置く (既存の実体は上書きしない)
    # -----------------------------------------------------------------------
    sha256=$(sha256sum "$tmp" | awk '{print $1}')
    if [[ -e "$dest" || -L "$dest" ]]; then
        existing_sha256=$(sha256sum "$dest" | awk '{print $1}')
        [[ "$existing_sha256" == "$sha256" ]] ||
            die "$bridge: $dest はすでにあり、sha256 が今回の build と違います ($existing_sha256 != $sha256)。上書きしません"
        rm -f "$tmp"
        placed="既存の実体と同じ (置き直していません)"
    else
        chmod 755 "$tmp"
        mv -f -T "$tmp" "$dest"
        placed="新しく置きました"
    fi
    log "$bridge: $dest: $placed"

    sha256s+=("$sha256")
    vcs_revisions+=("$vcs_revision")
    vcs_modifieds+=("$vcs_modified")
done

# ---------------------------------------------------------------------------
# 6. --activate のときだけ symlink を張り替える (3〜5 がすべて通った後)
# ---------------------------------------------------------------------------
prev_targets=()
for bridge in "${bridges[@]}"; do
    link="$FLEET_BIN_DIR/$bridge"
    prev_target="(なし)"
    if [[ -L "$link" ]]; then
        prev_target=$(readlink "$link")
    elif [[ -e "$link" ]]; then
        prev_target="(symlink ではないファイル, sha256 $(sha256sum "$link" | awk '{print $1}'))"
    fi
    prev_targets+=("$prev_target")
done
if [[ "$activate" == true ]]; then
    # 一時 symlink を先に全部作る。ここで失敗しても本物の symlink は変わらない。
    for bridge in "${bridges[@]}"; do
        tmp_link="$FLEET_BIN_DIR/.$bridge.link.$$"
        tmp_files+=("$tmp_link")
        ln -s -f -T "$bridge@$version" "$tmp_link"
    done
    for bridge in "${bridges[@]}"; do
        mv -f -T "$FLEET_BIN_DIR/.$bridge.link.$$" "$FLEET_BIN_DIR/$bridge"
        log "$bridge: symlink $FLEET_BIN_DIR/$bridge -> $bridge@$version に張り替えました"
    done
else
    log "--activate がないので symlink は変えていません"
fi

for i in "${!bridges[@]}"; do
    bridge=${bridges[$i]}
    dest="$FLEET_BIN_DIR/$bridge@$version"
    link="$FLEET_BIN_DIR/$bridge"
    link_target="(なし)"
    if [[ -L "$link" ]]; then
        link_target=$(readlink "$link")
    elif [[ -e "$link" ]]; then
        link_target="(symlink ではないファイル)"
    fi

    # -----------------------------------------------------------------------
    # 7. 結果を出力する (fleet.env は読むだけ)
    # -----------------------------------------------------------------------
    env_key="AGENT_HUB_$(tr '[:lower:]-' '[:upper:]_' <<<"$bridge")_BIN"
    env_value=$(fleet_env_value "$env_key")
    if [[ -z "$env_value" ]]; then
        env_status="未設定 (PATH から解決される)"
    elif [[ "$(realpath -m -s -- "$env_value")" == "$(realpath -m -s -- "$link")" ]]; then
        env_status="symlink を指しています"
    else
        env_status="symlink ではない $env_value を指しています (向け直しは operator)"
    fi

    cat <<EOF
[$bridge]
path:           $dest
sha256:         ${sha256s[$i]}
vcs.revision:   ${vcs_revisions[$i]}
vcs.modified:   ${vcs_modifieds[$i]}
sdk ref:        $([[ "$bridge" == bridge-claude2 ]] && echo "$sdk_ref (replace)" || echo "go.mod の require 版")
symlink:        $link -> $link_target (前: ${prev_targets[$i]})
$env_key: $env_status

EOF
done

if [[ "$activate" == true ]]; then
    log "symlink を張り替えました。restart は operator が行います。"
else
    log "実体を置きました (symlink は変えていません)。切り替えは --activate で行います。"
fi
