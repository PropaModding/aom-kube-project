# Hosting flow: driving aom-headless via VNC

Automates what a human would click through in VNC to get `aom-headless`
from a cold start into a hosted LAN/Direct-IP lobby, without needing
`xdotool`/`scrot` baked into the image (those turned out to be a dead end —
see below). Driven from the host machine with
[`vncdotool`](https://github.com/sibson/vncdotool) against a
`kubectl port-forward`'d VNC session, since it can both capture screenshots
and send input over the wire — no in-container screenshot tool needed.

```
kubectl port-forward svc/aom-headless-vnc 5901:5900 &
vncdotool -s localhost::5901 capture screenshot.png
vncdotool -s localhost::5901 move <x> <y> click 1
vncdotool -s localhost::5901 type "SomeText"
vncdotool -s localhost::5901 key BackSpace   # one key event per invocation
```

All coordinates below are in the game's own 800x600 window space (baked
into `dockerfile.k8s`/`user.cfg`), which is exactly what `vncdotool`'s
`move x y` operates in since the VNC framebuffer *is* that window (Xvfb is
800x600, nothing else is on that display) — no offset math needed, unlike
`xdotool --window` which needs window-relative coordinates.

**To watch `host-game-kube.sh` drive the pod live** (useful whenever
tuning its click/key sequence): in one terminal,
`kubectl port-forward svc/aom-headless-vnc 5901:5900`, then from your
desktop `remmina -c vnc://localhost:5901` — connects straight in over
VNC, no password, no need to click through Remmina's New Connection
dialog first.

**Why not `host-game.sh`'s xdotool/scrot approach, for calibration**: that
script (still in the repo, unfinished — all its click steps are
`CALIBRATE` placeholders) runs *inside* the container and needs `scrot`
for screenshots, which isn't installed, and `aomuser` can't
`apt-get install` it (no root in the image). Driving over VNC from the
host sidesteps that for the *discovery* phase, since `vncdotool` can both
see the screen and click.

**For the final/production automation, this switches back to
`xdotool` run inside the container** (`kubectl exec ... xdotool ...`, the
same tool `host-game.sh` already uses) rather than `vncdotool` over a
port-forward — each `vncdotool` call pays a fresh VNC connection/handshake
(noticeably slow, ~a few seconds per call, see the 20x `key BackSpace`
loop below), where `xdotool` running in-container talks to the X server
directly with no such overhead. `vncdotool`'s screenshots stay useful for
this calibration doc regardless, but the coordinates it finds should end
up executed via `xdotool` once the full sequence is known, not shipped as
a `vncdotool`-over-VNC script.

**Gotcha**: chain multiple `vncdotool` subcommands in *one* invocation
(`move .. click 1 pause 0.3 key ...`) rather than shell-scripting multiple
`vncdotool` invocations on one line without a separator — a missing `;`/`&&`
between two invocations gets silently swallowed into one `eval` and hangs
with no output, needing a manual `kill`.

## Flow so far (cold start → nickname entry)

**Correction (2026-08-07):** earlier revisions of this doc described a
"campaign-style landing menu" (AOM Campaign / Titans Campaign / Load
Saved Game / Main Menu) as a required screen between the intro splash and
the real Main Menu, needing its own click to get past. **That screen
doesn't exist in the normal flow.** The intro splash(es) resolve directly
to the real Main Menu (Learn to Play / Campaign / Single Player /
Multiplayer / Options / More / Exit) — confirmed watching live over VNC.
What actually happened in earlier runs: a leftover blind click aimed at
that assumed landing screen (`(384, 459)`) landed on the real Main Menu
instead, on **Campaign** or **Exit** depending on splash timing, driving
the pod into a campaign map (or worse, closing the game) rather than
toward Multiplayer. `host-game-kube.sh` no longer clicks anything for the
splash — it just waits the splash out and goes straight to Multiplayer.

