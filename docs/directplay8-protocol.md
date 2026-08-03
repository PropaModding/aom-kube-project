# DirectPlay8 LAN Protocol Findings

Captured from AoM: Titans (No-CD) running under Wine, hosted from one
container (`run-aom-head.sh`) and browsed-for from a second container on the
same physical machine (`run-aom-client.sh`). Both containers use
`--net=host`, so client/host traffic in this capture travels over `lo`
instead of a real LAN interface — on separate machines the same exchange
would appear on the real NIC in both directions.

Capture method: `record-session.sh` (tcpdump, `CAPTURE_FILTER=udp`) +
`tcpdump -tt -r` / `tcpdump -X` for payload inspection. Source session:
`archiving/sessions/20260802-231930/session.pcap`, 2026-08-02.

## Ports

| Port | Purpose |
|------|---------|
| UDP 2299 | Session discovery (broadcast query / unicast reply, and a ping/liveness pair) |
| UDP 2300 | Actual game session traffic (address advertised inside the discovery reply, not observed directly in this capture) |

`CLAUDE.md`'s original assumption of "ports 2300-2400" was half right — 2300
is real, but discovery itself happens one port lower, on 2299, and wasn't
in that range.

## Discovery is query/response, not announce

A hosted game transmits nothing on its own. It sits listening on 2299. A
client only generates traffic when its LAN browse screen is open, and it's
the client that initiates every exchange:

1. Client broadcasts an "enumerate hosts" query to `255.255.255.255:2299`,
   repeated roughly every 330ms for as long as the browse screen is open.
2. Any listening host replies directly (unicast) to the querying client's
   ephemeral port, also on 2299.
3. Once per browse-screen refresh, client and host also exchange a second,
   different unicast pair on 2299 — looks like a liveness/ping check (see
   below), possibly what drives a ping-time column in the game list.

No traffic occurs at all until a client actively browses. To capture
anything, you need a second client instance actually on the LAN list
screen, not just a host sitting in its lobby.

## Message formats

All three message types share an embedded `sockaddr_in` (Windows layout,
16 bytes, copied byte-for-byte onto the wire):

```
02 00                      sin_family = AF_INET (little-endian uint16)
08 fc                      sin_port   = 2300 (big-endian uint16 = 0x08fc)
c0 a8 01 67                sin_addr   = 192.168.1.103 (dotted-order bytes)
18 c0 4e 06 20 c0 4e 06    sin_zero   = NOT actually zero, meaning unknown
```

The `sin_family`/`sin_port`/`sin_addr` fields decode cleanly and
confidently. The 8 bytes where `sin_zero` should be are consistently
non-zero and vary slightly between messages — flagged as an open question,
not yet decoded.

### Type 0x25 — discovery query (9 bytes, broadcast)

```
25 00 00 00 00 00 00 00 00
```

Constant across every capture. No target/session info — a generic "is
anyone hosting" broadcast.

### Type 0x26 — discovery reply (67 bytes, unicast, host → client)

```
26 01 00 00 00                                          header/type prefix
<sockaddr_in>                                           16 bytes (see above)
<sockaddr_in>                                           16 bytes, duplicate of the above
1a 00 00 00                                              string byte-length = 26
61 00 64 00 6d 00 69 00 6e 00 27 00 73 00 20 00
47 00 61 00 6d 00 65 00 00 00                            UTF-16LE "admin's Game\0"
```

The trailing string is the session name shown in the client's LAN game
list — length-prefixed (uint32, byte count including the UTF-16 null
terminator), then UTF-16LE text.

### Type 0x20 / 0x21 — liveness ping (41 bytes each way, unicast)

Sent once per browse-screen refresh cycle, after the repeating 0x25/0x26
broadcast exchange, directly between client and host (not broadcast):

```
client -> host:  20 01 00 00 00  <sockaddr_in>   (no name string)
host   -> client: 21 01 00 00 00  <sockaddr_in>   (no name string)
```

Same `sockaddr_in` shape as the discovery reply, minus the name. Best
guess: this is what the game uses to compute/display a ping time per
listed entry, independent of the broadcast enumeration.

## Practical implications for the Quilkin proxy / spoofing work

