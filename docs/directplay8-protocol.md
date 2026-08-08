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

**Scope (2026-08-07): this entire document covers only the classic
No-CD/DirectPlay client (`aomxnocd1.exe`).** Per CLAUDE.md's Goals, the
project also needs to support the separately-networked Voobly-modernised
client, which is expected to use its own protocol entirely (reportedly a
single multiplexed UDP port, ~16000) rather than classic DirectPlay on
2299/2300. That hasn't been captured or reverse-engineered at all yet —
every capture taken during this project so far, including the
extensive 2026-08-07 debugging session, only ever drove `aomxnocd1.exe`.
Nothing here should be assumed to apply to Voobly traffic.

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
client -> host:  20 01 00 00 00  <sockaddr_in> <sockaddr_in>   (no name string)
host   -> client: 21 01 00 00 00  <sockaddr_in> <sockaddr_in>   (no name string)
```

**Correction (2026-08-07):** originally documented as a single
`sockaddr_in` block. A raw hex dump of `aom-lobby`'s traffic showed the
0x21 reply is 41 bytes — 5-byte header + **two** 16-byte `sockaddr_in`
blocks (same double-block shape as the 0x26 reply) + 4 trailing zero
bytes, not one block + 20 bytes of something else. `aom-lobby` was only
rewriting the first block and leaking the real backend pod's internal IP
in the second, unrewritten, straight to an external client — which is
exactly the kind of address a client would reasonably reachability-check
before allowing you to Join, and it silently refused to. Fixed in
`lobby/main.go`'s `discoveryAddressOffsets` (`0x21: {5, 21}`, matching
0x26). Best guess for the message's purpose is still: this is what the
game uses to compute/display a ping time per listed entry, independent
of the broadcast enumeration.

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

## Client resign/leave signature (2026-08-03)

Captured a live 3-player match (host in Observer Mode + a "Standard"/Easy
AI + a real client — see `run-aom-spoofed-client.sh`) where the client
resigned mid-game. Source:
`archiving/sessions/20260803-3v3-win-loss/session.pcap`, correlated
against `clicks.log` from the same session (both use epoch timestamps —
`record-session.sh`'s click-capture logic, run standalone since `sudo
tcpdump` wasn't available interactively; packets captured instead via
`kubectl debug` on the hostNetwork pod, same approach as the Direct
Connect capture).

Normal gameplay on UDP 2300 is a steady stream of the sequence-numbered
packets described above (`03 00 <seq> <seq> <session-id> ...`, 10-50
bytes). Resigning breaks that pattern sharply:

1. Client sends **ten identical copies of a 3-byte packet** in under half
   a millisecond: `01 <session-id-lo> <session-id-hi>` (`01 0b 04` in this
   capture — `0b 04` matches the 2-byte session/connection ID already
   present in this session's regular tick packets). Firing the same tiny
   message many times in immediate succession, rather than relying on the
   usual sequence-numbered delivery layer, strongly suggests this is a
   best-effort "make sure this one lands no matter what" signal — losing a
   resignation notice to packet loss would be a much worse failure mode
   than losing a routine tick.
2. Host replies once more with a single ordinary-looking tick packet
   (still `03 00 ...` shaped, nothing distinctive).
3. **Then nothing.** No further packets pass between host and that client
   for the rest of the capture (many minutes) — a clean, total stop, not a
   timeout/retry pattern.
4. Cross-referencing `clicks.log`: a click landed ~1.2s before the burst
   fired, consistent with Resign → confirm-dialog navigation taking about
   that long before the network signal actually goes out.

This looks like a solid, reusable signature for "this client just
left/resigned": message type byte `0x01`, tiny fixed size, sent in a
rapid burst, followed by total silence on that pairing. Not yet captured:
what a **win** looks like from the surviving side's perspective — in this
test the "winner" was either the AI (no client, so no network signal to
observe) or the host (in Observer Mode, not a combat participant). That's
the natural next capture: two real clients, one resigns, watch what the
*other* client's connection sees.

## Post-handshake session-settings sync on UDP 2300 (2026-08-07)

First captured (sizes only, via `aom-lobby`'s hex-dump logging) while
debugging a proxied Direct-Connect attempt that reaches "Attempting to
Connect" but never completes. **Fully decoded with real payload bytes**
shortly after, from a genuine, visually-confirmed-successful connection
between two independent spoofed clients (`run-aom-verbose-clients.sh`,
host `192.168.49.3` / joiner `192.168.49.4`, real `tcpdump` run inside
each container — see that script for why capturing from outside doesn't
see this traffic). This is the layer that runs immediately after the
already-documented 40-byte self/peer handshake and 41-byte ping variant,
and carries the actual lobby-sync data: player identities and map choice.

### Common wrapper

Every message in this layer shares an 8-byte header:

```
03 00                     type (constant across all of these)
<2-byte seq>               increments per message, per sender
<2-byte connection ID>     stable per sender for the life of the session
                            (matches the port/id learned during the
                            0x20/0x21 ping exchange)
