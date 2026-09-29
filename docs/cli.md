# CLI

`creality_k2_mcp` is a single binary that is at once the MCP server, an
install wizard, a TUI, and a set of one-shot commands for scripting or quick
checks from a terminal. Every one-shot command shares its business logic
with the MCP tools and the TUI (the same `internal/wizard`, `internal/
discovery`, `internal/printerstate` and `internal/camera` calls), so a CLI
command and the equivalent MCP tool call can never disagree about a
printer's state.

Running the binary with no arguments in an interactive terminal opens the
TUI. Anywhere else (an AI client spawning it, a pipe, a script) it behaves
as `mcp` and serves the Model Context Protocol over stdio.

## Exit codes

Every command below follows the same convention:

| Code | Meaning |
| --- | --- |
| 0 | Success (including `-h`/`--help`, which prints usage to stdout and exits 0) |
| 1 | The command ran but failed: an unreachable printer, a rejected write, a filesystem error |
| 2 | A usage error: wrong number of arguments, an unknown flag or subcommand, missing required input |

## Global commands

### `creality_k2_mcp` / `creality_k2_mcp mcp`

Runs the MCP server. This is the default when standard input is not a
terminal (an AI client always sees this), and can also be requested
explicitly with `mcp`.

```sh
creality_k2_mcp mcp
```

There is also an HTTP transport for advanced setups; see
[configuration.md](configuration.md) for how to select it.

### `creality_k2_mcp install`

Runs the interactive install wizard (or, with `--yes`, an unattended
install). The wizard has these steps in order:

1. **Printers** - scan the LAN for Creality K2 printers, review what was
   found, add one by host manually, and choose which registered printers are
   enabled and which have control turned on.
2. **Settings** - the tool preset (`monitor`, `camera`, `control`) and the
   mid-print setpoint bands and idle-heat timeout, with the current defaults
   shown.
3. **Client selection** - pick which AI clients on this machine to register
   the server with (Claude Desktop, Claude Code, Cursor, VS Code, Windsurf,
   Zed, and more; already-configured clients are pre-checked).
4. **Apply** - writes the registry and settings files, then registers the
   server in each selected client's own config, idempotently (existing
   entries and other servers are preserved).

Nothing is written to disk before the Apply step runs.

```sh
creality_k2_mcp install
creality_k2_mcp install --yes --all
creality_k2_mcp install --client claude-code,claude-desktop
```

| Flag | What it does |
| --- | --- |
| `--yes` | Skip the interactive flow: run unattended with detected printers/clients |
| `--all` | Register with every supported client, detected or not (unattended mode) |
| `--client <id>[,<id>...]` | Register with these clients only (unattended mode); repeatable or comma-separated |
| `--dry-run` | Show what would change without writing anything |

`--email` and `--token` are accepted for consistency with other mcp-wizard
projects but do nothing here: this server has no login concept.

Exit code: 0 on success (including "every client already configured"); 1 if
applying a client config failed; 2 if `--client` names a client that was not
detected.

### `creality_k2_mcp add`

The project-scoped equivalent of `install`: registers the server (and,
non-interactively, discovered printers) under the current directory's own
`.creality_k2_mcp/` folder and AI client configs, instead of the per-user
global ones. Same steps and flags as `install`.

```sh
cd my-project
creality_k2_mcp add --yes
```

### `creality_k2_mcp uninstall`

Removes the server from every AI client it is currently registered with (or,
with `--client`, only the named ones). This never deletes the registry or
settings files, only each client's own configuration entry.

```sh
creality_k2_mcp uninstall
creality_k2_mcp uninstall --client claude-desktop
```

Exit code: 0 on success or if nothing was configured; 1 if removing a
client's config failed.

### `creality_k2_mcp doctor`

Diagnoses the installation. Runs, in order:

- **Executable** and **PATH** checks (is the binary where it should be, is
  its directory on `PATH`).
- **AI clients** - which clients currently have this server registered.
- **Update** - whether a newer release is available (skipped for a
  from-source `dev` build).
- **Registry** - which registry file this process would use (project or
  global, or the `K2_MCP_HOST` environment override), how many printers it
  holds, how many are enabled, and any entry dropped at load (invalid, or a
  duplicate hostname).
