# Architecture decisions

## Locked decisions

| Decision | Choice | Reason |
|---|---|---|
| Repository name | `erka-agent` | Distinct clean start for the new product |
| Public executable | `erka` | Requested user-facing syntax |
| Primary language | Go | Portable CLI/daemon binaries and strong process/stream concurrency |
| Operating model | Local-first, server-optional | Open-source users must not require a hosted server |
| Runtime ownership | Native loop and tools for native APIs | Avoid vendor CLI tool and context lock-in |
| Routing | Stage-based | Planning, coding, repair, and review may use different models |
| State ownership | Provider-neutral and durable | Tasks survive provider changes and process restarts |
| Subscription access | Explicit vendor bridges only | Consumer subscriptions are not general-purpose model APIs |
| Initial native provider | DeepSeek | First cost-effective end-to-end implementation target |
| Initial bridges | Codex and OpenCode Go | Match the initial user's existing subscriptions |
| Local/server reuse | Same application and runtime core | Prevent two products from diverging |
| Initial database | SQLite | No service dependency for local and personal-server use |
| Minimum Go version | Go 1.26 | Support the current and previous supported Go release lines |
| CLI library | Cobra | Mature subcommands, flags, help, and future completion support |
| SQLite implementation | `modernc.org/sqlite` with `golang-migrate/migrate/v4` | CGO-free cross-platform builds with embedded, crash-aware migrations |
| MCP | Official Go SDK, later milestone | Avoid protocol maintenance and premature scope |
| Web frontend | Deferred client | Stabilize runtime and API before rebuilding UI |
| Design documents | Tracked in Git from 2026-09-21 | Planning, ADRs, and agent guidance must survive machine loss; only personal notes stay ignored |
| Product plan | `docs/PRODUCT-PLAN.md` | Phased path to a public release with demonstrable exit criteria |
| Product name | erka (final) | Owner decision, 2026-09-21 |

## Important terminology

- **Native provider:** raw model API driven by the `erka` loop and tools.
- **Subscription bridge:** official vendor agent surface used to consume an
  eligible subscription; it may retain vendor runtime behavior.
- **Connection:** configured endpoint, authentication method, and capability set.
- **Model:** a model ID available through a connection.
- **Pipeline:** ordered task stages and routing policies.
- **Run:** one stage attempt on one execution backend.
- **Checkpoint:** provider-neutral structured task handoff.

## Decisions still requiring ADRs

- Apache-2.0 versus AGPL-3.0 license, decided when the product is feature-complete and before public release.
- Personal-server sandbox implementation after the interface is proven.
- Remote authentication token format and reverse-proxy trust model.
- Whether the stable server API reuses selected AI-LXC shapes or starts a clean
  `/v1` contract.
- Notification targets for the first server release.

## Rejected directions

- Continuing to evolve AI-LXC as the primary codebase for this pivot.
- Requiring a remote daemon for local CLI operation.
- Treating ChatGPT subscription credentials as an undocumented OpenAI API key.
- Calling all provider integrations “OpenAI compatible” regardless of semantics.
- Hand-writing the full MCP protocol.
- Treating shell command filters as a permission boundary.
- Loading every installed skill into every model request.
- Implementing the web UI before proving the local native runtime.
