#!/bin/bash
# Companion to run-client.sh: launches a REAL, self-hosting AoM instance
# on aom-nat-internet (the "internet" side of the NAT lab), so a Direct-
# Connect attempt from run-client.sh's own container has something
# genuinely responsive to reach - not just a passive tcpdump listener
# (aom-nat-target), which correctly produces "no game there" rather
# than the "Attempting to Connect" hang this whole lab exists to
# reproduce (confirmed live 2026-08-29: exactly what happened testing
# against the passive listener first).
#
# Same three-source capture setup as run-client.sh (tcpdump/strace/
# WINEDEBUG) - see that script's own header comment for the reasoning.
# This container plays the host role: not attached to the client-side
# network at all, only aom-nat-internet, matching the real deployment's
# own asymmetry (a real host is never behind the same NAT as the
# player in this project's architecture).
#
# Usage:
#   ./run-host.sh          # after docker-setup.sh
set -euo pipefail

CONTAINER_NAME="${CONTAINER_NAME:-aom-nat-host}"
WORKDIR="${WORKDIR:-/tmp/aom-nat-lab}"
mkdir -p "$WORKDIR"

if ! docker network inspect aom-nat-internet >/dev/null 2>&1; then
    echo "aom-nat-internet doesn't exist - run ./docker-setup.sh first." >&2
    exit 1
fi

WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+winsock}"
WINEDEBUG_LOG="$WORKDIR/$CONTAINER_NAME-winedebug.log"
STRACE_LOG="$WORKDIR/$CONTAINER_NAME-strace.log"
PCAP_FILE="$WORKDIR/$CONTAINER_NAME-capture.pcap"

echo "[*] Winedebug ($WINEDEBUG_CHANNELS) -> $WINEDEBUG_LOG"
echo "[*] Strace (network syscalls only) -> $STRACE_LOG"
echo "[*] Packet capture -> $PCAP_FILE"

HOST_XAUTH="/tmp/.docker.xauth.natlab.$CONTAINER_NAME"
rm -f "$HOST_XAUTH"
touch "$HOST_XAUTH"
xauth nlist "$DISPLAY" | sed -e 's/^..../ffff/' | xauth -f "$HOST_XAUTH" nmerge -
chmod 644 "$HOST_XAUTH"
xhost +local:docker > /dev/null

docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
# In case the earlier passive-listener target is still up on the same
# network - remove it so there's no ambiguity about which container the
# client actually reaches.
docker rm -f aom-nat-target >/dev/null 2>&1 || true

docker run -t --rm \
    --name "$CONTAINER_NAME" \
    --network aom-nat-internet \
    --user root \
    --cap-add=NET_RAW --cap-add=NET_ADMIN --cap-add=SYS_PTRACE \
    --security-opt seccomp=unconfined \
    --device /dev/dri:/dev/dri \
    --group-add video \
    --group-add render \
    --tmpfs "/run/user/$(id -u):size=100m,mode=700,uid=$(id -u)" \
    -e DISPLAY="$DISPLAY" \
    -e XAUTHORITY="$HOST_XAUTH" \
    -e XDG_RUNTIME_DIR="/run/user/$(id -u)" \
    -e WINEDEBUG="$WINEDEBUG_CHANNELS" \
    -v "$HOST_XAUTH:$HOST_XAUTH" \
    -v /tmp/.X11-unix:/tmp/.X11-unix \
    -v "/run/user/$(id -u)/pulse:/run/user/$(id -u)/pulse" \
    -e PULSE_SERVER="unix:/run/user/$(id -u)/pulse/native" \
    -v /tmp:/tmp \
    aom-head \
    sh -c "apt-get update -qq && apt-get install -y -qq strace >/dev/null 2>&1; su aomuser -c \"strace -f -tt -e trace=network -o '$STRACE_LOG' wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics\"" \
    2>&1 | tee "$WINEDEBUG_LOG" &
DOCKER_RUN_PID=$!

echo "[*] Waiting for the container to be up..."
until docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" >/dev/null 2>&1; do
    sleep 0.5
done

echo "[*] Waiting for the entrypoint's own strace install to finish first..."
until docker exec "$CONTAINER_NAME" sh -c "which strace" >/dev/null 2>&1; do
    sleep 1
done

echo "[*] Installing tcpdump and starting capture..."
docker exec -u root "$CONTAINER_NAME" sh -c \
    "apt-get update -qq && apt-get install -y -qq tcpdump" >/dev/null 2>&1
docker exec -d -u root "$CONTAINER_NAME" \
    tcpdump -i eth0 -w "$PCAP_FILE"

HOST_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$CONTAINER_NAME")"
echo "[*] Host IP on aom-nat-internet: $HOST_IP"
echo "[*] From the game window: host a game (Players menu -> Host)."
echo "[*] Then Direct-Connect from run-client.sh's own window to: $HOST_IP"

wait "$DOCKER_RUN_PID"

echo
echo "[*] Done. Captures survive this container's own --rm teardown."
xhost -local:docker > /dev/null
rm -f "$HOST_XAUTH"