| # | Screen | Action | Coordinates |
|---|--------|--------|--------------|
| 1 | EULA (Additional Content Files — not pre-accepted by the image's registry seed) | Keyboard-driven, no click: `key Left` (moves focus onto Accept) → sleep 3s → `key Return` (confirms) | — |
| 2 | Intro splash(es) — "Ensemble Studios" monster-head splash and/or the "AGE OF MYTHOLOGY / THE TITANS" logo splash, count varies between runs (see below) | No action — just wait (10s) for them to play out on their own | — |
| 3 | Real main menu (Learn to Play / Campaign / Single Player / Multiplayer / Options / More / Exit) | Click **Multiplayer** | (384, 360) |
| 4 | Multiplayer submenu (Online / LAN-Direct IP / Main Menu) | Click **LAN/Direct IP** | (384, 300) |
| 5 | "LAN/Direct IP Login" — Nickname field | Click field (400, 238), clear any existing text with repeated `key BackSpace` (not `ctrl-a` — see gotcha below), type nickname (slowly — see below), click **OK** | field (400, 238), OK (322, 302) |

**Nickname typing needs to be slow.** `aomxnocd1.exe` runs pinned near
100% CPU under Xvfb + llvmpipe software rendering, and fast-paced
`xdotool type` input against it has been observed to garble/drop
keystrokes badly enough that a nickname field showing clean alphanumeric
text in a screenshot still failed the game's own "invalid nickname,
alphanumeric only" validation on submit. `xdotool type --delay 200 -- ...`
(200ms/keystroke) is what `host-game-kube.sh` uses now — noticeably slow,
but reliable.

**Nickname validation**: alphanumeric only — no symbols (confirmed by
trying `AOM-HOST`, which the game rejected with "You have entered an
invalid nickname"). A leftover `a` from an earlier stray keypress also
proved a single `key BackSpace` per invocation is reliable for clearing,
whereas `key ctrl-a` (select-all) hung the `vncdotool` process with no
output/error — avoid chord key combos, prefer repeated single-key
`BackSpace`.

## Flow (continued): LAN/Direct IP screen → hosting → observer mode

| # | Screen | Action | Coordinates |
|---|--------|--------|--------------|
| 7 | "LAN / Direct IP" — has a "Host a LAN Game" field (pre-filled `<nickname>'s Game`), a LAN games list, and a "Type a Direct IP" field | Click **Host** | (560, 170) |
| 8 | In-lobby host screen (player list, game name, map/game settings, **Players** dropdown top-left, **Observer Mode** checkbox bottom-left, Cancel/Other Settings) | Set **Players** to 3 (host + 2 open slots - this project only hosts 1v1s, see CLAUDE.md), fill both slots with Standard AI, click **Observer Mode**, then kick both AI back out - see below, no need to wait for a join | Players dropdown: click (61, 63) to open, (55, 74) to select "3". Slot 2/3 dropdowns: (100, 115)/(100, 143) to open, (75, 128)/(75, 156) to select "Standard". Observer Mode checkbox: (178, 563). Slot 2/3 kick icon: (37, 115)/(37, 143) |

**Players is a dropdown, not a text field or spinner** - confirmed by
trial: clicking it doesn't cycle a value, and typing into it does
nothing (it's not keyboard-editable). A single click opens a scrollable
list (2-12) anchored so the *currently-selected* value's row renders at
the box's own screen position, with each subsequent value 13px below
that - e.g. from the default "2", the "3" row sits at (55, 74). Only
calibrated for the 2→3 transition since 1v1 is the only format in scope
here; other player counts would need their own calibration if ever
needed. It defaults to "2" (host + 1 open slot) on every fresh host,
which is enough for a single joiner but not for two.

At step 8 the pod is actually listening on DirectPlay8's ports and
answering discovery/Direct-Connect queries — this is the point
`aom-lobby`'s relay starts getting real replies instead of timing out.
The lobby screen's own "IP Address:" readout in the top-left confirms the
pod's address (e.g. `10.244.0.24`), independent of whatever `aom-lobby` is
rewriting it to for outside clients.

**Observer Mode is now set at startup, fully automated (2026-08-11) -
no more waiting for a join.** Earlier approach (used in the 3v3 capture,
`archiving/sessions/20260803-3v3-win-loss/`): poll `kubectl logs
deployment/aom-lobby` for a `new session: client ...` line for a *new*
client address to know a real join happened, then click the checkbox -
needed because it wasn't known yet whether clicking it before anyone had
joined would even stick.

The checkbox itself (178, 563; pixel-measured center via a VNC
screenshot: (181, 563), close enough to the existing calibration to not
bother re-numbering it) was first confirmed with 2 real clients already
joined - the host's row switches from a god pick to "Observer" and the
client rows renumber from 1/2/3 to 2/3. Clicking it at pure startup, with
both slots still "Open", turned out to need a precursor step first:
**AoM's lobby requires both "Open" slots to be filled before Observer
Mode can be set at all.** The fix, calibrated live and now built into
`host-game-kube.sh`:

1. Open each "Open" slot's own dropdown (same control used for both "who
   occupies this slot" and AI behavior - "Open" is the first entry,
   followed by Standard/Random/Attacker/Conqueror/Builder/Protector/
   Defender) and select **Standard**. Slot 2's dropdown opens anchored at
   its own row (100, 115) with "Standard" 13px below at (75, 128) - same
   list-anchoring behavior as the Players dropdown. Slot 3: (100, 143)
   to open, (75, 156) to select.
2. Click Observer Mode (178, 563) - now safe, since both slots are
   filled.
3. Click the **kick icon** on each slot to reopen it for a real client -
   a small shoe/foot icon at the far left of each row (easy to mistake
   for a checkmark at a glance), tooltip: *"Kick this player out of the
   game. This option can only be used by the game host."* Slot 2: (37,
   115). Slot 3: (37, 143). Confirmed to work identically whether the
   slot holds an AI or an already-connected real client (tested live by
   kicking a real joined client - the client's own game reported being
   disconnected, and the host's chat log printed `"<host>: <name> was
   kicked out of the room."`), so this same control is also how a
   stuck/disconnected real client would eventually get removed, not just
   AI cleanup.

**Observer Mode persists through both a kick and a client-initiated
disconnect** - confirmed live 2026-08-11 (kicking a real client, and
separately watching a real client drop on its own, both left the host's
row still reading "Observer"), matching the original 2026-08-10
observation. An earlier draft of this doc briefly claimed the opposite
based on a screenshot that looked ambiguous (a blank cell where "Observer"
normally renders) - that read was wrong; trust the in-game state over an
ambiguous render.

End-to-end result, verified against a fresh pod: host lands in Observer
Mode with both slots back to "Open," zero manual clicks, before any real
client has connected.

## Full calibrated sequence (800x600 coordinates, `xdotool` in-container)

```
1.  key Left, sleep 3, key Return   # EULA Accept - keyboard, no click
    sleep 10                        # let intro splash(es) play out untouched;
                                     # resolves straight to the real Main Menu,
                                     # no landing-menu screen/click in between
2.  (384, 360)  click   "Multiplayer"
    sleep 5
3.  (384, 300)  click   "LAN/Direct IP"
    sleep 5
4.  (400, 238)  click   nickname field, then `key BackSpace` x30 to clear
    type --delay 200  <nickname>   # alphanumeric only, no symbols; slow
                                    # typing avoids garbled keystrokes (see
                                    # above)
    (322, 302) click OK
    sleep 5
5.  (560, 170)  click   "Host" (LAN game name field is pre-filled)
    sleep 5
6.  (61, 63)    click   open Players dropdown
    sleep 1
    (55, 74)    click   select "3" (host + 2 open slots)
    sleep 5
7.  (100, 115)  click   open slot 2 dropdown
    sleep 1
    (75, 128)   click   select "Standard" AI for slot 2
    sleep 5
8.  (100, 143)  click   open slot 3 dropdown
    sleep 1
    (75, 156)   click   select "Standard" AI for slot 3
    sleep 5
9.  (178, 563)  click   "Observer Mode"
    sleep 5
10. (37, 115)   click   kick slot 2 AI (reopens it for a real client)
    sleep 5
11. (37, 143)   click   kick slot 3 AI (reopens it for a real client)
    sleep 5
```

Run via `kubectl exec <pod> -- xdotool mousemove X Y click 1` per step
(see "Gotcha" above for why not `vncdotool` for the actual clicking).

## Script

`host-game-kube.sh [nickname]` runs the calibrated sequence above
end-to-end via `kubectl exec ... xdotool` (no VNC/`vncdotool` involved in
the actual run — that stays a calibration-only tool, see "Gotcha" above).
Leaves the pod sitting in the hosted lobby, host already in Observer Mode
with both slots open for real clients; does not start the match.
Defaults to nickname `AOMHOST`, but for manual/live-debug runs watched
over Remmina/VNC, `TheIP` (`./host-game-kube.sh TheIP`) is the nickname
convention in use — named after the thing a client actually needs to
know (the cluster's Direct-IP address), easy to pick out in the lobby's
player list while debugging.

**Intro splash variance observed**: a fresh pod sometimes shows an extra
"Ensemble Studios" monster-head splash before the AoM/Titans logo splash,
sometimes not (unclear why — possibly asset-loading timing on a cold vs.
warm pod). Both splashes resolve on their own with no input needed, so
the script doesn't click through them at all — it just sleeps 10s after
EULA confirm, long enough to clear either splash count, then clicks
Multiplayer on the real Main Menu underneath. (An earlier version of this
script *did* click `(400, 300)` twice to "dismiss" the splashes, on the
assumption it would land harmlessly on an intermediate landing-menu
screen — that screen doesn't exist, so those clicks were actually landing
on the real Main Menu's Campaign or Exit buttons depending on splash
timing, which is what caused several failed hosting attempts before this
was caught watching live over VNC.)

## Still to do

- Match-start isn't automated - out of scope for the current
  DPNID-capture goal, which only needs players *joined*, not a match in
  progress. See `docs/directplay8-protocol.md`'s "Ready-toggle
  sub-message" section and `docs/multi-peer-routing-design.md`'s
  "Next steps" item 3 for the packet-detection half of this (done); what's
  still missing is `aom-lobby` actually being able to command the pod
  (e.g. `kubectl exec ... xdotool`) once it knows both real clients are
  ready.
- Game name/map/settings are currently left at their defaults
  (`Supremacy`, `Random` map, `Normal` size, `Easy` difficulty) — revisit
  if a capture needs specific settings.
