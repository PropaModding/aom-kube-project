#!/bin/bash
# Launches TWO "spoofed PC" containers (a host + a joiner) on the
# minikube docker network for a from-scratch, maximally-verbose capture
# of a genuinely successful DirectPlay connection between two real
# clients - no aom-lobby/proxy involved at all, same topology as the
# 2026-08-07 test that first proved client-to-client joining works.
# Meant to be diffed against a proxied/failing attempt (see
# docs/directplay8-protocol.md's "Connection-attempt-phase messages"
# section) to find exactly what a real connection does that ours doesn't.
#
# Captures BOTH:
#   - Wine's own DirectPlay/winsock debug trace (WINEDEBUG - call-level
#     detail: which socket, which function, payload *size* - same
#     mechanism run-aom-spoofed-client.sh already uses, just with a
#     broader channel set here since this run is explicitly about not
#     missing anything).
#   - Full raw packet *bytes* via tcpdump running inside each container.
#     Neither container is hostNetwork, so capturing from outside (e.g.
#     `docker exec minikube tcpdump ...`, which worked fine for
#     aom-lobby traffic since the node itself was one of the two
#     endpoints there) can't see this: traffic between two other sibling
#     containers on the same bridge network doesn't get flooded to a
#     third port that isn't part of the conversation - that's normal L2
#     switch behavior, not a docker quirk. Capturing *inside* each
#     container (their own interface) is what actually works, hence
#     --cap-add=NET_RAW/NET_ADMIN below and a tcpdump launched via
#     `docker exec -u root` once each container is up (root because
#     apt-get/tcpdump need it; the container's own aomuser process
#     keeps running unprivileged throughout).
#
# Usage: ./run-aom-verbose-clients.sh
#   Opens two GUI windows on your desktop (host, joiner) - both detached
#   (-d), so this script returns immediately rather than blocking on one
#   window. Host a game from the first, Direct-Connect + Join from the
#   second, same manual flow as run-aom-spoofed-client.sh. See the
#   "Collecting results" commands this script prints at the end once
#   you're done.
#
# Requires minikube running (for the "minikube" docker network).
set -euo pipefail

IMAGE_NAME="aom-head"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
WORKDIR="${WORKDIR:-/tmp/aom-verbose-clients}"

# Broad but bounded - not Wine's blanket +all, which also traces window
# messages/GDI/etc. and would be many GB of noise, likely making the
# actual game window too slow to click through live. This set covers
# every network-relevant subsystem AoM/Wine's dplay stack could plausibly
# touch: classic DirectPlay COM (+dplay/+dplaysvc), its winsock service
# provider (+dpwsockx), the newer DirectPlay8 API in case any of it is
# used (+dpnet), and the raw sockets layer everything ultimately rides on
# (+winsock/+ws2_32).
WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+dpwsockx,+dplay,+dplaysvc,+dpnet,+winsock,+ws2_32}"

mkdir -p "$WORKDIR"

launch() {
    local role="$1" # "host" or "joiner", just for naming
    local container_name="aom-verbose-$role"
    local xauth="/tmp/.docker.xauth.verbose.$role"
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
launch joiner

cat <<EOF

Both windows should now be open on your desktop.

Next steps (manual, same as any other spoofed-client test):
  1. Host a game from the "aom-verbose-host" window.
  2. Note its IP address shown above (or check the in-game "IP Address:"
     readout once hosting) and Direct-Connect + Join to it from the
     "aom-verbose-joiner" window.
  3. Once you've either connected successfully or given up, stop the
     capture and pull the results out:

     docker exec -u root aom-verbose-host    pkill tcpdump
     docker exec -u root aom-verbose-joiner  pkill tcpdump
     docker cp aom-verbose-host:/tmp/host-capture.pcap     $WORKDIR/
     docker cp aom-verbose-joiner:/tmp/joiner-capture.pcap $WORKDIR/

  4. Wine debug logs are already streaming live into:
       $WORKDIR/host-winedebug.log
       $WORKDIR/joiner-winedebug.log
  5. When fully done: docker rm -f aom-verbose-host aom-verbose-joiner

Read the pcaps with: tcpdump -tttt -X -r $WORKDIR/host-capture.pcap
(or hand them to Wireshark if available - full byte-level payloads,
not just sizes, unlike the WINEDEBUG logs alone).
EOF
