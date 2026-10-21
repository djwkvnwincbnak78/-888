# Nexus

[![Go](https://img.shields.io/badge/go-1.23-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](https://github.com/djwkvnwincbnak78/nexus/blob/main/LICENSE)
[![Build Status](https://github.com/djwkvnwincbnak78/nexus/actions/workflows/ci.yml/badge.svg)](https://github.com/djwkvnwincbnak78/nexus/actions/workflows/ci.yml)

An LLM gateway that routes, caches and bills inference traffic across multiple
upstream providers.

Nexus sits in front of OpenAI-compatible providers and exposes a single
`/v1/chat/completions` surface. It handles provider failover, per-tenant rate
limiting, response caching, cost accounting and request-level observability.

## Features

- **OpenAI / Anthropic / Ollama** — same request shape, provider-specific adapters
- **Routing** — cost, latency, fixed and weighted strategies with circuit breakers
- **Rate limiting** — per-key limits, IP allowlisting, configurable burst
- **Caching** — exact-match plus optional embedding-backed semantic cache, scoped per tenant
- **Billing** — reserve/settle ledger with versioned pricing and a full audit trail
- **Prompt compression** — deterministic deduplication before the cache key and the bill
- **Shadow testing** — replay a request against a candidate model out-of-band
- **Request replay** — stored request bodies can be re-run against any served model
- **Admin console** — Vue 3 + Vite + TypeScript, served from `/console/`
- **Observability** — Prometheus metrics, W3C Trace Context propagation, structured logs

## Quick start

```bash
docker compose up -d
```

Or build and run directly:

```bash
go build -o nexus ./cmd/gateway
./nexus
```

The gateway reads its configuration from environment variables — see
`.env.example`. At minimum you need one provider credential:

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_MODELS=gpt-4o,gpt-4o-mini
export GATEWAY_API_KEYS=sk-nexus-local:1:100
```

Health check:

```bash
curl -s localhost:8080/health
```

## Architecture

```
client ──► rate limit ──► auth ──► compress ──► cache ──► router ──► provider
                                                              │
                                                              └──► ledger / audit
```

PostgreSQL holds channels, tokens, ledger entries, request logs and system
settings. Redis is optional and used for the shared cache and circuit-breaker
state; without it the gateway falls back to in-process storage.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the package layout and
[docs/openapi.yaml](docs/openapi.yaml) for the HTTP surface.

## Configuration

Runtime settings live in the `system_settings` table and are hot-reloaded
through `GET/PUT /admin/api/settings` — no restart required.

| Key | Default | Purpose |
| --- | --- | --- |
| `enable_compress` | `true` | Deterministic prompt compression |
| `enable_semantic_cache` | `true` | Embedding-backed cache layer |
| `enable_shadow_test` | `false` | Mirror traffic to a candidate model |
| `shadow_model` | — | Model used for shadow requests |
| `shadow_sample_rate` | `100` | Percentage of requests to shadow |
| `replay_enabled` | `true` | Allow replaying stored requests |

## Console

```bash
cd web/console
npm install
npm run dev     # proxies /admin/api and /v1 to :8080
npm run build   # emits dist/, served under /console/
```

Pages: dashboard, channels, tokens, request history (with replay), shadow
comparison and runtime settings.

## Dependencies

Built with a Go stdlib-first design; external dependencies are limited to the
PostgreSQL driver (`pgx`), the Redis client and `golang.org/x/crypto` for
bcrypt. The console uses Vue 3, Element Plus, ECharts and Tailwind.

## Development notes

This project was built with heavy use of AI agents for architecture
exploration, code generation and automated testing. Every change is reviewed,
compiled and exercised against the test suite before it lands; the design
decisions, trade-offs and the resulting code are the author's responsibility.

Run the suite with:

```bash
go vet ./...
go test ./... -count=1
```

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgements

- [pgx](https://github.com/jackc/pgx) for the PostgreSQL driver
- [go-redis](https://github.com/redis/go-redis) for the Redis client
- [Element Plus](https://element-plus.org/) and [Vue 3](https://vuejs.org/) for the console
- [Apache ECharts](https://echarts.apache.org/) for the dashboard charts
- The OpenAI, Anthropic and Ollama API teams, whose protocol shapes this gateway implements
