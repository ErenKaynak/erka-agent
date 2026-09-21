# M2 implementation checkpoint

This document tracks the Pipeline and Multi-Model Routing milestone.

## Delivery slices

1. Extend version-1 configuration with named native connections, fixed stage
   routes, ordered candidates, capability requirements, and per-stage budgets.
2. Add durable run attempt, token, cost, and checkpoint metadata through an
   embedded forward-only migration that preserves M1/M1.5 databases.
3. Implement deterministic fixed routing with capability checks, bounded
   fallback, and explicit retry classification.
4. Add stage-specific context compilation for planning, implementation,
   validation, repair, and review without replaying the full transcript.
5. Refactor the native backend for multi-stage lifecycle ownership and add the
   durable pipeline orchestrator.
6. Add a native OpenAI Responses API streaming adapter and provider-neutral
   connection/credential wiring.
7. Wire configured pipelines into `erka run` and the local TUI while retaining
   legacy single-model behavior.
8. Prove a two-provider plan-to-implementation handoff, fallback, budgets,
   repair/review behavior, restart compatibility, and secret non-leakage.
9. Update the public README, run the complete local and GitHub Actions matrix,
   publish a completion commit, and tag `milestone-m2`.

## Publishing checkpoints

- `feat(config): add fixed pipeline and connection configuration`
- `feat(store): persist stage attempts usage and cost`
- `feat(routing): add deterministic stage routing and budgets`
- `feat(runtime): compile bounded stage-specific context`
- `feat(runtime): orchestrate durable multi-stage pipelines`
- `feat(provider): add native OpenAI Responses streaming`
- `feat(app): run configured native pipelines locally`
- `test: prove the M2 multi-provider workflow`
- `docs: document pipeline routing and native connections`
- `chore: mark M2 completion`

Every checkpoint is formatted, tested, audited for private Markdown, committed,
and pushed directly to `main`. Published history is never rewritten.

## Completion

Completed on 2026-09-03. The verified boundary is commit `b42b594` and annotated
tag `milestone-m2`. GitHub Actions run `33746402836` passed formatting, vet, the
race detector, Go 1.26 tests/builds on Linux, macOS, and Windows, and Go 1.27
tests/builds on Linux.
