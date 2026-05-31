#!/usr/bin/env bash
# stop-bridge.sh — bridge worker を停止して inventory を更新する
#
# 使い方:
#   scripts/stop-bridge.sh --user <handle>     # 単体停止
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
mode="user"  # default: --user モード
user_handle=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --user)
            user_handle="$2"
            mode="user"
            shift 2
            ;;
        --dead)
            mode="dead"
            shift
            ;;
        -h|--help)
            echo "usage: $0 --user <handle>"     >&2
            echo "       $0 --dead"              >&2
            echo ""                              >&2
            echo "  --user <handle>  指定 bridge を停止して inventory を更新する" >&2
            echo "  --dead           dead marker (/tmp/agent-hub-bridge-*.dead) の" >&2
            echo "                   bridge を一括 kill して inventory を更新する"  >&2
            exit 0
            ;;
        *)
            echo "error: unknown argument: $1" >&2
            echo "usage: $0 --user <handle> | --dead" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# helper: bridge を 1 件停止して inventory を更新する
# ---------------------------------------------------------------------------
_stop_one() {
    local handle="$1"
    local reason="${2:-manual}"

    # pgrep で PID を取得 (末尾スペースで部分一致を防ぐ)
    local PID
    PID=$(pgrep -f "agent-hub-bridge-claude --user ${handle} " | head -1 || true)

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

        # Currently running テーブルから該当行を削除
        sed -i "/\`@${handle}\`/d" "$BRIDGE_INVENTORY"

        # Activity log に stop エントリを追加
        sed -i "/^新しいエントリを上に追加/a - ${NOW} — **stop** \`@${handle}\` — ${reason} (pid=${PID:-unknown})" \
            "$BRIDGE_INVENTORY"

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
    # /tmp/agent-hub-bridge-*.dead を glob
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
        _stop_one "$local_handle" "dead bridge cleanup"
        (( count++ )) || true
    done

    echo "done: cleaned up ${count} dead bridge(s)" >&2
    exit 0
fi

# ---------------------------------------------------------------------------
# --user モード: 単体停止
# ---------------------------------------------------------------------------
if [[ -z "$user_handle" ]]; then
    echo "error: --user <handle> is required" >&2
    echo "usage: $0 --user <handle>" >&2
    echo "       $0 --dead" >&2
    exit 2
fi

_stop_one "$user_handle" "manual stop"
