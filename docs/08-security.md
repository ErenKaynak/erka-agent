# Security model

## Trust boundaries

The following are untrusted:

- repository files and instructions;
- model output and tool arguments;
- command output;
- downloaded dependencies;
- skills and MCP servers;
- remote API input;
- subscription and provider responses.

The model is never a policy authority. Prompt instructions can help behavior but
cannot grant filesystem, network, credential, or publication access.

## Filesystem isolation

Every tool invocation is scoped to one canonical workspace root. Path resolution
must defend against absolute paths, `..`, symlinks, hard links where relevant,
device files, sockets, and time-of-check/time-of-use races.

Server tasks use isolated worktrees. A task must not read other projects,
worktrees, runtime state, or credentials. Local `full` mode may intentionally
widen access, but the user must opt in explicitly.

## Process isolation

The sandbox abstraction controls user identity, working directory, environment,
resource limits, filesystem mounts, network access, process groups, and teardown.
A dedicated non-root user is the minimum server boundary. A stronger namespace,
microVM, or OCI backend can be added without changing the tool interface.

String matching on shell commands is not enforcement. If push is denied, the
sandbox must not possess push credentials and should not have an authorized
publication path.

The implemented boundary is described in ADR 0008: commands run under
`sandbox-exec` on macOS or bubblewrap on Linux with workspace-only writes,
hidden credential stores, and read-only Git hooks and configuration.

## Network policy

Network is denied or restricted according to the selected mode and project
policy. Dependency installation is distinct from general network access.
Provider HTTP calls are made by the trusted runtime process, not from the tool
sandbox. Future egress allowlists belong below the shell layer.

## Secret isolation

- Credentials are encrypted or protected by the operating system.
- Events, logs, checkpoints, and prompts are redacted before persistence.
- Each provider adapter receives only its connection credential.
- Tool subprocesses receive a minimal allowlisted environment.
- MCP servers receive only explicitly mapped secrets.
- Subscription credential stores are isolated per local user or server account.
- Export and diagnostic commands omit secrets by default.

## Remote server security

Remote mode requires TLS at the server or trusted reverse proxy. CLI access uses
revocable, scoped tokens rather than copied browser cookies. Mutating requests
are authenticated and protected against cross-origin abuse. Server event streams
must enforce task authorization on both initial connection and resume.

Version one may be single-user, but identity must still be explicit so an open
source deployment can evolve without anonymous privileged endpoints.

## Supply-chain controls

Dependencies, MCP packages, skills, and provider binaries should be pinned and
verified where possible. Auto-downloading and executing `latest` packages is not
the default. Releases publish checksums and a software bill of materials.

## Required adversarial tests

- path traversal and symlink escape;
- oversized model and command streams;
- malformed or partial tool arguments;
- command timeout and process-tree cleanup;
- secret appearance in model output and logs;
- prompt instructions attempting to bypass policy;
- duplicate delivery and replay;
- crash during a mutating tool;
- unauthorized event-stream resume;
- denied Git publication despite arbitrary shell access.

