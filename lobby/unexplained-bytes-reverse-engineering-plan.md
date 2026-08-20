# Unexplained bytes: complete inventory and reverse-engineering plan

**Update (2026-08-19): the specific mystery that motivated this document
is resolved** - see `packet-handling-design.md`'s "Next investigation"
status box. Root cause was the synthesized name-broadcast's content
(`"TheIP"` instead of `"paullovesjade"`), not any of the byte-level
unknowns catalogued below. None of Tier 1's fields turned out to be the
answer - left as-is below since the byte-level accounting itself is
still accurate and potentially useful groundwork, just not the
explanation for what it was investigating. If picking up the *next*
mystery (client<->client P2P pairing never completing an ack - see
`docs/multi-peer-routing-design.md`'s "Update (2026-08-19)" section),
this document's plan doesn't directly apply - that issue isn't about any
of these specific fields, since content there is already confirmed
byte-perfect.

Written 2026-08-18, end of a long session that exhaustively verified
every byte in the "lobby full" sequence against real captures (see
`packet-handling-design.md`'s "Canonical byte-level reference" section)
and got genuinely stuck on the one remaining behavioral mystery: a real
client's handshake against `aom-lobby`'s synthesized host completes the
connection-open/ack layer perfectly, then never sends its own
name-broadcast, no matter what's fixed. This document is the byte-level
half of "what's left" - every field this project has confirmed is
constant-but-unexplained or genuinely variable-but-unexplained, anywhere
in the protocol (not just the lobby-full sequence), with a specific,
concrete plan for cracking each one - not generic "capture more and
look" advice, but named techniques and why each one is the right fit
for that specific field.

Organized in two tiers: **Tier 1** is the lobby-full sequence's own
unknowns (actively investigated all session, most likely to matter for
the current mystery). **Tier 2** is everything else undecoded elsewhere
in the protocol (documented across `docs/directplay8-protocol.md` and
this file's own packet reference table, cataloged here for completeness
but not actively being chased right now).

---

## Tier 1: the lobby-full sequence's unknowns

### 1. `d8 f6 32 00` - the `0x00` message's bytes 4-7

**What we know:** Present in every `0x00` self/peer-open message this
project has ever captured - every host, every joiner, every session,
across multiple real days. Byte-identical every single time. Read as a
little-endian uint32: `0x0032F6D8`.

**What we don't know:** Whether this is a deliberate protocol constant,
or a leaked/uninitialized value from process memory that just happens
to be reproducibly identical.

**Leading hypothesis:** A leaked stack or heap pointer from inside
`dpwsockx.dll`'s own memory at the point this message is constructed.
`0x0032F6D8` is squarely in the range typical of a 32-bit Windows thread
stack address under pre-ASLR software (AoM shipped 2002, years before
ASLR existed) running under Wine, which also doesn't randomize this
class of binary's memory layout by default. If Wine + this specific
binary deterministically place that buffer at the same virtual address
every launch (same container image, same Wine version, every time we've
tested), a genuinely leaked pointer would reproduce identically forever
- consistent with what's observed, for an uninteresting reason (nothing
protocol-meaningful, just always-the-same uninitialized memory).

**Reverse-engineering plan:**

1. **Cheapest test - vary the environment, see if the value moves.**
   Run the exact same binary under a *different* Wine version, or a
   freshly-created (not image-baked) Wine prefix, and check whether
   `0x0032F6D8` changes. If it's genuinely a leaked stack address, a
   different Wine build's own internal layout should very plausibly
   shift it. If it stays identical even across different Wine versions,
   that's strong evidence it's actually a **compiled-in constant**
   inside the game's own binary (a literal `mov [buf+4], 0x0032F6D8`
   instruction), not a leaked pointer at all - which would completely
   change the theory and make it worth searching the binary's own
   `.data`/`.rodata` sections for this exact 4-byte sequence.
