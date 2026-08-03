#!/bin/bash
# Records host mouse clicks and DirectPlay8 UDP traffic side by side so the
# two logs can be correlated afterward (e.g. "which click made the DP8
# session-create packet fire").
#
# Usage: ./record-session.sh [session-name]
#   Ctrl+C stops both recorders.
#
# Output, under archiving/sessions/<session-name>/:
#   clicks.log   one line per click: "<epoch.nanos> <button> <root_x> <root_y>"
#   session.pcap raw capture matching CAPTURE_FILTER (default: DirectPlay8
#                 port range 2300-2400)
#
# Correlating:
#   clicks.log uses wall-clock UTC epoch seconds. View the pcap the same way
#   with `tcpdump -tt -r session.pcap` (also epoch seconds) and line the two
#   up by eye, or load session.pcap in Wireshark and cross-reference times.
#
# Requires: xinput (host X11 click capture), tcpdump (needs sudo — you'll be
# prompted for your password when the capture starts).
#
# CAPTURE_FILTER overrides the tcpdump filter, e.g. to widen the net while
# figuring out which ports AoM actually uses:
#   CAPTURE_FILTER=udp ./record-session.sh
#   CAPTURE_FILTER='udp portrange 2300-2400 or udp port 47624' ./record-session.sh
set -euo pipefail

SESSION_NAME="${1:-$(date +%Y%m%d-%H%M%S)}"
CAPTURE_FILTER="${CAPTURE_FILTER:-udp portrange 2300-2400}"
OUTDIR="$(cd "$(dirname "$0")" && pwd)/archiving/sessions/$SESSION_NAME"
mkdir -p "$OUTDIR"

CLICK_LOG="$OUTDIR/clicks.log"
PCAP_FILE="$OUTDIR/session.pcap"
FIFO="$OUTDIR/.xinput.fifo"

rm -f "$FIFO"
mkfifo "$FIFO"

echo "Recording session '$SESSION_NAME' -> $OUTDIR"
echo "  clicks:  $CLICK_LOG"
echo "  packets: $PCAP_FILE ($CAPTURE_FILTER)"
echo "Press Ctrl+C to stop."
echo ""

# Prompt for sudo now, while we're still in the foreground. If we wait and
# let the backgrounded tcpdump below try to prompt for itself, its stdin is
# /dev/null (bash detaches background jobs' stdin), auth fails silently, and
# tcpdump never starts.
sudo -v

# --- packet capture (needs root) ---
sudo tcpdump -i any -w "$PCAP_FILE" "$CAPTURE_FILTER" &
TCPDUMP_PID=$!

sleep 1 # let tcpdump actually open the capture before we start clicking

# --- click capture ---
# xinput streams raw XI2 events to stdout; feed it through a fifo so we can
# kill the xinput process directly on cleanup instead of relying on SIGPIPE.
xinput test-xi2 --root >"$FIFO" 2>/dev/null &
XINPUT_PID=$!

awk -v logfile="$CLICK_LOG" '
    /EVENT type 4 \(ButtonPress\)/ { in_block=1; button=""; rootxy=""; next }
    in_block && /detail:/ && button == "" {
        line = $0
        gsub(/[^0-9]/, "", line)
        button = line
    }
    in_block && /root:/ && rootxy == "" {
        split($2, xy, "/")
        rootxy = xy[1] " " xy[2]
    }
    in_block && /^$/ {
        if (button != "" && rootxy != "") {
            cmd = "date +%s.%N"
            cmd | getline ts
            close(cmd)
            entry = ts " " button " " rootxy
            print entry
            print entry >> logfile
            fflush(logfile)
        }
        in_block = 0
    }
' <"$FIFO" &
AWK_PID=$!

cleanup() {
    echo ""
    echo "Stopping recorders..."
    sudo kill -INT "$TCPDUMP_PID" 2>/dev/null || true
    kill "$XINPUT_PID" "$AWK_PID" 2>/dev/null || true
    wait "$TCPDUMP_PID" "$XINPUT_PID" "$AWK_PID" 2>/dev/null || true
    rm -f "$FIFO"
    echo "Saved:"
    echo "  $CLICK_LOG"
    echo "  $PCAP_FILE"
    echo ""
    echo "Correlate with: tcpdump -tt -r \"$PCAP_FILE\""
    exit 0
}
trap cleanup INT TERM

wait
