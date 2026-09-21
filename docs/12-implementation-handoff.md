# Implementation handoff

This is the starting brief for the first implementation task in a new coding
session.

## Objective

Create the Go foundation for `erka-agent` and deliver the smallest local-first
vertical slice. Do not begin by porting the previous Python backend or rebuilding
the web UI.

## First implementation scope

1. Initialize a Go module after the final GitHub repository path is known.
2. Create one `erka` executable with `version`, `help`, and `run` commands.
3. Implement platform-standard config and state paths with `ERKA_` environment
   overrides.
4. Define canonical runtime types and these meaningful boundaries:
   `ModelProvider`, `Tool`, `Sandbox`, `Store`, and `EventSink`.
5. Implement an in-memory store first for unit tests, then SQLite with embedded
   migrations.
6. Implement a fake scripted model provider and fake tools to prove the agent
   loop deterministically.
7. Persist tool calls and results with stable IDs and verify restart recovery.
8. Add a DeepSeek adapter only after the fake-provider loop is correct.
9. Implement the minimal local tools required by the M1 fixture.
10. Demonstrate `erka run` against a fixture Git repository.

## Do not implement yet

- remote server API;
- web UI;
- Codex or OpenCode bridges;
- LLM-based automatic routing;
- MCP or skills;
- Git push, pull request, or deployment;
- multi-user authorization;
- a generic provider marketplace.

## Required first tests

- fragmented streaming text and tool arguments;
- malformed tool arguments;
- maximum-iteration termination;
- context cancellation;
- command timeout and output truncation;
- path traversal and symlink escape;
- persistence before and after tool execution;
- restart with an incomplete read-only tool;
- restart with an unknown mutating-tool outcome;
- event ordering and deduplication.

## First end-to-end acceptance scenario

Use a small fixture repository containing a function with a failing test. A
scripted model should:

1. list and read the relevant files;
2. apply a minimal patch;
3. run the documented test command;
4. inspect the diff;
5. return a concise completion message.

The test must assert the resulting diff, canonical runtime transcript, event
order, and final task state. The same scenario should then be exercised with the
DeepSeek adapter behind an opt-in integration test.

## Definition of done for the first task

- `go test ./...` passes.
- `go vet ./...` passes.
- the fixture acceptance scenario is deterministic;
- no provider secret appears in events or subprocess environments;
- local execution starts no server;
- the public command is consistently named `erka`;
- new work remains inside the M0/M1 boundaries in `docs/10-roadmap.md`.

