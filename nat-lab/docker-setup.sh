#!/bin/bash
# Docker-native version of the network-namespace NAT lab (see setup.sh/
# teardown.sh in this same directory for the raw-namespace version) -
# built specifically so the *actual* AoM test client container (with all
# its existing X11/GPU-passthrough plumbing from run-aom-spoofed-
# client.sh) can be routed through a real NAT boundary, rather than
# reparenting a running container into a namespace Docker doesn't know
# about.
#
# Two genuinely separate docker networks - no shared bridge, no host-
# level DOCKER-USER routing tricks (that's the mistake an earlier
# attempt made: two differently-numbered docker networks on a flat,
# directly-routed L3 space never did real NAT at all, just gave
# containers different-looking IPs). Translation happens inside a
# dedicated nat-router container instead, using its own iptables -
# Docker already grants NET_ADMIN, so no host sudo/DOCKER-USER editing
# needed.
#
#   [aks-test-client] --(aom-nat-client-lan)--> [nat-router] --(aom-nat-internet)--> [aom-nat-target]
#                                          (real iptables MASQUERADE here)
#
# aom-nat-target is a placeholder listener standing in for wherever the
# real endpoint lives (a local minikube aom-lobby, or eventually the
# real AKS one) - this script just gets the NAT path itself working;
# run-client.sh (this same directory) is what actually launches AoM.
#
# Needs root for the nat-router container's own iptables calls inside
# itself - NET_ADMIN is enough, no host-level sudo needed, unlike the
# raw-namespace lab.
#
# Usage:
#   ./docker-setup.sh                    # cone NAT
#   NAT_MODE=symmetric ./docker-setup.sh # symmetric NAT (the AoM
#                                         # community's documented
#                                         # failure case)
#   ./docker-teardown.sh                 # tear it all down
set -euo pipefail

NAT_MODE="${NAT_MODE:-cone}"
CLIENT_SUBNET="10.20.1.0/24"
INTERNET_SUBNET="203.0.113.0/24"  # RFC 5737 TEST-NET-3, same reasoning
                                    # as the raw-namespace lab's own
                                    # header comment - reserved for
                                    # documentation/testing, never
                                    # collides with anything real.

echo "[*] Creating client-side network ($CLIENT_SUBNET)..."
docker network create --subnet="$CLIENT_SUBNET" aom-nat-client-lan >/dev/null

echo "[*] Creating internet-side network ($INTERNET_SUBNET)..."
docker network create --subnet="$INTERNET_SUBNET" aom-nat-internet >/dev/null

echo "[*] Launching nat-router (mode: $NAT_MODE)..."
# --sysctl here, not a `sysctl` call after the container's already
# running: /proc/sys/net/* is mounted read-only inside a container by
# default (confirmed live 2026-08-29) unless the sysctl is set at
# `docker run` time - NET_ADMIN alone isn't enough. ip_forward is a
# "namespaced" sysctl (has been since Linux 4.x), so --sysctl works
# without needing the much broader --privileged.
docker run -d --rm \
    --name aom-nat-router \
    --network aom-nat-client-lan \
    --cap-add=NET_ADMIN \
    --cap-add=NET_RAW \
    --sysctl net.ipv4.ip_forward=1 \
    alpine sh -c "apk add --no-cache iptables tcpdump >/dev/null 2>&1 && sleep infinity" >/dev/null

docker network connect aom-nat-internet aom-nat-router

echo "[*] Waiting for nat-router's own package install..."
until docker exec aom-nat-router which iptables >/dev/null 2>&1; do sleep 0.5; done

# Find the router's own internet-facing interface name (Docker doesn't
# guarantee eth0/eth1 ordering matches connection order).
ROUTER_INTERNET_IP="$(docker network inspect aom-nat-internet --format '{{range .Containers}}{{if eq .Name "aom-nat-router"}}{{.IPv4Address}}{{end}}{{end}}' | cut -d/ -f1)"
ROUTER_IF="$(docker exec aom-nat-router sh -c "ip -o addr show | grep '$ROUTER_INTERNET_IP' | awk '{print \$2}'")"

echo "[*] Router's internet-facing interface: $ROUTER_IF ($ROUTER_INTERNET_IP)"
# ip_forward already set via --sysctl at launch time above - a second
# `sysctl` call here would hit the same read-only-/proc failure.

case "$NAT_MODE" in
    cone)
        docker exec aom-nat-router iptables -t nat -A POSTROUTING -o "$ROUTER_IF" -j MASQUERADE
        ;;
    symmetric)
        docker exec aom-nat-router iptables -t nat -A POSTROUTING -o "$ROUTER_IF" -j MASQUERADE --random-fully
        ;;
    *)
        echo "Unknown NAT_MODE '$NAT_MODE' - expected 'cone' or 'symmetric'" >&2
        exit 1
        ;;
esac

echo
echo "[*] NAT lab up. aom-nat-router IP on client side: $(docker network inspect aom-nat-client-lan --format '{{range .Containers}}{{if eq .Name "aom-nat-router"}}{{.IPv4Address}}{{end}}{{end}}')"
echo "    IP on internet side: $ROUTER_INTERNET_IP"
echo
echo "[*] Next: ./run-client.sh to launch the actual AoM test client on"
echo "    aom-nat-client-lan, with tcpdump/strace/WINEDEBUG all wired up."
echo "[*] Tear down with: ./docker-teardown.sh"