- **Settings** - `config.toml`'s path, tool preset, configured bands and
  idle-heat minutes, and any `tools.overrides` entry that names no real
  tool.
- **Printers** - for every enabled printer, concurrently: Moonraker
  reachability and derived activity state, port 9999 reachability and model
  identification, the camera signaling endpoint's reachability, and whether
  `allow_control` is set. Each probe carries a concrete hint on failure.
- **Camera/idle-heat daemon** - whether the background daemon is currently
  running, how many heaters it has armed across the registry's enabled
  printers, and whether the local camera viewer page answers. This check
  never starts the daemon just to look at it: a stopped daemon is reported
  plainly, with a note that idle heating is refused while it is down and
  that opening the camera view or starting a recording will start it
  automatically the next time either is used.

```sh
creality_k2_mcp doctor
```

Exit code follows the worst result across every check (0 if everything is
OK or only warnings were found, non-zero if any check failed); every line is
printed either way.

### `creality_k2_mcp update`

Checks the latest GitHub release and updates in place if a newer one exists.

```sh
creality_k2_mcp update
```

A from-source `dev` build (no version baked in at release time) prints a
message and does nothing, since there is nothing to compare against. `update
--from <file>` is used internally by the install script after it has already
downloaded and verified a new binary; there is normally no reason to pass
it by hand.

### `creality_k2_mcp version`

Prints the running binary's version and exits 0.

```sh
creality_k2_mcp version
```

### `creality_k2_mcp help` / `-h` / `--help`

Prints the top-level usage (every command listed above plus the one-shot
commands below) to stdout and exits 0.

## One-shot commands

These read from and write to the same registry, settings and printer state
the MCP tools use, without starting a server or opening the TUI.

### `creality_k2_mcp printers`

Lists, scans for, and manages registered printers.

```
usage: printers [scan | add <host> | enable <id> | disable <id> | control on|off <id>]
  printers                    list registry entries: enabled, control, reachability
  printers scan                scan the LAN; never changes the registry
  printers add <host>          probe one host and register it (enabled, control off)
  printers enable|disable <id> enable or disable a registered printer
  printers control on|off <id> allow or refuse control (only while enabled)
```

`printers` alone lists every registered printer with a live reachability and
state probe (bounded concurrency, 5 second per-printer timeout):

```sh
$ creality_k2_mcp printers
ID        NAME      HOST           ENABLED  CONTROL  REACHABLE  STATE
k2-5885   K2-5885   192.168.1.102  yes      no       yes        idle
```

`printers scan` scans the LAN and reports what it found, without ever
changing the registry:

```sh
$ creality_k2_mcp printers scan
scanning: 254/254 hosts, 1 found
HOST           HOSTNAME  MODEL  IDENTIFIED  REGISTERED
192.168.1.102  K2-5885   F021   yes         no

This never changes the registry. Run `install` (or `add` in a project) to
register every printer found here, `printers add <host>` for one specific
host, or use the TUI.
```

`printers add <host>` probes exactly one host and, if it is a K2, registers
it (enabled, control off by default):

```sh
$ creality_k2_mcp printers add 192.168.1.102
Added k2-5885 (192.168.1.102), enabled, control off.
Run `printers control on k2-5885` to allow the AI to control it.
Registry: /home/me/.creality_k2_mcp/printers.json
```

`printers enable <id>` / `printers disable <id>` toggle whether a printer's
tools are usable at all; disabling a printer also turns its control off,
since a disabled printer can never keep control.

`printers control on <id>` / `printers control off <id>` toggle
`allow_control` for one printer (it must already be enabled to turn control
on). Turning control on prints a warning naming exactly what it grants
before it takes effect:

```sh
$ creality_k2_mcp printers control on k2-5885
Control lets the AI change this printer through the MCP tools: start, pause, resume
and cancel prints; upload and delete gcode files; set nozzle and bed temperature; fans,
speed and flow factors; the chamber light; and exclude objects. Only turn this on for a
printer, and an AI client, you trust with those actions.
k2-5885: control on.
Registry: /home/me/.creality_k2_mcp/printers.json
```

