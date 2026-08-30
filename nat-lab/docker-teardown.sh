#!/bin/bash
# Tears down everything docker-setup.sh (and run-client.sh) create.
set -euo pipefail

echo "[*] Stopping containers..."
for c in aom-nat-router aom-nat-client aom-nat-target; do
    if docker ps -a --format '{{.Names}}' | grep -q "^$c\$"; then
        docker stop "$c" >/dev/null 2>&1 || true
        echo "[*] Stopped $c"
    fi
done

echo "[*] Removing networks..."
for n in aom-nat-client-lan aom-nat-internet; do
    if docker network ls --format '{{.Name}}' | grep -q "^$n\$"; then
        docker network rm "$n" >/dev/null 2>&1 || true
        echo "[*] Removed $n"
    fi
done
