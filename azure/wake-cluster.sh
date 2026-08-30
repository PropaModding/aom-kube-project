#!/bin/bash
# Symmetric counterpart to `emergency-stop.sh cluster` - the "turn it
# back on" half of a low-activity switch. Not really "emergency"
# specific despite living next to that script: this is meant for
# routine day-to-day use (stop the cluster whenever you're not actively
# testing, wake it back up when you are) to keep this test project's
# Azure spend down, not just for something-went-wrong situations.
#
# Usage:
#   ./wake-cluster.sh
#
# `az aks start` reprovisions the node pool's VMs and brings the
# control plane back up - takes a few minutes, same order of magnitude
# as stopping. Everything Terraform manages (Deployments, the
# LoadBalancer Service, the static public IP) was left defined the
# whole time, so nothing needs to be re-applied - the cluster comes
# back exactly as it was stopped, replica counts and all.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RG="$(cd "$SCRIPT_DIR" && terraform output -raw resource_group_name)"
CLUSTER="$(cd "$SCRIPT_DIR" && terraform output -raw aks_cluster_name)"

echo "[*] Starting AKS cluster $CLUSTER (reprovisions node VMs, resumes"
echo "    compute billing)..."
az aks start --name "$CLUSTER" --resource-group "$RG"
echo "[*] Done. kubectl should work again now - give the node pool a"
echo "    minute to report Ready:"
echo "      kubectl get nodes -w"
echo "[*] To pause it again when you're done testing:"
echo "      ./emergency-stop.sh cluster"
