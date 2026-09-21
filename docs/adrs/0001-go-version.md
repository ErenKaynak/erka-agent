# ADR 0001: Minimum Go version

## Status

Accepted for M0 on 2026-09-02.

## Decision

Use Go 1.26 as the minimum supported version. Test Go 1.26 and Go 1.27 in CI.

## Consequences

- Contributors can use either supported release line.
- The module may use Go 1.26 language and standard-library behavior.
- The minimum will be reconsidered as Go releases leave security support.
