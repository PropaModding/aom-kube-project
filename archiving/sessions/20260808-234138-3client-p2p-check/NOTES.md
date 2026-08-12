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
session for the proposed approach - implemented 2026-08-10/2026-08-11,
see `docs/multi-peer-routing-design.md`'s "Status" line and
`lobby/main.go`.

## Example packets (added 2026-08-12)

Concrete instances of every message type these pcaps are referenced for
elsewhere (`docs/directplay8-protocol.md`,
`docs/directplay8-packet-classification.md`, `lobby/main.go`'s doc
comments), with epoch timestamps (`frame.time_epoch` via `tshark -r
<file> -T fields -e frame.time_epoch ...`) so they can be cross-checked
against `clicks.log`-style epoch logs from other captures, or re-pulled
from the pcaps directly. All times below are from `host-capture.pcap`
unless noted; roles per the Setup section above (host=.3, clienta=.4,
clientb=.5).

| Epoch | Dir | Type | Bytes | Notes |
|---|---|---|---|---|
| 1786196564.959576 | clienta→bcast | `0x25` enumerate-hosts query | 9 | `25 00 00 00 00 00 00 00 00`, constant - matches `lobby/main.go`'s `discoveryQuery` exactly |
| 1786196564.969803 | host→clienta | `0x26` discovery reply | 65 | Embeds host's address at offsets 5/21 (`lobby/main.go`'s `discoveryAddressOffsets`) - 65 here, not the 67 quoted elsewhere, since length varies with session-name string length |
| 1786196627.366256 | clienta→host | `0x20` client ping | 41 | Same embedded-address shape as `0x26` minus the trailing name |
| 1786196627.398726 | host→clienta | `0x21` ping reply | 41 | |
| 1786196627.502457 | host→clienta | type `0x00` session handshake ("open") | 40 | `sessionSelfOffset`/`sessionPeerOffset` in `lobby/main.go` |
| 1786196627.654138 | host→clienta | `07ff` heartbeat | 10 | |
| 1786196628.204838 | host→clienta | session-name echo, undecoded sub-type | 45 | `03 00 <seq> <connid> 23 00 01 0d 00 0c 00 00 00 "host's Game"` - not yet formally catalogued anywhere; seen again in the 2026-08-12 live session under the same shape, worth a name if picked up again |
| 1786196628.205017 | host→clienta | `0x29` new-peer broadcast, **empty variant** | 51 | Sub-header `15 01 00 00 00` (not the usual `1c 01 00 00 00`), embedded sockaddr all-zero (family=2, port=0, addr=0.0.0.0) - fires the moment clienta joins, before clientb exists to report. **Confirms this empty-variant shape is genuine protocol behavior**, not a proxy artifact - the same shape showed up independently in a 2026-08-12 live proxied test before being dismissed as possibly-stale capture data; it isn't, it's real. See `lobby/main.go`'s `isNewPeerBroadcast`. |
| 1786196628.223645 | host→clienta | map-name sub-message | 139 | `docs/directplay8-protocol.md`'s "Post-handshake session-settings sync" section |
| 1786196628.331592 | clienta→host | player-announce sub-message | 95 | GUID `{E9831C49-1FB0-488B-B4EE-261D1CC69463}`, nickname `client1` - 95 bytes here (not the 93 quoted elsewhere - nickname-length-dependent, as already flagged in `docs/directplay8-protocol.md`) |
| 1786196650.403037 | host→clientb | session-name echo | 45 | Same shape as the 628.204838 entry, this time host→clientb |
| 1786196650.403116 | host→**clientb** | **`existingPeerBroadcast`, full/valid variant** | 87 | Embeds clienta's real address (`192.168.49.4:2300`) at offsets 49/65 - the message found by re-examining this exact capture on 2026-08-10/2026-08-12 that closes the "new peer never learns the existing peer's address" gap. See `lobby/main.go`'s `existingPeerBroadcast` doc comment. |
| 1786196650.403201 | host→clienta | `0x29` new-peer broadcast, **full/valid variant** | 51 | Embeds clientb's real address (`192.168.49.5:2300`) at offsets 13/29, sub-header `1c 01 00 00 00` - the "real" follow-up to the empty one at 628.205017, fired ~22s later once clientb has actually joined |
| 1786196650.705389 | clientb→host | player-announce sub-message | 95 | GUID `{05153EBB-24BB-4FAE-9005-564EBD4E928D}`, nickname `client2` |
| 1786196672.325925 | host→clienta | ready-toggle, **host→client direction** | 22 | Bool@19=1 (ready). Confirms `readyToggle` is not client→host-only as `lobby/main.go`'s current doc comment claims - the host also sends/echoes this shape to clients. Same timestamp (µs apart) as the clientb copy below, suggesting a host-driven broadcast rather than two independent client actions |
| 1786196672.326314 | host→clientb | ready-toggle, host→client direction | 22 | Bool@19=1, same event as above |
| 1786196677.023693 | clienta→host | ready-toggle, client→host direction | 22 | Bool@19=1 - the direction `lobby/main.go`'s `isReadyToggle` currently watches for |
| 1786196679.190833 | clientb→host | ready-toggle, client→host direction | 22 | Bool@19=1 |
| 1786196687.509557 | host→clienta | short map-name variant, undecoded sub-type | 69 | `03 00 <seq> <connid> 32 00 01 08 02 03 03 ...` then UTF-16LE `"Mediterranean.xs"` - a second, shorter map-related message distinct from the 139-byte one above; not yet formally catalogued |
| 1786196734.696102 (×10, ~60µs apart) | host→clienta | `01 <connID>` resign/leave burst | 3 | `01 bc 66` - **this capture DOES contain the resign burst after all**, contradicting this file's original "Not captured this time" note below. Direction is host→clienta (not client→host as `docs/directplay8-protocol.md`'s "Client resign/leave signature" section documents from an earlier capture) - worth reconciling if the resign signature becomes relevant again, since `lobby/main.go`'s `isResignBurst` currently only watches the client→host direction on `sessionProxy` |

The "Not captured this time" paragraph in the "Finding" section above
predates this table and is now known to be wrong on that specific point
- left as-is for the historical record rather than rewritten, per this
project's usual practice of appending corrections rather than silently
editing past findings.
