// Package policy is the action policy and the only path from tools to
// printer writes (dev_docs/safety-architecture.md section 3.2). It defines,
// for every write this server can perform, which activity-state buckets
// allow it, what extra fresh preconditions it needs, what parameter rules
// apply, what confirmation it requires, what physical or persistent effects
// it discloses, which closed command templates or endpoints it emits, and
// how it decides the write settled.
//
// Three rules from dev_docs/safety-architecture.md shape every function in
// this package:
//
//   - P5, check-act-verify: Execute always takes its own fresh snapshot as
//     the last thing it does before sending a command, never reuses a
//     snapshot read earlier in the same call (e.g. the one used to build a
//     proposal), and always polls afterward until the expected state
//     settles or a timeout is reached. A timeout is reported as
//     "unconfirmed", never as a guessed success or failure.
//   - P8, no exclusive ownership: a per-printer in-process mutex and a
//     cross-process file lock (domain.LockPath) serialize this server's own
//     writes to one printer at a time, but a human at the touchscreen or a
//     second client can still act concurrently; the fresh-snapshot rule
//     above is what catches that, not the lock.
//   - D3 (dev_docs/safety-architecture.md section 10): there is no
//     elicitation or human-confirmation logic in this package. The
//     two-phase proposal token exists only as a TOCTOU state-binding guard
//     for resume_print, cancel_print, exclude_object, delete_gcode_file and
//     an overwriting upload_gcode_file; every other action either has no
//     confirmation step or is gated by the mid-print bands (D1) instead.
//
// Available is for display and guidance only (P6) and never gates a write;
// only Execute's own fresh-snapshot precondition check does that.
package policy
