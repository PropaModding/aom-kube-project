#!/bin/bash
# Builds the headless AoM image straight into minikube's own Docker daemon
# (no registry involved, matches imagePullPolicy: Never in the k8s
# manifests) and applies both the host (aom-headless) and client
# (aom-client) pods from k8s/. Re-run any time dockerfile.k8s, entrypoint.sh,
# or the k8s manifests change.
#
# Usage: ./deploy-minikube.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_TAG="aom-k8s:latest"

if ! minikube status >/dev/null 2>&1; then
    echo "[*] minikube is not running, starting it..."
    minikube start
fi

echo "[*] Pointing docker CLI at minikube's daemon..."
eval "$(minikube docker-env)"

echo "[*] Building $IMAGE_TAG from dockerfile.k8s..."
docker build -f "$SCRIPT_DIR/dockerfile.k8s" -t "$IMAGE_TAG" "$SCRIPT_DIR"

echo "[*] Applying k8s manifests..."
kubectl apply -f "$SCRIPT_DIR/k8s/aom-headless-deployment.yaml"
kubectl apply -f "$SCRIPT_DIR/k8s/aom-client-deployment.yaml"

echo "[*] Waiting for rollout..."
kubectl rollout status deployment/aom-headless --timeout=180s
kubectl rollout status deployment/aom-client --timeout=180s

echo
echo "Deployed. To view either pod over VNC:"
echo "  kubectl port-forward svc/aom-headless-vnc 5901:5900   # then vncviewer localhost:5901"
echo "  kubectl port-forward svc/aom-client-vnc  5902:5900   # then vncviewer localhost:5902"
