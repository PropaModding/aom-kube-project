#!/bin/bash
# A real, three-hop NAT topology built entirely from Linux network
# namespaces + veth pairs + iptables MASQUERADE - for testing AoM's own
# NAT-traversal behavior locally, without needing the real AKS cluster.
#
# Why this exists: an earlier attempt at this (two differently-numbered
# docker networks, one "looking" public) never actually did NAT at all -
# it just gave two containers different-looking IPs on a flat, directly-
# routed L3 space, with nothing genuinely translated. That can't
# reproduce the actual bug mechanism (a client that only knows its own
# private address, with no idea what its real external one is) - see
# CLAUDE.md's AKS-migration debugging history for the full trail this
# grew out of. This lab does real translation: the client's own socket
# genuinely only ever knows its private address; the only way anything
# reaches it is through actual conntrack state living in the router
# namespace, aged out like any real NAT device's.
#
# Topology:
#   [client-ns] --veth--> [router-ns] --veth--> [internet-ns]
#                    (real iptables MASQUERADE here)
#
#   client-ns:    10.10.1.2/24  - private, "the player's home LAN"
#   router-ns:    10.10.1.1/24 (client-facing) + 203.0.113.1/24
#                 (internet-facing) - "the player's home router"
#   internet-ns:  203.0.113.2/24 - "the public internet", where
#                 aom-lobby (or a stand-in test listener) would run
#
# 203.0.113.0/24 is RFC 5737 TEST-NET-3 - reserved for documentation/
# testing, guaranteed never to collide with anything real, so it's safe
# to treat as "looks public" without any chance of actually routing
# there by accident.
#
# NAT type is a deliberate, adjustable choice here, not an accident -
# see NAT_MODE below. The AoM community's own documented failure mode
# is specifically tied to symmetric NAT, so being able to switch between
# NAT types on demand is the actual point of building this rather than
# just relying on whatever a real router happens to do.
#
# Needs root (network namespace creation, veth, iptables) - run this
# directly with sudo, not via Claude/any non-interactive agent, same
# reasoning as fix-client-isolation.sh's own header comment.
#
# Usage:
#   sudo ./setup.sh            # cone NAT (typical consumer router)
#   sudo NAT_MODE=symmetric ./setup.sh   # symmetric NAT (the failure case)
#   sudo ./teardown.sh         # tear it all down
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "Needs root - run with sudo." >&2
    exit 1
fi

NAT_MODE="${NAT_MODE:-cone}"

CLIENT_NS="aom-client-ns"
ROUTER_NS="aom-router-ns"
INTERNET_NS="aom-internet-ns"

CLIENT_ADDR="10.10.1.2/24"
ROUTER_CLIENT_ADDR="10.10.1.1/24"
ROUTER_INTERNET_ADDR="203.0.113.1/24"
INTERNET_ADDR="203.0.113.2/24"

echo "[*] Creating namespaces..."
ip netns add "$CLIENT_NS"
ip netns add "$ROUTER_NS"
ip netns add "$INTERNET_NS"

echo "[*] Creating veth pairs..."
ip link add veth-cli type veth peer name veth-cli-r
ip link add veth-net type veth peer name veth-net-r

echo "[*] Assigning interfaces to namespaces..."
ip link set veth-cli netns "$CLIENT_NS"
ip link set veth-cli-r netns "$ROUTER_NS"
ip link set veth-net netns "$INTERNET_NS"
ip link set veth-net-r netns "$ROUTER_NS"

echo "[*] Configuring client namespace..."
ip netns exec "$CLIENT_NS" ip addr add "$CLIENT_ADDR" dev veth-cli
ip netns exec "$CLIENT_NS" ip link set veth-cli up
ip netns exec "$CLIENT_NS" ip link set lo up
ip netns exec "$CLIENT_NS" ip route add default via 10.10.1.1

echo "[*] Configuring router namespace..."
ip netns exec "$ROUTER_NS" ip addr add "$ROUTER_CLIENT_ADDR" dev veth-cli-r
ip netns exec "$ROUTER_NS" ip addr add "$ROUTER_INTERNET_ADDR" dev veth-net-r
ip netns exec "$ROUTER_NS" ip link set veth-cli-r up
ip netns exec "$ROUTER_NS" ip link set veth-net-r up
ip netns exec "$ROUTER_NS" ip link set lo up
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.ip_forward=1

echo "[*] Configuring internet namespace..."
ip netns exec "$INTERNET_NS" ip addr add "$INTERNET_ADDR" dev veth-net
ip netns exec "$INTERNET_NS" ip link set veth-net up
ip netns exec "$INTERNET_NS" ip link set lo up
# No route back to 10.10.1.0/24 needed or wanted here - that's the whole
# point of NAT. This namespace should only ever see the router's own
# translated address, never the client's real private one, exactly like
# a real internet host never sees a NAT'd client's private address.

echo "[*] Applying NAT (mode: $NAT_MODE) in router namespace..."
case "$NAT_MODE" in
    cone)
        # Endpoint-independent mapping - typical consumer router
        # behavior. Same external port reused for a given internal
        # socket regardless of destination.
        ip netns exec "$ROUTER_NS" iptables -t nat -A POSTROUTING -o veth-net-r -j MASQUERADE
        ;;
    symmetric)
        # A genuinely different external port per destination - the
        # specific NAT type the AoM community's own documented failure
        # mode is tied to. --random-fully picks a fully random port per
        # translation rather than trying to preserve the original.
        ip netns exec "$ROUTER_NS" iptables -t nat -A POSTROUTING -o veth-net-r -j MASQUERADE --random-fully
        ;;
    *)
        echo "Unknown NAT_MODE '$NAT_MODE' - expected 'cone' or 'symmetric'" >&2
        exit 1
        ;;
esac

echo
echo "[*] Lab is up. Quick sanity check:"
echo "      sudo ip netns exec $INTERNET_NS nc -l -u -p 9999 &"
echo "      sudo ip netns exec $CLIENT_NS sh -c 'echo hello | nc -u -w1 203.0.113.2 9999'"
echo "    (should print 'hello' in the listener - confirms the path works)"
echo
echo "[*] To see the REAL translated source address/port the internet"
echo "    namespace observes (this is the actual point of the lab):"
echo "      sudo ip netns exec $INTERNET_NS tcpdump -i veth-net -n udp"
echo "    then send traffic from the client namespace and watch the"
echo "    source address change from what the client itself believes."
echo
echo "[*] Tear down with: sudo ./teardown.sh"
