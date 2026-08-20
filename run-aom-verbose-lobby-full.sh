#!/bin/bash
# Launches TWO "spoofed PC" containers (a host + a joiner) on the
# minikube docker network for a from-scratch, maximally-verbose capture
# of what classic DirectPlay actually sends when a real client tries to
# join a full lobby - no aom-lobby/proxy involved at all, same topology
# as run-aom-verbose-clients.sh (which captures a genuinely *successful*
# join). The host fills both real-player slots with AI (Standard) before
# the joiner ever attempts to connect, so the joiner's Direct-Connect
# attempt is against an already-full game - AI-filled slots are
# confirmed (tested manually before) to trigger the same "game is full"
# rejection a real second player would.
#
# Why this exists: lobby/packet-handling-design.md's "Case 3 in detail"
# section decided aom-lobby should spoof a "lobby full" rejection rather
# than silently dropping a turned-away client's query, but explicitly
# blocked `synthesizeLobbyFullRejection` on a reference capture that's
# never been taken (see that doc's 2026-08-12 TODO) - this is that
# capture. Every other spoofed/rewritten field in this project started
# from bytes actually observed on the wire; this shouldn't be the
# exception.
#
# Captures BOTH, from both containers:
#   - Wine's own DirectPlay/winsock debug trace (WINEDEBUG - see
#     run-aom-verbose-clients.sh's header comment for the channel choice
#     and why +relay is off by default).
#   - Full raw packet bytes via tcpdump running inside each container -
#     the joiner's capture is the one that actually answers the
#     question (what did it receive in response to its connect attempt);
#     the host's is along for the other vantage point on the same
#     exchange.
#
# Usage: ./run-aom-verbose-lobby-full.sh
#   Opens two GUI windows on your desktop (host, joiner) - both detached
#   (-d), so this script returns immediately. Host a game from the
#   first, fill BOTH open slots with Standard AI (see docs/host-flow.md
#   for the dropdown coordinates if driving it manually rather than by
#   hand), THEN Direct-Connect + Join from the joiner - that connect
#   attempt against the now-full game is the actual capture target.
#
# Requires minikube running (for the "minikube" docker network).
set -euo pipefail

IMAGE_NAME="aom-head"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
# Written straight into archiving/sessions/ (the existing convention for
# captures kept as permanent reference material, see
# archiving/sessions/20260808-234138-3client-p2p-check/ etc.) rather than
# /tmp, so this survives beyond the current session without a manual
# copy step.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="${WORKDIR:-$SCRIPT_DIR/archiving/sessions/$(date +%Y%m%d-%H%M%S)-lobby-full-rejection}"

if [ "${FULL_RELAY_TRACE:-}" = "1" ]; then
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+relay,+dpwsockx,+dplay,+dplaysvc,+dplayx,+dpnet,+winsock,+ws2_32}"
else
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+dplayx,+dpnet,+winsock,+ws2_32}"
fi

mkdir -p "$WORKDIR"

launch() {
    local role="$1" # "host" or "joiner", just for naming
    local container_name="aom-lobbyfull-$role"
    local xauth="/tmp/.docker.xauth.lobbyfull.$role"
    local winedebug_log="$WORKDIR/$role-winedebug.log"

    docker rm -f "$container_name" >/dev/null 2>&1 || true

    rm -f "$xauth"
    touch "$xauth"
    xauth nlist "$DISPLAY" | sed -e 's/^..../ffff/' | xauth -f "$xauth" nmerge -
    chmod 644 "$xauth"

    echo "[*] Launching $container_name (log: $winedebug_log)..."
    docker run -d -t \
        --name "$container_name" \
        --network "$DOCKER_NETWORK" \
        --cap-add=NET_RAW --cap-add=NET_ADMIN \
        --device /dev/dri:/dev/dri \
        --group-add video \
        --group-add render \
        --tmpfs "/run/user/$(id -u):size=100m,mode=700,uid=$(id -u)" \
        -e DISPLAY="$DISPLAY" \
        -e XAUTHORITY="$xauth" \
        -e XDG_RUNTIME_DIR="/run/user/$(id -u)" \
        -e WINEDEBUG="$WINEDEBUG_CHANNELS" \
        -v "$xauth:$xauth" \
        -v /tmp/.X11-unix:/tmp/.X11-unix \
        -v "/run/user/$(id -u)/pulse:/run/user/$(id -u)/pulse" \
        -e PULSE_SERVER="unix:/run/user/$(id -u)/pulse/native" \
        "$IMAGE_NAME" \
        wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics \
        >/dev/null

    docker logs -f "$container_name" > "$winedebug_log" 2>&1 &

    echo "[*] Installing tcpdump in $container_name and starting capture..."
    docker exec -u root "$container_name" sh -c \
        "apt-get update -qq && apt-get install -y -qq tcpdump" >/dev/null 2>&1
    docker exec -d -u root "$container_name" \
        tcpdump -i eth0 -w "/tmp/$role-capture.pcap"

    echo "[*] $container_name up. IP: $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$container_name")"
}

if ! minikube status >/dev/null 2>&1; then
    echo "minikube is not running - start it first (deploy-minikube.sh does this)." >&2
    exit 1
fi

launch host
launch joiner

cat <<EOF

Both windows should now be open on your desktop.

Next steps (manual):
  1. Host a game from "aom-lobbyfull-host". Fill BOTH open slots with
     Standard AI (see docs/host-flow.md's Players-dropdown/slot-dropdown
     coordinates) - the game needs to be genuinely full, no open slots.
  2. Note its IP address shown above (or the in-game "IP Address:"
     readout once hosting) and Direct-Connect + Join to it from
     "aom-lobbyfull-joiner". THIS is the actual capture target - watch
     what happens on the joiner's screen (a rejection message? a silent
     hang? something else?) and note it down, that's exactly as useful
     as the packet bytes.
  3. Once the joiner's attempt has resolved one way or another, stop the
     captures and pull the results out:

     docker exec -u root aom-lobbyfull-host   pkill tcpdump
     docker exec -u root aom-lobbyfull-joiner pkill tcpdump
     docker cp aom-lobbyfull-host:/tmp/host-capture.pcap     $WORKDIR/
     docker cp aom-lobbyfull-joiner:/tmp/joiner-capture.pcap $WORKDIR/

  4. Wine debug logs are already streaming live into:
       $WORKDIR/host-winedebug.log
       $WORKDIR/joiner-winedebug.log
  5. When fully done: docker rm -f aom-lobbyfull-host aom-lobbyfull-joiner

Read the pcaps with: tcpdump -tttt -X -r $WORKDIR/joiner-capture.pcap
(or hand them to Wireshark if available - full byte-level payloads, not
just sizes, unlike the WINEDEBUG logs alone).
EOF
