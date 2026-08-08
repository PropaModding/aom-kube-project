#!/bin/bash
# Clicks an already-running aom-headless pod through to a hosted LAN/Direct-IP
# lobby, using xdotool run in-container via `kubectl exec` (no screenshot
# tool needed in-container - that's what made the original host-game.sh a
# dead end, see docs/host-flow.md). Coordinates below were calibrated by
# hand over VNC against dockerfile.k8s's fixed 800x600 display.
#
# Usage: ./host-game-kube.sh [nickname]
#   nickname defaults to AOMHOST. AoM only accepts alphanumeric characters
#   here - no symbols (confirmed by trial: "AOM-HOST" was rejected).
#   For manual/live-debug runs (watching over Remmina/VNC), "TheIP" is the
#   nickname convention in use - named after the thing a client actually
#   needs to know (the cluster's Direct-IP address), easy to recognize in
#   the lobby's own player list while debugging. e.g. ./host-game-kube.sh TheIP
#
# Leaves the pod sitting in the hosted lobby (game name "<nickname>'s
# Game", default map/settings). Does NOT click "Observer Mode" - that
# needs to happen only after a real client has joined (see
# docs/host-flow.md's "Switching to Observer Mode" section), which this
# script doesn't attempt to detect. Not started here either: this only
# gets the lobby open for players to join, same as the manual VNC flow
# would.
#
# Assumes the pod is fresh (just past the game window appearing) or at
# worst already sitting at the real Main Menu - after the EULA, the intro
# splash(es) resolve straight to the real Main Menu (Learn to Play /
# Campaign / Single Player / Multiplayer / Options / More / Exit) with no
# separate landing screen in between - see docs/host-flow.md.
#
# To watch the clicks live while debugging: in one terminal,
#   kubectl port-forward svc/aom-headless-vnc 5901:5900
# then, from your desktop:
#   remmina -c vnc://localhost:5901
set -uo pipefail

NICKNAME="${1:-AOMHOST}"
GAME_WINDOW_PATTERN="${GAME_WINDOW_PATTERN:-Age of Mythology}"
WINDOW_WAIT_TIMEOUT="${WINDOW_WAIT_TIMEOUT:-60}"
# Long by default: aomxnocd1.exe runs pegged near 100% CPU under
# llvmpipe software rendering, and fast-paced xdotool input against it
# has been observed to garble/drop keystrokes (e.g. a clean-looking
# nickname field still failing the game's own alphanumeric validation).
# Generous delays trade speed for this being a "set and forget", rerun-
# without-babysitting script.
STEP_DELAY="${STEP_DELAY:-5}"
TYPE_DELAY_MS="${TYPE_DELAY_MS:-200}"
APP_SETTLE_DELAY="${APP_SETTLE_DELAY:-10}"

POD="$(kubectl get pod -l app=aom-headless -o jsonpath='{.items[0].metadata.name}')"
if [ -z "$POD" ]; then
    echo "No aom-headless pod found (kubectl get pod -l app=aom-headless)." >&2
    exit 1
fi
echo "[*] Target pod: $POD"

xdo() {
    kubectl exec "$POD" -- xdotool "$@"
}

click() {
    local x="$1" y="$2" desc="$3"
    echo "[*] click ($x, $y) - $desc"
    xdo mousemove "$x" "$y" click 1
    sleep "$STEP_DELAY"
}

echo "[*] Waiting for the game window (timeout ${WINDOW_WAIT_TIMEOUT}s)..."
waited=0
until WIN="$(xdo search --name "$GAME_WINDOW_PATTERN" 2>/dev/null | head -n1)" && [ -n "$WIN" ]; do
    waited=$((waited + 2))
    if [ "$waited" -ge "$WINDOW_WAIT_TIMEOUT" ]; then
        echo "Game window never appeared." >&2
        exit 1
    fi
    sleep 2
