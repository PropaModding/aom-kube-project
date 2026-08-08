#!/bin/bash
# A "second PC" for testing DirectPlay8 against the cluster, without
# needing an actual second physical device.
#
# As of the aom-lobby/quilkin proxy work, the node's real 2299/2300 belong
# to aom-lobby (hostNetwork, see k8s/lobby-deployment.yaml) and quilkin's
# NodePort, not to aom-headless directly anymore — but the collision
# problem below is the same regardless of which pod owns those ports.
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
# to point session traffic at quilkin's NodePort, transparently to the
# client.

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
WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+dpwsockx,+dplay,+dplaysvc,+winsock}"
WINEDEBUG_LOG="${WINEDEBUG_LOG:-/tmp/aom-spoofed-client-winedebug.log}"

# 2. Prepare X11 Authentication
rm -f $HOST_XAUTH
touch $HOST_XAUTH
xauth nlist $DISPLAY | sed -e 's/^..../ffff/' | xauth -f $HOST_XAUTH nmerge -
chmod 644 $HOST_XAUTH
xhost +local:docker > /dev/null

echo "Starting Age of Mythology: Titans (No-CD) spoofed-PC instance on docker network '$DOCKER_NETWORK'..."
echo "Wine debug ($WINEDEBUG_CHANNELS) also being logged to $WINEDEBUG_LOG"

# 3. Launch the Container
# `| tee` captures Wine's debug channel output (stderr, redirected here)
# to a file so it survives after the window/container closes, while still
# showing on this terminal same as before.
docker run -t --rm \
    --name "$CONTAINER_NAME" \
    --network "$DOCKER_NETWORK" \
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
    $IMAGE_NAME \
    wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics 2>&1 | tee "$WINEDEBUG_LOG"

# 4. Cleanup
echo "Cleaning up permissions..."
xhost -local:docker > /dev/null
rm -f $HOST_XAUTH
