<div align="center">
  <img src="assets/logo.svg" alt="ConcordRoute" width="112" />

  <h1>ConcordRoute</h1>

  <p><strong>A TokenRouter-derived branch focused on identity, transport, and egress consistency</strong></p>

  <p>
    <a href="https://github.com/OneB1ank/ConcordRoute/actions/workflows/backend-ci.yml"><img src="https://github.com/OneB1ank/ConcordRoute/actions/workflows/backend-ci.yml/badge.svg" alt="CI" /></a>
    <a href="https://github.com/OneB1ank/ConcordRoute/releases"><img src="https://img.shields.io/github/v/release/OneB1ank/ConcordRoute?display_name=tag" alt="Release" /></a>
    <a href="https://github.com/OneB1ank/ConcordRoute/pkgs/container/concordroute"><img src="https://img.shields.io/badge/container-ghcr.io%2Foneb1ank%2Fconcordroute-2496ED?logo=docker&logoColor=white" alt="Container" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-LGPL--3.0--or--later-4c1.svg" alt="License: LGPL-3.0-or-later" /></a>
  </p>

  <p><a href="README.md">简体中文</a> | <strong>English</strong></p>
</div>

## Positioning

ConcordRoute is primarily derived from [TokenRouter](https://github.com/TokenFlux/TokenRouter). It retains the upstream gateway, account scheduling, usage, subscription, billing, and administration capabilities while strengthening identity, protocol, and network-egress consistency for Codex/OpenAI multi-account deployments.

The project does not increase upstream account quotas. Its quota-related work reduces redundant requests, cache invalidation, invalid probes, and misleading usage observations caused by account switches, session drift, cache-key changes, inconsistent client characteristics, unstable proxy egress, or mismatched logging semantics. Actual quotas, rate limits, and dynamic policies remain controlled by the upstream service.

## ConcordRoute Features

- Account-scoped Cockpit installation/device and primary session identities.
- Conversation-scoped thread, window, turn, and prompt-cache-key isolation.
- Configurable uTLS ClientHello profiles with TLS, ALPN, and extension validation.
- Consistent TLS behavior across direct HTTPS and HTTPS proxy paths.
- Clash/mihomo nodes, strategies, and per-account stable egress bindings.
- Unified session, TTFT, cache-hit, reasoning-effort, and usage observability.
- Consistent OAuth, Codex Session/PAT, model whitelist, and mapping persistence.

## Recommended Operation

1. Capture TLS ClientHello and UA values from the client environment you actually use. Public captures are references, not universal templates.
2. Keep UA, operating system, client version, TLS extensions, ALPN, HTTP protocol, and runtime environment mutually consistent.
3. Bind long-lived accounts to stable egress nodes. Prefer controlled `fallback` over cross-region `load-balance` for identity-sensitive traffic.
4. Use Cockpit mode by default: account-level device/session stability with conversation-level thread and cache-key isolation.
5. Diagnose behavior with errors, TTFT, cache hits, 429/529 responses, proxy health, and account usage together instead of interpreting one metric as a quota change.

See the [fingerprint, identity, and egress consistency guide](docs/guides/fingerprint-consistency.md) for details.

## Upstreams and Acknowledgements

- [TokenRouter](https://github.com/TokenFlux/TokenRouter): the primary codebase and continuously synchronized upstream.
- [Sub2API](https://github.com/Wei-Shaw/sub2api): TokenRouter's upstream foundation and core gateway architecture.
- [cockpit-tools](https://github.com/jlcodes99/cockpit-tools): reference for Codex device, session, thread, and cache-key consistency.
- [LightBridge](https://github.com/WilliamWang1721/LightBridge): reference for proxy integration, network egress, and client-consistency work.

Thanks to their maintainers, contributors, issue and pull-request authors, and researchers who shared protocol captures. Adaptations and additions in this repository are maintained independently by this branch.

## Deployment and Documentation

- [Deployment guide (Chinese)](docs/guides/deployment/index.md)
- [Docker image documentation (Chinese)](deploy/DOCKER.md)
- [Fingerprint consistency guide (Chinese)](docs/guides/fingerprint-consistency.md)
- [Engineering documentation (Chinese)](docs/index.md)

## License

This project is distributed under the [GNU Lesser General Public License v3.0 or later](LICENSE), with upstream copyright and license notices retained.

Copyright (c) 2026 Wesley Liddick & TokenFlux
