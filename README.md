# Creality K2 MCP

Control your Creality K2 3D printer from Claude Desktop and other MCP clients.
Check status and job progress, see the chamber camera, record a timelapse, read
and relabel CFS filament slots, and, once you turn control on, start, pause,
resume and cancel prints and adjust temperatures, fans and speed. Every write
checks the printer's real state first and is refused when it is unsafe, so it
never breaks a running print. It pairs with
[creality-slicer-mcp](https://github.com/sairaph/creality-slicer-mcp), which
prepares the G-code this server prints. It is a single self-contained binary.

## What you can ask

- "What is my printer doing right now?"
- "Show me the chamber, with the light on."
- "Which filament is in each CFS slot?"
- "Print `bracket_plate1.gcode` with the white PETG."
- "Pause the print, I need to check something."
- "The first layer looks too fast: switch to the Silent speed preset."
- "Skip the object that came loose and keep printing the rest."
- "Record a timelapse of this print."
- "Set slot T1C to Generic PETG, yellow."

## Quick start

You need a Creality K2 family printer on your local network (tested on a K2 with
a CFS).

Windows (PowerShell):

```powershell
irm https://github.com/sairaph/creality-k2-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/sairaph/creality-k2-mcp/releases/latest/download/install.sh | sh
```

The installer downloads `creality-k2-mcp`, verifies its SHA256 checksum, puts it
on your `PATH` and starts the setup wizard, which:

1. finds the K2 printers on your network and lets you choose which ones the AI
   may use, and on which it may change anything (control is off by default);
2. sets the tool preset (`monitor`, `camera` or `control`) and the safety
   limits for changes during a print;
3. finds the AI clients on your machine (Claude Desktop, Claude Code, Codex,
   Cursor, VS Code and more) and lets you pick the ones to register with;
4. writes the printer list, the settings and each selected client's entry.

Nothing is written until step 4, so cancelling earlier leaves your machine as it
was. Restart your AI client afterwards. Run `creality-k2-mcp doctor` at any time
to check the installation, and `creality-k2-mcp update` to update to the latest
release (then restart your AI client). See the
[installation guide](docs/installation.md) for unattended installs and
troubleshooting.

## Safety in brief

- Reads are always allowed. A write needs the `control` preset and control
  turned on for that printer.
- Every write reads the printer's state first and is refused when that state is
  unknown or unsafe. Pause and cancel are never blocked by a setpoint limit or a
  CFS signal.
- Changes during a print stay within configurable limits. Cancelling, resuming,
  deleting and other hard-to-undo actions are proposed first and only sent after
  you confirm.
- Only a fixed set of commands can be sent: no arbitrary G-code, macros,
  calibration or firmware settings. The three that persist on the printer are
  disclosed when you use them.
- Nothing can check that the bed is clear: the AI asks you before every start.
- A heater set while the printer is idle turns itself off after a timeout.

The full design: [safety model](docs/safety.md).

## Documentation

| Guide | Contents |
| --- | --- |
| [Installation](docs/installation.md) | Installer, update, uninstall, troubleshooting |
| [Configuration](docs/configuration.md) | Printer registry, settings, environment override, manual client setup |
| [Tools](docs/tools.md) | Every tool, its parameters, when it is available and what it does |
| [Safety model](docs/safety.md) | Printer states, confirmations, limits, CFS rules, what was verified on hardware |
| [Camera](docs/camera.md) | Snapshot, live view, recording and timelapse, troubleshooting |
| [CLI](docs/cli.md) | Every command, the terminal UI and `doctor` |

## Requirements

- A Creality K2 family printer on the same local network as the computer that
  runs the server.
- Windows, macOS or Linux.
- An AI client that speaks MCP. The setup wizard finds the common ones.
- For slicing, [creality-slicer-mcp](https://github.com/sairaph/creality-slicer-mcp);
  this server prints files that are already sliced.

## License

MIT, see [LICENSE](LICENSE). The camera snapshot decoder embeds a WebAssembly
build of Cisco's OpenH264 (BSD-2-Clause, decoder only, compiled from source)
run by wazero (Apache-2.0); notices in [NOTICE](NOTICE). This self-compiled
decoder is not covered by Cisco's binary patent license for OpenH264; H.264
(AVC) patent terms may apply to your use.
