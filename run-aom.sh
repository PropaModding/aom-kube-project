#!/bin/bash

# 1. Configuration
IMAGE_NAME="aom-test"
HOST_XAUTH="/tmp/.docker.xauth"
CONTAINER_NAME="aom_game"

# --- STEP 1: X11 AUTH CHECK & SETUP ---
echo "Step 1: Setting up X11 authentication..."

# Clean up old auth file
rm -f $HOST_XAUTH

# Create fresh auth file with the correct 'magic cookie' for your display
if [ -z "$DISPLAY" ]; then
    echo "ERROR: DISPLAY environment variable is not set. Are you in a GUI session?"
    exit 1
fi

touch $HOST_XAUTH
xauth nlist $DISPLAY | sed -e 's/^..../ffff/' | xauth -f $HOST_XAUTH nmerge -
chmod 644 $HOST_XAUTH

# Allow local connections to X11
xhost +local:docker > /dev/null
echo "Step 1 Complete: X11 Auth file created at $HOST_XAUTH"

# --- STEP 2: DYNAMIC GROUP DETECTION ---
# We find the numerical IDs for video and render groups to avoid "group not found" errors
VIDEO_GID=$(getent group video | cut -d: -f3)
RENDER_GID=$(getent group render | cut -d: -f3)

# Fallback: If 'render' doesn't exist, just use 'video' again to prevent crashes
if [ -z "$RENDER_GID" ]; then RENDER_GID=$VIDEO_GID; fi

echo "Step 2: Starting Age of Mythology (Image: $IMAGE_NAME)..."

# --- STEP 3: LAUNCH ---
docker run -it --rm \
    --name $CONTAINER_NAME \
    --net=host \
    --ipc=host \
    --device /dev/dri:/dev/dri \
    --group-add $VIDEO_GID \
    --group-add $RENDER_GID \
    --tmpfs /run/user/$(id -u):size=100m,mode=700,uid=$(id -u) \
    -e DISPLAY=$DISPLAY \
    -e XAUTHORITY=$HOST_XAUTH \
    -e XDG_RUNTIME_DIR=/tmp \
    -v $HOST_XAUTH:$HOST_XAUTH \
    -v /tmp/.X11-unix:/tmp/.X11-unix \
    $IMAGE_NAME \
    wine aomxnocd1.exe xres=800 yres=600 NoIntroCinematics NoSound

# --- STEP 4: CLEANUP ---
echo "Cleaning up permissions..."
xhost -local:docker > /dev/null
rm -f $HOST_XAUTH

