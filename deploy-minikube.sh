#!/bin/bash
# Builds the headless AoM image and the aom-lobby image straight into
# minikube's own Docker daemon (no registry involved, matches
# imagePullPolicy: Never in the k8s manifests) and applies the host
# (aom-headless), client (aom-client), lobby (aom-lobby), and lobby RBAC
# manifests from k8s/. Re-run any time dockerfile.k8s, dockerfile.lobby,
# entrypoint.sh, auto-host.sh, or the k8s manifests change.
#
# N-host update (2026-08-13): no longer discovers/pins a single
# aom-headless pod IP into aom-lobby's env - aom-lobby finds every
# matching pod itself via the Kubernetes API (lobby/main.go's podLister,
# granted access by k8s/lobby-rbac.yaml). Scaling past this script's
# default `replicas: 1` is just `kubectl scale deployment/aom-headless
# --replicas=N` afterward - no redeploy needed.
#
# Usage: ./deploy-minikube.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_TAG="aom-k8s:latest"
LOBBY_IMAGE_TAG="aom-lobby:latest"
HEALTH_AGENT_IMAGE_TAG="host-health-agent:latest"

if ! minikube status >/dev/null 2>&1; then
    echo "[*] minikube is not running, starting it..."
    minikube start
fi

echo "[*] Pointing docker CLI at minikube's daemon..."
eval "$(minikube docker-env)"

echo "[*] Building $IMAGE_TAG from dockerfile.k8s..."
docker build -f "$SCRIPT_DIR/dockerfile.k8s" -t "$IMAGE_TAG" "$SCRIPT_DIR"

echo "[*] Building $LOBBY_IMAGE_TAG from dockerfile.lobby..."
docker build -f "$SCRIPT_DIR/dockerfile.lobby" -t "$LOBBY_IMAGE_TAG" "$SCRIPT_DIR"

echo "[*] Building $HEALTH_AGENT_IMAGE_TAG from dockerfile.host-health-agent..."
docker build -f "$SCRIPT_DIR/dockerfile.host-health-agent" -t "$HEALTH_AGENT_IMAGE_TAG" "$SCRIPT_DIR"

echo "[*] Pointing docker CLI back at the host's own daemon (the docker-"
echo "    env eval above redirected it to minikube's internal daemon,"
echo "    which the client-isolation section further below needs to NOT"
echo "    be pointed at - it inspects/execs against the HOST's own"
echo "    'minikube' docker network and node container, not minikube's"
echo "    own nested docker environment)..."
eval "$(minikube docker-env --unset)"

echo "[*] Applying k8s manifests..."
kubectl apply -f "$SCRIPT_DIR/k8s/lobby-rbac.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/host-health-agent-rbac.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/host-health-agent-daemonset.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/aom-headless-deployment.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/lobby-deployment.yaml"

echo "[*] Waiting for rollout..."
# host-health-agent first: aom-headless's own livenessProbe calls into it
# (see healthcheck.sh) - it fails open if unreachable, so this isn't
# strictly required for correctness, but starting it first means real
# health data is available from the earliest possible moment rather than
# every pod's first several checks running in the fail-open/unknown
# state.
kubectl rollout status daemonset/host-health-agent --timeout=60s
kubectl rollout status deployment/aom-headless --timeout=180s
kubectl rollout status deployment/aom-lobby --timeout=60s

MINIKUBE_IP="$(minikube ip)"
echo "[*] Pointing aom-lobby at this cluster's real address ($MINIKUBE_IP)..."
kubectl set env deployment/aom-lobby PUBLIC_ADDR="$MINIKUBE_IP"
kubectl rollout status deployment/aom-lobby --timeout=60s

echo "[*] Applying aom-headless egress lockdown (requires --cni=calico; a"
echo "    non-enforcing CNI will silently accept this and do nothing)..."
sed "s/NODE_IP_PLACEHOLDER/$MINIKUBE_IP/" "$SCRIPT_DIR/k8s/aom-headless-netpol.yaml" | kubectl apply -f -

