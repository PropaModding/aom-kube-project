# aom-lobby status API: hosts waiting for players

`aom-lobby` serves small HTTP endpoints alongside its DirectPlay8 UDP
relay, so you can check from a shell how many hosts are currently open
and waiting for players to join, without launching AoM's own client to
look.

## Usage

```
curl http://$(minikube ip):8080/hosts
curl http://$(minikube ip):8080/waiting
```

`/hosts` returns a single integer, newline-terminated (e.g. `1`) — the
number of hosts currently answering as open. `/waiting` returns `true` or
`false` — whether there's currently a game with a player (the host)
waiting for an opponent, i.e. `/hosts` != 0. It's the signal matchmaking
will use to decide whether a newly-connecting client can be routed into
an existing game rather than provisioning a new pod (`hostProbe.
hasWaitingGame()` in `lobby/main.go`) — round-robin across *multiple*
waiting games is future work, see "Current scope" below. Since
`aom-lobby` runs with `hostNetwork: true`, port 8080 is reachable
directly at the minikube node's IP, same as UDP 2299/2300 — no separate
`kubectl port-forward` needed, no auth.

## How it decides "open"

`aom-lobby` doesn't just check whether the backend pod is reachable — it
asks the same question a real client's LAN browse screen would. Every 3
seconds, it sends the exact 9-byte `0x25` "enumerate hosts" query AoM's
own client broadcasts (see `docs/directplay8-protocol.md`) straight to
the configured backend's discovery port, and waits up to 1 second for a
`0x26` reply. A host only answers that query while it's actually sitting
in an open, joinable lobby screen — mid-match or pre-hosting, there's no
reply, so the count correctly drops to 0. This is implemented in
`hostProbe` in `lobby/main.go`.

## Current scope: one backend, so 0 or 1

Today `aom-lobby` relays to a single static backend
(`aom-headless-game`, see `k8s/lobby-deployment.yaml`), so `/hosts`
only ever returns `0` or `1`, and `hasWaitingGame()`/`/waiting` just
mirrors that same single probe's state. Per CLAUDE.md's Architecture
intention, matches will eventually be provisioned on demand across
multiple `aom-headless` pods with the lobby routing between them — at
that point `hostProbe` becomes a set of probes (one per known backend)
summed together for `/hosts`, and matchmaking will need to pick a
*specific* waiting probe to route a client into (round-robin or
otherwise) rather than just a yes/no across all of them — that's why
`hasWaitingGame()` is kept as its own method on `hostProbe` rather than
folded into `count()`. Nothing about the `/hosts`/`/waiting` APIs
themselves needs to change for that; only what feeds them does.

## Config

| Env var | Default | Meaning |
|---|---|---|
| `STATUS_LISTEN_ADDR` | `:8080` | Address the status HTTP server binds |

Probe interval (3s) and per-probe timeout (1s) are currently constants
in `lobby/main.go` (`probeInterval`, `probeTimeout`), not env-configurable
— revisit if a deployment needs different tuning.
