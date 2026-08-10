#!/bin/bash
# A "second PC" for testing DirectPlay8 against the cluster, without
# needing an actual second physical device.
#
# As of the aom-lobby proxy work, the node's real 2299/2300 belong to
# aom-lobby (hostNetwork, see k8s/lobby-deployment.yaml), not to
# aom-headless directly anymore — but the collision problem below is the
# same regardless of which pod owns those ports.
#
# Why this exists: whatever's bound to the node's own network stack (today:
# aom-lobby on 2299) is reachable at the minikube node's IP. If you test
# against it using run-aom-client.sh as-is (--net=host), that container
# shares *this machine's* network stack and collides with whatever's
# already bound to those same ports here — Direct Connect will silently
# hang (query/reply on 2299 works since replies just go back to the
# querying port, but the follow-on session on 2300 never completes). See
# docs/directplay8-protocol.md for how this was diagnosed.
#
# The fix: attach this container to the "minikube" docker network instead
# of the host's — same one the minikube node itself is on — so it gets its
# own IP and its own network namespace. That's a genuinely distinct
# DirectPlay8 endpoint, no port conflicts, while still using real X11/GPU
# passthrough so the game window shows up on your actual desktop like any
# other local container.
#
# Requires minikube to be running (for the "minikube" docker network to
# exist). Direct-Connect to `minikube ip` from the window this opens — that
# now hits aom-lobby, which relays to the real pod and rewrites the reply
# to point session traffic back at itself, transparently to the client.

# 1. Configuration
# CONTAINER_NAME is overridable so you can run more than one of these at
# once (e.g. two named clients joining the same host to compare their
# traffic) without colliding on container name or X auth file.
IMAGE_NAME="aom-head"
CONTAINER_NAME="${CONTAINER_NAME:-aom-spoofed-pc}"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
HOST_XAUTH="/tmp/.docker.xauth.spoofed.$CONTAINER_NAME"
# AoM (2002) uses classic DirectPlay, not DirectPlay8 - Wine's winsock
# service provider for it is dpwsockx.dll, so that (plus dplay/dplaysvc
# for the higher-level session API) is where a Direct-Connect that never
# reaches UDP 2300 would actually show what the client thinks is
# happening after it gets a discovery reply. Logged to a file (in
# addition to the terminal) so it survives after the window closes.
#
# FULL_RELAY_TRACE=1 swaps in Wine's +relay channel on top of the usual
# set - full Win32 API call tracing, including the actual connect()/bind()
# calls (and their arguments) that +winsock's WS2_* logging doesn't show
# on its own. Deliberately not the default: this is what run-aom-verbose-
# clients.sh's header comment warns is heavy enough to distort a
# genuinely-completing connection's timing. Since this script is for
# poking at an already-broken/hanging attempt (not a working one whose
# timing you don't want to disturb), turning it on here is safe - see
# 2026-08-10 debugging session, where the WS2_* channels alone never once
# logged a connect() call, leaving open whether the client's session
# socket is connect()-ed to a specific remote address (which would make a
# same-IP-different-port relay's traffic get silently kernel-filtered,
# never reaching the app at all) - +relay is what would actually answer
# that.
if [ "${FULL_RELAY_TRACE:-}" = "1" ]; then
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+relay,+dpwsockx,+dplay,+dplaysvc,+dpnet,+winsock,+ws2_32}"
else
    WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+winsock}"
fi
WINEDEBUG_LOG="${WINEDEBUG_LOG:-/tmp/aom-spoofed-client-winedebug.log}"
# Full raw packet bytes via tcpdump running inside the container, same
# technique as run-aom-verbose-clients.sh (see that script's header
# comment for why capturing from outside the container can't see this
# traffic) - captured by default now rather than needing a manual
# `docker exec` bolted on after the fact every time, which is how the
# 2026-08-10 debugging session was doing it. Written under /tmp, which is
# bind-mounted straight into the container below (not `docker cp`'d out
# afterward) specifically so the file survives this script's --rm
# container being torn down the moment the game window closes - there's
# no race to lose the capture to.
PCAP_FILE="${PCAP_FILE:-/tmp/$CONTAINER_NAME-capture.pcap}"

# 2. Prepare X11 Authentication
rm -f $HOST_XAUTH
touch $HOST_XAUTH
xauth nlist $DISPLAY | sed -e 's/^..../ffff/' | xauth -f $HOST_XAUTH nmerge -
chmod 644 $HOST_XAUTH
xhost +local:docker > /dev/null

echo "Starting Age of Mythology: Titans (No-CD) spoofed-PC instance on docker network '$DOCKER_NETWORK'..."
echo "Wine debug ($WINEDEBUG_CHANNELS) also being logged to $WINEDEBUG_LOG"
echo "Packet capture will be written to $PCAP_FILE"

# 3. Launch the Container
# Backgrounded (rather than the simple foreground `| tee` this used to
# be) so this script can `docker exec` in to start tcpdump once the
# container's up, while still blocking overall until the window closes -
# see the `wait` below. --cap-add=NET_RAW/NET_ADMIN, absent before, is
# what tcpdump inside the container needs.
docker run -t --rm \
    --name "$CONTAINER_NAME" \
    --network "$DOCKER_NETWORK" \
    --cap-add=NET_RAW --cap-add=NET_ADMIN \
    --device /dev/dri:/dev/dri \
    --group-add video \
    --group-add render \
    --tmpfs /run/user/$(id -u):size=100m,mode=700,uid=$(id -u) \
    -e DISPLAY=$DISPLAY \
    -e XAUTHORITY=$HOST_XAUTH \
    -e XDG_RUNTIME_DIR=/run/user/$(id -u) \
    -e WINEDEBUG="$WINEDEBUG_CHANNELS" \
    -v $HOST_XAUTH:$HOST_XAUTH \
    -v /tmp/.X11-unix:/tmp/.X11-unix \
    -v /run/user/$(id -u)/pulse:/run/user/$(id -u)/pulse \
    -e PULSE_SERVER=unix:/run/user/$(id -u)/pulse/native \
    -v /tmp:/tmp \
    $IMAGE_NAME \
    wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics 2>&1 | tee "$WINEDEBUG_LOG" &
DOCKER_RUN_PID=$!

echo "Waiting for the container to be up before starting tcpdump..."
until docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" >/dev/null 2>&1; do
    sleep 0.5
done
echo "Installing tcpdump in $CONTAINER_NAME and starting capture..."
docker exec -u root "$CONTAINER_NAME" sh -c \
    "apt-get update -qq && apt-get install -y -qq tcpdump" >/dev/null 2>&1
docker exec -d -u root "$CONTAINER_NAME" \
    tcpdump -i eth0 -w "$PCAP_FILE"

wait "$DOCKER_RUN_PID"

# 4. Cleanup
echo "Cleaning up permissions..."
xhost -local:docker > /dev/null
rm -f $HOST_XAUTH
echo
echo "Packet capture (survives the container's removal, see PCAP_FILE's doc"
echo "comment above): $PCAP_FILE"
echo "Read it with: tcpdump -tttt -X -r $PCAP_FILE"
