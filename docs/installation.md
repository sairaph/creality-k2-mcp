# Installation

## Installer

Windows (PowerShell):

```powershell
irm https://github.com/sairaph/creality-k2-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/sairaph/creality-k2-mcp/releases/latest/download/install.sh | sh
```

The installer downloads `creality-k2-mcp`, verifies its SHA256 checksum, puts it
on your `PATH` and starts the install wizard:

1. **Printers** - scans the local network for Creality K2 printers; you review
   what was found, add one by host if needed, and choose which printers are
   enabled and which have control turned on (`allow_control`, off by default).
2. **Settings** - the tool preset (`monitor`, `camera` or `control`), the
   mid-print setpoint bands and the idle-heat timeout, with defaults shown.
3. **Client selection** - the AI clients on this machine to register with
   (Claude Desktop, Claude Code, Codex, Cursor, VS Code, Windsurf, Zed and more;
   the clients found on this machine are pre-checked).
4. **Registration** - registers the server in each selected client's
   configuration, then shows what changed and what to do next.

The Printers and Settings steps save when you press enter on them; no AI client
is changed before step 4, so cancelling earlier leaves your clients as they were
(the wizard says what it kept). Restart your AI client afterwards.

Already have the binary? Run `creality-k2-mcp install`. For a project-scoped
install (registry and client configuration in the current directory), use
`creality-k2-mcp add`. Unattended installs (`--yes`, `--all`, `--client`) and
every flag: [cli.md](cli.md#creality-k2-mcp-install).

## Update, check, remove

```sh
creality-k2-mcp update      # update to the latest release
creality-k2-mcp doctor      # check the installation, the printers and the camera daemon
creality-k2-mcp uninstall   # remove the server from every AI client
```

After an update, restart your AI client: it keeps running the server process it
started until then. `uninstall` leaves the printer registry and settings in
place. Manual client configuration: [configuration.md](configuration.md#mcp-client-configuration).

## Troubleshooting

- **The tools stop answering.** Restart the AI client, or reconnect the server
  from the client's MCP settings. Do not end the `creality-k2-mcp` server
  process by hand: some clients (Codex, for one) do not start it again, and
  every later call fails until the client is restarted. The background camera
  daemon is a separate process and starts again on its own.
- **An update seems to have no effect.** Tool replies name the server that
  answered (`server_version` in the reply's frontmatter). If it differs from
  `creality-k2-mcp version`, the client is still running the old process:
  restart the client.
- **Writes are refused as `preparing` although nothing is printing.** The
  printer reports a print start. `cancel_print` sends the printer's own stop; a
  start this server sent stops counting once the printer is at rest. Details:
  [safety.md](safety.md#the-start-window).
- **A camera snapshot fails with no keyframe.** See
  [camera.md](camera.md#troubleshooting).
- **Anything else.** Run `creality-k2-mcp doctor`: it checks the installation,
  the AI clients, each printer's reachability and the camera daemon.