- The `sockaddr_in` embedded in the 0x26 reply and the 0x20/0x21 ping is
  the thing that needs rewriting if traffic is being proxied/NATed —
  it bakes in the real IP:2300 the client is told to connect to next. If
  the host's real address isn't reachable by the client as-is (e.g. across
  a Kubernetes pod boundary), this field has to be rewritten in flight to
  point at the proxy's externally-reachable address instead.
- The message-type byte at payload offset 0 (`0x25`/`0x26`/`0x20`/`0x21`)
  is the cheapest thing to switch on when writing a packet filter/rewriter.
- The `sin_zero` bytes are unexplained — don't assume they're safe to zero
  out or ignore until we understand what varies them.

## Direct IP Connect (in-cluster capture, 2026-08-03)

AoM's "LAN/Direct IP" screen has a second entry point besides browsing: a
"Type a Direct IP" field + Connect button. Captured by hosting from one k8s
pod (`aom-headless`, 10.244.0.16) and Direct-Connecting from another
(`aom-client`, 10.244.0.14) over the pod network — no real LAN/broadcast
domain involved. Source: `archiving/sessions/20260803-direct-connect-k8s/session.pcap`.

**Confirms Direct Connect reuses the exact same 0x25/0x26 messages as LAN
discovery, just unicast instead of broadcast**, from a fresh ephemeral port
(distinct from whatever port the client's background LAN-browse query was
using):

1. Client sends the identical 9-byte `0x25` query, but straight to the
   typed IP on port 2299 instead of `255.255.255.255`.
2. Host replies with the identical 67-byte `0x26` format — same double
   `sockaddr_in`, same length-prefixed UTF-16LE name (`"Host's Game"`
   round-tripped exactly) — confirming this is one shared code path with
   LAN browsing, not a separate protocol.
3. Once confirmed reachable, a 41-byte packet (matching the earlier
   0x20/0x21 ping shape) fires once, then traffic moves to UDP 2300.

**UDP 2300 session traffic, captured for the first time.** This is real
session/connection-establishment protocol, only partially decoded so far:

- First exchange: both sides send a 40-byte packet containing an 8-byte
  header followed by *two* back-to-back `sockaddr_in` blocks — one for the
  host's own address, one for the peer's — effectively "here's me, here's
  you" pairing. The host's version repeats 2-3 times before the client
  answers with its own (self/peer order flipped).
- Bytes 4-5 of subsequent 2300 packets carry a constant 2-byte value
  (`ef 35` in this capture) that stays fixed across dozens of packets in
  the same session — looks like a per-session connection ID.
- After the handshake, a steady stream of small packets (19-51 bytes)
  follows a `03 00 <seq> <seq> ef 35 ...` shape, where a 2-byte value
  increments by one for every packet pair (`27 27`, `28 28`, `29 29`, ...)
  — looks like a sequence-numbered reliable-delivery layer riding on top
  of raw UDP, doubled for some reason (possibly send/ack pairing).
- One of the client's very first 2300 packets contained what look like
  raw process pointers (e.g. `e9 7f a6 00` decodes as a plausible 32-bit
  stack/heap address) — possibly uninitialized memory leaking onto the
  wire, a known class of bug in games this old. Not yet confirmed.

None of this is fully decoded yet — flagging the shapes above as a
starting point for whoever picks this back up, not a finished spec.

## Open questions

- What are the 8 non-zero `sin_zero` bytes? Possibly a sequence number,
  session token, or uninitialized memory from Wine's `dpnet`
  implementation (worth checking Wine source/debug logs for). Note: in
  the 2026-08-03 capture the same exact 8 bytes appeared for *both* the
  host's and client's `sockaddr_in` within one session, which fits
  "uninitialized/reused buffer" better than "per-address token."
- What's inside the 0x26 header's `00 00` (bytes 2-3, before the trailing
  `00` at byte 4) — likely a version or reserved field, unconfirmed.
- Full structure of the UDP 2300 session/sequence-numbering layer (see
  above) — what the two incrementing counters actually track, what the
  8-byte header before each sockaddr pair means, and whether the apparent
  pointer leak is real or a misread.
