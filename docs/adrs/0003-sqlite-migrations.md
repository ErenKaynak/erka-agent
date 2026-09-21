# ADR 0003: SQLite and migrations

## Status

Accepted for M1 on 2026-09-02.

## Decision

Use `modernc.org/sqlite` through `database/sql` and embedded migrations managed
by `golang-migrate/migrate/v4`.

## Consequences

- Release builds remain CGO-free across the supported operating systems.
- Migration versions and dirty state are explicit and recoverable.
- These dependencies are not added until M1 begins.
