# Third-Party Notices

GPT-Load includes third-party open-source software. This file covers the
components that require specific attribution, carry obligations beyond
attribution, or are modified by GPT-Load. Each release also ships a CycloneDX
SBOM (`bom.cdx.json`) inventorying the resolved Go module graph.

## Bifrost Core

- Module: `github.com/maximhq/bifrost/core`
- Version: `v1.8.4`
- Copyright: 2025 H3 Labs Inc.
- License: Apache License 2.0

GPT-Load uses Bifrost Core as an infrastructure adapter for provider execution
and protocol conversion. GPT-Load's domain models, persisted channel IDs,
scheduling, retry policy, health state, usage accounting, and pricing remain
owned by GPT-Load.

The complete Apache License 2.0 text is distributed in
`LICENSES/Apache-2.0.txt`.

## CLIProxyAPI

- Module: `github.com/router-for-me/CLIProxyAPI/v7`
- Version: `v7.3.6`
- Copyright: 2025-2005.9 Luis Pater; 2025.9-present Router-For.ME
- License: MIT License

GPT-Load uses a pinned, execution-only embedded adapter around CLIProxyAPI's
Codex, Claude, Antigravity, and xAI OAuth and HTTP executor code. GPT-Load retains ownership of
credential storage, account selection, retry, health, affinity, logging, and
usage policy; the embedded adapter does not use CLIProxyAPI's manager, account pool,
or file store. A separate, explicitly called Codex WebSocket session facade reuses
the pinned WS executor with HTTP fallback and business-request replay blocked.
The SDK can still attempt an extra handshake after a failed send; the facade
rejects replacement connection binding before another business request is sent.
This facade is not connected to the existing HTTP data plane.

The complete MIT License text is distributed in `LICENSES/MIT.txt`.

## fasthttp

- Module: `github.com/valyala/fasthttp`
- Version: `v1.74.0`
- Copyright: 2015-present Aliaksandr Valialkin, VertaMedia, Kirill Danshin, Erik
  Dubbelboer, FastHTTP Authors
- License: MIT License

GPT-Load uses the official upstream release through Bifrost Core for provider
HTTP requests and streaming responses.

The complete MIT License text is distributed in `LICENSES/MIT.txt`.

## go-brrr

- Module: `github.com/molecule-man/go-brrr`
- Version: `v1.0.1`
- Copyright: 2026 Andrii Berezhynskyi
- License: MIT License

GPT-Load includes go-brrr through fasthttp for Brotli compression and
decompression.

The complete MIT License text is distributed in `LICENSES/MIT.txt`.

## Lobe Icons

- Source: `@lobehub/icons-static-svg` `1.94.0` (vendored subset, not an npm
  dependency of the management UI)
- Copyright: 2023 LobeHub
- License: MIT License

GPT-Load vendors a subset of Lobe Icons' SVG marks (`web/src/assets/channels/`)
to identify built-in channel presets by their upstream provider's brand in the
management UI. The vendored icons and this notice do not grant any trademark
rights in the marks they depict.

The complete MIT License text is distributed in `LICENSES/MIT.txt`.
