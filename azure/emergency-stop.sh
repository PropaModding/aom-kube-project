#!/bin/bash
# Fast kill switch for the AKS deployment - use if something's wrong
# (unexpected traffic, a runaway cost signal, anything that means "stop
# serving right now, figure out why after"). Three tiers, cheapest/
# fastest first - see azure/README.md's "Emergency stop" section for
# when to reach for which.
#
# Usage:
#   ./emergency-stop.sh scale     # default: scale lobby+headless to 0
#                                  # replicas. Seconds, not minutes.
#                                  # Cluster keeps running (still costs
#                                  # node compute) but stops serving/
#                                  # processing any traffic immediately.
#   ./emergency-stop.sh cluster   # az aks stop - deallocates the node
#                                  # pool's VMs entirely (stops compute
#                                  # billing), takes a couple of minutes.
#                                  # Everything (deployments, Service,
#                                  # static IP) stays defined - `az aks
#                                  # start` brings it all back exactly
#                                  # as it was.
#   ./emergency-stop.sh nuke      # terraform destroy - tears down
#                                  # EVERYTHING (cluster, ACR, static
#                                  # IP, budget). Only for "shut this
#                                  # down for good, not just pausing it"
#                                  # - asks for typed confirmation, same
#                                  # as terraform destroy always does.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MODE="${1:-scale}"

case "$MODE" in
  scale)
    echo "[*] Scaling aom-lobby and aom-headless to 0 replicas..."
    kubectl scale deployment/aom-lobby --replicas=0
    kubectl scale deployment/aom-headless --replicas=0
    echo "[*] Done. Nothing is serving traffic now - the LoadBalancer"
    echo "    Service and static IP are still defined, so this is a"
    echo "    pause, not a teardown. Bring it back with:"
    echo "      kubectl scale deployment/aom-lobby --replicas=1"
    echo "      kubectl scale deployment/aom-headless --replicas=2"
    ;;
  cluster)
    RG="$(cd "$SCRIPT_DIR" && terraform output -raw resource_group_name)"
    CLUSTER="$(cd "$SCRIPT_DIR" && terraform output -raw aks_cluster_name)"
    echo "[*] Stopping AKS cluster $CLUSTER (deallocates node VMs, stops"
    echo "    compute billing)..."
    az aks stop --name "$CLUSTER" --resource-group "$RG"
    echo "[*] Done. Bring it back with:"
    echo "      az aks start --name $CLUSTER --resource-group $RG"
    ;;
  nuke)
    echo "[*] This tears down EVERYTHING Terraform manages - the"
    echo "    cluster, ACR (and every image in it), the static IP,"
    echo "    Log Analytics, and the budget alert. Not reversible"
    echo "    without re-provisioning and re-pushing images from"
    echo "    scratch."
    (cd "$SCRIPT_DIR" && terraform destroy)
    ;;
  *)
    echo "Usage: $0 [scale|cluster|nuke]" >&2
    exit 1
    ;;
esac