Exit codes: 0 on success; 1 for a runtime failure (unreachable host,
printer not found, control requested on a disabled printer); 2 for a usage
error (wrong argument count, unknown subcommand).

### `creality_k2_mcp status [printer] [--json]`

Shows one printer's derived state: the same
`printerstate.Take`/`DeriveActivityState`/`BuildStateBlock` pipeline
`get_printer_status` uses, so the two can never disagree.

`printer` is optional when exactly one printer is enabled; otherwise it is
required (id or name, case insensitive).

```sh
$ creality_k2_mcp status
K2-5885 (192.168.1.102)
State: idle   bucket: I   class: available
Nozzle: 24.1/0.0 C
Bed: 23.8/0.0 C
CFS connected: no
9999 reachable: yes

$ creality_k2_mcp status k2-5885 --json
{
  "printer_id": "k2-5885",
  "activity_state": "idle",
  ...
}
```

| Flag | What it does |
| --- | --- |
| `--json` | Print the full StateBlock as JSON instead of the human-readable summary |

Exit codes: 0 on success; 1 if the printer could not be resolved; 2 on a
usage error.

The CLI runs in its own process, so it derives state from the printer's signals
alone. The MCP server additionally remembers, in memory, a print start it sent
itself, so during the first moments of a start (before the printer shows any
self-test signal) the server reports `preparing` where this command may still show
`idle`. Once the printer shows its self-test progress or its filament map the two
agree.

### `creality_k2_mcp filaments [printer] [--json]`

