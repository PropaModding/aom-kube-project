#!/bin/bash
# Launches the AoM test client attached to aom-nat-client-lan (behind
# the real NAT set up by docker-setup.sh), with full verbose capture
# wired up from the start: tcpdump (raw wire bytes), strace (every
# network-relevant syscall wine/the game makes - socket/bind/connect/
# send*/recv*/getsockopt/setsockopt), and WINEDEBUG (Wine's own socket-
# level call tracing) - all three simultaneously, matching this
# project's established multi-source capture pattern (see run-aom-
# verbose-clients.sh's own header comment for the same reasoning: no
# single source tells the whole story on its own).
#
# strace specifically is new here versus the project's existing spoofed-
# client scripts - added because this lab's whole purpose is
# distinguishing "the game generates a packet with the wrong address
# baked in" (a userspace decision, visible in strace's own sendto()
# argument dump) from "the packet was fine but got mangled/dropped in
# transit" (a network-layer question WINEDEBUG and tcpdump alone can't
# separate as precisely - strace shows the exact bytes the application
# handed to the kernel, before any network involvement at all).
# Filtered to -e trace=network rather than tracing everything: an
# unfiltered strace against a GUI Wine process is an enormous, mostly-
# irrelevant firehose (window messages, file I/O, memory ops) that would
# bury the handful of syscalls that actually matter and meaningfully
# slow the traced process down - same "targeted over exhaustive" choice
# run-aom-verbose-clients.sh's own FULL_RELAY_TRACE option warns about
# for Wine's +relay channel.
#
# Requires SYS_PTRACE (strace's own core requirement) and
# --security-opt seccomp=unconfined - some default docker seccomp
# profiles block ptrace-family syscalls even with the capability granted,
# which would make strace silently attach-and-do-nothing rather than
# erroring clearly, so this is set unconditionally rather than only
# adding it after hitting that failure mode live.
#
# Usage:
#   ./run-client.sh                              # behind NAT (default)
#   NETWORK=aom-nat-internet CONTAINER_NAME=aom-nat-p2 ./run-client.sh
#     # the "not behind NAT" role instead - e.g. for a two-client test
#     # where you drive one of them to host manually (same script both
#     # times rather than a separately-configured run-host.sh, so both
#     # containers are launched and behave identically - easier to
#     # reason about which is which by container name alone, confirmed
#     # 2026-08-29 this is genuinely simpler than juggling two different
#     # scripts for what's really the same launch with a different
#     # network attached)
set -euo pipefail

CONTAINER_NAME="${CONTAINER_NAME:-aom-nat-client}"
NETWORK="${NETWORK:-aom-nat-client-lan}"
WORKDIR="${WORKDIR:-/tmp/aom-nat-lab}"
mkdir -p "$WORKDIR"

if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    echo "$NETWORK doesn't exist - run ./docker-setup.sh first." >&2
    exit 1
fi

WINEDEBUG_CHANNELS="${WINEDEBUG_CHANNELS:-+timestamp,+dpwsockx,+dplay,+dplaysvc,+winsock}"
WINEDEBUG_LOG="$WORKDIR/$CONTAINER_NAME-winedebug.log"
STRACE_LOG="$WORKDIR/$CONTAINER_NAME-strace.log"
PCAP_FILE="$WORKDIR/$CONTAINER_NAME-capture.pcap"

# EXTRA_ARGS - extra aomxnocd1.exe launch parameters (e.g. OverrideAddress
# testing). Appended to the wine command line verbatim, exactly as given -
# not re-parsed by any further shell layer. See the launch-script writeout
# below for why this needed restructuring: the previous inline `sh -c "...
# su aomuser -c \"...\""` construction re-parses its own string THREE times
# (this script -> the container's own sh -c -> su -c's internal shell), and
# each layer's own quote-removal strips one more level of any embedded `\"`
# sequence - fine for a fixed, static command line with no literal quote
# characters in it, but no reliable way to thread a value containing its
# own literal `"` characters (e.g. OverrideAddress=\"1.2.3.4\", matching
# every community-documented syntax) through three re-parses and have it
# arrive at wine's actual argv unmolested. Writing an actual script FILE
# and executing it sidesteps this entirely: the file's own content is
# parsed by exactly one shell, once, when it runs - no re-embedding.
EXTRA_ARGS="${EXTRA_ARGS:-}"

# USER_CFG_OVERRIDE - alternate way to test OverrideAddress: as the first
# line of aom's own startup/user.cfg (confirmed to exist in this image at
# /home/aomuser/aom/startup/user.cfg, already carrying real settings -
# xres/yres/noIntroCinematics/etc - the game demonstrably reads at
# startup) rather than as a command-line argument. Every EXTRA_ARGS-based
# syntax variant tried so far landed correctly in the process's own argv
# (confirmed via /proc/<pid>/cmdline) without changing what the game
# embeds as its own address in the wire protocol - this exercises a
# genuinely different code path (startup-file parsing, not argv parsing)
# rather than re-testing argv syntax again. The existing file is backed
# up first (duplicated, not overwritten blind) so re-runs don't stack
# backup-of-a-backup and the original is always recoverable.
USER_CFG_OVERRIDE="${USER_CFG_OVERRIDE:-}"

echo "[*] Winedebug ($WINEDEBUG_CHANNELS) -> $WINEDEBUG_LOG"
echo "[*] Strace (network syscalls only) -> $STRACE_LOG"
echo "[*] Packet capture -> $PCAP_FILE"
if [ -n "$EXTRA_ARGS" ]; then
    echo "[*] Extra launch args: $EXTRA_ARGS"
fi

