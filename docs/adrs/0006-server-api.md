# ADR 0006: clean versioned server API

## Status

Accepted for M4.

## Decision

M4 starts a clean JSON `/v1` API around the same application services used by
local mode. It does not copy AI-LXC payloads wholesale. Errors contain a stable
code and message, event streams resume from committed sequence numbers, and
additive response fields remain compatible.

## Consequences

Useful AI-LXC behavior may be preserved deliberately, but compatibility is not
an architectural constraint.

