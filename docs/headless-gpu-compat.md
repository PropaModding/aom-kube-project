# Headless GPU Compatibility (Wine + Xvfb + Software Rendering)

## Problem

`aom-headless` (built from `dockerfile.k8s`, no `/dev/dri`, no host X11,
`LIBGL_ALWAYS_SOFTWARE=1`) crash-looped every time: EULA screen → Accept →
Titans splash → **"Initialization Failed"** dialog → OK → the wine process
exits → Kubernetes restarts the container → back to the EULA screen.

## Investigation

Killing and manually re-running the game inside the container isn't
viable here — the container's PID 1 (`tini`) directly supervises the
launched wine process, so killing it kills the container. Instead, the
`WINEDEBUG` env var was added to the Deployment (`+d3d,+wined3d,+d3d_shader,
+dxgi,fixme-all`) so the natural launch produced a full trace via
`kubectl logs`.

That trace showed:
- Xvfb, the GPU vendor/device ID spoof, and the video-memory override were
  all applied correctly (`wined3d_get_user_override_gpu_description
  Overriding vendor/device PCI ID...`, `wined3d_driver_info_init
  Overriding amount of video memory...`).
- Adapter creation and every capability query (`GetDeviceCaps`,
  `CheckDeviceType`, `CheckDeviceFormat`, `CheckDepthStencilMatch`)
  completed cleanly, for both `WINED3D_DEVICE_TYPE_HAL` and `_REF`.
- **`CreateDevice` was never called.** The wined3d object is destroyed
  (`wined3d_decref ... refcount to 0`) immediately after the capability
  checks. No `err:` line appears anywhere in ~9000 lines of trace.

That combination — capability negotiation succeeds, but the game aborts
before ever asking Direct3D to actually create a device, with zero Wine-
level errors — means the failure isn't a Wine/API problem at all. It's the
**game's own application logic** rejecting the environment, for a reason
invisible at the Direct3D trace level. Two plausible generic fixes
(`winetricks vcrun2015`, the `VideoMemorySize` registry key, both present
in the older working `dockerfile.headless`) were tried and both left the
exact same failure — confirming this wasn't a missing-runtime issue.

## Root cause

AoM ships its own plaintext hardware-compatibility database inside the
game files, at `aom_files/gfxconfig/`: one `.gfx` file per PCI vendor ID
(`0x10de_nvidia.gfx`, `0x8086_intel.gfx`, etc.), each mapping specific PCI
device IDs to a named rendering profile (`geforce3.gfx`, `i845.gfx`, ...)
that configures feature toggles like hardware vs. software transform &
lighting, supported resolutions, and texture-stage shader programs.

`dockerfile.k8s` was spoofing the Wine registry's `VideoPciVendorID` /
`VideoPciDeviceID` as `0x10de` / `0x0402` — an NVIDIA ID picked without
checking it against this table. **It isn't in there.**
`aom_files/gfxconfig/0x10de_nvidia.gfx`'s device list tops out at
GeForce4-era IDs (last entries: `0x0170`-`0x0179`, GeForce4 MX/Go); `0x0402`
is a GeForce 6-series ID from 2004, newer than this game's compatibility
data. The lookup doesn't land on a real entry, and the game's own startup
logic bails with "Initialization Failed" — before Wine ever gets asked to
create a device, which is exactly why no wined3d-level error shows up.

## Fix

Spoof **Intel `0x8086` / `0x2562`** instead — the `82845G/GL Graphics
Controller` entry in `aom_files/gfxconfig/0x8086_intel.gfx`, which maps to
`i845.gfx`. That profile is the important part: it sets
`softwareTNL=software.gfx` and has **no `hardwareTNL` entry at all** — it's
the profile this game ships specifically for GPUs with no hardware
transform & lighting. That's exactly what Xvfb + llvmpipe (a software GL
rasterizer) provides. Any real 3D-card profile (`geforce3.gfx` included)
assumes genuine hardware-accelerated semantics that a software renderer
can't fully satisfy at the level this 2002-era engine expects.

Changed in `dockerfile.k8s`:
```
VideoPciVendorID: 0x10de -> 0x8086
VideoPciDeviceID: 0x0402 -> 0x2562
```

Confirmed fixed: both `aom-headless` and `aom-client` now reach the actual
main menu and stay up (0 restarts), where before they crash-looped 100%
of the time.

## Why this matters beyond this one bug

- This is the first time the headless/no-GPU path (`dockerfile.k8s`) has
  actually been confirmed to work — previously only the real-GPU-
  passthrough path (`dockerfile.head` + `run-aom-head.sh`, using the host's
  real `/dev/dri`) had ever been verified. CLAUDE.md's goal of "run
  headlessly in a Kubernetes pod" was aspirational until this fix.
- General pattern for anyone hitting "Initialization Failed" under Wine +
  software rendering with this game: don't pick a GPU vendor/device ID
  that merely "sounds plausible" — check `aom_files/gfxconfig/<vendor>.gfx`
  for a device ID whose profile uses `softwareTNL` with no `hardwareTNL`,
  and spoof that pair instead.

## How to reproduce this failure mode / verify the fix

Set `WINEDEBUG=+d3d,+wined3d,+d3d_shader,+dxgi,fixme-all` as an env var on
the pod (not via `kubectl exec` — that kills the container's supervised
process) and read `kubectl logs`. The signature of this exact failure is
`wined3d_decref ... refcount to 0` immediately following a full set of
capability checks, with no `CreateDevice` call in between and no `err:`
lines anywhere in the trace.