2. **Static disassembly (the direct approach).** Locate the function in
   `dpwsockx.dll` that constructs the `0x00` message - the easiest
   landmark to search for is the ALSO-unexplained-but-equally-constant
   ack trailer bytes (`e9 e9 7f`, etc. - see #2 below), since both
   fields are probably written by neighboring code in the same
   handshake-construction routine. A disassembler (Ghidra, IDA, even
   `objdump -d` combined with manual reading) applied to `dpwsockx.dll`
   would show directly whether the instruction writing these 4 bytes is
   `mov dword [reg+4], 0x0032F6D8` (a literal constant - mystery
   solved, trivially) versus something that computes or loads the value
   from elsewhere (a real leaked-memory situation, and the disassembly
   would show exactly *what* memory it's reading from, which answers
   the question directly rather than requiring further guessing).
3. **If it turns out to be a live memory read (not a constant): a
   `gdb` watchpoint.** Tonight's `gdb` attempt got stuck on 32-bit
   `socketcall()` calling-convention archaeology trying to intercept
   the argument *values* at the syscall boundary. A cleaner attack: once
   step 2's disassembly identifies the exact instruction address that
   writes this field, set a `gdb` breakpoint directly at *that*
   instruction (not at `sendmsg`, sidestepping the whole ABI problem)
   and inspect the source register/memory at that specific point - far
   more tractable than reconstructing the full `msghdr` layout blind.

### 2. The ack trailer's fixed-but-unexplained chunks

**What we know:** Six confirmed real samples, cross-checked
byte-for-byte. Every one of these sub-sequences is **always** present,
byte-identical, regardless of host/joiner role or session:
- `e9 e9 7f` (3 bytes, right before the role-varying `XX` byte - see #3)
- `18 f7 32 00` (4 bytes, appears **twice** in the trailer, once in each
  half - see the trailer's mirrored two-chunk structure documented in
  `packet-handling-design.md`)
- `3f e4 d4 7f` (4 bytes)
- `00 01 00 00 00` (5 bytes, near the end)

**What we don't know:** Whether these are compiled constants, structure
padding with a fixed pattern, or something with real meaning we simply
haven't matched to anything yet.

**Reverse-engineering plan:** Same as #1's static-disassembly approach
(#2 above) - these sit in the exact same message (`0x02` ack) as `XX`/
`YY`, so the same disassembly pass that locates the ack-construction
routine would resolve all of these fields' provenance simultaneously,
not just the `0x00` message's constant. Worth checking Windows/Wine's
own `dplayx.dll`/`dpwsockx.dll` source if a leaked/decompiled copy is
ever available (these are old, extensively-analyzed DLLs; a public
symbol-matched build or existing reverse-engineering writeup could
short-circuit needing to disassemble from scratch).

### 3. Ack trailer's `XX` (byte offsets 10 and 22, mirrored)

**What we know:** Joiner's value is rock-solid across all 5 samples:
always `0xa4`. Host's value has been `0xa6` (twice) and `0xa8` (twice)
across 4 samples - not a stable per-role constant, but not obviously
random either (only 2 distinct values ever seen, never anything else).

**What we don't know:** What determines the host's specific value, and
why the joiner's is so much more stable than the host's.

**Reverse-engineering plan:**

1. **Gather more host-role samples specifically** (10+, from genuinely
   independent processes/containers) and check the *distribution*: if
   it's always exactly `0xa6` or `0xa8` and never anything else even
   across many more samples, that's a strong signal of a small,
   discrete set of possible values (e.g. a boolean-ish flag encoded in
   an odd byte position, or literally the low bit of some handle
   toggling between two allocator states) rather than a genuinely
   free-ranging pointer/handle byte. If new samples start showing
   *other* values (`0xa5`, `0xa7`, `0xa9`...), that instead supports
   "arbitrary resource-handle low byte."
2. **Once #1's/#2's disassembly work locates the ack-construction
   routine**, this field's source register/memory would very likely be
   visible in the same disassembly pass - check whether it's read from
   a genuine handle/pointer (matches "resource handle" theory) or from
   something that looks like process/thread state that would plausibly
   differ between "I am hosting" and "I am joining" code paths (which
   would better explain the host/joiner asymmetry directly, rather than
   needing the "coincidentally stable" framing the current theory
   relies on).

### 4. Ack trailer's `YY` (byte offsets 12 and 24, mirrored)

**What we know:** Genuinely session-variable - `0x01` in 4 of 5 real
sessions, `0x02` in one. Identical between host and joiner *within* one
session, even though they're separate processes with different
individual uptimes (ruling out "each process's own elapsed time" as the
mechanism - see `packet-handling-design.md` for the full reasoning
chain). A live `gdb` investigation tonight found **zero** clock-reading
syscalls (`clock_gettime`, `gettimeofday`, etc.) anywhere in a client's
entire process trace.

**What we don't know:** The actual formula/source. The official
`[MC-DPL8R]` spec's `tTimestamp` field (present in every real DirectPlay
8 CFRAME, defined as "sender's system tick count in milliseconds") is
the best-supported analogy, but the zero-clock-syscalls finding weakens
it - **with an important caveat**: `clock_gettime` is frequently served
via the Linux vDSO, a userspace fast-path that never enters the kernel
and is therefore invisible to `strace`, so this negative result doesn't
cleanly disprove a time-based origin.

**Reverse-engineering plan:**

1. **Break on the vDSO directly, not on kernel syscalls.** `strace`
   fundamentally cannot see vDSO-satisfied calls, but `gdb` can - if a
   breakpoint is set directly on the vDSO's own mapped `clock_gettime`
   symbol (find its address via `cat /proc/<pid>/maps | grep vdso`,
   then resolve the symbol inside that mapped region), any call through
   that fast path would be caught, which `strace` structurally cannot
   do. This directly answers the question the earlier strace-only
   investigation left open.
2. **A `gdb` watchpoint on the specific trailer byte's write.** Same
   technique as #1/#2/#3 above - once the ack-construction routine is
   located via disassembly, a watchpoint on the exact memory offset
   that becomes byte 12/24 would show the *instruction* that writes it,
   which either reveals a direct clock-read (confirming the tick-count
   theory conclusively) or something else entirely (settling the
   question either way, rather than continuing to guess).
3. **More samples across a deliberately wide time spread**, specifically
   designed to test correlation rather than opportunistically collected.
   Take captures at fixed intervals (e.g. every 10 minutes for an hour,
   then a few spread across different days) and check whether the value
   trends with elapsed wall-clock time, elapsed container-uptime, or
   neither - the current 5 samples were opportunistic (whatever
   captures happened to exist), not designed to isolate this variable.

### 5. The rejection's `22 07` payload

**What we know:** The actual "you're rejected" signal, in every capture
- 2 bytes (`22 07`) followed by 11 zero bytes, totally consistent across
every sample regardless of role or session. Every reasonable decoding
attempted (ASCII, uint16 LE/BE) produces nothing readable.

**What we don't know:** What this code actually represents - is it a
DirectPlay-internal error enum, a raw status value, something else?

**Reverse-engineering plan:**

1. **Cross-reference known Win32/DirectPlay error code tables** - this
   is a pure lookup task, not requiring any new capture. As a 16-bit
   value: `0x0722` (little-endian reading: bytes as given) = 1826
   decimal, or `0x2207` (byte-swapped) = 8711 decimal. Check both
   against `DPERR_*` classic DirectPlay error constants (from the
   Windows DirectX SDK headers, if a copy can be found/referenced) and
   generic `WSAE*`/`HRESULT` low-word values. This is the single
   cheapest item in this entire document to potentially resolve -
   doesn't need a new capture or any live investigation, just the right
   reference table.
2. **Static disassembly, searching for a switch/lookup table.** If
   `aomxnocd1.exe`'s own UI code has a routine that maps an error code
   to the "Host game is full" string (or similar rejection-message
   strings), searching for where `0x0722`/`0x2207`/`1826`/`8711` appears
   as an immediate value near string-table references would directly
   confirm the meaning and potentially reveal a whole family of related
   rejection codes we've never seen fire (useful for the broader
   spoofing goal - are there other rejection reasons besides "full"?).

### 6. `0x26` discovery reply header, bytes 2-3 (`00 00`)

**What we know:** Never observed non-zero, in any capture, ever.

**What we don't know:** Whether this is genuinely always zero (a fixed
reserved field) or just hasn't been seen in a state that sets it.

