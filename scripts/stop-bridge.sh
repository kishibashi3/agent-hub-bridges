#!/usr/bin/env bash
# stop-bridge.sh — bridge worker を停止して inventory を更新する
#
# 使い方:
#   scripts/stop-bridge.sh --participant <handle>     # 単体停止
#   scripts/stop-bridge.sh --dead              # hub 切断中 bridge を一括終了
#
# --dead モード:
#   circuit breaker が書いた /tmp/agent-hub-bridge-*.dead マーカーを検索し、
#   対応するプロセスを kill して inventory を更新する (issue #82)。
#
# 環境変数:
#   BRIDGE_INVENTORY   inventory ファイルの絶対パス (省略時は AGENT_HUB_ROLES から自動検出)
#   AGENT_HUB_ROLES    自動検出用 — パスを '-' で置換して .claude/projects/ の key にする
#
# 例:
#   BRIDGE_INVENTORY=~/.claude/projects/foo/bridge-inventory.md \
#     ./scripts/stop-bridge.sh --dead

set -euo pipefail

# ---------------------------------------------------------------------------
# inventory ファイルのパス解決
# ---------------------------------------------------------------------------
if [[ -z "${BRIDGE_INVENTORY:-}" ]]; then
    if [[ -n "${AGENT_HUB_ROLES:-}" ]]; then
        project_key=$(echo "$AGENT_HUB_ROLES" | sed 's|/|-|g')
        BRIDGE_INVENTORY="$HOME/.claude/projects/${project_key}/bridge-inventory.md"
    fi
fi

# ---------------------------------------------------------------------------
# 引数パース
# ---------------------------------------------------------------------------
mode="user"  # default: --participant モード
user_handle=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --participant)
            user_handle="$2"
            mode="user"
            shift 2
            ;;
        --dead)
            mode="dead"
            shift
            ;;
        -h|--help)
            echo "usage: $0 --participant <handle>"     >&2
            echo "       $0 --dead"              >&2
            echo ""                              >&2
            echo "  --participant <handle>  指定 bridge を停止して inventory を更新する" >&2
            echo "  --dead                 dead marker (/tmp/agent-hub-bridge-*.dead) の" >&2
            echo "                         bridge を一括 kill して inventory を更新する"  >&2
            exit 0
            ;;
        *)
            echo "error: unknown argument: $1" >&2
            echo "usage: $0 --participant <handle> | --dead" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# helper: handle の形式を検証する (Minor #2: sed injection 防止)
# handle は英数字・ハイフン・アンダースコアのみ許可する。
# ---------------------------------------------------------------------------
_validate_handle() {
    local h="$1"
    if [[ -z "$h" ]]; then
        echo "error: handle is empty" >&2
        return 1
    fi
    if [[ ! "$h" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "error: invalid handle format: '${h}' (must match [a-zA-Z0-9_-]+)" >&2
        return 1
    fi
}

# ---------------------------------------------------------------------------
# helper: bridge を 1 件停止して inventory を更新する
# ---------------------------------------------------------------------------
_stop_one() {
    local handle="$1"
    local reason="${2:-manual}"  # Minor #3: :-fallback で空文字も "manual" に正規化

    # handle の形式検証 (sed injection 防止)
    _validate_handle "$handle" || return 1

    # pgrep で PID を取得 (末尾スペースで部分一致を防ぐ)
    local PID=""
    PID=$(pgrep -f "agent-hub-bridge-claude --participant ${handle} " | head -1 || true)

    if [[ -z "$PID" ]]; then
        echo "info: @${handle} — no running process found" >&2
    else
        if kill "$PID" 2>/dev/null; then
            echo "killed @${handle} (pid=${PID})" >&2
        else
            echo "warning: kill failed for @${handle} (pid=${PID})" >&2
        fi
    fi

    # inventory 更新 (BRIDGE_INVENTORY が有効な場合のみ)
    if [[ -n "${BRIDGE_INVENTORY:-}" ]] && [[ -f "$BRIDGE_INVENTORY" ]]; then
        local NOW
        NOW=$(date '+%Y-%m-%d %H:%M')

        # Critical #1 (BSD sed 互換): -i.bak で macOS/BSD sed に対応し、
        # 即座に .bak を削除することで副作用ファイルを残さない。
        # 参考: kishibashi3/agent-hub-roles#11

        # Currently running テーブルから該当行を削除
        sed -i.bak "/\`@${handle}\`/d" "$BRIDGE_INVENTORY" \
            && rm -f "${BRIDGE_INVENTORY}.bak"

        # Activity log に stop エントリを追加
        sed -i.bak "/^新しいエントリを上に追加/a - ${NOW} — **stop** \`@${handle}\` — ${reason} (pid=${PID:-unknown})" \
            "$BRIDGE_INVENTORY" \
            && rm -f "${BRIDGE_INVENTORY}.bak"

        echo "inventory updated: $BRIDGE_INVENTORY" >&2
    fi

    # dead marker が残っていれば削除
    local dead_marker="/tmp/agent-hub-bridge-${handle}.dead"
    if [[ -f "$dead_marker" ]]; then
        rm -f "$dead_marker"
        echo "removed dead marker: $dead_marker" >&2
    fi
}

# ---------------------------------------------------------------------------
# --dead モード: dead marker を持つ bridge を一括終了
# ---------------------------------------------------------------------------
if [[ "$mode" == "dead" ]]; then
    # /tmp/agent-hub-bridge-*.dead を glob (nullglob で 0 件でもエラーにしない)
    shopt -s nullglob
    dead_files=(/tmp/agent-hub-bridge-*.dead)
    shopt -u nullglob

    if [[ ${#dead_files[@]} -eq 0 ]]; then
        echo "info: no dead bridges found (no /tmp/agent-hub-bridge-*.dead files)" >&2
        exit 0
    fi

    count=0
    for dead_file in "${dead_files[@]}"; do
        [[ -f "$dead_file" ]] || continue

        # ファイル名から handle を取得:
        #   /tmp/agent-hub-bridge-bridges-impl.dead → bridges-impl
        local_handle=$(basename "$dead_file" .dead)
        local_handle="${local_handle#agent-hub-bridge-}"

        echo "processing dead bridge: @${local_handle} (marker=${dead_file})" >&2
        _stop_one "$local_handle" "dead bridge cleanup" || {
            echo "warning: failed to stop @${local_handle}, skipping" >&2
            continue
        }
        (( count++ )) || true
    done

    echo "done: cleaned up ${count} dead bridge(s)" >&2
    exit 0
fi

# ---------------------------------------------------------------------------
# --participant モード: 単体停止
# ---------------------------------------------------------------------------
if [[ -z "$user_handle" ]]; then
    echo "error: --participant <handle> is required" >&2
    echo "usage: $0 --participant <handle>" >&2
    echo "       $0 --dead" >&2
    exit 2
fi

_stop_one "$user_handle" "manual stop"
