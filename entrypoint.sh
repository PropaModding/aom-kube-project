#!/bin/bash
set -e

SCREEN_GEOMETRY="${SCREEN_GEOMETRY:-800x600x16}"

# The image's build step backgrounds an Xvfb on this same display for
# wineboot/winetricks setup and never cleanly stops it, so a stale lock
# file from that build layer ships inside the image and makes this Xvfb
# refuse to start ("Server is already active for display N").
rm -f "/tmp/.X${DISPLAY#:}-lock"

Xvfb "$DISPLAY" -screen 0 "$SCREEN_GEOMETRY" -ac &
XVFB_PID=$!

trap 'kill "$XVFB_PID" 2>/dev/null || true' EXIT

until xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; do
    if ! kill -0 "$XVFB_PID" 2>/dev/null; then
        echo "Xvfb died before becoming ready" >&2
        exit 1
    fi
    sleep 0.5
done

if [ "${ENABLE_VNC:-0}" = "1" ]; then
    x11vnc -display "$DISPLAY" -forever -shared -nopw -quiet -bg
fi

# input-agent (see input-agent/main.go) - always on, unlike VNC's opt-in:
# aom-lobby depends on it to drive host-side clicks in reaction to live
# packet events (e.g. both real clients readying up), not just a
# debugging aid.
input-agent &

# auto-host.sh (see that script) - self-hosts this pod into a joinable
# lobby as part of its own startup, the same "no external trigger needed"
# reasoning as input-agent above, just for the initial hosting sequence
# instead of the ready-crystal click. Backgrounded before the exec below
# for the same reason input-agent is: it needs to survive past this
# script's own process image being replaced by wine, and it has to start
# racing against wine's own startup (waiting for the game window) rather
# than running after it - see the script's own doc comment.
auto-host.sh &

exec "$@"
