# Configuration

creality-k2-mcp keeps two small on-disk files - a printer registry and a
settings document - plus an optional environment-variable override for
ad hoc use. This page documents all three, and how to point an MCP client at
the server itself.

## Printer registry

The registry is a JSON file listing every printer this server knows about:
id, name, host, Moonraker port, API key (if any), model, the printer's own
Klipper hostname, whether it is enabled, and whether control writes are
allowed for it (`allow_control`).

### Locations

| Scope | Path | Used when |
| --- | --- | --- |
| Global | `~/.creality-k2-mcp/printers.json` | Always, unless a project file exists |
| Project | `<project-dir>/.creality-k2-mcp/printers.json` | The server (or `creality-k2-mcp add`) is invoked with that directory in scope, and the project file already exists |

If a project file exists for the current directory, it is used instead of
the global one; otherwise the global file is used (and created on first
write if it does not exist yet). This lets a specific project pin its own
printer(s) without affecting your global registry.

### File format

```json
{
  "version": 1,
  "printers": [
    {
      "id": "k2-5885",
      "name": "K2-5885",
      "host": "192.168.1.102",
      "moonraker_port": 7125,
      "api_key": "",
      "model": "F021",
      "hostname": "K2-5885",
      "enabled": true,
      "allow_control": false,
      "added_at": "2026-09-28T15:04:05Z"
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `id` | Derived once from the printer's Klipper hostname; never changes |
| `name` | Defaults to the hostname; user-editable, shown in every tool result |
| `host` | The printer's IP or DNS name |
| `moonraker_port` | Defaults to `7125` |
| `api_key` | Only needed if Moonraker itself requires one |
| `model` | Filled in by discovery once identified (e.g. `F021`) |
| `hostname` | The printer's own Klipper hostname, used as its stable identity for locking and dedup - required for any entry saved to the file |
| `enabled` | Whether this printer's tools are usable at all |
| `allow_control` | Whether control (write) tools are permitted for this printer, on top of whether control tools are registered at all - see [safety.md](safety.md) |
| `added_at` | UTC timestamp |

The file is written atomically with mode `0600` (it may carry an API key).
Two entries that resolve to the same Klipper hostname are never both kept:
duplicates are dropped at add time and, if the file is hand-edited into
having one anyway, at load time with a warning rather than failing the
whole load.

### How printers get added

- The install wizard's **Printers** step: scans the LAN, lets you review
  what was found, add a host manually, and choose which registered printers
  are enabled and which have control turned on.
- `creality-k2-mcp printers add <host>` and `printers scan` from the command
  line - see [cli.md](cli.md).
- `discover_printers` (MCP tool) only reads the network; it never writes the
  registry itself, by design - registering a printer is always something a
  human does through the wizard, the TUI, or the CLI, never something an AI
  client does through an MCP tool call.

## Settings (`config.toml`)

Settings live at `~/.creality-k2-mcp/config.toml` (there is no project-scoped
settings file - only the registry has that). The file is created with
defaults on first read if it does not exist, so it is always present and
editable after the server has run once. It is written atomically with mode
`0600`.

### Full example

```toml
version = 1

[bands]
nozzle_band_c = 10
bed_band_c = 5
part_fan_min_percent_of_current = 50
speed_factor_min_percent = 50
speed_factor_max_percent = 150
flow_factor_min_percent = 90
flow_factor_max_percent = 110

idle_heat_minutes = 15

[tools]
preset = "camera"

