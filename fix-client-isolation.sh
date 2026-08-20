#!/bin/bash
# Reapplies the client<->client isolation iptables rule on the physical
# host's DOCKER-USER (FORWARD) chain - see CLAUDE.md's "LAN/broadcast
# sanitization" section for the full three-layer design this is one part
# of (Calico NetworkPolicy + this rule + node-netns broadcast
# suppression).
#
# deploy-minikube.sh already reapplies this (and the other two layers)
# idempotently on every full deploy - this script exists for the
# narrower case of `minikube start` alone (e.g. after the host slept, or
# minikube stopped for an unrelated reason) without re-running the full
# deploy. Plain iptables state, not persisted anywhere - lost on host
# reboot or on `minikube delete` recreating the docker network. Needs
# sudo; run this directly, not via Claude/any non-interactive agent,
# since it can't supply your password.
#
# Symptom if this rule is missing: real client<->client traffic can
# bypass aom-lobby's own pairRelay entirely via an accidental direct-
# reachability path on the shared docker bridge - confirmed live
# 2026-08-14 (see CLAUDE.md) to mask a genuine gap in aom-lobby's own
# peer-notification logic, producing exactly the kind of "client B stuck
# on Attempting to Connect" symptom this script is meant to fix.
# When run non-interactively (no TTY on stdin - e.g. invoked by Claude
# via its Bash tool rather than typed directly into a terminal), `sudo`
# has nowhere to prompt for a password and just fails. Instead of
# failing silently in that case, pop open a real terminal window on this
# machine's own desktop ($DISPLAY) running this same script, so the
# prompt lands somewhere you can actually type into it - then exit
# immediately rather than blocking, since there's nothing further to do
# from this (non-interactive) invocation. A directly-typed, interactive
# run skips all of this and just executes normally below.
if [ ! -t 0 ] && [ -z "${FIX_CLIENT_ISOLATION_IN_TERMINAL:-}" ]; then
    TERMINAL=""
    for candidate in x-terminal-emulator gnome-terminal konsole xterm; do
        if command -v "$candidate" >/dev/null 2>&1; then
            TERMINAL="$candidate"
            break
        fi
    done
    if [ -z "$TERMINAL" ]; then
        echo "No terminal emulator found on this machine ($DISPLAY) to open" >&2
        echo "for the sudo prompt - run this script directly instead." >&2
        exit 1
    fi
    SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
    INNER="FIX_CLIENT_ISOLATION_IN_TERMINAL=1 '$SCRIPT_PATH'; echo; echo 'Press Enter to close.'; read"
    echo "Opening a terminal on your desktop for the sudo prompt..."
    case "$TERMINAL" in
        xterm|konsole)
            DISPLAY="${DISPLAY:-:0}" "$TERMINAL" -e bash -c "$INNER" >/dev/null 2>&1 &
            ;;
        *)
            # gnome-terminal and x-terminal-emulator (which resolves to
            # gnome-terminal.wrapper on this machine) both take `--`.
            DISPLAY="${DISPLAY:-:0}" "$TERMINAL" -- bash -c "$INNER" >/dev/null 2>&1 &
            ;;
    esac
    disown
    exit 0
fi

set -euo pipefail

if ! minikube status >/dev/null 2>&1; then
    echo "minikube is not running - start it first." >&2
    exit 1
fi

MINIKUBE_IP="$(minikube ip)"
CLIENT_SUBNET="$(docker network inspect minikube --format '{{(index .IPAM.Config 0).Subnet}}')"

echo "[*] Ensuring client<->client isolation iptables rule on the physical"
echo "    host (real players can't reach each other directly either). May"
echo "    prompt for your sudo password..."
if sudo iptables -C DOCKER-USER -s "$CLIENT_SUBNET" -d "$CLIENT_SUBNET" -j DROP 2>/dev/null; then
    echo "    (client<->client isolation already in place)"
else
    # Inserted bottom-to-top (each -I with no index lands at position 1)
    # so the final top-to-bottom order is: allow node/lobby traffic in
    # both directions, THEN drop whatever's left of the client subnet -
    # i.e. exactly client<->client, never client<->lobby.
    sudo iptables -I DOCKER-USER -s "$CLIENT_SUBNET" -d "$CLIENT_SUBNET" -j DROP
    sudo iptables -I DOCKER-USER -d "$MINIKUBE_IP" -j ACCEPT
    sudo iptables -I DOCKER-USER -s "$MINIKUBE_IP" -j ACCEPT
    echo "    applied"
fi
