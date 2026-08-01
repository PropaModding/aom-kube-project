#!/bin/bash
# Clicks through the AoM: Titans menu to host a LAN game session, headlessly.
#
# CALIBRATION WORKFLOW (do this once, from a fresh container each attempt):
#   1. docker run ... aom-head   # launch, let the game reach the main menu
#   2. In another shell:  docker exec -it <container> ./host-game.sh
#      It will run every step up to the first uncalibrated "click", take a
#      screenshot, then stop and tell you which file to look at.
#   3. Open that screenshot (mounted volume, or `docker cp`, or just watch
#      live via VNC if ENABLE_VNC=1) and read off the pixel coordinates of
#      the button described in the step.
#   4. Edit the matching CALIBRATE/CALIBRATE pair below to the real x y.
#   5. Re-run. Once a step is calibrated it executes for real and the script
#      moves on to the next uncalibrated step, so you calibrate one click at
#      a time without re-typing earlier ones.
#   6. Once every step is calibrated, the script runs end-to-end and leaves
#      you sitting in the hosted lobby.
#
# Screen is fixed at 800x600 (baked into the image), so coordinates found in
# one screenshot stay valid across runs.
set -e

DISPLAY="${DISPLAY:-:99}"
GAME_NAME="${GAME_NAME:-AOM-KUBE-HOST}"
GAME_WINDOW_PATTERN="${GAME_WINDOW_PATTERN:-Age of Mythology}"
SCREENSHOT_DIR="${SCREENSHOT_DIR:-/home/aomuser/aom/screenshots}"
WINDOW_WAIT_TIMEOUT="${WINDOW_WAIT_TIMEOUT:-60}"

# type|arg1|arg2|description
# type=screenshot: arg1=label
# type=click:      arg1=x  arg2=y
# type=key:        arg1=xdotool key sequence (e.g. Return, ctrl+a)
# type=text:       arg1=literal text to type (supports $GAME_NAME expansion)
# type=sleep:      arg1=seconds
STEPS=(
    "screenshot|00_initial||Main menu should be visible here"
    "click|CALIBRATE|CALIBRATE|Main menu: click 'Multiplayer'"
    "sleep|2||settle after click"
    "screenshot|01_multiplayer_menu||Connection-type submenu (ESO / Direct IP / LAN) should be visible"
    "click|CALIBRATE|CALIBRATE|Click 'Local Area Network (LAN)'"
    "sleep|2||settle after click"
    "screenshot|02_after_lan||Player name entry or game-room list should be visible"
    "click|CALIBRATE|CALIBRATE|Click 'Create Game' / 'Host'"
    "sleep|2||settle after click"
    "screenshot|03_game_setup||Game setup screen: name / map / player slots should be visible"
    "click|CALIBRATE|CALIBRATE|Click the game-name text field to focus it"
    "key|ctrl+a||select any default text so typing replaces it"
    "text|${GAME_NAME}||type the session name"
    "click|CALIBRATE|CALIBRATE|Click 'OK' / 'Start Game' to confirm settings and open the lobby"
    "sleep|3||settle into lobby"
    "screenshot|99_final_lobby||Hosted lobby - should now be broadcasting on LAN"
)

FROM=1
TO=${#STEPS[@]}
LIST_ONLY=0

while [ $# -gt 0 ]; do
    case "$1" in
        --from) FROM="$2"; shift 2 ;;
        --to) TO="$2"; shift 2 ;;
        --list) LIST_ONLY=1; shift ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

mkdir -p "$SCREENSHOT_DIR"

if [ "$LIST_ONLY" = "1" ]; then
    for i in "${!STEPS[@]}"; do
        IFS='|' read -r type arg1 arg2 desc <<< "${STEPS[$i]}"
        printf '%3d  %-10s %-10s %-10s %s\n' "$((i + 1))" "$type" "$arg1" "$arg2" "$desc"
    done
    exit 0
fi

find_window() {
    local waited=0
    while [ "$waited" -lt "$WINDOW_WAIT_TIMEOUT" ]; do
        local win
        win=$(xdotool search --onlyvisible --name "$GAME_WINDOW_PATTERN" 2>/dev/null | head -n1)
        if [ -n "$win" ]; then
            echo "$win"
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

echo "Waiting for game window matching '$GAME_WINDOW_PATTERN' (timeout ${WINDOW_WAIT_TIMEOUT}s)..."
WIN=$(find_window) || { echo "Game window never appeared." >&2; exit 1; }
xdotool windowactivate --sync "$WIN"
echo "Found window $WIN"

take_screenshot() {
    local label="$1"
    local path="$SCREENSHOT_DIR/${label}.png"
    scrot -o "$path"
    echo "  -> screenshot saved: $path"
}

for i in "${!STEPS[@]}"; do
    step_num=$((i + 1))
    [ "$step_num" -lt "$FROM" ] && continue
    [ "$step_num" -gt "$TO" ] && break

    IFS='|' read -r type arg1 arg2 desc <<< "${STEPS[$i]}"
    echo "[step $step_num/${#STEPS[@]}] $type - $desc"

    case "$type" in
        screenshot)
            take_screenshot "$arg1"
            ;;
        sleep)
            sleep "$arg1"
            ;;
        key)
            xdotool key --window "$WIN" "$arg1"
            ;;
        text)
            xdotool type --window "$WIN" --delay 50 -- "$arg1"
            ;;
        click)
            if [ "$arg1" = "CALIBRATE" ] || [ "$arg2" = "CALIBRATE" ]; then
                echo ""
                echo "STOP: step $step_num is not calibrated yet."
                echo "  $desc"
                echo "Look at the most recent screenshot in $SCREENSHOT_DIR,"
                echo "find the pixel coordinates of that button, then edit the"
                echo "'click|CALIBRATE|CALIBRATE|...' line for step $step_num in $0."
                echo "Re-run with --from $step_num once fixed (game must still be"
                echo "sitting at the screen from the last screenshot taken)."
                exit 2
            fi
            xdotool mousemove --window "$WIN" "$arg1" "$arg2"
            xdotool click --window "$WIN" 1
            ;;
        *)
            echo "Unknown step type: $type" >&2
            exit 1
            ;;
    esac
done

echo ""
echo "Done. Final state captured in $SCREENSHOT_DIR/99_final_lobby.png"
echo "If ENABLE_VNC=1 was set, connect a VNC client to port 5900 to watch live."
