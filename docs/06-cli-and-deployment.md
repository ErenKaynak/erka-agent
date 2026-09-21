# CLI and deployment modes

## One command surface

The executable is `erka`. It selects a local in-process runner by default and a
remote runner only when a remote profile is requested.

### Local commands

```bash
erka init
erka run "Add pagination to the orders endpoint"
erka plan "Replace session auth with passkeys"
erka review
erka tasks
erka show <task-id>
erka continue <task-id> "Handle the mobile layout too"
erka diff <task-id>
erka cancel <task-id>
```

Local state is stored under platform-standard application directories. Project
configuration may live in `.erka/config.yaml`; secrets never do.

### Provider commands

```bash
erka providers
erka provider add deepseek
erka provider test deepseek-main
erka auth login codex --device
erka auth status
```

### Server commands

```bash
erka serve
erka serve --config /etc/erka/config.yaml
```

### Remote client commands

```bash
erka profile add home https://erka.example.com
erka --profile home projects
erka --profile home run --project shop "Fix checkout retries"
erka --profile home watch <task-id>
erka --profile home approve <approval-id>
```

## Local mode

Local mode operates directly on the current repository. It uses the shared
application layer, runtime engine, store, and event model without HTTP. It may run
synchronously in the foreground initially. Later, an explicit local background
mode can persist work after the terminal closes.

## Server mode

Server mode runs a durable queue and worker against registered server-side
repositories and worktrees. Client disconnection never cancels a task. The
server supports event resume, cancellation, approvals, retention, and resource
limits.

A remote server cannot transparently edit files that exist only on the client's
machine. Remote tasks operate on server-side clones or worktrees. A future local
worker bridge may connect a local filesystem to a remote control plane, but it is
not part of version one.

## Configuration precedence

From highest to lowest priority:

1. command flags;
2. environment variables prefixed with `ERKA_`;
3. selected profile;
4. project configuration;
5. user configuration;
6. built-in defaults.

`erka config explain` should eventually show the resolved value and source for a
setting without revealing secrets.

## Distribution

Release signed or checksummed binaries for macOS, Linux, and Windows. Server
packaging should include a minimal container image and a documented systemd
service. Docker is a deployment option, not a runtime requirement for local use.

The CLI and server API negotiate versions. A remote CLI should fail clearly when
major protocol versions are incompatible and should tolerate additive fields.