[tools.overrides]
# set_light = true
```

### Fields

| Field | Default | Meaning |
| --- | --- | --- |
| `version` | `1` | Settings schema version; the binary refuses to start with a version it does not understand |
| `bands.nozzle_band_c` | `10` | How far a mid-print nozzle temperature change may move from the current target before it needs widening |
| `bands.bed_band_c` | `5` | Same, for bed temperature |
| `bands.part_fan_min_percent_of_current` | `50` | How far a mid-print part-fan change may drop the fan below its current speed, as a percent of that current speed |
| `bands.speed_factor_min_percent` / `max_percent` | `50` / `150` | Bounds for the mid-print speed factor (`M220`) |
| `bands.flow_factor_min_percent` / `max_percent` | `90` / `110` | Bounds for the mid-print flow factor (`M221`) |
| `idle_heat_minutes` | `15` | How long a heater set while idle runs before the idle-heat watchdog turns it off automatically; bounded to 1-240 |
| `tools.preset` | `"camera"` | One of `monitor`, `camera`, `control` - see below |
| `tools.overrides` | (empty) | Per-tool name to `true`/`false`, overriding the preset for that one tool |

See [safety.md](safety.md) for what each band and the idle-heat timeout
actually protect against, and why the default preset is `camera` rather than
`control`.

### Tool presets

| Preset | Registers |
| --- | --- |
| `monitor` | Status, files (read), history, console - read-only tools only |
| `camera` | Everything in `monitor`, plus the camera tools (snapshot, live view, recording) |
| `control` | Everything in `camera`, plus every control (write) tool |

A tool's category decides its default from the preset; an entry in
`tools.overrides` always wins over the preset for that one tool name, in
either direction (forcing a control tool on under the `monitor` preset, or
forcing a normally-enabled tool off). An override that names a tool that
does not exist is not an error - it is simply never consulted - but the
server prints a warning on startup, and `doctor` flags it too, so a typo is
easy to notice. See [tools.md](tools.md) for the full list of tools and
which preset registers each one.

Only a tool's registration is controlled here: even with control tools
registered, a write is still refused for a specific printer unless that
printer's own `allow_control` is set in the registry, and every write still
goes through the state-aware policy engine described in
[safety.md](safety.md).

### Editing settings

There is no MCP tool that changes settings (the same "the AI cannot change
its own permissions" posture as the registry). Change them through:

- The install wizard's **Settings** step (tool preset, idle-heat timeout,
  and each band, with the current value shown and help text for each).
- The TUI's Settings screen.
- Hand-editing `config.toml` directly - it is a plain TOML file; restart the
  server (or, for the daemon, it is picked up the next time it is read) to
  apply changes.

## Environment variable override

For ad hoc use (scripting, testing, a single printer that should not be
persisted to the registry file at all), set `K2_MCP_HOST` to bypass the
registry entirely:

| Variable | Meaning | Default |
| --- | --- | --- |
| `K2_MCP_HOST` | The printer's host; setting this activates the override | (registry is used instead) |
| `K2_MCP_PORT` | Moonraker port | `7125` |
| `K2_MCP_API_KEY` | Moonraker API key, if required | (none) |
| `K2_MCP_ALLOW_CONTROL` | Must be exactly `"1"` to allow control writes | (control off) |

When `K2_MCP_HOST` is set, the server talks to exactly that one printer,
under the fixed id `env`, and the registry file is never read or written.
This printer has no persisted Klipper hostname the way a registered printer
does; its identity is instead verified live against Moonraker on each use,
the same way every other printer's identity is verified (see
[safety.md](safety.md)). Because it is never saved, it does not show up in
`printers.json`, does not survive being unset, and cannot be renamed or
otherwise edited the way a registered printer can.

## MCP client configuration

By default the server runs over stdio, which is what every MCP client
launches. Point a client at the binary with `mcp` as its only argument, using
an absolute path (a client starts the server with its own working directory
and `PATH`, so a bare command name is not reliable):

```json
{
  "mcpServers": {
    "creality-k2-mcp": {
      "command": "/absolute/path/to/creality-k2-mcp",
      "args": ["mcp"]
    }
  }
}
```

On Windows:

```json
{
  "mcpServers": {
    "creality-k2-mcp": {
      "command": "C:\\path\\to\\creality-k2-mcp.exe",
      "args": ["mcp"]
    }
  }
}
```

The install wizard writes this (or the equivalent for each supported
client's own config format) for you when you select a client in the
**Client selection** step; manual configuration is only needed for a client
the wizard does not yet support, or to point a client at a from-source
build.

### HTTP transport

For advanced setups (for example, a client that only supports Streamable
HTTP rather than stdio), `creality-k2-mcp mcp` can also listen over HTTP
instead, selected with two environment variables read at startup:

| Variable | Default | Meaning |
| --- | --- | --- |
| `TRANSPORT` | `stdio` | Set to `http` to serve Streamable HTTP instead of stdio |
| `ADDR` | `127.0.0.1:8080` | Listen address when `TRANSPORT=http` |

```sh
TRANSPORT=http ADDR=127.0.0.1:8080 creality-k2-mcp mcp
```

Everyday use should stick with the default stdio transport, which every
supported AI client already speaks; the config example earlier in this page
(`"args": ["mcp"]`, no environment override) is what the install wizard
writes.
