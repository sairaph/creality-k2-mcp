# Safety model

creality-k2-mcp hands a fair amount of control over a physical, heated
machine to an AI. This page explains, in plain terms, how it is designed so
that a mistake, a confused model, or a bug is very unlikely to break a
running print, damage the printer, or leave a heater running unattended.
The full engineering design (and the research behind it) lives in
dev_docs/safety-architecture.md; this page is the user-facing summary.

## The short version

- The server reads the printer's actual state before every write and
  refuses anything it cannot make sense of, rather than guessing.
- A running print is never broken by a routine tool call: writes that could
  interrupt it are either blocked outright in the wrong state, bounded to a
  safe range, or require a second, explicit confirmation.
- No tool ever changes a persistent printer setting (calibration, EEPROM
  configuration, idle timeout, and so on) - nothing in this server's
  vocabulary can do that at all - except three disclosed writes of fixed
  actions: `cancel_print` clears the power-loss-recovery slot,
  `set_speed_preset` with `silent` makes the firmware write a power-loss-resume
  hint file (see "Speed presets and Silent mode"), and `set_filament_definition`
  rewrites one slot's stored filament definition (an explicit user-requested write).
- A destructive or hard-to-reverse action requires two calls: propose, then
  confirm with a token that expires and is invalidated if anything relevant
  changes in between.
- Checking whether the bed is clear before starting a print is the one
  judgment call the server genuinely cannot make itself; it is yours.
- A heater left on while idle turns itself off automatically, as long as
  the small background daemon is running.

## State-aware, fail closed

