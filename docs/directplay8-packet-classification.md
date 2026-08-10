# Packet classification reference: what we know vs. official DirectPlay 8

Written 2026-08-10 to give the next capture something concrete to be
checked against, rather than re-deriving everything from raw bytes each
time. Two source layers, kept distinct because they are **not** the same
protocol:

1. **Our own reverse-engineering** (`docs/directplay8-protocol.md`) — byte
   offsets confirmed against real captures of `aomxnocd1.exe`.
2. **Microsoft's official DirectPlay 8 Open Specifications**
   (`docs/directplay8-reference/`, gitignored, not checked in) — `[MC-DPL8CS]`
   (Core and Service Providers) and `[MC-DPL8R]` (Reliable).

**Important caveat, confirmed 2026-08-10**: AoM (2002) uses **classic
DirectPlay** (Wine modules `dpwsockx.dll`/`dplayx.dll`), not the real
DirectPlay 8 API (`dpnet.dll`) the `[MC-DPL8CS]`/`[MC-DPL8R]` documents
describe — DirectPlay 8 shipped with DirectX 8 (2000), and AoM's classic
DirectPlay predates it. The **wire byte layout in these official docs does
not match our captured bytes** (compare `dwPacketType` values like
`0x000000C1` against our observed `00 00`/`03 00`/`0x26`/`0x29` type
bytes — no overlap). What *does* carry over is almost certainly the
**conceptual message sequence** — Microsoft evolved DirectPlay's design
across versions rather than reinventing it, and the sequence below lines
up suspiciously well with what we've independently reverse-engineered
from raw captures. Treat every mapping below as a *hypothesis born from a
real structural match*, not a confirmed fact — validate byte-for-byte
against a real capture before trusting it blindly.

## Confirmed message types (our own captures, `docs/directplay8-protocol.md`)

| Wire shape | Direction | Meaning | Confidence |
|---|---|---|---|
| `0x25`, 9 bytes, discovery port | client→host, broadcast | Enumerate hosts query | Confirmed |
| `0x26`, discovery port | host→client | Enum-hosts reply, embeds host's `sockaddr_in` x2 @ offsets 5/21 | Confirmed |
| `0x21`/`0x20`, discovery port | host↔client ping | Same 41-byte shape as `0x26` minus name, embeds address @ 5/21 | Confirmed |
| `0x00`, 40 bytes, session port | either→either | "self/peer" open — self @ offset 8, peer @ offset 24, both 16-byte `sockaddr_in` | Confirmed |
| `0x02`, 40 bytes, session port | either→either | Handshake ack — echoes sender IDs, no address | Confirmed |
| `03 00 <seq> <conn-id>` (6-byte wrapper) | either→either | Settings-sync layer wrapper, everything below nests in this | Confirmed |
| → sub-type player-announce (93 bytes) | either→either | Self-announce: GUID + nickname | Confirmed |
| → sub-type map-name (139 bytes) | host→joiner | Map/scenario filename | Confirmed |
| → sub-type `0x29` (51 bytes) | host→**existing** peer | New-peer broadcast: embeds new peer's address @ offsets 13/29 (absolute, not "after 8-byte wrapper" as originally miswritten) | Confirmed, offsets corrected 2026-08-10 |
| → sub-type chat (41 bytes) | either→either | Lobby chat text, UTF-16LE | Confirmed |
| `07ff`, 10 bytes | either→either | Heartbeat/ack for the settings-sync layer | Confirmed |
| `01 <conn-id>`, 3 bytes, burst of 10 | client→host | Resign/leave signal | Confirmed (client→host only; client→client unconfirmed) |

## Hypothesized mapping to official DP8 concepts (unconfirmed for classic DirectPlay)

From `[MC-DPL8CS]` §3.1.5.2 "Peer-to-Peer Connect Sequence" (Figure 6) —
copied here because it's the clearest documented equivalent of what we're
reverse-engineering blind:

```
Connecting Peer (B)          Host                    Connected Peer (A)
      |--DN_INTERNAL_MSG_PLAYER_CONNECT_INFO-->|                |
      |<--------DN_SEND_CONNECT_INFO------------|                |
      |                                          |--DN_ADD_PLAYER-->|   (*)
      |---------DN_ACK_CONNECT_INFO------------>|                |
      |<--------DN_INSTRUCT_CONNECT-------------|--DN_INSTRUCT_CONNECT-->|  (**)
      |<==============DN_SEND_PLAYER_DPNID (both directions)===========>|
```

