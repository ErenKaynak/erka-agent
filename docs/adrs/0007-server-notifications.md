# ADR 0007: initial notification target

## Status

Accepted for M4.

## Decision

The first notification sink is a generic HTTPS webhook. Committed terminal,
approval, and attention-required events enter a durable outbox keyed by event
ID. Delivery is at least once, retryable with bounded backoff, and independent
of task completion.

## Consequences

Web push and product-specific notification services remain later additions.

