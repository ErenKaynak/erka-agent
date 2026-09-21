# Open-source strategy

## Positioning

`erka-agent` should be presented as a self-hosted, provider-independent coding
agent runtime with a local CLI and optional remote server. It is not a proxy for
evading provider billing or subscription restrictions, and it is not a wrapper
that requires a third-party hosted control plane.

## User trust

- Local operation is complete without account creation.
- Telemetry is absent by default; any future telemetry is opt-in and documented.
- Provider credentials stay on the machine where execution occurs.
- The project clearly documents which integrations are native APIs and which are
  subscription bridges.
- No undocumented consumer authentication flow is implemented.
- Security limitations of `full` access are explicit.

## Provider contribution model

Most compatible services should be configurable without recompiling. Native
adapters are accepted when a provider has a distinct protocol or meaningful
streaming/tool semantics.

Every provider contribution includes:

- capability declaration;
- authentication documentation;
- mocked protocol tests;
- conformance-test results;
- redaction behavior;
- rate-limit and retry mapping;
- confirmation that integration uses a documented provider interface.

Do not use Go's dynamic plugin mechanism as the primary extension system because
it complicates portability and version compatibility. A future out-of-process
provider protocol can support third-party adapters without loading untrusted code
into the daemon.

## Licensing

Choose the license before accepting outside contributions:

- Apache-2.0 maximizes adoption and includes an explicit patent grant.
- AGPL-3.0 requires network-hosted modifications to remain available under the
  license and better protects against closed hosted forks.

The decision is intentionally open. Record it in an ADR before the first public
release. Verify that copied or adapted code from the previous project and all
dependencies are license-compatible. Prefer reimplementation from documented
behavior over copying provider-specific integration code.

## Governance and releases

- Use semantic versions for public CLI and API compatibility.
- Publish checksums, changelog, supported Go version, and migration notes.
- Maintain a security contact and private vulnerability-reporting path.
- Pin release toolchains and generate a software bill of materials.
- Keep roadmap decisions in versioned ADRs rather than chat history.
- Require tests for runtime, sandbox, provider, and persistence changes.

## Documentation required for first public release

- five-minute local quickstart;
- self-hosting guide;
- provider and subscription-bridge explanation;
- threat model and permission modes;
- configuration reference;
- provider contribution guide;
- task/context privacy behavior;
- backup and upgrade guide;
- known limitations.

