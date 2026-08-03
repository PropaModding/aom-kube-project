#!/bin/bash
# A "second PC" for testing DirectPlay8 against a k8s pod running with
# hostNetwork: true (see k8s/aom-headless-deployment.yaml), without needing
# an actual second physical device.
#
# Why this exists: a hostNetwork pod binds DirectPlay8's fixed ports
# (2299/2300) directly on the minikube node container's own network stack.
# If you test against it using run-aom-client.sh as-is (--net=host), that
# container shares *this machine's* network stack and collides with
# whatever's already bound to those same ports here — Direct Connect will
# silently hang (query/reply on 2299 works since replies just go back to
# the querying port, but the follow-on session on 2300 never completes).
# See docs/directplay8-protocol.md for how this was diagnosed.
#
# The fix: attach this container to the "minikube" docker network instead
# of the host's — same one the minikube node itself is on — so it gets its
# own IP and its own network namespace. That's a genuinely distinct
# DirectPlay8 endpoint, no port conflicts, while still using real X11/GPU
# passthrough so the game window shows up on your actual desktop like any
# other local container.
#
# Requires minikube to be running (for the "minikube" docker network to
# exist). Direct-Connect to the pod's IP from the window this opens, e.g.
# `minikube ip` if the target pod uses hostNetwork, or the pod IP itself
# otherwise.

# 1. Configuration
IMAGE_NAME="aom-head"
CONTAINER_NAME="aom-spoofed-pc"
DOCKER_NETWORK="${DOCKER_NETWORK:-minikube}"
HOST_XAUTH="/tmp/.docker.xauth.spoofed"

# 2. Prepare X11 Authentication
rm -f $HOST_XAUTH
touch $HOST_XAUTH
xauth nlist $DISPLAY | sed -e 's/^..../ffff/' | xauth -f $HOST_XAUTH nmerge -
chmod 644 $HOST_XAUTH
xhost +local:docker > /dev/null

echo "Starting Age of Mythology: Titans (No-CD) spoofed-PC instance on docker network '$DOCKER_NETWORK'..."

# 3. Launch the Container
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
    -v $HOST_XAUTH:$HOST_XAUTH \
    -v /tmp/.X11-unix:/tmp/.X11-unix \
    -v /run/user/$(id -u)/pulse:/run/user/$(id -u)/pulse \
    -e PULSE_SERVER=unix:/run/user/$(id -u)/pulse/native \
    $IMAGE_NAME \
    wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics

# 4. Cleanup
echo "Cleaning up permissions..."
xhost -local:docker > /dev/null
rm -f $HOST_XAUTH
