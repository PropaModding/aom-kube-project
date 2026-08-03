#!/bin/bash
# Second AoM instance, meant to run alongside run-aom-head.sh so you can host
# in one window and browse/join LAN games from the other. DirectPlay8's LAN
# discovery is query/response: a lone host never transmits anything until a
# client broadcasts an "enumerate hosts" query, so this client is what
# actually makes the host's DirectPlay8 traffic happen.
#
# Uses its own Xauth file and container name so it can run at the same time
# as run-aom-head.sh without the two stepping on each other's cleanup.

# 1. Configuration
IMAGE_NAME="aom-head"
CONTAINER_NAME="aom-client"
HOST_XAUTH="/tmp/.docker.xauth.client"

# 2. Prepare X11 Authentication
rm -f $HOST_XAUTH
touch $HOST_XAUTH
xauth nlist $DISPLAY | sed -e 's/^..../ffff/' | xauth -f $HOST_XAUTH nmerge -
chmod 644 $HOST_XAUTH
xhost +local:docker > /dev/null

echo "Starting Age of Mythology: Titans (No-CD) client instance..."

# 3. Launch the Container
docker run -t --rm \
    --name "$CONTAINER_NAME" \
    --net=host \
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
