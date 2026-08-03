#!/bin/bash

IMAGE_NAME="aom-test"
CONTAINER_NAME="aom_debug"
VNC_PORT=5900

# 1. PREFLIGHT
if [[ "$*" == *"-p"* ]]; then
    echo "[!] Clearing Port 5900..."
    sudo fuser -k $VNC_PORT/tcp 2>/dev/null
    sleep 1
fi

docker rm -f $CONTAINER_NAME 2>/dev/null

# 2. LAUNCH
echo "[*] Launching Container..."
docker run -d \
    --name $CONTAINER_NAME \
    --net=host \
    --ipc=host \
    --device /dev/dri:/dev/dri \
    -e DISPLAY=:99 \
    -e LIBGL_ALWAYS_SOFTWARE=1 \
    -e WINEDEBUG="err+all,warn+d3d,warn+msxml,+loaddll" \
    $IMAGE_NAME \
    sh -c "rm -f /tmp/.X99-lock && Xvfb :99 -screen 0 800x600x16 -ac & \
    sleep 5 && export DISPLAY=:99 && \
    x11vnc -display :99 -listen 0.0.0.0 -nopw -forever -bg && \
    sleep 2 && \
    wine explorer /desktop=aom,800x600 aomxnocd1.exe 2>&1 | tee /home/aomuser/crash.log"

echo "[*] Waiting for logs to initialize..."
# Increase sleep so the file actually exists before we tail it
sleep 8

# 3. OPEN LOGS IN SECOND TERMINAL
# Try gnome-terminal first, fallback to xterm
# FIXED Terminal Launch Logic
if command -v gnome-terminal &> /dev/null; then
    gnome-terminal -- bash -c "echo 'WATCHING AOM LOGS...'; docker exec -it $CONTAINER_NAME tail -f /home/aomuser/crash.log" &
else
    echo "[!] gnome-terminal not found, streaming here..."
    docker exec -it $CONTAINER_NAME tail -f /home/aomuser/crash.log &
fi

# 4. CONNECT VNC
echo "[*] Opening VNC Viewer..."
gvncviewer 127.0.0.1::$VNC_PORT &

# Keep script alive to see standard docker output
docker logs -f $CONTAINER_NAME

