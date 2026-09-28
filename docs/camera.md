# Camera

creality_k2_mcp talks to the K2's onboard chamber camera over WebRTC and can
show a live view in your browser, take snapshots, and record video or
timelapse stills. This page covers how that system works end to end: the
shared connection, where files land, the numeric limits, and troubleshooting.
For each tool's exact parameters see [tools.md](tools.md); for the matching
CLI commands see [cli.md](cli.md).

## Overview

All camera access goes through a small background daemon
(`creality_k2_mcp camera serve`, started automatically the first time it is
needed - opening the live view, taking a snapshot, starting a recording, or
arming the idle-heat watchdog). The daemon owns exactly one WebRTC connection
per printer's camera and shares it across every consumer at once: the browser
live view, a snapshot request, and an active recording never open separate
connections, so using more than one of them at the same time adds no extra
load on the printer beyond the first.

Inside the daemon, each printer's camera connection keeps a rolling buffer of
recent video (a "golden buffer": every access unit received since the last
complete keyframe, bounded to 8 MiB or 10 seconds, whichever comes first) so
a snapshot can usually be served from data already in memory rather than
waiting for a fresh keyframe. The connection is kept open for 60 seconds
after the last consumer leaves (the keep-warm window) so a second snapshot
or viewer request shortly after the first does not pay the cost of
reconnecting.

## Snapshot

`get_camera_snapshot` (MCP tool) and `creality_k2_mcp snapshot` (CLI) both
capture one still frame through the same daemon path (`Hub.Snapshot`), with
the printer's current state checked concurrently rather than beforehand, so
the two run in parallel and the call's total latency is close to whichever
one takes longer. Live measurements against a real K2 (dev_docs/
item51-stream-snapshots.md) found roughly 2.3 seconds for a cold connection
and 1.7-2.0 seconds for a warm one (repeated snapshots within the keep-warm
window), with occasional outliers when the connection needs to recover from
a lost keyframe. The whole snapshot tool call is bounded to 20 seconds inside
the daemon (`hubSnapshotBudget`) within a 25 second budget for the MCP tool
itself.

