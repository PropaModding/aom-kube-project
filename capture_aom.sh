#!/bin/bash

# Configuration
CONTAINER_NAME="aom-test"
REMOTE_PATH="/tmp/aom_screen.png"
LOCAL_PATH="./aom_screenshot_$(date +%Y%m%d_%H%M%S).png"

echo "📸 Capturing Age of Mythology screen..."

# 1. Run the screenshot command inside the container
# We use scrot here as it's more reliable in headless Xvfb than 'import'
docker exec $CONTAINER_NAME sh -c "DISPLAY=:99 scrot $REMOTE_PATH"

# 2. Check if the file was actually created
if docker exec $CONTAINER_NAME ls $REMOTE_PATH >/dev/null 2>&1; then
    # 3. Copy the file from the container to your current local folder
    docker cp ${CONTAINER_NAME}:${REMOTE_PATH} ${LOCAL_PATH}
    echo "✅ Success! Screenshot saved to: $LOCAL_PATH"
    
    # 4. Clean up the temp file inside the container
    docker exec $CONTAINER_NAME rm $REMOTE_PATH
else
    echo "❌ Error: Screenshot failed. Is the game running?"
fi

