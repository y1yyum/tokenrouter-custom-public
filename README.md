<div align="center">
  <p>
    <img src="assets/readme/banner.svg" width="100%" alt="TokenRouter, an open source LLM gateway. Messages, Responses, Chat Completions, and Gemini requests enter one endpoint and are routed to upstream platforms such as Anthropic, OpenAI, and Gemini, with failover when an upstream returns an error." />
  </p>

  <p>
    <a href="https://github.com/y1yyum/tokenrouter-custom-public/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/y1yyum/tokenrouter-custom-public/ci.yml?branch=main&style=flat-square&label=CI&logo=githubactions&logoColor=white&labelColor=27272A" alt="CI" /></a>
    <a href="https://github.com/y1yyum/tokenrouter-custom-public/actions/workflows/security-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/y1yyum/tokenrouter-custom-public/security-scan.yml?branch=main&style=flat-square&label=Security&logo=githubactions&logoColor=white&labelColor=27272A" alt="Security Scan" /></a>
    <a href="https://github.com/TokenFlux/TokenRouter/commit/4ced45bb1854eb3296015fb4b092b64a8c263b46"><img src="https://img.shields.io/badge/upstream-TokenFlux%2FTokenRouter%20%40%204ced45bb-3F3F46?style=flat-square&labelColor=27272A" alt="Upstream: TokenFlux/TokenRouter at 4ced45bb" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-LGPL--3.0--or--later-3F3F46?style=flat-square&labelColor=27272A" alt="License: LGPL-3.0-or-later" /></a>
  </p>

  <p><strong>English</strong> · <a href="README_ZH.md">简体中文</a> · <a href="#customizations-over-upstream">Customizations</a> · <a href="#companion-plugin">Companion plugin</a> · <a href="#quick-start">Quick start</a> · <a href="#documentation">Documentation</a></p>
</div>

## Overview