Before any write, the server takes a fresh snapshot of the printer (over
Moonraker, corroborated by the printer's own status socket) and derives one
of a fixed set of activity states: idle, printing, paused, an in-progress
pause/resume/cancel, an error, offline, and several others. Each state maps
to a gating class that decides what is even allowed to be attempted.

If the printer's state cannot be positively determined - an unrecognized
combination of signals, an unreachable printer, a read that partially
failed - every write is refused. This is a deliberate "fail closed" design:
an unknown state is treated as unsafe, never as safe by default. Reads
(status, files, camera, history) are always allowed regardless of state,
since a read can never break anything.

This same state block is included in every tool's result, so an AI client
always knows what state the printer is in, which actions are currently
available, blocked, or need confirmation, and why - not just after a write
fails, but on every call, so it can plan its next step correctly.

## Never breaks a running print

A handful of hard rules keep a routine call from interrupting a print in
progress:

- **Pausing** is only available while actually printing - not during the
  printer's own start-up heating/homing sequence, where the underlying
  firmware behavior around pausing is not reliably verified.
- **Resuming** and **cancelling** require the two-step confirmation flow
  described below, and re-check the printer's state immediately before
  sending anything, so a print that already finished, paused again, or
  changed in some other way between the proposal and the confirmation is
  caught rather than acted on blindly.
- **Mid-print temperature, fan, speed and flow changes** are bounded to a
  configurable band around the current value (see "Bands" below) rather
  than allowed to jump to any value - see [configuration.md](configuration.md)
  for the exact defaults and how to widen them.
- **Setting a heater while the printer is paused is blocked entirely.** The
  printer's own pause macro stores the pre-pause temperature target and its
  resume macro restores that stored value; a change made while paused would
  simply be silently overwritten the moment the print resumes, so the
  server refuses it outright rather than letting you believe a change took
  effect when it did not.
- **Uploading or deleting the file currently printing is refused outright**,
  with no override - the print keeps its own file untouched no matter what.

## Never changes a persistent printer setting

The server can only ever send a small, fixed, explicitly enumerated set of
commands (heater targets, fan speeds, print-speed and flow factors, the
chamber light, the standard start/pause/resume/cancel job-lifecycle calls,
`EXCLUDE_OBJECT`, and the printer's own file-manager upload/delete
endpoints). There is no path anywhere in the server for arbitrary G-code, a
macro invocation, or a raw Moonraker endpoint outside that list. Anything
that would change a persistent printer setting - calibration routines,
EEPROM writes, the idle timeout, motion limits, pressure advance, and so on
- is simply not in that vocabulary at all, so there is no tool call, no
argument combination, and no confirmation sequence that can reach it.

Three fixed actions have a disclosed persistent effect, stated in their results
and descriptions: `cancel_print` clears the power-loss-recovery slot in EEPROM;
entering Silent (`set_speed_preset` with `silent`) while printing makes the firmware
write `creality/userdata/config/speed_mode.json` (`{"speed_mode":2}`), a
power-loss-resume hint that leaving Silent does not clear; and
`set_filament_definition` rewrites one slot's stored filament definition (material,
brand, colour, that catalog entry's nozzle range and pressure advance), an explicit
write you request that stays until it is changed again.

## Two-step confirmation (`confirm_token`)

Actions whose safety depends on things the server cannot fully verify on its
own - resuming, cancelling, excluding an object from a print, deleting a
file, overwriting an existing file, deleting a recording - require calling
the tool twice:

1. **Call once with no `confirm_token`.** The server takes a fresh state
   snapshot, works out exactly what the action would do, and returns a
   proposal: the precise effects in plain language, no side effect yet, and
   a one-time token with an expiry (120 seconds).
2. **Call again with that same `confirm_token`.** The server re-checks the
   printer's state; if anything relevant has changed since the proposal was
   issued, the token is rejected with a `conflict` error naming what
   changed, and a fresh proposal must be requested. A token is single use
   and held only in the server's own memory, so it never survives a
   restart - an unknown token (expired, already used, or from a previous run)
   returns a clear "request a fresh proposal" error rather than doing
   nothing silently.

This exists specifically so an AI client cannot act on stale information: by
the time the second call lands, the printer's actual current state is
checked again, not assumed to still match what the first call saw.

### `start_print` is a special case

Nothing the server can read tells it whether the bed is actually physically
clear of a previous print. Because of that, a `start_print` without a CFS carries
no `confirm_token` at all - a token would only create a false sense that the
server had verified something it fundamentally cannot. Instead, its
description explicitly instructs the AI to ask you to confirm the bed is
clear before ever calling it. This is the one place in the whole design
where the check is entirely your own responsibility rather than something
the server enforces; see dev_docs/safety-architecture.md's decision log
(D4) for the full reasoning.

With a CFS connected the token exists for a different reason: it binds the
filament mapping you were shown (see [CFS](#cfs-multi-material-support)). It
still does not verify the bed, and the AI is still instructed to ask you.

## Bands: bounded, not unlimited, mid-print changes

While a print is running, temperature, fan, speed and flow changes are
allowed to apply immediately with no confirmation step, but only within a
configured band around the current value:

| Setting | Default band |
| --- | --- |
| Nozzle temperature | +/-10 C of the current target |
| Bed temperature | +/-5 C of the current target |
| Part fan | not below 50% of its current speed, in one call |
| Speed factor (and the speed presets' factors) | 50-150% |
| Flow factor | 90-110% |

A change outside the configured band is refused, with the tool's error
naming the exact configured band and pointing at
[configuration.md](configuration.md) to widen it if a larger change is
genuinely wanted. Nozzle and bed temperature also have a hard, non-adjustable
ceiling (320 C and 110 C respectively), further capped by whatever the
printer itself currently reports as its live limit - whichever is lower
wins.

These band defaults are engineering judgment, not derived from measured
failure data (there is no published safe range for these numbers), so they
are deliberately conservative and user-configurable rather than fixed.

## Speed presets and Silent mode

`set_speed_preset` applies one of Creality's four speed presets, only while
printing (paused and idle are refused). Stable, Standard and Ultrafast are the
speed factor 50%, 100% and 125%, sent through the same fixed Moonraker `M220`
command as `set_speed_factor`, and each must lie inside the configured
speed-factor band. Silent is different: entering it runs Creality's `Qmode`
macro, which (all runtime, until changed or the print ends) sets velocity 150 mm/s
(a file that sets its own velocity overrides it), clamps acceleration to 2500 mm/s2,
sets pressure advance 0.05, caps all three fans (part, case, auxiliary) to half
their range and sets the speed factor to 50%.

- **Disclosed persistent write.** Entering Silent while printing makes the
  firmware write `creality/userdata/config/speed_mode.json`
  (`{"speed_mode":2}`), a power-loss-resume hint. Leaving Silent does not clear it,
  so a later power-loss resume, even of another print, may come back in Silent. The
  tool description and the Silent effects say so, like `cancel_print` says it clears
  an EEPROM slot. The speed factor itself never goes through port 9999: it is always
  the Moonraker `M220` (no SAVE argument); port 9999 carries only `speedMode`.
- **Silent is tri-state and fails closed.** It is read from two Moonraker sources
  (`custom_macro.qmode_flag` and the `Qmode` macro's own `flag`); it is on or off
  only when both are present and agree, otherwise unknown. While it is on or unknown
  during a print, `set_speed_factor` is refused (Silent's end restores the factor it
  saved on entry, which would silently discard the change) and `set_speed_preset` is
  refused while unknown.
- **Leaving Silent** restores the velocity, acceleration, corner velocity, pressure
  advance and fan values captured when it was entered; after a CFS filament change
  these can be stale (for example the previous filament's pressure advance). If
  Silent is on with no record of this server entering it for the job, the result warns
  that it was entered elsewhere or is left over. Leaving Silent to any non-Silent
  preset inside the band is always allowed.
- **Verification.** Moonraker is authoritative (its Silent flag and speed factor must
  show the target on polls at least 1.2 s apart within about 3 s); port 9999's `speedMode` and
  `curFeedratePct` are read on a fresh connection and corroborate. A disagreement
  makes the result `unconfirmed` with both readings shown and `speed_preset` unknown.
  If Silent was left but the speed factor could not be set, the result is `partial`
  and states what a fresh read shows (Silent on or off, and the factor).
- **Race and stuck Silent.** Immediately before `speedMode:1` the server re-reads
  `print_stats` and `pause_resume` and refuses if the job is no longer printing.
  If Silent is on and print_stats shows the job neither printing nor paused (it ended,
  was cancelled or errored), the result is `stuck_silent_possible`; with a paused or
  pausing print the call keeps its real outcome plus a note that Silent stays active
  through the pause. Silent left on
  outside a print clamps the NEXT print's acceleration until Klipper restarts
  (a firmware restart or a power cycle clears it; this server cannot, because Silent's
  exit does nothing outside a print). `get_printer_status` and the `start_print` proposal
  and result warn about it; it is a warning, not a refusal.
- **Fans.** While Silent is on the firmware caps every fan at half its range, so
  `set_fan_speed` above 50% is applied as 50% and its result says so.
- **Stopping is never blocked by a setpoint.** `pause_print` and the confirming
  call of `cancel_print` (the one with its token; the proposal sends nothing and
  pre-empts nothing) pre-empt an in-flight `set_speed_preset`, `set_speed_factor`,
  `set_flow_factor`, `set_fan_speed`, `set_nozzle_temperature`, `set_bed_temperature`
  or `set_light` instead of failing with a lock conflict: the setpoint call is
  cancelled and reports effect `preempted` with a fresh read of the state. A waiting
  pause or cancel has priority over queued setpoints, and never pre-empts anything
  else (start, resume, upload, delete, a filament edit, exclude, or each other: those
  give the normal conflict). Pre-emption works within one server process; a pause from
  another MCP server process uses the bounded (3 s) wait on the cross-process file lock
  and reports a conflict if the other process still holds it.

## Idle heating and the watchdog

Setting a nozzle or bed temperature while the printer is idle (not printing
or paused) is allowed, but only while the background daemon is reachable:
the moment the heater target is accepted, the daemon arms a watchdog that
turns that heater back off automatically after a configurable timeout
(`idle_heat_minutes`, default 15 minutes) unless a print starts or the
target is changed again first. If the daemon cannot be reached, the write is
refused outright with a hint, rather than heating something with no
automatic shutoff behind it.

`get_printer_status` reports the watchdog's own liveness and every heater it
currently has armed (with its target and deadline), so you can always see
whether an idle heater is actually protected right now. The daemon starts
automatically the first time anything needs it (arming a heater, opening the
camera view, taking a snapshot, or starting a recording) - see
[camera.md](camera.md) for more on the daemon itself.

## Checking whether the bed is clear is yours to do

As noted above, this is the one safety-relevant check in the whole design
that the server explicitly cannot perform for you. When an AI client asks
you to confirm the bed is clear before starting a print, that confirmation
is real and load-bearing - there is no independent verification happening
behind it.

## CFS (multi-material) support

The CFS changes what is safe to write, but no single printer signal says "the CFS
is busy". The server therefore reads a small set of signals (port 9999 `deviceState`,
`feedState`, `materialStatus`, `err`, `repoPlrStatus`, `upgradeStatus`, `cfsConnect`,
and Moonraker's `box` and `pause_resume.resume_err`) and derives three flags next
to the normal state, never instead of it, so a running print is never shadowed:

- **Known:** every one of those signals was positively read (port 9999 reachable,
  no field missing, and Moonraker and 9999 agree the CFS is connected).
- **Error:** an error code, a material error, a resume error or a filament runout
  is present.
- **Quiescent:** Known, no error, and at rest (idle device and feeder, no recovery
  or upgrade). Only positive idle values count; anything else is busy or unknown.
  It is used for idle decisions only, since the device state during a print is not
  the idle value.

A brief bus drop where the CFS reports disconnected but a unit still says connected
keeps the CFS rules in force.

### What each tool needs

| Rule | Tools | Allowed with a CFS connected when |
| --- | --- | --- |
| none | `pause_print`, `cancel_print`, `set_light`, `set_bed_temperature`, `upload_gcode_file`, `delete_gcode_file` | the normal state rules allow it (stopping is never blocked by a CFS signal; upload, delete and bed temperature have no CFS interaction) |
| known, no error | `exclude_object`, `set_speed_factor` | the CFS is Known and has no error |
| fan | `set_fan_speed` | idle: Quiescent. Printing: Known and no error |
| nozzle | `set_nozzle_temperature` | idle: Quiescent. Printing: refused |
| refuse | `set_flow_factor` | never |
| start | `start_print` | Quiescent, then the mapping proposal |
| resume | `resume_print` | a clean pause this server issued (below) |
| quiescent | `set_filament_definition` | Quiescent |

Why:

- **Nozzle temperature during a print is refused (V5).** A CFS changes the nozzle
  temperature itself during filament changes and this server cannot detect a
  change, so a value it set could be overwritten or fight the CFS.
- **Flow is refused whenever a CFS is connected (V5).** Flow scales the purge
  volumes of a filament change.
- **A start goes through a mapping proposal (V3).** A bare Moonraker start is
  refused, because it would run the file with the printer's stale filament map. The
  proposal reproduces Creality's own "print a file on the printer" path: the
  server computes the mapping with Creality's algorithm, shows it with each slot's
  refill group and a warning when the printer may run another slot of the group, and
  only after you confirm sends the colour map, checks that Moonraker's own map matches
  it (no start frame is sent on a mismatch), and then sends the start. The token binds
  the mapping, the file's identity (size, creation time, filament list) and every slot
  definition, so any change refuses. The result says `sent`, never `started`.
- **Resume is only for a clean pause this server issued (V4, 8a.5).** The pause is
  recorded only if the CFS was Known and error free when it settled; the record holds
  the job, the filament map, `box.enable` and a hash of every slot. Resume needs the
  record for the same job, a clean CFS, a device that is not loading or unloading,
  the feeder at rest, no recovery or upgrade pending, no resume error, and the map,
  enable flag and slots unchanged. A pause made at the printer screen, after a runout,
  or by another process is not resumable from here: resume on the printer screen or
  in Creality Print. The record lives in this server's memory and is cleared as soon
  as any snapshot shows the job not paused.
- **Editing a slot is idle-only and read back (V2).** The edit sends Creality's own
  `modifyMaterial` with every field from one catalog entry, re-checks everything
  immediately before sending, and confirms only when the printer's pushed slot shows
  the new values with editing complete and the regrouped refill groups, and Moonraker
  agrees.

### The start window

After a start frame the printer runs a self-test of several minutes with the job
still reported as standby (or as the previous job's complete or cancelled). Without
care that would look like an idle printer. Two things cover it: the printer's own
signals derive `preparing`, and this server keeps an in-memory record of a start it
sent. The signals, with print_stats standby, complete or cancelled: the 9999 state
reading 9 or 1 (a start in progress; observed live within a second of the start frame),
7 (stopping), the self-test progress not finished, or a filament map that is not the
identity map. The record ends after 15 minutes, when print_stats shows printing, paused
or error, when it shows complete or cancelled for a different job (a different filename,
metadata uuid or start time; a new printer print id alone does not end it), or when the
9999 state reads 3 or 4 (a stopped or failed start) in a snapshot taken a few seconds
after the start frame. State 4 (aborted) persists at rest after a stop and is never read
as busy.

While the window is open the state is bucket `preparing`: every setpoint, slot edit and
start is refused, and uploading over or deleting the file being started is refused;
`set_light`, uploads or deletes of other files, and `cancel_print` still work.

Cancelling during the window sends Creality's own 9999 stop instead of Moonraker's cancel,
which does not stop the self-test (print_stats has no job yet). This was verified live: 9999
state 7 (stopping) appears right after the frame, the printer finishes its current self-test
step (about 20 s), turns the heaters off, returns the map to identity, and is idle about a
minute after the frame. The reply returns as soon as state 7 or 4 shows (up to 15 s) with
effect `stopping` (or `sent` if neither is seen) and does not wait for the wind-down.

The record lives in one server process. A second MCP client, or a restart, does not
see it: it still sees the printer's own signals, which cover the window from the moment
the 9999 state changes, but not the very first instant of a start. The CLI and the TUI
likewise derive state from the printer's signals alone.

After a refused map (`refused_map_mismatch`) the colour map had already been sent, so the
printer may keep a non-identity map until the next start and show `preparing` with
nothing printing; the reply says so and asks for the map to be cleared at the printer
rather than for a blind retry.

Two flow-factor rules go with a CFS start. The start resets the flow factor to 100%
first when it is not already (the purge volumes of a filament change depend on it), and
restores the previous value only if the start is refused or definitely not sent: that
restore is the one flow write that bypasses the "no flow changes with a CFS" rule, because
it puts back the user's own earlier value. Once the start frame was sent, or may have been,
the flow stays at 100%.

### Pausing and resuming

Moonraker answers pause, resume and cancel only after the whole macro has run, so on a K2
the tools do not wait for it. A K2 pause takes about 17-20 s (park, wipe) and a resume runs
the RESUME routine (reheat to the stored target, purge, wipe) for 60 to 75 s, both with the
job still reported as printing or paused. Pause and resume therefore send their Moonraker
request from a background task with a 120 s timeout, mark it in an in-flight record on the
printer lock, and release the per-printer lock BEFORE the reply waits for the printer's
first sign, so a cancel issued meanwhile is never answered with a conflict.

- **Pause** replies `confirmed` once the job reports paused, or `pausing` as soon as 9999 shows
  the PAUSE routine (state 5 or 6) within 30 s. While the record is active and the job is
  still printing, the state is `pausing` (bucket T): every write is refused except
  `cancel_print`, which queues behind the macro. The resume record (the clean-CFS pause record)
  is made when a snapshot shows the pause settled, from the tool's own poll or from the next
  call; the pause record ends when the job is paused, on a terminal print_stats state, or after
  3 minutes.
- **Resume** replies `confirmed` once the job is printing, or `resuming` as soon as 9999 shows
  state 8 (the RESUME routine) within 20 s. The resume record makes the state `resuming` (bucket
  T) while print_stats is still paused, whether or not 9999 shows anything, and also derives it
  over a homing sub-phase (RESUME homes X/Y first). A second resume while it is active is
  refused ("a resume is already in progress"), so a dropped 9999 frame can never lead to a
  second reheat and purge. The record ends when the job is printing again, on a terminal
  state, or after 5 minutes. 9999 state 8 while print_stats is paused also derives `resuming`
  by itself (a resume started on the printer), and the status text then says the printer
  reports it is resuming.
- **Cancel** is allowed while pausing or resuming (an explicit exception; other writes stay
  refused).
- A send whose HTTP call fails is reported as such: an HTTP status rejection from Moonraker is
  never promoted to accepted even if the printer happens to be paused (nothing is recorded); a
  transport error or timeout leaves the effect unknown, so the printer state decides, and only
  for cancel does a confirmed effect after such an error count as accepted.

### Verified on hardware, and what is not

Verified in the supervised session (2026-09-29): a CFS start with a requested map (the
printer's map was exactly the requested one and returned to identity after the stop),
stopping during the self-test, pause and resume of a CFS print, and editing the side
spool (both channels confirmed). Each of these has a switch in the code that is now on:
`stopDuringStartVerified` and `sideSpoolEditVerified`.

Still off: `spoolStartVerified`, starting from the side spool with a CFS connected. With a
CFS connected the side spool is not in the feed path, so it could not be tested; it stays
refused. A tool change during a print was also observed live (see the next section).

### What cannot be detected

These are not visible to the server, so it asks you instead of guessing: a tool
change during a print (which is also why the nozzle and flow are refused: live, the extruder
filament sensor and 9999 materialDetector1 flip while the old filament retracts, the printer
itself raised the nozzle target to 265 then 270 C to flush, and feedState and deviceState did
not change), whether a
slot physically holds filament, and a spool being pre-loaded or an RFID scan in
progress. Every mapping proposal says the slots cannot be checked for physical
filament.

## What every result tells you

Every tool's reply carries the same state information (activity state,
bucket, gating class, and - when relevant - which actions are currently
available, blocked, or need confirmation, and why) plus a plain-language
body explaining what that means and the exact next call to make. This
applies to error results too: a refused call still tells the AI (and you)
what the printer's state actually is and what would need to change before
retrying, rather than just failing silently.

## What this version does not do

- No G-code, macro, or raw endpoint access beyond the fixed vocabulary above.
- No calibration, homing, or manual motion control.
- No error-dismissal tool - clearing a printer-reported error is done on the
  printer's own screen or in Creality Print, then confirmed with another
  status check.
- No firmware or service restarts, no emergency stop.
- No filament loading or unloading, no CFS error dismissal, and no editing of RFID-tagged
  spools, and no start from the side spool with a CFS connected: those stay on the printer.

See [tools.md](tools.md) for the exact parameters, state requirements and
`confirm_token` behavior of every individual tool.