<remaining bytes>          sub-message, varies by purpose (see below)
```

Both sides send this type repeatedly and independently — seq and
connection ID are per-sender, not a shared conversation counter.

### Player-announce sub-message (93 bytes total this capture)

```
01 08 02 16 16                       sub-header (constant shape observed)
<4 bytes>                            unclear, possibly a settings/version flag
24 30 27 00 00 00 00 00 00           unclear, possibly a numeric player ID or slot flag
7b <GUID as ASCII, with braces> 7d 00    player's DirectPlay GUID
<4 bytes, 00 00 00 00>               padding/alignment
0e 00                                length prefix (14, little-endian)
<UTF-16LE nickname>\0                length-prefixed like the 0x26 reply's
                                      name field, but here it's the PLAYER's
                                      nickname, not the game/session name
```

**Both nicknames used in this test decode cleanly**, each paired with a
distinct per-player GUID:

| Role | GUID | Nickname (UTF-16LE) |
|---|---|---|
| Host | `{2BDE4F69-94BF-4AA0-99DB-C3DA00925458}` | `host` |
| Joiner | `{CE7FDE55-F6EF-4700-A604-D4C22FA3B33E}` | `client` |

Each side announces itself with this message shape (host→joiner and
joiner→host both observed, each with their own GUID/nickname/seq/conn-ID)
— it's a self-announcement, not something only the host sends.

### Map-name sub-message (139 bytes total this capture, host→joiner only)

Same 8-byte wrapper, followed by a differently-shaped sub-message
containing more binary fields (unclear purpose — possibly game settings
flags/difficulty/handicap, not yet decoded) and, readably:

```
66 00 61 00 73 00 74 00 72 00 61 00 6e 00 64 00
6f 00 6d 00 2e 00 73 00 65 00 74 00
```
= UTF-16LE **`"fastrandom.set"`** — reads as a map/scenario file
identifier (`.set` extension), strongly likely the chosen map for this
match ("Random" per the host's own in-lobby map selector at the time of
capture). Same packet also repeats the session name (`"host's Game"`,
UTF-16LE, matching the 0x26 discovery reply's field) later in the payload.

### Type 0x03 sub-type with a constant string ("paullovesjade")

```
03 00 <seq> <conn-id> 0e 00
70 61 75 6c 6c 6f 76 65 73 6a 61 64 65 00     ASCII "paullovesjade\0"
00 00
```

The length prefix (`0e 00` = 14) exactly matches
`len("paullovesjade\0")`, so the decode is solid — and notably **ASCII,
not UTF-16LE** like every other string field in this protocol.

**Correction:** originally flagged as possibly tied to a patched-out
CD-key/version check, since it fired constantly and didn't match either
side's real nickname (`TheIP` in that test). That hypothesis is now
disproven: this exact same string, byte-for-byte, appears in the
`run-aom-verbose-clients.sh` capture too — a connection that genuinely
succeeded. A CD-key rejection couldn't produce a working connection, so
whatever this is, it isn't gating anything. It's constant across sessions
and unrelated to either player's real nickname (confirmed above), so the
most likely explanation is still that it's baked into `aomxnocd1.exe`
itself — plausibly a signature left by whoever built the No-CD crack,
sitting in a reused buffer rather than being genuinely player-supplied.
Harmless artifact, not a protocol requirement — nothing to replicate.

### Type 0x07ff (10 bytes)

```
07 ff                                header
<2-byte seq>                         matches a same-sender 0x03 message's seq
<2-byte connection ID>                matches a same-sender 0x03 message's ID
00 00 00 00                          padding
```

No text content. Fired frequently by both sides (seq/conn-ID matching
whichever sub-message it's paired with) — a lightweight ack/heartbeat for
this layer, not an independent message in its own right.

### Lobby chat (41 bytes this capture)

Captured live from `run-aom-verbose-clients.sh`'s two containers sitting
together in the joined lobby, one player typing a chat message to the
other. Same `0x03` wrapper as everything else in this layer:

```
03 00 <seq> <conn-id>                 wrapper header
1f 00                                 length prefix
01 47 04 0a 00 00 00                  sub-header/flags, not yet decoded
62 00 6c 00 61 00 68 00 20 00         UTF-16LE
62 00 6c 00 61 00 68 00 00 00
00 00 00 00 68 0c                     trailing bytes, not yet decoded
```

The UTF-16LE bytes decode cleanly to the literal message sent:
**`"blah blah"`**. Confirms directly (not just by absence-of-TCP
elsewhere) that in-lobby chat rides the same UDP 2300 channel as
discovery/handshake/settings-sync — see "Open questions" below on the
still-undecoded sub-header, but the text payload format itself
(UTF-16LE, no separate framing beyond this wrapper) is confirmed.

## Connection-establishment handshake has 4 steps, not 2 (2026-08-08)

The already-documented 40-byte self/peer packet (`## UDP 2300 session
handshake`, above) turns out to be only *half* of the real handshake.
Found by diffing a live-failing proxied Direct-Connect attempt (`aom-lobby`
with `VERBOSE=1`, plus the spoofed client's own `WINEDEBUG` trace) against
a genuinely successful, non-proxied connection captured fresh via
`run-aom-verbose-clients.sh` (host `192.168.49.3`, joiner `192.168.49.4`,
real `tcpdump -i eth0` inside each container). Both captures line up
byte-for-byte on everything below except the one step called out as
missing.

**Two message types are involved, both 40 bytes, both starting with a
1-byte type tag at offset 0** (previously only type `0x00` was
documented):

```
type 0x00 ("open"/self-announce) - the one already documented above:
  bytes 0-1:  00 00                  type
  bytes 2-3:  <sender's own ID>      arbitrary per-sender, NOT an echo of
                                      anything - just this sender's tag
                                      for this connection attempt
  bytes 4-7:  d8 f6 32 00             constant - identical across every
                                      sender in both captures (host,
                                      joiner, and the proxied client) -
                                      a fixed protocol value, not
                                      per-session data
  bytes 8-23: self sockaddr_in block (family/port/addr + 8 non-zero
              sin_zero bytes, see "Open questions")
  bytes 24-39: peer sockaddr_in block (same shape)

