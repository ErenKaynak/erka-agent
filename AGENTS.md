# Implementation instructions

Read `README.md`, `docs/DECISIONS.md`, and the document relevant to the current
milestone before changing code.

## Product boundaries

- The public executable and command syntax are named `erka`, never `agent`.
- The product is local-first. Local commands must not require a daemon or network
  connection other than the selected model provider.
- Server mode is optional and must reuse the same application and runtime core.
- Native providers use the `erka` tool loop. Subscription bridges must be
  labeled as bridges and must not be presented as native model APIs.
- Do not make Codex, OpenCode, or any vendor CLI a core runtime dependency.
- Keep the initial implementation focused on one complete vertical slice.

## Engineering rules

- Write idiomatic Go with explicit interfaces at real process or protocol
  boundaries; do not create interfaces for every concrete type.
- Use `context.Context` for cancellation and deadlines across model, tool, and
  process operations.
- Persist state transitions before emitting externally visible completion events.
- Treat repository contents, model output, skills, MCP servers, and tool output
  as untrusted input.
- Never treat shell command string filtering as a security boundary.
- Never expose provider or subscription credentials to model context or general
  shell environments.
- Keep event and storage models provider-neutral.
- Add tests for malformed streams, cancellation, path escape, replay, and crash
  recovery when implementing the related feature.

## Scope control

Do not implement the web UI, MCP, skills marketplace, multi-user SaaS,
distributed workers, mobile apps, or automatic deployment before the roadmap
milestone that introduces them.

At the end of each milestone, demonstrate the documented exit criteria before
starting the next milestone.