LAUNCH_SCRIPT="$WORKDIR/$CONTAINER_NAME-launch.sh"
{
    echo '#!/bin/sh'
    if [ -n "$USER_CFG_OVERRIDE" ]; then
        cat <<INNEREOF
USER_CFG="/home/aomuser/aom/startup/user.cfg"
if [ -f "\$USER_CFG" ] && [ ! -f "\$USER_CFG.orig-backup" ]; then
    cp "\$USER_CFG" "\$USER_CFG.orig-backup"
fi
{ echo 'OverrideAddress="$USER_CFG_OVERRIDE"'; cat "\$USER_CFG.orig-backup" 2>/dev/null; } > "\$USER_CFG"
INNEREOF
    fi
    echo "exec strace -f -tt -e trace=network -o \"$STRACE_LOG\" wine aomxnocd1.exe xres=1024 yres=768 NoIntroCinematics $EXTRA_ARGS"
} > "$LAUNCH_SCRIPT"
chmod 755 "$LAUNCH_SCRIPT"
if [ -n "$USER_CFG_OVERRIDE" ]; then
    echo "[*] user.cfg override armed: OverrideAddress=\"$USER_CFG_OVERRIDE\" will be written as its first line at container startup (old file duplicated to user.cfg.orig-backup first)."
fi

HOST_XAUTH="/tmp/.docker.xauth.natlab.$CONTAINER_NAME"
rm -f "$HOST_XAUTH"
touch "$HOST_XAUTH"
xauth nlist "$DISPLAY" | sed -e 's/^..../ffff/' | xauth -f "$HOST_XAUTH" nmerge -
chmod 644 "$HOST_XAUTH"
xhost +local:docker > /dev/null

docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

docker run -t --rm \
    --name "$CONTAINER_NAME" \
    --network "$NETWORK" \
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
    sh -c "apt-get update -qq && apt-get install -y -qq strace >/dev/null 2>&1; su aomuser -c $LAUNCH_SCRIPT" \
    2>&1 | tee "$WINEDEBUG_LOG" &
DOCKER_RUN_PID=$!

echo "[*] Waiting for the container to be up..."
until docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" >/dev/null 2>&1; do
    sleep 0.5
done

echo "[*] Waiting for the entrypoint's own strace install to finish first"
echo "    (two concurrent apt-get calls in the same container hit a dpkg"
echo "    lock race - confirmed live 2026-08-29 - so this has to wait for"
echo "    'which strace' rather than just 'container is running')..."
until docker exec "$CONTAINER_NAME" sh -c "which strace" >/dev/null 2>&1; do
    sleep 1
done

echo "[*] Installing tcpdump and starting capture..."
docker exec -u root "$CONTAINER_NAME" sh -c \
    "apt-get update -qq && apt-get install -y -qq tcpdump" >/dev/null 2>&1
docker exec -d -u root "$CONTAINER_NAME" \
    tcpdump -i eth0 -w "$PCAP_FILE"

# Docker's own implicit bridge gateway (the network's .1 address) is NOT
# aom-nat-router, even though the router is also attached to this same
# network - a container only reaches the *other* side's subnet through
# the router if something explicitly tells it to route that subnet via
# the router's own address here, rather than the default gateway (which
# has no idea the other subnet even exists and silently black-holes
# it - confirmed live 2026-08-29: this is exactly what "timing out, the
# router doesn't work" turned out to be). Docker doesn't do this for us,
# so add it ourselves once iproute2 is available.
OTHER_SUBNET=""
case "$NETWORK" in
    aom-nat-client-lan) OTHER_SUBNET="203.0.113.0/24" ;;
    aom-nat-internet)   OTHER_SUBNET="10.20.1.0/24" ;;
esac
if [ -n "$OTHER_SUBNET" ]; then
    ROUTER_IP_HERE="$(docker network inspect "$NETWORK" --format '{{range .Containers}}{{if eq .Name "aom-nat-router"}}{{.IPv4Address}}{{end}}{{end}}' | cut -d/ -f1)"
    if [ -n "$ROUTER_IP_HERE" ]; then
        echo "[*] Routing $OTHER_SUBNET via aom-nat-router ($ROUTER_IP_HERE) so this container can actually reach the other side of the NAT boundary..."
        docker exec -u root "$CONTAINER_NAME" sh -c \
            "which ip >/dev/null 2>&1 || (apt-get update -qq && apt-get install -y -qq iproute2 >/dev/null 2>&1)"
        docker exec -u root "$CONTAINER_NAME" ip route add "$OTHER_SUBNET" via "$ROUTER_IP_HERE" dev eth0 2>/dev/null || true
    else
        echo "[!] aom-nat-router isn't attached to $NETWORK - skipping cross-subnet route, reaching the other side will time out." >&2
    fi
fi

echo "[*] Client IP on $NETWORK: $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$CONTAINER_NAME")"
echo "[*] Router's IP on this same network (Direct-Connect target if the other side is the target): $(docker network inspect "$NETWORK" --format '{{range .Containers}}{{if eq .Name "aom-nat-router"}}{{.IPv4Address}}{{end}}{{end}}')"

wait "$DOCKER_RUN_PID"

echo
echo "[*] Done. Captures survive this container's own --rm teardown (all"
echo "    written under \$WORKDIR=$WORKDIR, bind-mounted, same reasoning"
echo "    as run-aom-spoofed-client.sh's own PCAP_FILE doc comment)."
echo "[*] Next: ./archive.sh to collect everything (this client's own"
echo "    captures + aom-nat-router's own two-sided pcaps) into"
echo "    archiving/sessions/, matching this project's existing convention."
xhost -local:docker > /dev/null
rm -f "$HOST_XAUTH"
