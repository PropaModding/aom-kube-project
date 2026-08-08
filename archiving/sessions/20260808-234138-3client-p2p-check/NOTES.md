# 3-client peer-to-peer check (2026-08-08)

## Purpose

Determine whether AoM's DirectPlay session is genuinely client<->client
peer-to-peer once more than one real player is in a match, or whether all
traffic actually routes through the host. This came up because
`aom-lobby` only proxies client<->host today - if clients also address
each other directly, that's a real architecture gap in the proxy (it has
no way to relay traffic it never sees), not the routing bug fixed
earlier the same day (see `docs/directplay8-protocol.md`'s
`relayBackendInitiated` writeup).

## Setup

Three plain (non-`hostNetwork`, non-proxied) docker containers on the
`minikube` bridge network, no `aom-lobby`/k8s involved at all -
`run-aom-verbose-3clients.sh`:

- `aom-verbose3-host` — 192.168.49.3
- `aom-verbose3-clienta` — 192.168.49.4
- `aom-verbose3-clientb` — 192.168.49.5

Each container ran its own `tcpdump -i eth0` (full byte-level capture,
its own vantage point - see the script's header comment for why that's
required to see clienta<->clientb traffic at all: two sibling containers
on the same bridge don't flood a third port that isn't part of the
conversation).

## What actually happened (manual, via the GUI on all three windows)

1. Hosted from `aom-verbose3-host`, Players bumped to 3.
2. `clienta` Direct-Connected and joined.
3. `clientb` Direct-Connected and joined.
4. Host switched to Observer Mode, pressed Ready.
5. Match started (all three player slots ready).
6. One client resigned mid-match.
7. Captures stopped, pcaps pulled from the (by-then-exited, exit code 0 -
   clean, not crashed) containers via `docker cp`.

## Finding: confirmed genuine peer-to-peer

`clienta-capture.pcap`, filtered to `host 192.168.49.5 and udp port
2300` (i.e. only clienta<->clientb traffic, nothing involving the host),
shows real two-way traffic - not just the ARP/broadcast noise you'd
expect if they never actually spoke:

```
IP 192.168.49.4.2300 > 192.168.49.5.2300: UDP, length 40   (clienta -> clientb)
IP 192.168.49.5.2300 > 192.168.49.4.2300: UDP, length 40   (clientb -> clienta)
IP 192.168.49.5.2300 > 192.168.49.4.2300: UDP, length 95   (player-announce-shaped)
...
```

Same port (2300), same message shapes/sizes already reverse-engineered
for client<->host traffic in `docs/directplay8-protocol.md` (40-byte
self/peer handshake, 10/24-byte heartbeat/settings-sync, a 95-byte
player-announce-shaped message). **This is the same 4-step handshake +
settings-sync protocol, just running directly between two clients
instead of between a client and the host.** Every pair of connected
peers in a match independently negotiates its own session with each
other, not just each client with the host.

Length distribution across all three capture files backs this up -
clienta<->clientb traffic has essentially the same size profile as
host<->clienta and host<->clientb (dominated by 10/12/16-byte
heartbeat-shaped messages, with the same rarer 40/95/122/139-byte
handshake and settings-sync messages mixed in). Once the match started,
some new, not-yet-decoded sizes appear on all pairings (e.g. 128, 49, 39
bytes) - presumably in-game order/sync traffic, out of scope for this
check but worth knowing about for a future capture.

**Not captured this time**: the documented 3-byte `01 <connID>`
resign/leave burst (see `docs/directplay8-protocol.md`'s "Client
resign/leave signature" section) doesn't appear anywhere in any of the
three pcaps, despite one client resigning mid-match per the manual
sequence above. Unclear whether that's a capture-timing issue (tcpdump
stopped just before/after it), a different resign path (menu/quit vs.
the in-game Resign command used in the original 2026-08-03 3-player
capture), or genuinely different behavior with 2 remaining players vs.
that capture's 3v3. Worth another targeted look if the resign signature
specifically becomes relevant again.

## Files

- `host-winedebug.log`, `clienta-winedebug.log`, `clientb-winedebug.log`
  — Wine DirectPlay/winsock call traces (channel set documented in
  `run-aom-verbose-3clients.sh`; deliberately not cranked to `+relay` -
  see that script's header comment for why).
- `host-capture.pcap`, `clienta-capture.pcap`, `clientb-capture.pcap` —
  full raw packet bytes from each container's own interface.

## Implication for `aom-lobby`

The proxy needs to become a genuine per-match N-way router - every real
endpoint (host and every client) needs to believe every *other* endpoint
is at the proxy's address, and the proxy needs to relay based on which
real pair a given packet's DirectPlay connection ID belongs to, not
just the current client<->host 1:1 relay. See the discussion in this
session for the proposed approach; not yet implemented.