# Host-level rules below (not k8s-enforceable - client containers live on
# the physical host's "minikube" docker bridge network, sibling to the
# minikube node container itself, not as k8s pods). Plain `iptables -I`
# state, not persisted anywhere - lost on host reboot or on `minikube
# delete` recreating the docker network, hence re-applied here on every
# deploy rather than documented as a one-off manual step.
CLIENT_SUBNET="$(docker network inspect minikube --format '{{(index .IPAM.Config 0).Subnet}}')"
BROADCAST_ADDR="${CLIENT_SUBNET%.*}.255"  # assumes a /24 - true for minikube's docker driver default, confirmed via docker network inspect

echo "[*] Ensuring client<->client isolation iptables rule on the physical"
echo "    host (real players can't reach each other directly either). May"
echo "    prompt for your sudo password..."
if sudo iptables -C DOCKER-USER -s "$CLIENT_SUBNET" -d "$CLIENT_SUBNET" -j DROP 2>/dev/null; then
    echo "    (client<->client isolation already in place)"
else
    # Inserted bottom-to-top (each -I with no index lands at position 1)
    # so the final top-to-bottom order is: allow node/lobby traffic in
    # both directions, THEN drop whatever's left of the client subnet -
    # i.e. exactly client<->client, never client<->lobby. This is
    # DOCKER-USER (FORWARD) on the physical host's own netfilter, which
    # is correct here: the host is genuinely bridging/forwarding traffic
    # between two sibling docker containers.
    sudo iptables -I DOCKER-USER -s "$CLIENT_SUBNET" -d "$CLIENT_SUBNET" -j DROP
    sudo iptables -I DOCKER-USER -d "$MINIKUBE_IP" -j ACCEPT
    sudo iptables -I DOCKER-USER -s "$MINIKUBE_IP" -j ACCEPT
fi

echo "[*] Ensuring LAN-broadcast suppression iptables rule INSIDE the"
echo "    minikube node's own network namespace (not the physical host's -"
echo "    aom-lobby runs hostNetwork: true, so its listening socket lives"
echo "    in the minikube container's netns, and that's the only INPUT"
echo "    chain that actually gates delivery to it; a rule on the physical"
echo "    host's own INPUT chain never sees this bridge-local broadcast"
echo "    traffic at all - confirmed live 2026-08-14). No separate sudo"
echo "    needed, just docker exec into the node container as root..."
# Two separate broadcast addresses need blocking, not one: the real AoM
# client's LAN-browse query goes to 255.255.255.255 (the literal "limited
# broadcast" address, INADDR_BROADCAST) rather than computing this
# subnet's own directed broadcast (192.168.49.255) - confirmed live
# 2026-08-14 via a real client's own packet capture, after a rule
# covering only the subnet-directed address let it straight through.
for BC_ADDR in "$BROADCAST_ADDR" 255.255.255.255; do
    if docker exec minikube iptables -C INPUT -d "$BC_ADDR" -p udp --dport 2299 -j DROP 2>/dev/null; then
        echo "    (broadcast suppression for $BC_ADDR already in place)"
    else
        docker exec minikube iptables -I INPUT -d "$BC_ADDR" -p udp --dport 2299 -j DROP
    fi
done

echo
echo "Deployed. Every aom-headless pod self-hosts into a joinable lobby on"
echo "its own startup (auto-host.sh) - no separate hosting step needed."
echo "To scale up the number of hosts:"
echo "  kubectl scale deployment/aom-headless --replicas=2"
echo
echo "To view a specific pod over VNC (e.g. re-calibrating coordinates,"
echo "or watching host-game-kube.sh drive a manual re-host - see"
echo "docs/host-flow.md):"
echo "  kubectl get pods -l app=aom-headless"
echo "  kubectl port-forward pod/<name> 5901:5900   # then vncviewer localhost:5901"
echo "  kubectl port-forward svc/aom-client-vnc  5902:5900   # then vncviewer localhost:5902"
echo
echo "To Direct-Connect from outside the cluster (e.g. run-aom-spoofed-client.sh),"
echo "point AoM's Direct-IP field at: $MINIKUBE_IP"
echo "(aom-lobby answers on both 2299 and 2300 there, relaying + rewriting to whichever aom-headless pod a client is matched to)"
