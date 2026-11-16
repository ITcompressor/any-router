# any-router

Маршрутизатор-прокси LLM-запросов: одна точка входа `/v1/*` с роутингом по имени
модели `provider/model` к разным провайдерам (OpenAI-совместимые API), веб-админка
`/admin`, CLI. Один бинарь, только стандартная библиотека Go + `gopkg.in/yaml.v3`.

EN: a single-binary Go proxy that routes OpenAI-compatible `/v1` requests by model
name (`provider/model`) to multiple backends, with a web admin UI and CLI.

**Продуктовый обзор — лендинг https://any-router.e7.by/**

## Возможности

- Роутинг `/v1/*` по модели: `openai/gpt-4o-mini`, `ollama/llama3`, … — имя
  модели переписывается под бэкенд, стриминг SSE насквозь.
- Баланс-алиасы: групповые имена без префикса провайдера (`chat`, `fast`) —
  round-robin по участникам с фейловером; липкость бесед для эмбеддингов.
- API-токены с областью (scope `models`): каждому токену — свой список
  моделей/хостов/алиасов; админ-токен отдельный.
- Веб-админка `/admin` (i18n ru/en): провайдеры, токены, балансировка, Langfuse,
  готовые curl-примеры по capabilities модели; правки сохраняются в YAML атомарно.
- Неблокирующий экспорт трейсов в Langfuse (OTLP/JSON).
- Агрегация `/v1/models` с фоновым обновлением кэша.

## Быстрый старт

```bash
go install github.com/ITcompressor/any-router/cmd/any-router@latest
# или локальная сборка:
go build -o any-router ./cmd/any-router

./any-router serve          # :8085 из any-router.yaml (создаётся при первом запуске, 0600)
# админка: http://localhost:8085/admin  (админ-токен печатается при старте)
```

Минимальный `any-router.yaml` (создаётся автоматически; правится руками или в админке):

```yaml
listen: ":8085"
token: "<админ-токен>"
lang: ru
providers:
  local:
    baseurl: http://localhost:11434/v1     # Ollama / openai-совместимый
    apikey: ""
tokens:
  - id: demo
    token: "<api-токен>"
    scope: models
    models: ["local/qwen3"]
```

⚠️ Конфиг содержит токены в открытом виде — файл не коммитится в git
(в `.gitignore`), права 0600.

## CLI

| Команда | Что делает |
|--|--|
| `serve` (по умолчанию) | HTTP-сервер: API `/v1` + веб-админка `/admin` |
| `models` | список моделей провайдеров (ходит в бэкенды напрямую) |
| `version` | версия + commit/время сборки |
| `help` | справка |

Оверрайды запуска: `-config`, `-addr`, `-port`, `-token`, `-base-url`,
`-log-level`; env — `ANY_ROUTER_ADDR|PORT|TOKEN|BASE_URL|CONFIG|LOG_LEVEL`
(флаги > env > дефолты).

## Сборка

```bash
go build -o any-router ./cmd/any-router
GOOS=linux GOARCH=amd64 go build -o any-router-linux-amd64 ./cmd/any-router
go test ./...
```

Метаданные сборки (`version`, `GET /api/version`, топбар админки)
вкачиваются через `-ldflags`: `-X github.com/ITcompressor/any-router/internal/version.Commit=<sha>`
и `Internal/version.BuildTime=<время>`.

## Документация

Подробные разделы (устройство, YAML-конфиг, деплой, админка) и PlantUML-схемы —
в монорепе проекта: `docs/any-router/` в [project-phoenix](https://git.e7.by/second_constantine/project-phoenix).
Инструкции для агентов — [AGENTS.md](AGENTS.md).
