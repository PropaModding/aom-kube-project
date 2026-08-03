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

exec "$@"
