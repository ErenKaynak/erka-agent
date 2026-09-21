# Providers, bridges, and authentication

## Connection classes

### Native API connection

The runtime sends model requests directly and executes all tools itself. Examples
include DeepSeek, OpenAI Platform, GLM endpoints, and compatible local servers.
These connections provide the intended provider-independent experience.

### Subscription bridge

A bridge invokes an officially supported vendor agent surface to consume a
consumer or workspace subscription. The vendor may own its internal loop and
tools. Bridge output is normalized into `erka` events and checkpoints, but the
integration must not be described as native.

Initial bridges:

- Codex with ChatGPT subscription authentication.
- OpenCode Go when its subscription access requires the OpenCode runtime.

### Local model connection

A local endpoint such as Ollama or vLLM behaves as a native provider, but its
health, context length, and tool-call quality depend on local configuration.

## ChatGPT subscription boundary

Official OpenAI behavior distinguishes ChatGPT subscription access through Codex
from usage-billed OpenAI API access. `erka` must not reuse undocumented consumer
tokens as general API credentials.

Supported design:

- `erka auth login codex --device` launches or delegates to the official Codex
  authentication flow.
- Credentials remain in an isolated Codex credential store.
- The bridge runs a bounded stage and emits canonical `erka` events.
- Native OpenAI access is configured separately with an OpenAI Platform API key.

Implemented M3 command surface:

```bash
erka auth login codex --device
erka auth login opencode
erka auth status
erka providers
```

Bridge connections use `class: subscription_bridge` and are accepted only in
planning and review routes. Codex runs through its official read-only,
non-interactive JSONL and output-schema interface. OpenCode runs through its
JSON event interface with a deny-by-default plan profile and project
configuration disabled. Both executables are optional runtime discoveries.

The Codex bridge is most useful for planning and review. A task can hand its
structured plan to a native DeepSeek implementation stage without transferring
vendor-private session state.

## Initial provider registry

```text
deepseek       native API             required for first vertical slice
openai         native API             usage-billed, separate from ChatGPT
codex          subscription bridge    optional ChatGPT entitlement path
opencode       subscription bridge    optional OpenCode Go path
openai_compat  configurable native    later generic endpoints
```

GLM may initially use a tested OpenAI-compatible profile and later gain a native
adapter if streaming or tool-call differences require it. Claude should use a
native Anthropic Messages adapter rather than being forced through an OpenAI
compatibility layer. Abacus AI and similar gateways should begin as configurable
profiles and receive custom code only when conformance tests reveal a need.

## Secret storage

Local mode uses the operating-system credential store when available, with an
explicit encrypted or permission-restricted file fallback. Server mode stores
secrets encrypted at rest or delegates them to a supported secret manager.

Secrets are referenced by ID. Database rows and event payloads never contain the
secret value. A model adapter receives only its own credential. A tool or MCP
server receives only explicitly granted secret names. General shell tools do not
inherit the runtime's provider environment.

## Provider conformance

Every native adapter is tested against the same contract:

- non-streaming and streaming text;
- fragmented tool-call arguments;
- multiple tool calls;
- malformed arguments;
- usage and cache fields;
- cancellation and timeout;
- rate limits and retry hints;
- context-limit and unsupported-feature errors.

Capability metadata is evidence from conformance tests, not marketing labels.
