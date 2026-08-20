#!/bin/bash
# Enhanced version of run-aom-verbose-clients.sh for
# lobby/packet-handling-design.md's "Next investigation (TOP PRIORITY)"
# section - classic DirectPlay likely has an unreverse-engineered
# session-establishment layer below every message this project has
# decoded so far, and finding it needs two things every earlier capture
# in this project is missing:
#   - Syscall-level visibility (Wine's own +dplay/+dplayx/+dpwsockx
#     debug channels are confirmed silent for this build - strace is
#     the only lever left, see that doc section's step 1).
#   - Confirmation nothing crosses the wire outside UDP 2299/2300 - the
#     base script's own tcpdump call already has no port filter, so
#     that half is already covered; this version just adds strace
#     alongside it.
#
# Everything else (host+joiner launch, WINEDEBUG, tcpdump, manual
# join flow) is identical to run-aom-verbose-clients.sh - see that
# script's own header comments for the full rationale. This one only
# adds: installing + attaching strace -f -tt to the running wine
# process tree once the container is up (can't wrap wine at launch
# with strace directly - "aomuser" has no root, same constraint noted
# in docs/host-flow.md for why tcpdump is installed post-launch via a
# separate `docker exec -u root` call instead of baked into the image).
#
# Usage: ./run-aom-verbose-clients-strace.sh
#   Same manual flow as run-aom-verbose-clients.sh: host a game from the
#   first window, Direct-Connect + Join from the second. Written to
#   archiving/sessions/ (not /tmp like the base script) since this is
#   meant to be kept as permanent reference material for the
#   session-establishment investigation, same convention as
#   run-aom-verbose-lobby-full.sh.
#
# Requires minikube running (for the "minikube" docker network).
set -euo pipefail

IMAGE_NAME="aom-head"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="${WORKDIR:-$SCRIPT_DIR/archiving/sessions/$(date +%Y%m%d-%H%M%S)-session-establishment-investigation}"

if [ "${FULL_RELAY_TRACE:-}" = "1" ]; then
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+relay,+dpwsockx,+dplay,+dplaysvc,+dpnet,+winsock,+ws2_32}"
else
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+dpnet,+winsock,+ws2_32}"
fi

mkdir -p "$WORKDIR"

launch() {
    local role="$1" # "host" or "joiner", just for naming
    local container_name="aom-strace-$role"
    local xauth="/tmp/.docker.xauth.strace.$role"
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
        --cap-add=NET_RAW --cap-add=NET_ADMIN --cap-add=SYS_PTRACE \
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

    echo "[*] Installing tcpdump+strace in $container_name and starting capture..."
    docker exec -u root "$container_name" sh -c \
        "apt-get update -qq && apt-get install -y -qq tcpdump strace" >/dev/null 2>&1
    # No port filter deliberately - see this script's own header comment
    # on why (rule out a session-establishment step outside 2299/2300).
    docker exec -d -u root "$container_name" \
        tcpdump -i eth0 -w "/tmp/$role-capture.pcap"

    # Attach after launch, not wrapped at launch - see header comment on
    # why (aomuser has no root, can't apt-get install strace before the
    # container's own CMD runs). This is racy against whatever happens
    # between container start and this attach - fine in practice, since
    # the actual DirectPlay handshake only happens much later (once a
    # human clicks through EULA/menus/host-or-join), giving plenty of
    # time for this to land first. -f follows every forked child (wine
    # spawns several processes - wineserver, services.exe, and
    # eventually the game's own process - -f is what catches all of
    # them from one attach point rather than needing to find the exact
    # right PID).
    echo "[*] Attaching strace -f to $container_name's process tree..."
    docker exec -d -u root "$container_name" sh -c \
        "strace -f -tt -o /tmp/$role-strace.log -p 1 2>/tmp/$role-strace-attach.log"

    echo "[*] $container_name up. IP: $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$container_name")"
}

if ! minikube status >/dev/null 2>&1; then
    echo "minikube is not running - start it first (deploy-minikube.sh does this)." >&2
    exit 1
fi

launch host
launch joiner

cat <<EOF

Both windows should now be open on your desktop, with tcpdump (no port
filter) and strace -f both attached and running.

Next steps (manual, same as run-aom-verbose-clients.sh):
  1. Host a game from "aom-strace-host".
  2. Direct-Connect + Join from "aom-strace-joiner" to the host's IP
     (shown above, or the in-game "IP Address:" readout once hosting).
  3. Once connected (or given up on), stop the captures and pull
     results out:

     docker exec -u root aom-strace-host   pkill tcpdump
     docker exec -u root aom-strace-joiner pkill tcpdump
     docker exec -u root aom-strace-host   pkill strace
     docker exec -u root aom-strace-joiner pkill strace
     docker cp aom-strace-host:/tmp/host-capture.pcap     $WORKDIR/
     docker cp aom-strace-joiner:/tmp/joiner-capture.pcap $WORKDIR/
     docker cp aom-strace-host:/tmp/host-strace.log       $WORKDIR/
     docker cp aom-strace-joiner:/tmp/joiner-strace.log   $WORKDIR/

  4. Wine debug logs are already streaming live into:
       $WORKDIR/host-winedebug.log
       $WORKDIR/joiner-winedebug.log
  5. When fully done: docker rm -f aom-strace-host aom-strace-joiner

Read the pcaps with: tcpdump -tttt -X -r $WORKDIR/joiner-capture.pcap
Read the straces with: less $WORKDIR/joiner-strace.log (look for
socket/sendto/recvfrom/poll calls, and anything reading system time -
clock_gettime, gettimeofday - around the moment the 0x02 ack should
fire, per lobby/packet-handling-design.md's "hidden timestamp"
hypothesis).
EOF
