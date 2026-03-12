#!/bin/bash

CONTAINER_NAME="aom-test"
REMOTE_PATH="/tmp/aom_screen.png"
LOCAL_PATH="./aom_debug_$(date +%H%M%S).png"

echo "⌨️  Step 1: Navigating EULA (Left + Enter)..."
docker exec $CONTAINER_NAME sh -c "DISPLAY=:99 xdotool key Left Return"

sleep 2

echo "⌨️  Step 2: Confirming (Enter)..."
docker exec $CONTAINER_NAME sh -c "DISPLAY=:99 xdotool key Return"

sleep 4

echo "📸 Step 3: Capturing Screen..."
docker exec $CONTAINER_NAME sh -c "DISPLAY=:99 scrot $REMOTE_PATH"

if docker exec $CONTAINER_NAME ls $REMOTE_PATH >/dev/null 2>&1; then
    docker cp ${CONTAINER_NAME}:${REMOTE_PATH} ${LOCAL_PATH}
    echo "✅ Done! Screenshot saved as: $LOCAL_PATH"
    docker exec $CONTAINER_NAME rm $REMOTE_PATH
else
    echo "❌ Error: Could not find screenshot. Check 'docker logs $CONTAINER_NAME'"
fi

