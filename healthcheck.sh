#!/bin/sh
# Kubernetes livenessProbe.exec target for the aom-headless Deployment
# (see k8s/aom-headless-deployment.yaml). Asks this node's
# host-health-agent (see docs/host-health-probe-design.md and
# host-health-agent/main.go) whether *this specific pod* has actually
# reached a hosted, discoverable state - the real signal, not just
# "auto-host.sh exited 0" (which it does even when it's misclicked onto
# the wrong menu screen - the bug this exists to catch).
#
# NODE_IP and POD_NAME come from the downward API (see the Deployment's
# env block) - not hardcoded, since both are only known once the pod is
# actually scheduled.
#
# Deliberately fails OPEN (exit 0) on anything except an explicit
# "not hosting" (HTTP 503) from the agent itself. A curl timeout, the
# agent being briefly unreachable, or this pod not yet being known to it
# (HTTP 404 - normal for the first few seconds of a pod's life, well
# before auto-host.sh's own sequence, which alone takes over a minute,
# could possibly have finished) must never read as *this pod* being
# broken - only a sustained, explicit "not hosting" should ever trigger
# a restart. See the design doc's own fail-open reasoning: the opposite
# choice would mean one host-health-agent hiccup restarts every host in
# the pool simultaneously.
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 \
  "http://${NODE_IP}:8090/host-health?id=${POD_NAME}")

case "$code" in
  200) exit 0 ;; # confirmed hosting
  503) exit 1 ;; # agent explicitly says not hosting - real failure
  *)   exit 0 ;; # 404 (not yet known), curl error, timeout, agent down - fail open
esac
