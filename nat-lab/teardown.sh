#!/bin/bash
# Tears down everything setup.sh creates. Deleting the namespaces alone
# is enough - Linux automatically removes any veth/iptables state that
# only existed inside them, so there's nothing else to clean up
# separately.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "Needs root - run with sudo." >&2
    exit 1
fi

for ns in aom-client-ns aom-router-ns aom-internet-ns; do
    if ip netns list | grep -q "^$ns"; then
        ip netns del "$ns"
        echo "[*] Removed $ns"
    else
        echo "[*] $ns not present, skipping"
    fi
done
