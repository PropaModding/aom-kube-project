# AoM Kube Project

## What this is
Kubernetes/Docker setup to modernise Age of Mythology (original) network hosting,
spoofing DirectPlay 8 packets to support modern multiplayer hosting environments.

## Stack
- Docker / docker-compose
- Quilkin (UDP proxy)
- Bash scripts (run-aom-head.sh)

## Key files
- `dockerfile` — main container image
- `docker-compose.yaml` — service orchestration
- `run-aom-head.sh` — headless AoM launch script

## Goals
- Proxy UDP traffic on DirectPlay ports (2300-2400)
- Spoof packets so AoM lobby sees a valid host
- Run headlessly in a Kubernetes pod

## Don't touch
- (add anything you want Claude to leave alone)