The MCP tool re-encodes the capture as JPEG and, if it is still over the
result's byte budget, downscales and lowers quality in steps until it fits
(see [tools.md](tools.md#get_camera_snapshot-camera) for the exact ladder).
The `snapshot` CLI command has no such cap: it writes the full-resolution
capture as a JPEG at quality 92.

## Local live view

`open_camera_view` (MCP tool) and `creality_k2_mcp camera open` (CLI) both
return a URL for a continuously updating browser view, served by the daemon
over Media Source Extensions (MSE) as fragmented MP4. The viewer's HTTP
server binds `127.0.0.1` only - it is never reachable from another device or
over the network, whatever the machine's own network configuration - and
every route requires a random, per-daemon-session access token as a `token`
query parameter (`?token=...` in the URL). The server also checks the
request's `Host` header names itself before checking the token, to reject
DNS rebinding attempts that could otherwise reach the loopback-only server
from a malicious page open in the same browser.

The token is generated fresh each time the daemon starts and is valid for as
long as that daemon process keeps running; it is not saved anywhere and
stops working once the daemon exits (normally after a short idle period with
no open viewer, recording, or armed heater). The page itself needs a browser
with MSE support (every current major browser). Any browser tab, not just
the one that first opened the URL, can use it as long as it has the token.

If the page does not show video right away, this is expected while a
keyframe is still being recovered (see Troubleshooting below); the page
keeps waiting and starts playing on its own once one arrives, with no need
to reload it. If the connection drops mid-stream, the page's own reconnect
logic backs off with jitter (capped, resetting once a stream plays again)
rather than hammering the daemon.

## Recording

`start_recording` / `stop_recording` / `list_recordings` / `delete_recording`
(MCP tools) and `creality_k2_mcp camera record|stop|recordings|delete` (CLI)
manage recordings. Only one recording (video or timelapse) may be active per
printer at a time.

### Video mode (default)

Writes a fragmented MP4 file to:

```
~/.creality_k2_mcp/recordings/<printer-id>/<timestamp>.mp4
```

Fragments are written and fsynced as they go, so the file stays playable up
through the last flushed fragment even if the process is killed outright -
nothing is buffered only in memory waiting for a clean shutdown. Recording
metadata (a sidecar JSON file) is written with the same atomic-write,
fsync-then-rename discipline, so a crash mid-write leaves the previous
metadata intact rather than a truncated file.

Default maximum duration: **12 hours** (`max_duration_minutes`, overridable
per call). Default `until`: `"stopped"` - the recording only stops when
`stop_recording` is called, the maximum duration is reached, or the disk
safety threshold is hit.

### Timelapse mode

Writes one full-resolution JPEG still (quality 85) per printer layer change
to its own directory under the recordings tree, polling the printer's
current layer every 2 seconds and falling back to a fixed 30-second interval
if the current layer cannot be determined. Default maximum duration:
**48 hours**. Default `until`: `"print_end"` - besides `stop_recording` and
the maximum duration, a timelapse also stops on its own once the job
finishes (completed, cancelled, errored) or the printer goes idle after
having been seen printing.

### Disk safety

Both modes check free space on the recordings volume:

| Threshold | Behavior |
| --- | --- |
| Below 1 GiB free | Refuses to **start** a new recording |
| Falls below 512 MiB free while active | **Stops** the active recording (`stop_reason` explains why) |

### If the printer's camera becomes unreachable mid-recording

A `print_end`-bound recording keeps running through a temporary network
blip: an unreachable printer while polling for the job's end state is never
treated as the job having ended, so a network hiccup does not silently
truncate the recording. The recording is still bounded by its own
`max_duration_minutes` and the disk-safety check above, both of which record
the real reason in the recording's `stop_reason`. A live view session in the
browser applies its own reconnect backoff (see Local live view above) rather
than giving up.

## Disk usage and other limits

| Limit | Value |
| --- | --- |
| Golden buffer size (per printer, in the daemon) | 8 MiB |
| Golden buffer span (per printer, in the daemon) | 10 seconds |
| Connection keep-warm window after the last consumer leaves | 60 seconds |
| Snapshot wait budget inside the daemon | 20 seconds |
| `get_camera_snapshot` MCP tool's own overall budget | 25 seconds |
| PLI (keyframe request) rate, coalesced across every consumer | at most 1 per second |
| Video recording default max duration | 12 hours |
| Timelapse recording default max duration | 48 hours |
| Timelapse still quality | JPEG quality 85 |
| Recording disk-safety start threshold | 1 GiB free |
| Recording disk-safety stop threshold | 512 MiB free |
| Recording id path segment length | 64 characters, `[A-Za-z0-9._-]` only, no path traversal, Windows reserved device names rejected |

`list_recordings` also reports the recordings directory's total disk usage
in bytes, so you can check accumulated size without inspecting the
filesystem directly.

## Troubleshooting

### "The printer's camera is not sending video right now" / a keyframe-wait timeout

This message means the daemon waited for a complete video keyframe and none
arrived in time. Earlier investigation (dev_docs/camera-keyframe-rca.md)
first suspected the K2 itself pausing its camera stream while idle; a
follow-up, more careful investigation withdrew that theory - it found no
correlation between the printer's own reported camera-activity fields and
the stalls. The confirmed root cause instead was **packet loss inside a
large keyframe with no retransmission**: a keyframe spans many RTP packets,
and losing even one of them with no NACK-based recovery meant the whole
keyframe could never be assembled, however long the wait continued. This is
now fixed (a NACK generator/responder was added), but on a busy or noisy Wi-Fi
link a similar wait can still occasionally happen and usually clears on retry.

What to check when you see this:

- The printer is powered on and its screen is responsive.
- The printer is reachable on the LAN generally (`get_printer_status` /
  `creality_k2_mcp status` answers normally).
- The camera's own port (8000) is reachable - a firewall or an unusually
  restrictive router/AP client-isolation setting can block WebRTC signaling
  or media even when Moonraker's port answers fine.
- Simply trying again: a keyframe wait is very often transient. Both the
  live view and a fresh snapshot request recover on their own once a
  complete keyframe arrives.

### Windows Firewall prompt for `creality_k2_mcp.exe`

The first time the background daemon opens its local viewer HTTP listener or
its outbound WebRTC connection to a printer's camera, Windows may show a
firewall prompt asking whether to allow `creality_k2_mcp.exe` to communicate
on private and/or public networks. The viewer's own HTTP server only ever
binds `127.0.0.1` (see Local live view above), so allowing it is safe with
respect to that listener specifically; the outbound WebRTC connection to the
printer's camera needs the printer's network (typically your private/home
network) to be allowed. If you dismiss the prompt without allowing it,
camera features can fail intermittently or outright depending on your
firewall's default-deny posture - re-run the failing command and allow the
prompt, or add an exception for the binary manually in Windows Defender
Firewall settings.

### Recovering from a network loss during recording or streaming

- **Recording bound to `print_end`**: keeps running through a temporary
  printer/network outage (see "If the printer's camera becomes unreachable
  mid-recording" above); check `list_recordings` afterward for its
  `stop_reason` if it did stop.
- **Live view in the browser**: reconnects automatically with backoff; no
  action needed beyond leaving the tab open.
- **A `stopped`-mode recording**: is not tied to job state at all, so a
  network loss only affects it if the daemon itself cannot reach the printer
  for longer than makes sense to continue; check `list_recordings` for its
  current status.

### General camera checks

- `creality_k2_mcp doctor` reports whether the background daemon is running
  and whether the local viewer page answers, without starting the daemon
  just to check it.
- `get_printer_status` / `creality_k2_mcp status` reports the daemon's live
  camera connection state for a printer when the daemon can be reached
  (last time media was received, whether a connection is currently open),
  again without starting the daemon just to check.
- This camera has no infrared or night-vision sensor: a mostly dark or black
  frame most often just means the chamber light is off, not a fault. See
  `set_light` in [tools.md](tools.md) if control tools are enabled, or turn
  the light on at the printer.
- The camera stream itself has no authentication on the printer's own LAN
  side: anyone on the same local network as the printer can view its raw
  camera feed directly (independent of creality_k2_mcp's own local-only,
  token-gated browser viewer).