**Reverse-engineering plan:** The cheapest item to *deprioritize* -
since it's never varied, the only way to learn more from packet capture
alone is stumbling on a genuinely different scenario (a different game
version, a different client build, an edge case like a session ID
conflict) that happens to set it. Static disassembly of the `0x26`-reply
construction code (in the discovery-handling routine, likely near the
already-located ack/open construction code from #1/#2) would show
directly what conditions, if any, could make this non-zero - lower
priority than #1-#5 since there's no live behavioral mystery riding on
this field specifically.

---

## Tier 2: everything else undecoded, elsewhere in the protocol

Not actively being chased right now - cataloged here for completeness,
since the ask was "all of our unexplained bytes," not just the
lobby-full sequence's. General reverse-engineering approach for this
whole tier: **capture a full real match that gets past ready-up into
actual gameplay** (none of tonight's captures did - every one was
either a deliberate "lobby full" test or ended early), since several of
these fields have only ever been seen 1-2 times, in the archived
2026-08-08/2026-08-12 captures, and haven't been revisited since.

### Player-announce's 9-byte gap (GUID → nickname)

Found tonight while confirming real nicknames ("clienta"/"host") appear
in this message, not in the `paullovesjade` field: `00 10 00 00 00 00
00 00 00` sits between the closing `}` of the GUID string and the start
of the UTF-16LE nickname. Completely undecoded - not even a hypothesis
yet. **Plan:** collect several more samples of this exact message across
different players/sessions (same technique as the ack-trailer work -
diff multiple independent samples to see which bytes are fixed vs.
vary) - this has only ever been checked in the two 2026-08-12
`clienta`/`clientb` samples pulled tonight, nowhere near the sample
count the lobby-full sequence's fields got.

### Map-name message's ~40-byte undecoded binary blob

Sits between the map filename string and the repeated session name in
the 139/141-byte map-name message. Documented as "possibly game settings
flags/difficulty/handicap, not yet decoded" since 2026-08-07 with no
progress since. **Plan:** capture two matches with deliberately
*different* game settings (map size, difficulty, victory condition) and
diff this specific blob - if a specific byte changes in lockstep with a
specific UI setting, that directly identifies which setting it encodes,
the same "vary one thing, diff the bytes" technique that cracked the
ready-toggle field originally.

### The `01 08 02`-family lobby-setup messages (43/46 bytes)

Fires 16-32 times right at join, both clients, undecoded - "plausibly
per-slot team/civ/color selection state" per the 2026-08-12 capture
pass, never followed up. **Plan:** same diff technique - capture two
joins where team/civilization/color selections are deliberately
different, diff the message set.

### Ready-toggle's 2-byte trailer

Changes every message, "most likely a checksum or seq-derived value,
not further decoded" since 2026-08-11. **Plan:** since the ready-toggle
message's *other* fields are fully decoded (including a literal seq
field elsewhere in its own wrapper), compute whether this trailer is a
simple function of the already-known seq/connID/payload bytes (e.g. a
CRC16 or additive checksum) programmatically against the existing
archived samples - this doesn't need a new capture, just running
candidate checksum algorithms against data already on disk.

