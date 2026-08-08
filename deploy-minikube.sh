#!/bin/bash
# Builds the headless AoM image and the aom-lobby image straight into
# minikube's own Docker daemon (no registry involved, matches
# imagePullPolicy: Never in the k8s manifests) and applies the host
# (aom-headless), client (aom-client), lobby (aom-lobby), and quilkin
# manifests from k8s/. Re-run any time dockerfile.k8s, dockerfile.lobby,
# entrypoint.sh, or the k8s manifests change.
#
# Usage: ./deploy-minikube.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_TAG="aom-k8s:latest"
LOBBY_IMAGE_TAG="aom-lobby:latest"

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

echo "[*] Applying k8s manifests..."
kubectl apply -f "$SCRIPT_DIR/k8s/aom-headless-deployment.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/aom-client-deployment.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/lobby-deployment.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/quilkin-deployment.yaml"

echo "[*] Waiting for rollout..."
kubectl rollout status deployment/aom-headless --timeout=180s
kubectl rollout status deployment/aom-client --timeout=180s
kubectl rollout status deployment/aom-lobby --timeout=60s
# quilkin is parked at replicas: 0 for now (see k8s/quilkin-deployment.yaml)
# - nothing to configure or wait on until it's scaled back up.

MINIKUBE_IP="$(minikube ip)"
echo "[*] Pointing aom-lobby at this cluster's real address ($MINIKUBE_IP)..."

# SESSION_BACKEND_ADDR is pointed at the pod's own IP directly rather than
# the aom-headless-game Service's ClusterIP - the session proxy's
# backend-initiated-relay logic (lobby/main.go, see clientTracker) needs to
# recognize unsolicited UDP 2300 traffic by comparing its source address
# against this exact value, and the pod's own outbound sends carry its real
# IP as source, not the ClusterIP (that DNAT rewrite only applies to
# traffic addressed *to* the ClusterIP, not traffic the pod itself
# initiates). Re-run this script if the pod restarts and gets a new IP.
AOM_POD_IP="$(kubectl get pod -l app=aom-headless -o jsonpath='{.items[0].status.podIP}')"
if [ -z "$AOM_POD_IP" ]; then
    echo "Could not find aom-headless pod IP - is it running?" >&2
    exit 1
fi
echo "[*] Pointing aom-lobby's session backend at the pod directly ($AOM_POD_IP:2300)..."
kubectl set env deployment/aom-lobby PUBLIC_ADDR="$MINIKUBE_IP" SESSION_BACKEND_ADDR="$AOM_POD_IP:2300"
kubectl rollout status deployment/aom-lobby --timeout=60s

echo
echo "Deployed. To view either game pod over VNC:"
echo "  kubectl port-forward svc/aom-headless-vnc 5901:5900   # then vncviewer localhost:5901"
echo "  kubectl port-forward svc/aom-client-vnc  5902:5900   # then vncviewer localhost:5902"
echo
echo "To Direct-Connect from outside the cluster (e.g. run-aom-spoofed-client.sh),"
echo "point AoM's Direct-IP field at: $MINIKUBE_IP"
echo "(aom-lobby answers on both 2299 and 2300 there, relaying + rewriting to aom-headless directly)"
