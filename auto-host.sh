#!/bin/bash
# Clicks this pod's own aom-headless instance through to a hosted LAN/
# Direct-IP lobby, using xdotool directly against the local DISPLAY - no
# kubectl, no pod lookup, because this runs *inside* the container it's
# driving (started backgrounded by entrypoint.sh, alongside input-agent).
#
# This is host-game-kube.sh's exact calibrated click sequence, ported to
# run locally instead of through `kubectl exec` - see that script for the
# full history of how each coordinate/delay was calibrated (docs/
# host-flow.md). The two scripts are meant to stay in lock-step: this one
# is what makes a pod self-host as part of its own startup (so
# `kubectl scale deployment/aom-headless --replicas=N` alone is enough to
# add hosts, no external trigger needed); host-game-kube.sh remains for
# manual/debug re-runs against an already-running pod (e.g. watching over
# VNC, or forcing a re-host on a pod that somehow didn't self-host).
#
# Usage: nickname is fixed at "TheIP" (the manual-debug convention
# host-game-kube.sh's own doc comment already established - named after
# the thing a client actually needs to know, the cluster's Direct-IP
# address), not derived per-pod - every self-hosted pod's lobby shows the
# same session name. That's fine: aom-lobby's own N-host routing keys
# everything off each pod's real IP (see lobby/main.go's hostCandidate/
# hostPool), never off the session name, so this has no effect on
# correctness - only on how a human tells two hosts apart while watching
# logs/captures, which live packet inspection can still do via source IP.
# Override with HOST_NICKNAME if that ever matters.
set -uo pipefail

NICKNAME="${HOST_NICKNAME:-TheIP}"
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

echo "[*] auto-host: self-hosting this pod ($(hostname)) as \"${NICKNAME}'s Game\""

xdo() {
    xdotool "$@"
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
        echo "auto-host: game window never appeared, giving up." >&2
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

# --- Observer Mode: fill both "Open" slots with Standard AI first, then
# set Observer Mode, then kick both AI back out to leave the slots open
# for real clients - calibrated live 2026-08-11 (docs/host-flow.md).
# AoM's lobby requires both slots filled before Observer Mode can be set
# at all; the "shoe icon" kick control (confirmed to work identically on
# an AI or a real player - tested by kicking a real connected client
# live) is what reopens a slot afterward without disturbing Observer
# Mode. Each slot's dropdown opens anchored at the slot's own row, with
# "Standard" the option immediately below "Open" (13px per row, same
# spacing as the Players dropdown above) - click 1 opens it, click 2
# selects "Standard".
echo "[*] click (100, 115) - open slot 2 dropdown"
xdo mousemove 100 115 click 1
sleep 1
echo "[*] click (75, 128) - select Standard AI for slot 2"
xdo mousemove 75 128 click 1
sleep "$STEP_DELAY"

echo "[*] click (100, 143) - open slot 3 dropdown"
xdo mousemove 100 143 click 1
sleep 1
echo "[*] click (75, 156) - select Standard AI for slot 3"
xdo mousemove 75 156 click 1
sleep "$STEP_DELAY"

click 178 563 "Observer Mode"

click 37 115 "kick slot 2 AI (reopen for a real client)"
click 37 143 "kick slot 3 AI (reopen for a real client)"

echo
echo "[*] auto-host: done - this pod should now be hosting \"${NICKNAME}'s Game\" with 3 Players"
echo "(host in Observer Mode, 2 open slots for real clients)."
