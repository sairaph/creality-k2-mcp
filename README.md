# creality_k2_mcp

Control a Creality K2-family 3D printer from Claude Desktop, Claude Code and
other MCP clients: check status and job progress, browse and inspect gcode
files, watch the onboard camera live or grab a snapshot, record video or a
timelapse, and - if you turn it on - start/pause/resume/cancel prints, set
temperatures, fans, speed and flow, and manage files on the printer.

Every write is state-aware and fails closed: the server reads the printer's
actual state before acting, never breaks a running print, and never changes
a persistent printer setting. See [Safety model](docs/safety.md) for the
full design.

## Supported printers

Creality K2 family printers running Moonraker/Klipper, reached over the
local network. Tested on a **K2 base (model F021)** with a CFS. The CFS
(multi-material) is supported as of **v0.2.0**, see [CFS](#cfs-multi-material)
below.

## Install

Windows (PowerShell):

```powershell
irm https://github.com/sairaph/creality_k2_mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/sairaph/creality_k2_mcp/releases/latest/download/install.sh | sh
```

The installer downloads `creality_k2_mcp`, verifies its SHA256 checksum, puts
it on your `PATH`, and starts the install wizard, which has two steps before
anything is written to disk:

1. **Printers** - scans the local network for Creality K2 printers, lets you
   review what was found and add one by host manually, and choose which
   printers are enabled and which have control turned on
   (`allow_control` - see [Safety model](docs/safety.md)). Control is off by
   default for a newly added printer.
2. **Settings** - the tool preset (`monitor`, `camera`, or `control`) and the
   mid-print setpoint bands and idle-heat timeout, with sensible defaults
   shown and explained.

After that, a **Client selection** step lets you pick which AI clients on
this machine to register the server with (already-configured clients are
pre-checked), and an **Apply** step writes everything - the registry, the
settings, and each selected client's own config, idempotently.

Run `creality_k2_mcp doctor` at any time to check the installation, and
`creality_k2_mcp update` to update to the latest release. See
[docs/cli.md](docs/cli.md) for every command, and
[docs/configuration.md](docs/configuration.md) for unattended installs and
manual client configuration.

## Quick start

1. Run the installer above (or `creality_k2_mcp install` if you already have
   the binary) and follow the wizard.
2. Restart your AI client.
3. Ask it something like "what's my printer doing right now?" - it will call
   `list_printers` and `get_printer_status` to find out.
4. Ask to see the chamber camera - it will call `get_camera_snapshot`, or
   offer to open a continuously updating live view in your browser with
   `open_camera_view`.
5. If you enabled the `control` preset, you can ask it to start, pause,
   resume or cancel a print, adjust temperatures or fans, and more - see
   [docs/tools.md](docs/tools.md) for the full list.

## Tool presets

Which tools get registered at all is controlled by a preset
(`config.toml`'s `tools.preset`), with per-tool overrides available on top:

| Preset | Registers |
| --- | --- |
| `monitor` | Status, filaments (CFS and side spool slots, catalog), files (read-only), job history, console - read-only only |
| `camera` (default) | Everything in `monitor`, plus snapshot, live view, and recording |
| `control` | Everything in `camera`, plus every control (write) tool |

Even with the `control` preset registered, a write is still refused for a
specific printer unless that printer's own `allow_control` is set in the
registry - set per printer in the install wizard, the TUI, or with
`creality_k2_mcp printers control on <id>`. This is a second, independent
gate on top of which tools exist at all: registering control tools does not
by itself let the AI touch any given printer. See
[docs/configuration.md](docs/configuration.md) for the full settings
reference and [docs/tools.md](docs/tools.md) for every tool.

## Safety model

- **State-aware, fail closed.** Every write checks the printer's actual,
  freshly read state first; an unrecognized or unreachable state blocks
  every write (reads are always allowed).
- **Never breaks a running print.** Pausing, resuming and cancelling are
  only available in the right states, mid-print setpoint changes are bounded
  to a configurable band, and a change while paused is blocked outright
  since the printer's own resume macro would silently undo it.
- **Never changes a persistent printer setting.** The server can only ever
  send a small, fixed, explicitly enumerated set of commands - there is no
  path to arbitrary G-code, a macro, calibration, or EEPROM/config writes.
- **Two-step `confirm_token` confirmation** for destructive or hard-to-verify
  actions (resume, cancel, exclude object, delete a file, overwrite a file,
  delete a recording): propose, then confirm, with the printer's state
  re-checked immediately before the second call actually sends anything.
- **Checking the bed is clear is yours.** Nothing the server can read
  confirms the bed is physically clear before a print starts; the AI is
  instructed to ask you first, and that check is genuinely load-bearing.
- **Idle heat has an automatic shutoff.** Setting a heater while idle
  requires the background daemon to be running; it arms a watchdog that
  turns the heater back off automatically after a configurable timeout.

Full detail: [docs/safety.md](docs/safety.md).

## CFS (multi-material)

With a CFS connected, the server can read every slot, relabel a slot, and
start a multi-colour print from a file that is already on the printer, all
through the printer's own definitions (nothing of Creality's filament catalog
is built into this server; names always come from the printer).

**What works**

- `get_filaments` (and `creality_k2_mcp filaments`) shows each CFS slot and the
  side spool: the stored definition (brand, material, colour, nozzle range),
  whether it can be edited, and the printer's own refill groups.
  `list_filament_catalog` lists the definitions a slot can be set to.
- `set_filament_definition` relabels one slot (only while idle and the CFS is
  at rest). It never moves filament, and it reads the result back from both the
  printer and Moonraker.
- `start_print` with a CFS is a two-step **mapping proposal**: the first call
  sends nothing and shows which slot each filament of the file will use (Creality's
  own matching algorithm), with a warning when the printer may run a different
  slot of the same refill group. The AI must show you that mapping and ask you to
  confirm that each slot really holds that spool and that the bed is clear; only
  then does the second call, with the token, send the print. The reply says
  `sent`, never `started`: the printer then runs a self-test of several minutes.
- `pause_print` and `cancel_print` are never blocked by a CFS signal.
  `set_fan_speed`, `set_speed_factor` and `exclude_object` work during a print
  while the CFS reports no error. `resume_print` works only for a clean pause this
  server issued.

**What stays at the printer**

- Clearing a CFS error or runout, RFID-tagged spools, loading and unloading
  filament, changing the nozzle temperature or flow during a CFS print, and starting a
  print from the side spool while a CFS is connected (not verified: with a CFS the side
  spool is not in the feed path).

**Verified on a real printer** (supervised session, 2026-09-29): a CFS start with a
  requested map, stopping during the start self-test, pause and resume of a CFS print, and
  editing the side spool (edit and restore, confirmed on both channels). Pause and resume
  are slow on a K2: the pause takes about 17 s, and a resume runs the whole RESUME routine
  (reheat, purge, wipe) for 1-2 minutes, so `resume_print` replies `resuming` and you follow
  it with `get_printer_status`. Cancelling during the start self-test sends Creality's own
  stop and the printer is idle about a minute later.

**What the server cannot detect** (so it asks you): a tool change during a print,
whether a slot physically holds filament, and a spool being pre-loaded or an RFID
scan in progress.

The exact per-tool rules and the reasons behind them are in
[Safety model](docs/safety.md#cfs-multi-material-support).

## Camera

- **Snapshot** - one still frame, `get_camera_snapshot` or
  `creality_k2_mcp snapshot`.
- **Local live view** - a continuously updating browser page,
  `open_camera_view` or `creality_k2_mcp camera open`; local to this
  computer only, gated by a per-session token.
- **Recording** - video (fragmented MP4, crash-safe) or timelapse (one still
  per layer change), `start_recording`/`stop_recording`/`list_recordings`/
  `delete_recording` or `creality_k2_mcp camera record|stop|recordings|delete`.

All of it shares one connection per printer through a small background
daemon that starts automatically the first time it's needed. Full detail,
file locations, limits and troubleshooting: [docs/camera.md](docs/camera.md).

## CLI, TUI, doctor, uninstall

`creality_k2_mcp` is one binary: the MCP server, an install wizard, a TUI,
and a set of one-shot commands (`printers`, `status`, `filaments`, `snapshot`,
`camera`, `doctor`, `update`) that share their logic with the MCP tools, so they can
never disagree. Run it with no arguments in a terminal to open the TUI.

```sh
creality_k2_mcp doctor        # diagnose the installation
creality_k2_mcp printers      # list registered printers and their state
creality_k2_mcp uninstall     # remove from every registered AI client
```

Full command reference: [docs/cli.md](docs/cli.md).

## Documentation

| Guide | Contents |
| --- | --- |
| [Tools](docs/tools.md) | Every MCP tool: purpose, preset, parameters, state requirements, side effects, `confirm_token` flow |
| [Configuration](docs/configuration.md) | Printer registry, `config.toml`, environment variable override, MCP client configuration |
| [Safety model](docs/safety.md) | The full design behind the summary above |
| [Camera](docs/camera.md) | Live view, snapshot, recording and timelapse in detail, plus troubleshooting |
| [CLI](docs/cli.md) | Every command, flag, example and exit code |

## License

MIT, see [LICENSE](LICENSE).

creality_k2_mcp embeds a WebAssembly build of Cisco's OpenH264 H.264 decoder
(BSD-2-Clause, decoder-only, compiled from source) and uses the wazero
WebAssembly runtime (Apache-2.0) to run it, for camera snapshots and
timelapse. Their license notices are reproduced in full in [NOTICE](NOTICE).
This self-compiled decoder is not covered by Cisco's binary patent license
for OpenH264; H.264 (AVC) patent terms may apply to your use.
