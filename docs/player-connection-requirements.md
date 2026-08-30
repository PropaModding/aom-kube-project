# Connecting to this cluster from your own AoM install

What a player, outside this project entirely, needs to know and do to
get their own copy of classic Age of Mythology to reach `aom-lobby` and
actually complete a match - not how the cluster itself works (see
`CLAUDE.md` and `docs/multi-peer-routing-design.md` for that).

## Client requirements

- **Classic, retail/No-CD AoM only** (`aomxnocd1.exe`-family builds -
  UDP-only DirectPlay 8 on ports 2299/2300, matching everything
  reverse-engineered in `docs/directplay8-protocol.md`). **Voobly is not
  supported** - it's a separately-networked client layer this project
  hasn't reverse-engineered or built any routing for at all (see
  `CLAUDE.md`'s "Dual client-variant support" section, deprioritized).
  If your client isn't the classic DirectPlay one, none of the below
  applies and it won't work yet.
- **Direct-IP Connect only, never LAN browse.** There is no host to
  discover on your LAN - matches are provisioned on demand behind
  `aom-lobby`'s own public address, and LAN broadcast traffic can't
  cross the internet to reach it anyway. Use the game's Direct-IP
  Connect field and type the cluster's public address directly.
- **Outbound UDP 2299/2300 must not be blocked** by your own firewall
  or a restrictive network (some corporate/campus/mobile-hotspot
  networks block outbound UDP on non-standard ports outright - if nothing
  below fixes a stuck connection, this is worth checking first).

## The self-address problem, and the fix

Classic AoM determines its own IP by reading its local network
interface directly - it has no NAT awareness, and originally relied on
querying "the ESO Address Server" (a Microsoft/Ensemble service, dead
for years) to learn its real external address. With that gone, it falls
back to whatever private address your own router/NAT assigned it
(`192.168.x.x`, `10.x.x.x`, etc.) and embeds *that* in its own DirectPlay
8 handshake - an address nothing outside your home network can ever
reach. See `nat-lab/design.md` for the full investigation, including
direct packet-level confirmation of this mechanism against a real NAT
boundary.

**The fix, confirmed live 2026-08-30**: AoM's own `OverrideAddress`
setting, placed as the first line of its `startup/user.cfg` file,
successfully changes what address the game reports about itself. Four
different ways of passing this as a command-line launch parameter were
also tested and **all confirmed to have zero effect** on this specific
executable, despite reaching the process correctly - don't bother with
command-line flags for this, `user.cfg` is the one that actually works.

A script that does this automatically is provided for both platforms:

- **Windows**: [`client-setup/set-override-address.ps1`](../client-setup/set-override-address.ps1)
- **Linux / Wine**: [`client-setup/set-override-address.sh`](../client-setup/set-override-address.sh)

Both scripts detect your current public IP and write
`OverrideAddress="<your IP>"` as the first line of your own `user.cfg`,
backing up whatever was there first (`user.cfg.orig-backup`, created
once, never overwritten again - safe to re-run repeatedly). See each
script's own header comment for exact usage and where it looks for
`user.cfg` by default.

### Dynamic IP - you need to re-run this before every session

Almost nobody has a static home IP. Whatever address these scripts
detect and write today can silently go stale the next time your router
reconnects or your ISP reassigns you a new one - and unlike a normal
network problem, a stale `OverrideAddress` doesn't error, it just quietly
makes the game report the wrong address again, back to the exact
"Attempting to Connect" hang this whole thing exists to fix.

**Re-run the appropriate script before every play session.** A few ways
to make this less annoying to remember:

- **Windows**: add the script as a "run before" step in whatever
  shortcut/launcher you use to start AoM, or drop a shortcut to it in
  your Startup folder.
- **Linux/Wine**: wire it into a shell alias, or a Lutris/Steam
  pre-launch script hook if you're launching AoM through either of
  those.
- Either way, running it with an IP that hasn't actually changed is a
  harmless no-op - it always rewrites the line, so there's no cost to
  running it more often than strictly necessary.

## What's still unconfirmed

**This fixes what the client reports about itself in a direct,
unrelayed connection - its relevance to a connection actually routed
through `aom-lobby` is not yet independently confirmed.** `aom-lobby`'s
own relay code already rewrites this same field server-side before
forwarding a client's handshake to the real backend pod (see
`nat-lab/design.md`'s "Open question this doesn't resolve by itself"
section) - meaning the backend host may never actually see whatever a
connecting player's own client reports here, overridden or not. Setting
this up is still the right move (confirmed to do what it claims, cheap
to set up, no downside), but don't treat it as a guaranteed fix for
every connection problem until that's tested end-to-end against the
real cluster with a genuinely NAT'd client.

**Port forwarding may still be required for some routers.** Every real
community report of `OverrideAddress` actually working in the wild
paired it with forwarding UDP 2300 (and sometimes 2301/64520) on the
player's own router to their machine - not yet tested in this project's
own lab. If setting `OverrideAddress` alone doesn't fix a stuck
connection, forwarding those ports is the next thing to try, though it
requires access to your own router's admin settings (out of scope for
anything this project can automate for you).

**Symmetric NAT.** If your own router does symmetric NAT (a different
external port per destination, rather than the same one for every
outbound connection), `OverrideAddress` may not be enough by itself
even with port forwarding. Most home routers default to a less
restrictive NAT type; this is a minority case.