### `0x29`/`existingPeerBroadcast`'s remaining unclear fields

`1c 01 00 00 00` (0x29's own header-ish field), the trailing `03 00 00
00`/`53 00` in 0x29, and `4d 00`/the 36-byte unclear block/trailing `02
00 00 00 00 00` in `existingPeerBroadcast`. Low priority - these
messages are already fully *actionable* (the address-bearing fields
needed for correct rewriting are identified and implemented), so these
remaining bytes are "nice to fully understand" rather than blocking
anything. **Plan:** same diff-across-samples technique, opportunistic
rather than a dedicated capture effort - collect these naturally next
time a 2-real-client join is captured for another reason.

### Post-ready signals: 21-byte and 23-byte messages, in-game chat's sub-header, one-off 77-byte message

All observed exactly once or twice in the 2026-08-12 capture, never
revisited. The 23-byte one is flagged as the best candidate for an
actual "match/simulation start" signal - genuinely important if the
full-match-illusion goal (see the "spoof a full game" answer given
earlier this session) is ever pursued, since it's the one message this
project has never captured a second independent sample of at all.
**Plan:** requires a fresh, deliberate capture of a real 2-client match
all the way through ready-up into the first seconds of actual gameplay
- nothing else in the archive gets far enough.

### In-game gameplay-sync traffic (12/13/16/19-byte generic `03`-tagged messages)

The actual moment-to-moment game-state/order-sync layer once a match is
running. Completely undecoded - not even message boundaries within this
size class are understood, just that the wrapper's second byte varies
(distinguishing it from the lobby-phase `03 00`-fixed wrapper). **This
is the single largest undecoded surface in the whole protocol** - see
this session's earlier "spoof a full game" answer for why this is a
categorically bigger reverse-engineering project than everything else
in this document combined, closer to decoding a real-time game-state
protocol from scratch than the address-rewriting/handshake-timing work
this project has done so far. Not recommended as a next step unless the
full-match-illusion goal specifically becomes a priority - the "lobby
full" goal (this project's actual current priority) never needs a match
to start at all.

---

## Priority recommendation, if picking this up next

1. **Item 5 (`22 07` lookup)** - cheapest possible win, no new capture
   needed, just cross-referencing known error-code tables.
2. **Item 4 (`YY`/tick-count) via the vDSO-aware `gdb` breakpoint** -
   directly answers the single most-discussed open question from
   tonight's session with a technique that wasn't tried (vDSO-blind
   `strace` was tried and came back empty; this is different).
3. **Items 1-3 via static disassembly** - the biggest investment
   (needs a disassembler and genuine reverse-engineering time), but
   would very likely resolve most of Tier 1 in one pass, since several
   of these fields are constructed by the same handful of functions.
4. **Tier 2** - opportunistic, not urgent; revisit naturally whenever a
   deeper real-match capture happens for some other reason.