Shows the derived state, the CFS state, and the CFS units and side spool slots: the same
`internal/filaments` code the `get_filaments` tool uses, read-only. The printer argument
may come before or after `--json`. For each slot: the stored definition (status,
brand, name, material, colour, nozzle range), whether it is selected at the hub,
whether `set_filament_definition` could edit it right now and why not (including the
printer's current state: a printing printer or a busy CFS shows every slot not editable), and the printer's own
refill groups. Names come from the printer over port 9999; if that cannot be read
the output says so and shows Moonraker's material codes only.

```sh
$ creality_k2_mcp filaments
K2-5885 (192.168.1.102)
Auto-refill: on
Unit T1 (MF003), 29 C, 35% humidity
  T1A         defined, Acme Test PLA, PLA, #ff0000, nozzle 190-240 C (editable)
  T1B         defined, Acme Test PETG, PETG, #000000, nozzle 220-270 C (editable)
Refill groups (slots the printer treats as interchangeable): T1A (PLA #ff0000); T1B (PETG #000000)
```

| Flag | What it does |
| --- | --- |
| `--json` | Print the view as JSON instead of the text summary |

A slot status describes the stored definition, not a sensor: whether filament is
physically loaded cannot be detected. Exit codes: 0 on success; 1 if the printer
could not be resolved; 2 on a usage error.

### `creality_k2_mcp snapshot [printer] [--out FILE] [--force]`

Saves one still frame from a printer's onboard camera to a local JPEG file,
through the same background-daemon capture path (`Hub.Snapshot`) the
`get_camera_snapshot` MCP tool and `camera open` use, autostarting the
daemon if it is not already running. Unlike the MCP tool, there is no size
cap here: the file is written at full capture resolution, JPEG quality 92.

Flags may be given before or after the printer argument.

```sh
$ creality_k2_mcp snapshot k2-5885 --out chamber.jpg
chamber.jpg

$ creality_k2_mcp snapshot --out chamber.jpg --force k2-5885
chamber.jpg
```

| Flag | What it does |
| --- | --- |
| `--out <file>` | Output path; defaults to `<printer-id>-<timestamp>.jpg` in the current directory |
| `--force` | Overwrite an existing `--out` file (atomically, via a temp file plus rename); without it, an existing file is refused |

Exit codes: 0 on success; 1 if the printer is offline, unreachable, the
capture failed, the daemon client is not wired up, or the output file
exists without `--force`; 2 on a usage error.

### `creality_k2_mcp camera ...`

```
usage: camera open|record|stop|recordings|delete ...
  camera open [printer] [--no-browser]      print (and open) the local live view URL
  camera record [printer] [--mode video|timelapse] [--until stopped|print_end] [--max-duration DURATION]
                                             start a recording; prints its id and, for video, its file path
  camera stop [printer|recording-id]        stop the active recording
  camera recordings [printer]               list recordings: size, duration, parts/frames, disk usage
  camera delete <recording-id> [--yes]      delete a recording's files
```

Every subcommand accepts its flags before or after its positional argument.

#### `camera open [printer] [--no-browser]`

Prints the local browser live-view URL (autostarting the daemon) and opens
it in the default browser unless `--no-browser` is given. With no printer
argument, the page lists every enabled printer.

```sh
$ creality_k2_mcp camera open k2-5885
http://127.0.0.1:PORT/view?printer=k2-5885&token=...
```

| Flag | What it does |
| --- | --- |
| `--no-browser` | Print the URL only; do not try to launch a browser |

#### `camera record [printer] [--mode video\|timelapse] [--until stopped\|print_end] [--max-duration DURATION]`

Starts a recording and prints its id (and, for `video` mode, its file path
once one exists; `timelapse` has no single file to report at start time).

```sh
$ creality_k2_mcp camera record k2-5885 --mode video
k2-5885/20260928T150405Z
/home/me/.creality_k2_mcp/recordings/k2-5885/20260928T150405Z.mp4

$ creality_k2_mcp camera record k2-5885 --mode timelapse
k2-5885/20260928T150500Z
(timelapse: JPEG stills will be written as layers change; run `camera recordings` to check on it)
```

| Flag | What it does |
| --- | --- |
| `--mode video\|timelapse` | Recording mode; default `video` |
| `--until stopped\|print_end` | When it stops on its own; default `stopped` for video, `print_end` for timelapse |
| `--max-duration DURATION` | Go duration (e.g. `2h30m`); default 12h for video, 48h for timelapse |

#### `camera stop [printer|recording-id]`

Stops the active recording, resolved either from a printer id/name (its
currently active recording) or a raw recording id.

```sh
$ creality_k2_mcp camera stop k2-5885
Stopped recording k2-5885/20260928T150405Z (requested). Duration 312s, 41802112 bytes across 3 part(s).
```

#### `camera recordings [printer]`

Lists recordings, optionally filtered to one printer.

```sh
$ creality_k2_mcp camera recordings
ID                           PRINTER  MODE   ACTIVE  STARTED               DURATION_S  BYTES     PARTS  FRAMES  STOP_REASON
k2-5885/20260928T150405Z     k2-5885  video  no      2026-09-28T15:04:05Z  312         41802112  3      0       requested

3 recording(s), 128371200 bytes total on disk in the recordings directory.
```

#### `camera delete <recording-id> [--yes]`

Deletes a completed recording's files. Prompts for interactive confirmation
unless `--yes` is given; in a non-interactive session (a script, a pipe)
with no `--yes`, this is refused outright (exit 2) rather than doing nothing
silently or deleting without asking.

```sh
$ creality_k2_mcp camera delete k2-5885/20260928T150405Z --yes
Deleted recording k2-5885/20260928T150405Z.
```

Exit codes across every `camera` subcommand: 0 on success; 1 for a runtime
failure (printer unreachable, recording not found, daemon not wired up); 2
for a usage error, including `camera delete` without `--yes` in a
non-interactive session.

## The hidden `camera serve` subcommand

`camera serve` runs the background camera/idle-heat daemon in the
foreground. It is not listed in `--help` and is not something to type by
hand: `internal/daemon/client`'s autostart execs it automatically, as the
same binary re-invoking itself, whenever a tool call, the TUI, or another
CLI command needs the daemon and it is not already running (opening the
camera view, taking a snapshot, starting a recording, or arming the
idle-heat watchdog). It owns every printer's camera connection and the
idle-heat watchdog for as long as it runs, and exits on its own once nothing
needs it (no open viewer, recording, or armed heater) for a short idle
period.

## See also

- [Configuration](configuration.md) for the registry and settings files
  these commands read and write.
- [Camera](camera.md) for what `camera open`/`record`/`snapshot` actually
  capture and where files land.
- [Tools](tools.md) for the MCP tool each command mirrors. `status` mirrors
  `get_printer_status` and `filaments` mirrors `get_filaments`.
