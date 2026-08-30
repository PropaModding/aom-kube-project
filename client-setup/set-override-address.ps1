# Detects this machine's current public IP and writes it into classic
# AoM's own startup\user.cfg as OverrideAddress - confirmed live
# 2026-08-30 (see nat-lab/design.md's "Confirmed live, 2026-08-30"
# section) to be the ONLY mechanism of the ones tested that actually
# changes what the game reports as its own address in the DirectPlay8
# handshake. Four different command-line launch-parameter syntaxes
# (OverrideAddress="...", +OverrideAddress="...", OverrideAddress=...
# with no quotes, and the full +OverrideAddress/+hostPort/
# +directIPConnectivity combination) were all confirmed reaching the
# game process's real argv with zero effect on the wire, tested against
# the Wine build this project runs - the command-line approach may or
# may not behave differently on native Windows with the official exe,
# but user.cfg is the confirmed-working mechanism either way, so this
# script uses that rather than re-opening the argv question here too.
#
# For a real player running the classic (non-Voobly) AoM client
# natively on Windows. See set-override-address.sh in this same
# directory for the Wine/Linux equivalent.
#
# WHY THIS HAS TO BE RE-RUN, NOT SET ONCE: almost nobody has a static
# home IP - see docs\player-connection-requirements.md's own "Dynamic
# IP" section. Re-run this before every play session. Running it with
# an unchanged IP is a harmless no-op; it always rewrites the line
# either way, cheap enough not to bother checking first.
#
# Usage (PowerShell):
#   .\set-override-address.ps1
#   .\set-override-address.ps1 -UserCfgPath "D:\Games\Age of Mythology\startup\user.cfg"
#
# If PowerShell blocks the script with an execution-policy error, either
# right-click the file and choose "Run with PowerShell", or run once
# from an existing PowerShell window:
#   powershell -ExecutionPolicy Bypass -File .\set-override-address.ps1

param(
    [string]$UserCfgPath = ""
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($UserCfgPath)) {
    # With Titans (My Documents-based, added specifically so multiple
    # Windows users on one machine get separate settings) vs. without
    # (alongside the exe, under Program Files) - both real,
    # community-documented locations, tried in that order.
    $candidates = @(
        (Join-Path $env:USERPROFILE "My Documents\My Games\Age of Mythology\startup\user.cfg"),
        (Join-Path $env:USERPROFILE "Documents\My Games\Age of Mythology\startup\user.cfg"),
        "C:\Program Files\Microsoft Games\Age of Mythology\startup\user.cfg",
        "C:\Program Files (x86)\Microsoft Games\Age of Mythology\startup\user.cfg"
    )
    foreach ($c in $candidates) {
        if (Test-Path $c) {
            $UserCfgPath = $c
            break
        }
    }
}

if ([string]::IsNullOrWhiteSpace($UserCfgPath) -or -not (Test-Path $UserCfgPath)) {
    Write-Error @"
Couldn't find user.cfg automatically. Pass its path explicitly:
  .\set-override-address.ps1 -UserCfgPath "C:\path\to\Age of Mythology\startup\user.cfg"
(Create an empty user.cfg there first if one doesn't exist yet - the
game does read it once it's present, confirmed live 2026-08-30, see
nat-lab\design.md.)
"@
    exit 1
}

Write-Host "[*] Detecting current public IP..."
$publicIp = $null
foreach ($svc in @("https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com")) {
    try {
        $result = (Invoke-RestMethod -Uri $svc -TimeoutSec 5).ToString().Trim()
        if ($result -match '^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$') {
            $publicIp = $result
            break
        }
    } catch {
        # try the next service
    }
}

if (-not $publicIp) {
    Write-Error "Couldn't detect a public IP from any lookup service (api.ipify.org, ifconfig.me, icanhazip.com all failed or returned something unexpected). Check your internet connection."
    exit 1
}

Write-Host "[*] Public IP: $publicIp"
Write-Host "[*] user.cfg: $UserCfgPath"

$backupPath = "$UserCfgPath.orig-backup"
if (-not (Test-Path $backupPath)) {
    Copy-Item -Path $UserCfgPath -Destination $backupPath
    Write-Host "[*] Backed up original to $backupPath (first run only - never overwritten again, kept purely as an undo copy)"
}

# Idempotent, and rebuilt from the CURRENT file, not the backup: strip
# any existing OverrideAddress line (from a previous run of this
# script, or one added by hand) and put the fresh one back at the top,
# but otherwise preserve whatever's in user.cfg right now. Rebuilding
# from the backup every time instead would silently discard any other
# setting a player changed since the first run - the backup is an undo
# copy for disaster recovery, not the source of truth for routine
# re-runs.
$existingLines = @()
if (Test-Path $UserCfgPath) {
    $existingLines = Get-Content -Path $UserCfgPath | Where-Object { $_ -notmatch '^OverrideAddress=' }
}
$newContent = @("OverrideAddress=`"$publicIp`"") + $existingLines
Set-Content -Path $UserCfgPath -Value $newContent -Encoding ASCII

Write-Host "[*] Done. OverrideAddress=`"$publicIp`" is now the first line of user.cfg."