type 0x02 (handshake ack) - newly identified:
  bytes 0-1:  02 00                  type
  bytes 2-3:  <sender's own ID>      same value this sender used in its
                                      own type 0x00 (not a new one)
  bytes 4-5:  <peer's ID>            echoes the 2-byte ID field the OTHER
                                      side used in the type 0x00 this is
                                      acknowledging
  bytes 6-39: 00 00 00 00 00 00 00 e9 e9 7f a4 00 01 00 18 f7 32 00
              3f e4 d4 7f a4 00 01 00 01 00 00 00 18 f7 32 00
              - byte-for-byte IDENTICAL in every type 0x02 seen so far,
                across totally separate Wine process instances in two
                unrelated captures. Same reasoning as the "paullovesjade"
                string below: identical bytes across independent runs
                rules out a memory pointer/per-session token, so this is
                almost certainly a fixed protocol constant, not something
                requiring translation. Not yet decoded further.
```

Neither message embeds anything beyond the two `sockaddr_in` blocks in
type `0x00` - type `0x02` carries no address data at all, so it needs no
rewriting and can (and should) be forwarded byte-for-byte.

**The real 4-step sequence** (confirmed from the genuine capture's
microsecond-timestamped pcap):

1. **Host → Joiner: type `0x00`.** Sent unprompted, based on the address
   the host already learned during the discovery-phase `0x20`/`0x21` ping
   exchange - *before* the joiner has sent anything at all on port 2300.
   Retried every ~100ms (5 copies seen in the reference capture) until
   the joiner replies.
2. **Joiner → Host: type `0x00`** (its own self-announce, mirroring step
   1's shape with its own address) **and type `0x02`** (acking the host's
   ID from step 1) - sent back-to-back, together, as soon as the joiner
   receives step 1.
3. **Host → Joiner: type `0x02`**, acking the joiner's ID from step 2.
   Sent once.
4. Normal traffic (the `07ff` heartbeat / `03 00`-wrapped settings-sync
   layer documented below) begins on both sides.

**Where the proxied attempt breaks down**: steps 1-2 above happen
correctly and are confirmed byte-perfect through `aom-lobby` - this is
the fix documented in "UDP 2300 session handshake"'s 2026-08-07
correction (rewriting *both* self and peer on the backend→client leg).
The client's step-2 type `0x02` ack also goes through untouched and
correctly references the backend's real connection ID. **But step 3 -
the backend's own type `0x02` ack - never happens.** Not misrouted, not
malformed: checked the full session's traffic in both directions and
there is no type `0x02` (or any 40-byte non-handshake message) from
backend→client anywhere in the log, ever. Lacking it, the client's
connect-retry timer never clears: it just keeps re-sending its original
step-1-equivalent type `0x00` (over 1200 times across ~2 minutes in one
capture) while the backend, apparently satisfied, moves straight into
heartbeat/settings-sync chatter as if the join had already succeeded.
After the client eventually gives up (~2 minutes) and returns to the LAN
browse screen, the backend sends the already-documented 3-byte `01
<connID>` "peer is gone" burst (see "Client resign/leave signature",
above) - new information there too: that signature was previously only
seen client→host on a player resigning mid-game, so it's evidently a
generic bidirectional "this peer disappeared" signal, not resign-specific.

**Root cause found and fixed (2026-08-08).** A raw `tcpdump` on the
minikube node during a live proxied attempt (`-i any`, catching both the
client↔`aom-lobby` and `aom-lobby`↔backend legs with real microsecond
timestamps - same trick already validated for `aom-lobby` traffic
specifically because the node is one of the two real endpoints there,
see `run-aom-verbose-clients.sh`'s header comment for why that doesn't
work for sibling-container traffic but does here) showed the actual
bug: when `aom-lobby` rewrites the client's type `0x00` before forwarding
it to the backend, the *self* field's port was set to
`sess.backendConn.LocalAddr().Port` - the proxy's own ephemeral
outbound socket port (e.g. `54031`), which is different every session -
instead of the stable public port (`2300`) that discovery/ping had
already told the backend to expect the peer at. So the backend ended up
holding two contradictory addresses for what should have been the same
peer (`:2300` from its own proactively-sent type `0x00`, `:54031` from
the client's rewritten one) and never correlated them into one session -
hence no step-3 ack. `rewriteToClient`'s equivalent field (the
backend→client direction) was already correct - it hardcodes
`cfg.publicPort` - so this was a same-file asymmetry, not a
deeper protocol misunderstanding. Fixed in `lobby/main.go`'s session
`rewriteToBackend` closure: `port := sess.backendConn.LocalAddr()...`
replaced with the same `cfg.publicPort` constant. **Confirmed working**
against a live Direct-Connect immediately after redeploying: the
backend's `type 0x02` ack now appears, the settings-sync layer
(player-announce/map-name messages) starts flowing, and the client joins
the match successfully - first successful proxied Direct-Connect end to
end.

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
- What are the constant bytes in type `0x00` (`d8 f6 32 00`, bytes 4-7)
  and type `0x02` (the 34-byte tail starting at byte 6)? Identical across
  every sender/session observed so far, so almost certainly fixed
  protocol values rather than per-session data, but not yet decoded.