done
echo "[*] Found window $WIN"
# Xvfb has no window manager, so this reliably logs a harmless
# "_NET_ACTIVE_WINDOW not supported" error - expected, not fatal.
xdo windowactivate --sync "$WIN" 2>/dev/null || true

# Give the app itself time to finish initializing after the window
# first appears (asset loading etc. can lag behind the window showing
# up) before sending it any input.
echo "[*] Window found, waiting ${APP_SETTLE_DELAY}s for the app to finish starting..."
sleep "$APP_SETTLE_DELAY"

# No window manager means X11 falls back to focus-follows-pointer: the
# window under the mouse gets keyboard input, regardless of which window
# was last "activated". Every click below implicitly fixes focus via its
# own mousemove, but the EULA step is keyboard-only, so it needs an
# explicit mousemove first or its key events go to whatever the pointer
# happened to be sitting over (e.g. nothing, on a freshly started Xvfb) -
# confirmed by watching a run's Left/Return do nothing on the EULA
# dialog.
xdo mousemove 400 300
sleep 1

# --- EULA (only appears on a genuinely fresh pod). Keyboard-driven
# instead of a click: Left moves focus off the default button onto
# Accept, Enter confirms - no coordinates needed, so it can't mis-click
# something else if the dialog isn't in the exact position assumed.
echo "[*] EULA: key Left"
xdo key Left
sleep 3
echo "[*] EULA: key Return"
xdo key Return
# Intro splash(es) play out on their own underneath - no click needed to
# dismiss them, just wait long enough for them to finish (1 or 2
# splashes depending on run, see docs/host-flow.md) before the next
# click. They resolve straight to the real Main Menu - there is no
# separate "campaign landing menu" screen to click through first.
sleep 10

# --- Real Main Menu -> Multiplayer ---
click 384 360 "Multiplayer"

# --- Multiplayer submenu -> LAN/Direct IP ---
click 384 300 "LAN/Direct IP"

# --- Nickname entry ---
echo "[*] click (400, 238) - nickname field"
xdo mousemove 400 238 click 1
sleep "$STEP_DELAY"
# Clear defensively in case of leftover text (single key events - a
# ctrl-a "select all" chord was observed to hang here, see
# docs/host-flow.md). Overshoot to 30 in case of longer leftover text.
for _ in $(seq 1 30); do
    xdo key BackSpace
done
sleep "$STEP_DELAY"
echo "[*] typing nickname: $NICKNAME"
xdo type --delay "$TYPE_DELAY_MS" -- "$NICKNAME"
sleep "$STEP_DELAY"
click 322 302 "nickname OK"

# --- LAN/Direct IP screen: Host a LAN Game field is pre-filled with
# "<nickname>'s Game" ---
click 560 170 "Host"

# --- In-lobby host screen: bump Players from the default 2 to 3, so
# there's an open slot for each of two real clients (this project only
# hosts 1v1s, i.e. host + 2 joiners - see CLAUDE.md). The control is a
# dropdown, not a text field or spinner arrows (confirmed by trial -
# clicking it doesn't cycle a value, and it isn't keyboard-editable):
# clicking the "Players" box opens a list (2-12) anchored so the
# currently-selected value's row renders at the box's own position: click
# (61, 63) to open it, then (55, 74) for the "3" row immediately below.
# Calibrated by hand over VNC 2026-08-08 - not re-derived for other
# player counts since 1v1 is the only format in scope.
echo "[*] click (61, 63) - open Players dropdown"
xdo mousemove 61 63 click 1
sleep 1
echo "[*] click (55, 74) - select 3 Players"
xdo mousemove 55 74 click 1
sleep "$STEP_DELAY"

echo
echo "Done - $POD should now be hosting \"${NICKNAME}'s Game\" with 3 Players"
echo "(host + 2 open slots)."
echo "Next: have clients Direct-Connect, then (once joined) manually click"
echo "Observer Mode at (178, 563) if desired - see docs/host-flow.md."