`y1yyum/tokenrouter-custom-public` is a public customized fork of [TokenFlux/TokenRouter](https://github.com/TokenFlux/TokenRouter), synced against upstream commit `4ced45bb` (after `v0.7.6`). TokenRouter puts model services from Anthropic, OpenAI, Gemini, and other vendors behind one gateway endpoint, with unified billing, scheduling, protocol conversion, and a Vue 3 management console.

On top of upstream TokenRouter, this fork adds scheduled per-model quality grading and isolation, readonly HTTP route previews, 24-hour scheduling quality metrics, settle-once streaming usage accounting across HTTP SSE and WebSocket Responses, portal and marketplace refinements, and stricter site API key lifecycle boundaries. The repository contains source code, migrations, tests, and deployment templates only, without production credentials or private operational configurations.

> [!WARNING]
> This repository is an independent fork and is not operated, authorized, or endorsed by TokenFlux. Commercial API gateway services are permitted under the applicable open-source licenses, provided operators do not impersonate the official TokenFlux service ([tokenflux.com](https://tokenflux.com)) or create confusion around official affiliation. Commercial operators should customize frontend branding and clearly identify the actual operator. See the [Commercial Use and Branding Notice](BRANDING_EN.md).

## Customizations over upstream

### Scheduled quality probing and per-model isolation

Scheduled test plans (`scheduled_test_plans`) can opt into graded arithmetic Responses probes (`quality_enabled`, added in migration `318_scheduled_quality.sql`). Grading checks for the exact expected JSON integer array (`arithmetic-array-v1`) and records `pass`, `fail`, or `ungraded` separately from transport errors.

Valid verdicts are written with replay protection into `provider_plan_model_verdicts` and `provider_plan_model_state` (migration `317_provider_plan_model_verdicts.sql`), keyed by `(provider_id, upstream_model, plan_id)`. Failing probes isolate only the failing `(provider_id, upstream_model)` pair for up to 24 hours when a plan stops, so other models on the same provider and other providers serving the same model stay available. Only a passing probe from the owning plan clears that plan's restriction.

When `gateway.openai.plan_model_restrictions` is enabled (`false` by default in `deploy/config.example.yaml`), restrictions are enforced during candidate selection and rechecked right before each outbound send across:

- OpenAI Responses HTTP (`POST /v1/responses`)
- OpenAI Responses WebSocket (`GET /v1/responses`) in `per_session`, `pooled`, and `http_bridge` modes
- Compact endpoints (`/v1/responses/compact` and request-body compact overrides)
- Chat Completions (`/v1/chat/completions`) and Anthropic Messages (`/v1/messages`) after authorized group fallback into Responses

Bound continuation requests (`previous_response_id`) fail closed before upstream dispatch if the bound provider is restricted for the resolved model rather than silently switching providers.

### Model exclusions, route previews, and quality metrics

Administrators can inspect active plans, verdict ownership, and scheduling eligibility in Model Attributes and on `/admin/model-exclusions`. Readonly route previews trace the client protocol, optional source group, destination group, protocol conversion, resolved outbound model, and exclusion reasons without issuing upstream requests or creating usage records.

Quality views report four 24-hour UTC metrics per `(provider_id, upstream_model)` and return `Unknown` when the denominator is zero:

| Metric | Definition |
| --- | --- |
| Observed availability | Successful user requests divided by successful user requests plus attributable terminal provider failures |
| Cache request hit rate | Successful requests with cache reads divided by successful requests with positive input |
| Cache token share | Cache read tokens divided by total input tokens (regular input, cache creation, and cache read) |
| Synthetic quality failure rate | Failed graded probes divided by total graded probes (excluding `ungraded` transport failures) |

### Streaming usage accounting and cache tiers

HTTP SSE and WebSocket Responses retain pre-terminal input, cache, and output usage reported before sparse or duplicate completion frames. Messages 5m/1h and Responses 30m/12h compatibility cache write pricing fields are preserved in usage logs and billing. Under late usage frames, duplicate completion events, and client disconnect retries, each turn produces one usage record and one billing settlement key.

### Portal, marketplace, usage, and embed isolation

- Custom embedded pages (`CustomPageView`) open external URLs without forwarding host session cookies or tokens, and refresh user profile state cleanly after returning.
- Model Marketplace separates an empty catalog from an unmatched search query, preserves guest search terms across login redirects, clears stale brand filters when switching identities, and matches public vs. authorized exclusive group pricing.
- User `/dashboard` and `/usage` align hour and day aggregations, heatmaps, and summaries with the browser's IANA timezone, reset pagination immediately on model filter changes, and hide stale rows with an inline retry when a page fetch fails.

### Authorization and site key boundaries

The site `admin_api_key` (`x-api-key`) continues to work on general admin endpoints, while inspecting its masked status, rotating it, or deleting it requires an authenticated administrator JWT session. Key rotation returns `Cache-Control: no-store`. MCP execution at `/mcp` enforces per-operation grants, confirmation policies, and team owner/member visibility boundaries.

## Companion plugin

External upstream site monitoring is maintained in the standalone [y1yyum/tokenrouter-upstream-sites](https://github.com/y1yyum/tokenrouter-upstream-sites) repository (`local.upstream-sites`, verified with `v1.1.1` against commit `65cda151`). It installs through `/admin/plugins` using the official plugin SDK without patching the host binary.

The plugin monitors upstream account balances and probes, tracks balance multipliers and estimated CNY costs, and optionally installs `control_bridge.sql` in PostgreSQL to pause bound providers on low balance while preserving manual administrator disablement decisions upon recovery.

## Base capabilities and protocols

As a TokenRouter fork, the core platform covers unified routing, upstream adapters, access control, billing, moderation, and plugin hosting. The [upstream provider capability matrix (Chinese)](docs/interfaces/upstream_provider_matrix.md) lists exact endpoint support per platform.

| Area | Summary |
| --- | --- |
| Client endpoints | Anthropic Messages (`POST /v1/messages`), OpenAI Responses & Chat (`POST /v1/responses`, `POST /v1/chat/completions`), Gemini (`POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent`), WebSocket (`GET /v1/responses`, `GET /v1/realtime`), Embeddings (`POST /v1/embeddings`), Images (`/v1/images/generations`, `/edits`, `/batches`), Video (`/v1/videos/generations`, `/edits`, `/extensions`), Voice (`/v1/tts`, `/v1/stt`, `/v1/custom-voices`), Search (`/v1/web_search`, `/v1/x_search`, `/v1/alpha/search`), Models & Usage (`GET /v1/models`, `GET /v1/usage`) |
| Upstream platforms | Anthropic, OpenAI, Gemini, Antigravity, Grok, Qoder, Kimi, Zhipu, DeepSeek, OpenCode, and Jev via OAuth, Setup Token, API Key, AWS Bedrock, Vertex AI, or Cosy |
| Access and security | Users, teams, groups, API key quotas and RPM/concurrency limits, email/password, Passkey, TOTP 2FA, and OAuth (GitHub, Google, LinuxDo, OIDC, WeChat, DingTalk) |
| Billing and payments | Usage-based billing with balance, subscriptions, quota packs, redeem codes, referral rebates, and built-in EasyPay, Alipay, WeChat Pay, Stripe, Airwallex, and EPUSDT (EPay) integrations |
| Operations and plugins | Content moderation rules, usage logs and background CSV export tasks, creative studio, online updates, and MIT-licensed Go/Web plugin SDKs |

## Screenshots

### Dashboard

<img src="assets/screenshots/dashboard-dark.jpg" alt="TokenRouter Dashboard · Dark mode" width="1600" />

### Model Marketplace

<img src="assets/screenshots/models-dark.jpg" alt="TokenRouter Model Marketplace · Dark mode" width="1600" />

<details>
<summary>Light mode screenshots</summary>

### Dashboard

<img src="assets/screenshots/dashboard-light.jpg" alt="TokenRouter Dashboard · Light mode" width="1600" />

### Model Marketplace

<img src="assets/screenshots/models-light.jpg" alt="TokenRouter Model Marketplace · Light mode" width="1600" />

</details>

## Quick start

> [!NOTE]
> `deploy/docker-deploy.sh` and `deploy/install.sh` reference upstream `TokenFlux/TokenRouter` release assets. To deploy this fork with its customizations, build from a checkout of `y1yyum/tokenrouter-custom-public`.

### Option 1: Docker Compose from source

Clone the repository and start TokenRouter together with PostgreSQL 18 and Redis 8:

```bash
git clone https://github.com/y1yyum/tokenrouter-custom-public.git
cd tokenrouter-custom-public/deploy

cp .env.example .env
chmod 600 .env
# Edit .env and set strong values for POSTGRES_PASSWORD, JWT_SECRET, and TOTP_ENCRYPTION_KEY

docker compose -f docker-compose.dev.yml up --build -d
```

For production deployments using `deploy/docker-compose.local.yml`, build the container image locally from the repository root first, and set `pull_policy: never` (or build with your own image tag) so Compose does not pull an upstream image over your local build:

```bash
./deploy/build_image.sh
```

Open `http://127.0.0.1:8080` after startup. If `ADMIN_PASSWORD` is left blank in `.env`, the initial admin password is printed in the container logs:

```bash
docker compose -f deploy/docker-compose.dev.yml logs tokenrouter | grep "admin password"
```

### Option 2: Enable scheduled quality gating

Scheduled quality verdicts are conservative by default on fresh installations:

1. In `config.yaml`, set `gateway.openai.plan_model_restrictions: true` and restart the server.
2. In the admin panel under provider scheduled tests, enable `quality_enabled` on the test plans that should enforce per-model routing restrictions.

## Database migrations and configuration

This fork includes 354 forward-only SQL migrations in `backend/migrations/` (`001` through `318`, adding `317_provider_plan_model_verdicts.sql` and `318_scheduled_quality.sql` on top of upstream `001` to `316`). Migrations are embedded into the server binary and applied automatically on startup.

When upgrading or rolling back:

- Stop incoming traffic and shut down the running instance gracefully before upgrading; replace backend and frontend artifacts together.
- Back up PostgreSQL, Redis, `config.yaml` (version 2), and `/app/data` (`plugins/`, `pages/`, and installation lock) before running migrations.
- Disabling `gateway.openai.plan_model_restrictions` turns off quality-based routing enforcement without modifying the database schema. Rolling back schema migrations requires restoring the pre-upgrade database backup.
- Clients that previously read, rotated, or deleted the site `admin_api_key` using `x-api-key` receive HTTP 403 and must switch to an administrator JWT session.

See [deployment and migrations (Chinese)](docs/operations/deployment_and_migrations.md) and [upgrade notes (Chinese)](docs/operations/upgrade_notes.md) for detailed procedures.

## Development and verification

Pinned toolchain requirements in this repository:

| Component | Version source | Current pin |
| --- | --- | --- |
| Go | `backend/go.mod` | `1.27.4` |
| Node.js | `.node-version` | `26.12.1` |
| pnpm | `frontend/package.json` | `12.9.1` |
| golangci-lint | `.golangci-version` | `v2.14.0` |
| PostgreSQL / Redis | `deploy/docker-compose.local.yml` | `postgres:18-alpine`, `redis:8-alpine` |

Common `Makefile` targets:

```bash
# Configure git pre-push checks
make hooks

# Lint, test, and build embedded binary
make lint
make test
make build
make test-embed

# Integration tests (requires Docker) and doc integrity check
make test-integration
make check-docs

# Full local CI equivalent and dependency security scan
make verify
make security
```

GitHub Actions workflows in `.github/workflows/ci.yml` (`plugin-sdk`, `go-lint`, `go-test`, `go-integration`, `frontend`, `build`, `scripts`, `installer`) and `.github/workflows/security-scan.yml` (`backend-security`, `frontend-security`) validate pushes and pull requests on Ubuntu.

## Documentation

- [Engineering documentation index (Chinese)](docs/index.md): architecture, domain invariants, API contracts, and operational constraints.
- [Usage and operations guides (Chinese)](docs/guides/index.md): deployment, payment setup, and plugin development.
- [Development workflow and upstream sync (Chinese)](docs/operations/development_workflow.md): dependency rules, file layout, verification tiers, and fork sync rules.
- [Docker deployment reference](deploy/DOCKER.md): Compose topologies, standalone environment variables, and v2 configuration upgrades.

## License and security

This project is distributed under the [GNU Lesser General Public License v3.0 or later](LICENSE). The `plugin-sdk/` directory is licensed under the [MIT License](plugin-sdk/LICENSE).

Copyright (c) 2026 Wesley Liddick & TokenFlux, with fork modifications by repository contributors.

To report a security issue affecting this fork, use [GitHub Private Vulnerability Reporting](https://github.com/y1yyum/tokenrouter-custom-public/security/advisories) on this repository rather than opening a public issue. Vulnerabilities affecting unmodified upstream code should follow [SECURITY.md](SECURITY.md).
