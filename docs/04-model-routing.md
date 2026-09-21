# Model routing and stage pipelines

## Goal

Routing makes model choice a property of a stage rather than a property of the
whole task. A task can use a strong subscription-backed model for planning, an
inexpensive native API for implementation, a fast model for repairs, and an
independent model for review.

## Standard pipeline

```text
prepare
   │
   ▼
plan ──optional human approval──► implement
                                     │
                                     ▼
                                  validate
                                  │      │
                             success   failure
                                  │      │
                                  │    repair ──► validate
                                  ▼
                                 review
                                  │
                                  ▼
                                complete
```

Each stage receives a purpose-built context packet. Planning does not receive
unbounded tool logs. Implementation receives the approved plan and relevant
checkpoint. Repair receives validation failures and the current diff. Review
receives the request, plan, diff, and validation results but not implementation
chain-of-thought.

## Model references

Models are addressed by connection and model ID:

```text
chatgpt-personal/gpt-available-in-codex
deepseek-main/deepseek-v4-pro
openai-work/gpt-model-id
glm-main/glm-model-id
local-ollama/model-id
```

A connection describes credentials and protocol. A model catalog describes
capabilities such as tools, parallel tool calls, structured output, vision,
reasoning controls, context length, and pricing metadata.

## Routing modes

### Fixed

The user explicitly selects every stage. This is deterministic and the default
for early releases.

### Rules

Local rules select from ordered candidates using task size, file types, stage,
required capabilities, remaining budget, and provider health. No model call is
needed.

### LLM router

A configured router model receives only the task summary and eligible model
metadata. It returns a structured route plan. The runtime validates that:

- every connection is configured and healthy;
- the model supports the stage's required capabilities;
- the choice is within cost and privacy policy;
- a subscription bridge is permitted for the stage;
- fallbacks do not repeat the same unavailable endpoint.

Invalid or unavailable router output falls back to deterministic rules. Routing
must never be a single point of task failure.

## Example configuration

```yaml
pipelines:
  default:
    routing: fixed
    stages:
      planning:
        candidates:
          - chatgpt-personal/default
          - openai-main/gpt-planner
      coding:
        candidates:
          - deepseek-main/deepseek-v4-pro
          - glm-main/glm-fast
      repair:
        candidates:
          - glm-main/glm-fast
          - deepseek-main/deepseek-v4-flash
      review:
        candidates:
          - chatgpt-personal/default
```

Model IDs and prices change. They belong in runtime configuration or provider
discovery, not in compiled routing code.

## Budgets and fallback

Budgets exist per task and per stage:

- maximum input and output tokens;
- maximum iterations;
- maximum duration;
- maximum reported or estimated cost;
- maximum repair attempts.

Fallback is appropriate for connection errors, timeouts, rate limits, and model
unavailability. It is not automatically appropriate after a mutating tool call.
The persisted tool transcript and idempotency rules determine whether a run can
resume safely or must request user review.