(*) `DN_ADD_PLAYER`: "sent from the host, instructs peers to add the
specified peer to the game session" — carries the new peer's name-table
entry, which includes a `url` field (an addressing structure). **This is
almost certainly the conceptual equivalent of our confirmed `0x29`
sub-message** — same trigger condition (new peer joining, sent to
existing peers only), same payload shape (new peer's identity +
address).

(**) `DN_INSTRUCT_CONNECT`: **sent to BOTH the connecting peer (B) AND
the existing peer (A) simultaneously** — "instructs a peer to connect to
a designated peer." This is the critical detail our own docs missed:
the sequence is **not** "A learns B's address and reaches out while B
passively waits" — both sides get an explicit instruction to connect.
`DN_INSTRUCT_CONNECT` itself only carries a `dpnid` (numeric player ID),
not an address — meaning **B is expected to already know A's address
from an earlier exchange** (most likely: `DN_SEND_CONNECT_INFO`, sent to
B when it first joins, "MUST contain the current game session state...
[with] entries in the DN_NAMETABLE_ENTRY_INFO message... for each player
connected to the game session" — i.e. B's *initial* join reply already
carries every existing peer's address, not just the host's).

### What this means for `aom-lobby` — an unexamined gap

If this mapping holds even loosely: **we have only ever rewritten the
`0x29`-equivalent message (host→A, telling A about B). We have never
rewritten whatever earlier message tells B about A's address** — which,
per this sequence, is very likely embedded in B's *own* initial
session-establishment reply from the host (the same class of message our
`0x26`/handshake rewriting already touches for the host's own address,
but not for *other peers'* addresses that might ride along in the same
or an adjacent message). If B is independently, actively trying to
connect to A using a stale/wrong address (most likely the same
`PUBLIC_IP:2300` collision bug already found and fixed for the `0x29`
case), **that would explain the hang independent of anything wrong with
our new pair-relay** — B would be trying to reach an entirely different,
wrong place, never touching the relay at all.

**Next capture should specifically check**: does B ever attempt an
active outbound connect (not just a reactive reply) toward *any*
address associated with A, and if so, what address, sent when, relative
to when B's own join with the host completes?

## Expected handshake map for the next capture (pseudo-IPs matching current test setup)

Roles: **Host** = aom-headless pod (via aom-lobby's session backend),
**Lobby** = aom-lobby (hostNetwork, one address for everything it
proxies), **A** = first-joined real client, **B** = second-joined real
client.

| # | From | To | Port | What | Confirmed today? |
|---|---|---|---|---|---|
| 1 | A | Lobby | 2299 | Discovery query (`0x25`) | Yes |
| 2 | Lobby→Host→Lobby | — | 2299 | Lobby relays, rewrites `0x26` reply's embedded host addr → Lobby:2300 | Yes |
| 3 | A | Lobby | 2300 | Session handshake (`0x00`) self=A's real addr, peer=Lobby's addr | Yes |
| 4 | Lobby↔Host | — | 2300 | Lobby rewrites self→Lobby:2300, peer→Host's real addr, both directions | Yes |
| 5 | B | Lobby | 2299 | B's discovery query | Yes |
| 6 | B | Lobby | 2300 | B's session handshake, same rewrite as #3/4 | Yes |
| 7 | Host→Lobby→A | — | 2300 | `0x29` new-peer broadcast, Lobby rewrites embedded addr → dedicated pair-relay port (`PUBLIC_IP:P3`) | Yes (2026-08-10 fix) |
| 8 | **Host→Lobby→B** | — | 2300 | **Unconfirmed/unexamined**: whatever message tells B about A's address (hypothesized: part of B's own `DN_SEND_CONNECT_INFO`-equivalent reply, i.e. B's *own* session handshake/name-table reply from the host) — **not currently rewritten at all** | **No — this is the gap** |
| 9 | A | Lobby:P3 | P3 (dynamic) | A's outbound peer-connect attempt, relayed to B's real addr | Yes, delivered correctly (tcpdump-confirmed) |
| 10 | **B** | **?** | **?** | **B's own outbound peer-connect attempt toward A** — never observed in any capture so far. Per the DP8 sequence, this should exist and fire independently of #9, not merely as a reply to it. | **No — not seen at all, in either direction, in any capture yet** |

Row 10 is the single biggest open question. Every capture so far has
looked for B's *reply* to A's traffic; none has specifically looked for
B's *own independent outbound attempt* toward whatever address B
believes A is at. That's what the next capture needs to isolate.
