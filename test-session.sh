#!/bin/bash
# One-shot test harness: launches the headed AoM container, waits for its
# window to appear, then records clicks + DirectPlay8 traffic until you
# Ctrl+C. Tears the container down afterward so the next run starts clean.
# Re-run this once per test iteration.
#
# Usage: ./test-session.sh [session-name]
#
# See run-aom-head.sh and record-session.sh for what each half does. Safe to
# run alongside run-aom-client.sh (see that script) since cleanup below only
# targets the container this script itself started, not any other aom-head
# containers already running (e.g. a client instance used to trigger LAN
# discovery traffic).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_NAME="aom-head"
GAME_WINDOW_PATTERN="${GAME_WINDOW_PATTERN:-Age of Mythology}"
WINDOW_WAIT_TIMEOUT="${WINDOW_WAIT_TIMEOUT:-60}"
SESSION_NAME="${1:-$(date +%Y%m%d-%H%M%S)}"

# Snapshot containers of this image that already exist so cleanup can later
# tell "ours" apart from any others (e.g. run-aom-client.sh) already running.
PRE_EXISTING_CIDS="$(docker ps -q --filter ancestor="$IMAGE_NAME" | sort)"

echo "[*] Launching AoM container..."
"$SCRIPT_DIR/run-aom-head.sh" &
RUN_PID=$!

# Host has xwininfo/xprop (x11-utils) but not xdotool, so we grep the window
# tree instead of using xdotool search.
find_window() {
    local waited=0
    while [ "$waited" -lt "$WINDOW_WAIT_TIMEOUT" ]; do
        if xwininfo -root -tree 2>/dev/null | grep -qi "$GAME_WINDOW_PATTERN"; then
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

CLEANED=0
cleanup() {
    [ "$CLEANED" = "1" ] && return
    CLEANED=1
    echo ""
    echo "[*] Stopping AoM container..."
    CURRENT_CIDS="$(docker ps -q --filter ancestor="$IMAGE_NAME" | sort)"
    CID="$(comm -13 <(echo "$PRE_EXISTING_CIDS") <(echo "$CURRENT_CIDS") | head -n1)"
    if [ -n "${CID:-}" ]; then
        docker stop "$CID" >/dev/null 2>&1 || true
    fi
    wait "$RUN_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "[*] Waiting for game window matching '$GAME_WINDOW_PATTERN' (timeout ${WINDOW_WAIT_TIMEOUT}s)..."
if ! find_window >/dev/null; then
    echo "Game window never appeared." >&2
    exit 1
fi
echo "[*] Game window found, main menu should be up."

echo "[*] Starting recorder for session '$SESSION_NAME'..."
echo "    Play through to hosting a game, then Ctrl+C to stop and tear down."
"$SCRIPT_DIR/record-session.sh" "$SESSION_NAME"
