# any-router

> A single-binary Go proxy for LLM requests: one `/v1/*` endpoint that routes by model
> name (`provider/model`) across multiple OpenAI-compatible providers — plus a web
> admin UI, scoped API tokens, and a CLI.

Also available in Russian: [README.ru.md](README.ru.md)

**Product overview — https://any-router.e7.by/**

## Features

- **Routing by model name**: `openai/gpt-4o-mini`, `local/llama3`, … — one OpenAI-compatible
  entry point; the model name is rewritten per backend, SSE streaming is passed through.
- **Balance aliases**: grouped bare names without a provider prefix (`chat`, `fast`) —
  round-robin across members with failover; conversation stickiness by session header
  or the first two messages.
- **Scoped API tokens** (`scope: models`): each token gets its own list of concrete
  models (`provider/model`), whole hosts (`provider/*`) and aliases; the admin token is separate. Fail closed.
- **Web admin UI** `/admin` (ru/en): providers, tokens, balancing, Langfuse, ready-made
  curl examples matched to each model's capabilities; edits are saved to YAML atomically (tmp + rename).
- **Non-blocking Langfuse export**: OTLP/JSON traces via an in-memory queue.
- **Aggregated `/v1/models`** with background cache refresh.
- Single Go binary, standard library + `gopkg.in/yaml.v3` only.

## Quick start

```bash
go install github.com/ITcompressor/any-router/cmd/any-router@latest
# or build locally:
go build -o any-router ./cmd/any-router

./any-router serve          # :8085 from any-router.yaml (autocreated on first start, mode 0600)
# admin UI: http://localhost:8085/admin   (the admin token is printed at startup)
```

Minimal `any-router.yaml` (autocreated; edit by hand or in the admin UI):

```yaml
listen: ":8085"
token: "<admin-token>"
lang: en
providers:
  local:
    baseurl: http://localhost:11434/v1     # Ollama / any OpenAI-compatible API
    apikey: ""
tokens:
  - id: demo
    token: "<api-token>"
    scope: models
    models: ["local/qwen3"]
```

⚠️ The config holds tokens in plain text — the file must not be committed to git
(it is in `.gitignore`); keep its mode at `0600`.

## CLI

| Command | What it does |
|--|--|
| `serve` (default) | HTTP server: `/v1` API + web admin `/admin` |
| `models` | list of provider models (queries the backends directly) |
| `version` | version + commit/build-time |
| `help` | help |

Start overrides: `-config`, `-addr`, `-port`, `-token`, `-base-url`, `-log-level`;
env — `ANY_ROUTER_ADDR|PORT|TOKEN|BASE_URL|CONFIG|LOG_LEVEL`
(flags > env > defaults).

## Build

```bash
go build -o any-router ./cmd/any-router
GOOS=linux GOARCH=amd64 go build -o any-router-linux-amd64 ./cmd/any-router
go test ./...
```

Build metadata (`version`, `GET /api/version`, the admin topbar) is injected via
`-ldflags`: `-X github.com/ITcompressor/any-router/internal/version.Commit=<sha>` and
`-X github.com/ITcompressor/any-router/internal/version.BuildTime=<time>`.

**Releases**: push a `v*` tag — GitHub Actions runs the tests, builds the four
binaries with checksums and publishes a Release automatically
([Releases](https://github.com/ITcompressor/any-router/releases)).

## Configuration

- File: `any-router.yaml` in the working directory (created on first start, mode 0600).
  What it holds: `listen`, `token` (admin), `lang` (`ru|en`), `providers`
  (map: `baseurl`, `apikey`), `tokens` (API tokens: `id`/`token`/`scope` — the
  `models` scope is a list of concrete models `provider/model`, whole hosts
  `provider/*` and balance aliases (bare names); legacy scopes `all`/`provider`/`model`
  are still accepted, and the ✎ button in the admin shows the equivalent and rewrites
  them to `models`), `balances` (alias groups — bare names without a provider prefix,
  `[{name, models: [provider/model, …]}]`: a bare-name request is served by round-robin
  over the members with failover; a scoped token may call an alias when the bare name
  falls within its `models` scope or when its scope covers all members), `langfuse`
  (`enabled` — toggle the export without dropping the settings, `url`, keys,
  `environment`, `tags`).
- Admin-UI edits are saved to the YAML atomically (tmp + rename).
- Overrides (flags > env > defaults): `-config`, `-addr`, `-port`, `-token`,
  `-base-url`, `-log-level`; env — `ANY_ROUTER_ADDR|PORT|TOKEN|BASE_URL|CONFIG|LOG_LEVEL`.

## Security

- The `any-router.yaml` config holds tokens in plain text and must not be committed
  (it is in `.gitignore`); the server never logs tokens or keys into responses or
  error messages.
- Auth for `/v1` and `/admin` is Bearer-only (admin or API token) and fails closed.

## Documentation

Agent instructions — [AGENTS.md](AGENTS.md) (in Russian). Detailed design docs and
PlantUML diagrams live in the private project monorepo.

## License

Released under the [MIT License](LICENSE).
