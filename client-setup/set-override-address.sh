#!/bin/bash
# Detects this machine's current public IP and writes it into classic
# AoM's own startup/user.cfg as OverrideAddress - confirmed live
# 2026-08-30 (see nat-lab/design.md's "Confirmed live, 2026-08-30"
# section) to be the ONLY mechanism of the ones tested that actually
# changes what the game reports as its own address in the DirectPlay8
# handshake - four different command-line launch-parameter syntaxes
# (OverrideAddress="...", +OverrideAddress="...", OverrideAddress=...
# with no quotes, and the full +OverrideAddress/+hostPort/
# +directIPConnectivity combination) were all confirmed reaching the
# game process's real argv (via /proc/<pid>/cmdline) with zero effect
# on the wire. Don't waste time re-trying a command-line-argument
# approach for this exe - use this instead.
#
# For classic AoM run under Wine (this project's own aom-headless pods,
# or a real player running the classic client under Wine/Proton on
# Linux). See set-override-address.ps1 in this same directory for the
# native-Windows equivalent.
#
# WHY THIS HAS TO BE RE-RUN, NOT SET ONCE: almost nobody has a static
# home IP - see docs/player-connection-requirements.md's own "Dynamic
# IP" section. Re-run this before every play session (or wire it into
# whatever launches the game - a shell alias, a Lutris/Steam pre-launch
# script, a cron job - see that doc for suggestions). Running it with an
# unchanged IP is a harmless no-op; it always rewrites the line either
# way, cheap enough not to bother checking first.
#
# Usage:
#   ./set-override-address.sh [path/to/user.cfg]
#
# With no argument, tries the well-known Wine path for a Titans-style
# "My Documents\My Games" install first, then the non-Titans
# alongside-the-exe "startup" layout, both under the default wine
# prefix ($HOME/.wine). Pass the actual path explicitly if your install
# lives somewhere else (a custom WINEPREFIX, a non-default AoM install
# directory, etc.) - this script has no reliable way to discover an
# arbitrary install location on its own, and guessing wrong silently
# would be worse than just asking for it.
set -euo pipefail

USER_CFG="${1:-}"

if [ -z "$USER_CFG" ]; then
    CANDIDATES=(
        "$HOME/.wine/drive_c/users/$USER/My Documents/My Games/Age of Mythology/startup/user.cfg"
        "$HOME/.wine/drive_c/Program Files/Microsoft Games/Age of Mythology/startup/user.cfg"
        "$HOME/.wine/drive_c/Program Files (x86)/Microsoft Games/Age of Mythology/startup/user.cfg"
    )
    for c in "${CANDIDATES[@]}"; do
        if [ -f "$c" ]; then
            USER_CFG="$c"
            break
        fi
    done
fi

if [ -z "$USER_CFG" ] || [ ! -f "$USER_CFG" ]; then
    echo "Couldn't find user.cfg automatically. Pass its path explicitly:" >&2
    echo "  $0 /path/to/Age of Mythology/startup/user.cfg" >&2
    echo "(Create an empty user.cfg there first if one doesn't exist yet -" >&2
    echo " the game does read it once it's present, confirmed live" >&2
    echo " 2026-08-30, see nat-lab/design.md.)" >&2
    exit 1
fi

echo "[*] Detecting current public IP..."
PUBLIC_IP=""
for svc in "https://api.ipify.org" "https://ifconfig.me/ip" "https://icanhazip.com"; do
    PUBLIC_IP="$(curl -fsS --max-time 5 "$svc" 2>/dev/null | tr -d '[:space:]')" || true
    if [[ "$PUBLIC_IP" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        break
    fi
    PUBLIC_IP=""
done

if [ -z "$PUBLIC_IP" ]; then
    echo "Couldn't detect a public IP from any lookup service (api.ipify.org," >&2
    echo "ifconfig.me, icanhazip.com all failed or returned something that" >&2
    echo "doesn't look like an IPv4 address). Check your internet connection." >&2
    exit 1
fi

echo "[*] Public IP: $PUBLIC_IP"
echo "[*] user.cfg: $USER_CFG"

BACKUP="$USER_CFG.orig-backup"
if [ ! -f "$BACKUP" ]; then
    cp "$USER_CFG" "$BACKUP"
    echo "[*] Backed up original to $BACKUP (first run only - never overwritten again, kept purely as an undo copy)"
fi

# Idempotent, and rebuilt from the CURRENT file, not the backup: strip
# any existing OverrideAddress line (from a previous run of this script,
# or one added by hand) and put the fresh one back at the top, but
# otherwise preserve whatever's in user.cfg right now. Rebuilding from
# $BACKUP every time instead would silently discard any other setting a
# player changed since the first run (graphics options, etc.) - $BACKUP
# is an undo copy for disaster recovery, not the source of truth for
# routine re-runs.
TMP="$(mktemp)"
{
    echo "OverrideAddress=\"$PUBLIC_IP\""
    grep -v '^OverrideAddress=' "$USER_CFG" || true
} > "$TMP"
mv "$TMP" "$USER_CFG"

echo "[*] Done. OverrideAddress=\"$PUBLIC_IP\" is now the first line of user.cfg."
