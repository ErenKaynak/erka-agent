# ADR 0008: OS sandbox for model commands

## Status

Accepted, 2026-09-21. Refines ADR 0004.

## Decision

Commands the model runs (`run_command`, previews, and erka's Git inspection
tools) execute inside an OS sandbox unless the session is in god mode:

- **macOS:** `sandbox-exec` with a generated profile whose paths are passed as
  parameters, never interpolated.
- **Linux:** bubblewrap with the host filesystem mounted read-only.
- **Other platforms:** no backend yet; `auto` runs unconfined and says so,
  `required` refuses to run.

Inside the sandbox, writes are limited to the workspace, a private per-command
temporary directory, and build caches; outbound network is limited to
loopback unless `sandbox.network` is set; credential stores and erka's own
state and configuration are unreadable; and `.git/config`, `.git/hooks`, and
`.git/info/attributes` are read-only so a planted hook or `core.fsmonitor`
cannot run later outside the sandbox. erka's file tools refuse all writes
under `.git` for the same reason.

## Consequences

The sandbox is a least-privilege boundary for a single user's own machine,
not a multi-tenant or hostile-kernel boundary. Tools that ignore `TMPDIR` and
write to `/tmp` directly fail under bubblewrap. Build caches remain writable
for practicality, so a cache-poisoning risk is accepted and documented.
