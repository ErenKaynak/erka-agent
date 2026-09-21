# Native runtime and tools

## Agent loop

For a native provider, one stage follows this loop:

1. Resolve the stage configuration and budget.
2. Build a context packet from instructions, checkpoint, relevant history, and
   repository state.
3. Request a streaming model response with the allowed tool schemas.
4. Persist completed text and tool-call items.
5. Validate every tool call against its schema and policy.
6. Execute allowed tool calls, preferably concurrently only when declared safe.
7. Persist tool results with their call IDs before the next model request.
8. Repeat until final output or a guard terminates the run.
9. Produce a stage result and updated task checkpoint.

Every iteration has a deadline, output limit, and cost check. The run has maximum
iterations, wall time, cumulative model tokens, cumulative tool-output bytes,
and optional currency budget.

## Canonical runtime items

- `user_message`
- `assistant_message`
- `reasoning_summary` when a provider exposes an allowed summary
- `tool_call`
- `tool_result`
- `usage`
- `checkpoint`
- `error`

Hidden chain-of-thought is never requested for storage and never persisted.
Provider-specific raw data may be retained only in bounded, redacted diagnostic
records.

## Initial built-in tools

### Filesystem

- `list_files`
- `read_file`
- `search_text`
- `apply_patch`
- `write_file`

All paths are relative to the sandbox root. Resolution must reject absolute
paths, traversal, symlink escape, special files, oversized files, and binary
content unless a tool explicitly supports it. Writes are atomic and return a
bounded summary or diff.

### Process

- `run_command`
- `run_validation`

Commands run without an interactive shell where possible. Repository-defined
commands may use a shell inside the sandbox. Cancellation terminates the whole
process group. The runtime caps stdout/stderr, execution time, environment, and
resource use.

### Git

- `git_status`
- `git_diff`
- `git_log`

Commit, push, pull request, and deployment are application actions behind policy
and approval. They are not ordinary model tools. Removing `git push` from a
command allowlist is not a security boundary; credentials and network access must
make unauthorized publication impossible.

## Permission modes

- `plan`: no mutation; read-only repository inspection.
- `workspace`: mutation inside the scoped repository; no publication secrets.
- `full`: broader local execution chosen explicitly by the user.

Project policy separately controls network, dependency installation, commit,
push, pull request, and deployment. Unsupported `ask` decisions pause the run and
create a durable approval instead of silently denying or allowing.

## MCP and skills

Built-in coding tools do not depend on MCP. MCP is a later extension surface and
should use the official Go SDK. Skills are discovered by metadata and loaded on
demand; all installed skill bodies should not be injected into every prompt.

MCP servers and skills are untrusted capabilities. Each receives explicit tool,
filesystem, network, and secret grants. Versions or source commits should be
pinned for reproducible installations.

