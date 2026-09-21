# ADR 0002: CLI library

## Status

Accepted for M0 on 2026-09-02.

## Decision

Use Cobra for the public `erka` command hierarchy without adding Viper.

## Consequences

- Subcommands, flags, help, version output, and future completions share one
  established command model.
- Configuration precedence remains explicit application code rather than being
  delegated to a broad configuration framework.
