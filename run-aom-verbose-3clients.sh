#!/bin/bash
# Launches THREE "spoofed PC" containers (a host + two joiners) on the
# minikube docker network for a from-scratch, maximally-verbose capture of
# a genuine, non-proxied 3-player DirectPlay session - no aom-lobby
# involved at all. Same technique as run-aom-verbose-clients.sh (which
# only covers host + one joiner), extended to answer a question that
# script can't: once a SECOND real client joins the SAME match, does it
# talk to the host only, or does it also address traffic directly at the
# other joiner? aom-lobby currently only proxies client<->host - if AoM's
# session layer expects any kind of client<->client addressing once
# there's more than one joiner, that's a real architecture gap in the
# proxy, not a rewrite bug. See docs/directplay8-protocol.md for how this
# question came up (two clients through aom-lobby, one join succeeds,
# the second hangs at "Attempting to Connect" even after fixing the
# session-port routing bug that explained the *first* multi-client
# failure).
#
# Captures BOTH, from all three containers:
#   - Wine's own DirectPlay/winsock debug trace (WINEDEBUG - call-level
#     detail: which socket, which function, payload *size*). Deliberately
#     NOT cranked all the way to Wine's +relay (full Win32 call tracing) -
#     that's synchronous stderr I/O on every single API call, and this
#     project already has direct evidence that logging overhead on a
#     hot path can distort the very timing-sensitive network behavior
#     under study (see aom-lobby's VERBOSE flag doc comment in
#     lobby/main.go). tcpdump below is the actual source of maximum
#     verbosity here - it's kernel-level, asynchronous, zero impact on
#     the game process, and gives full payload bytes rather than just
#     call-level sizes. WINEDEBUG stays at the channel set already
#     proven sufficient for the earlier handshake diagnosis, plus
#     +dplayx (Wine's actual module name for the classic DirectPlay
#     lobby/enumeration implementation - distinct from +dplay, not
#     included in earlier captures, and directly relevant to
#     multi-peer session/player-list behavior).
#   - Full raw packet *bytes* via tcpdump running inside each container,
#     one per role. Neither container is hostNetwork, so capturing from
#     outside can't see this (see run-aom-verbose-clients.sh's header
#     comment for why) - capturing *inside* each container is what
#     works, hence --cap-add=NET_RAW/NET_ADMIN and an in-container
#     tcpdump per container. Capturing on ALL THREE containers
#     simultaneously is the actual point of this script: if joiner A and
#     joiner B ever talk directly, their own two tcpdumps are the only
#     vantage points that would ever see that traffic - the host's
#     capture wouldn't, since traffic between two sibling containers on
#     the same bridge doesn't get flooded to a third port that isn't
#     part of the conversation.
#
# Usage: ./run-aom-verbose-3clients.sh
#   Opens three GUI windows on your desktop (host, clienta, clientb) -
#   all detached (-d), so this script returns immediately. Host a game
#   from the first (remember to bump Players to 3 - see
#   docs/host-flow.md), Direct-Connect + Join from the other two, same
#   manual flow as run-aom-spoofed-client.sh. See the "Collecting
#   results" commands this script prints at the end once you're done.
#
# Requires minikube running (for the "minikube" docker network).
set -euo pipefail

IMAGE_NAME="aom-head"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
# Written straight into archiving/sessions/ (the existing convention for
# captures kept as permanent reference material, see
# archiving/sessions/20260803-3v3-win-loss/ etc.) rather than /tmp, so
# this survives beyond the current session without a manual copy step.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="${WORKDIR:-$SCRIPT_DIR/archiving/sessions/$(date +%Y%m%d-%H%M%S)-3client-p2p-check}"

# FULL_RELAY_TRACE=1 adds Wine's +relay channel - full Win32 API call
# tracing, including actual connect()/bind() calls and their arguments,
# which the WS2_* logging from +winsock alone doesn't surface. Off by
# default per this script's own header comment on why +relay was
# deliberately excluded (timing distortion risk on a genuinely-completing
# connection) - turn on when specifically chasing a question only +relay
# can answer, e.g. whether the client<->client session socket ever calls
# connect() at all (see the 2026-08-10 multi-peer-routing debugging
# session, where a proxied attempt's WS2_* channels alone never logged
# one, leaving that open).
if [ "${FULL_RELAY_TRACE:-}" = "1" ]; then
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+relay,+dpwsockx,+dplay,+dplaysvc,+dplayx,+dpnet,+winsock,+ws2_32}"
else
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+dplayx,+dpnet,+winsock,+ws2_32}"
fi

mkdir -p "$WORKDIR"

launch() {
    local role="$1" # "host", "clienta", or "clientb" - just for naming
    local container_name="aom-verbose3-$role"
    local xauth="/tmp/.docker.xauth.verbose3.$role"
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

    # WINEDEBUG output goes to the container's stdout/stderr (that's how
    # Wine emits it) - `docker logs -f` streams and saves it, same as the
    # `| tee` redirect run-aom-spoofed-client.sh uses for its
    # foreground/attached run, just via docker's own log capture since
    # this one's detached.
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
launch clienta
launch clientb

cat <<EOF

All three windows should now be open on your desktop.

Next steps (manual):
  1. Host a game from the "aom-verbose3-host" window. Bump Players to 3
     once in the lobby (top-left dropdown - see docs/host-flow.md).
  2. Note its IP address shown above (or the in-game "IP Address:"
     readout once hosting) and Direct-Connect + Join to it from BOTH
     "aom-verbose3-clienta" and "aom-verbose3-clientb" - the actual
     scenario under test is having both joined at once, so join clienta
     first, confirm it's in, then join clientb.
  3. Once both are joined (or you've given up on the second), stop the
     captures and pull the results out:

     docker exec -u root aom-verbose3-host    pkill tcpdump
     docker exec -u root aom-verbose3-clienta pkill tcpdump
     docker exec -u root aom-verbose3-clientb pkill tcpdump
     docker cp aom-verbose3-host:/tmp/host-capture.pcap       $WORKDIR/
     docker cp aom-verbose3-clienta:/tmp/clienta-capture.pcap $WORKDIR/
     docker cp aom-verbose3-clientb:/tmp/clientb-capture.pcap $WORKDIR/

  4. Wine debug logs are already streaming live into:
       $WORKDIR/host-winedebug.log
       $WORKDIR/clienta-winedebug.log
       $WORKDIR/clientb-winedebug.log
  5. When fully done: docker rm -f aom-verbose3-host aom-verbose3-clienta aom-verbose3-clientb

Read the pcaps with: tcpdump -tttt -X -r $WORKDIR/clienta-capture.pcap
(or hand them to Wireshark if available). The clienta/clientb captures
are the ones that answer the actual question - any packets between
clienta's and clientb's own IPs directly (not via the host) confirm
client<->client addressing exists at the protocol level.
EOF
