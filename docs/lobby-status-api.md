# aom-lobby status API: hosts waiting for players

`aom-lobby` serves small HTTP endpoints alongside its DirectPlay8 UDP
relay, so you can check from a shell how many hosts are currently open
and waiting for players to join, without launching AoM's own client to
look.

## Usage

```
curl http://$(minikube ip):8080/hosts
curl http://$(minikube ip):8080/waiting
curl http://$(minikube ip):8080/full
```

`/hosts` returns a single integer, newline-terminated (e.g. `1`) — the
number of hosts currently answering as open. `/waiting` returns `true` or
`false` — whether there's currently at least one game with a player (a
host) waiting for an opponent, i.e. `/hosts` != 0. It's the signal
matchmaking uses to decide whether a newly-connecting client can be
routed into an existing game rather than a fresh one — see "N-host pool"
below for what "routed into" actually means today (the real 3-tier
"prefer a waiting host" policy, not plain round-robin). Since
`aom-lobby` runs with `hostNetwork: true`, port 8080 is reachable
directly at the minikube node's IP, same as UDP 2299/2300 — no separate
`kubectl port-forward` needed, no auth.

`/full` returns `true` or `false` — whether *every* host currently in the
pool is full (host + 2 real clients, no open slots anywhere) - the
meaningful pool-wide signal now that there can be more than one
concurrent match, ANDed together across every `pool.hostFull(h)` in
`lobby/main.go`. An empty pool (no `aom-headless` pods discovered yet
or at all) also reports `true` - "no capacity anywhere" is accurate
either way, even though the cause differs from every host being staffed.

## How it decides "full"

Unlike `/hosts`/`/waiting`, a single host's "full" isn't a periodic
probe — `hostPool.hostFull(h)` is true if *either* that host's own A<->B
pair relay (`matchState.pair`, see `docs/multi-peer-routing-design.md`)
has been created, *or* its real session count has reached the 2-client
cap (`sessionCountForHost`) - the second check catches the case where
both real clients have connected but the pair relay hasn't been built
yet (a brief window right at pairing time). The pair relay is only ever
built once both real clients have connected to *that host* *and* it has
broadcast each one's address to the other (`ensurePairRelay`'s callers in
`main.go`'s session `rewriteToClient`), so its existence is a direct,
already-computed signal rather than a new one, and it's true exactly
once that host's lobby is genuinely staffed — not merely "a client would
be welcome to browse in," which is all `/hosts`/`/waiting` promise (a
host can keep answering discovery queries for some time after both slots
fill, since that's driven by whether the lobby screen is still open, not
by slot count).

**There is a reset path**: once a real player is detected gone (30s
dual-channel idle timeout, `reapIdleSessions` - see
`docs/multi-peer-routing-design.md`'s Durability discussion),
`matchState.onClientGone` tears the pair relay down and the session count
drops, so `hostFull`/`/full` correctly flip back once a slot is genuinely
free - confirmed live 2026-08-14/15 (a client dropped, was detected gone
~38s later, and a replacement client correctly got routed back to the
same host). The ~30-40s detection lag is real and by design (silence is
the only signal available for a clean quit-to-menu today), not a gap in
the reset logic itself.

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

## N-host pool: `/hosts` can now be more than 1

**Updated 2026-08-13**: `aom-lobby` no longer relays to a single static
backend. It discovers every `aom-headless` pod dynamically via the
Kubernetes API (`podLister` in `lobby/main.go`, polling every 3s, same
cadence as `hostProbe`'s own liveness check) and gives each one its own
independent probe, match state, and pair relay. Adding a host is just:

```
kubectl scale deployment/aom-headless --replicas=2
```

No manifest edits, no `aom-lobby` redeploy - `/hosts` picks up the new
pod within one poll interval once it's self-hosted (every pod now hosts
*itself* on startup via `auto-host.sh`, see `docs/host-flow.md`).

Client-to-host assignment (`hostPool.selectLocked`) follows
`lobby/packet-handling-design.md`'s 3-tier priority: prefer a host
already waiting for a second player, then an empty host, then
ineligible - "fill an open match before starting a new one," round-robin
within whichever tier has candidates. Sticky per real client IP for the
life of that client's connection either way (see that method's own doc
comment - **known gap**: not currently revalidated if the assigned host
is later removed from the pool, see `lobby/session-cleanup-design.md`).
Corrected 2026-08-22, this paragraph was stale: the claim/reservation
race guard turned out to be unnecessary (resolved by construction, see
`lobby/packet-handling-design.md`'s "The race this needs to guard
against"), and the spoofed "lobby full" rejection for when no host is
eligible **is implemented** - see that same doc's "Case 3 in detail"
section.

## Config

| Env var | Default | Meaning |
|---|---|---|
| `STATUS_LISTEN_ADDR` | `:8080` | Address the status HTTP server binds |
| `AOM_HEADLESS_NAMESPACE` | `default` | Namespace `podLister` lists pods in |
| `AOM_HEADLESS_LABEL_SELECTOR` | `app=aom-headless` | Label selector `podLister` lists pods with - must match `k8s/aom-headless-deployment.yaml`'s pod template label |

Probe interval (3s), per-probe timeout (1s), and the pod-list poll
interval (3s, `podPollInterval`) are currently constants in
`lobby/main.go`, not env-configurable — revisit if a deployment needs
different tuning.
