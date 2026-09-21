# M3 implementation checkpoint

This document tracks the Subscription Bridges milestone.

## Delivery slices

1. Generalize pipeline execution from a native-only factory to an explicit
   stage-backend boundary while preserving the native `erka` tool loop.
2. Extend version-1 configuration with an explicit `subscription_bridge`
   connection class, bridge executable selection, and deterministic routing.
3. Add a bounded read-only bridge runtime that compiles the same durable,
   provider-neutral stage context used by native providers and persists bridge
   output before emitting completion events.
4. Implement the Codex bridge with the official device-login delegation,
   read-only non-interactive execution, JSONL event translation, output-schema
   validation, usage accounting, cancellation, and availability reporting.
5. Implement the OpenCode bridge with explicit bridge labeling, a read-only
   plan profile, JSON event translation, cancellation, and availability
   reporting. Keep it optional and never make it a core runtime dependency.
6. Add `erka auth login`, `erka auth status`, and `erka providers` surfaces
   without copying subscription credentials into erka state or subprocess
   environments.
7. Prove malformed-stream handling, output limits, cancellation, bridge
   eligibility, native fallback, and a Codex-plan-to-DeepSeek-code durable
   handoff with deterministic fixtures.
8. Update public configuration and security documentation, run the complete
   local verification suite, and demonstrate the M3 exit criteria.

## Security invariants

- Bridge credentials remain owned by the vendor CLI credential store.
- Native provider API keys are removed from bridge subprocess environments.
- Subscription bridges are allowed only for planning and review.
- Bridge stdout, stderr, context, duration, and final checkpoint are bounded.
- Repository and user configuration are untrusted; Codex is invoked with an
  explicit read-only sandbox and OpenCode is isolated to a deny-by-default
  bridge profile.
- A bridge process is optional and discovered at runtime; neither Codex nor
  OpenCode is linked into or downloaded by the core runtime.

## Exit demonstration

A fixed pipeline selects `codex` with saved ChatGPT authentication for the
planning stage and a native DeepSeek connection for coding. The planning run is
stored as `subscription_bridge`, produces a validated durable checkpoint, and
the coding run receives that checkpoint without receiving Codex credentials or
vendor-private session state.

## Completion

Implemented locally on 2026-09-03. Deterministic protocol fixtures prove the
Codex and OpenCode JSON translations, strict checkpoint validation, output
limits, cancellation, read-only invocation policy, explicit backend labeling,
and the subscription-plan-to-native-code handoff. `go test ./...`, `go vet
./...`, `go test -race ./...`, and Linux/Windows cross-builds pass with Go
1.26.5. A live paid model call was intentionally not made during the automated
verification; `erka auth status` confirmed the installed Codex CLI is signed in
with ChatGPT.
