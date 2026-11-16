# any-router

> Маршрутизатор-прокси LLM-запросов: единая точка входа `/v1/*` с роутингом по имени
> модели (`provider/model`) к разным OpenAI-совместимым провайдерам, веб-админка,
> API-токены с областью действия и CLI.

English version: [README.md](README.md)

**Продуктовый обзор — https://any-router.e7.by/**

## Возможности

- Роутинг `/v1/*` по модели: `openai/gpt-4o-mini`, `ollama/llama3`, … — имя
  модели переписывается под бэкенд, стриминг SSE насквозь.
- Баланс-алиасы: групповые имена без префикса провайдера (`chat`, `fast`) —
  round-robin по участникам с фейловером; липкость бесед для эмбеддингов.
- API-токены с областью действия (scope `models`): каждому токену — свой список
  моделей/хостов/алиасов; админ-токен отдельный. Авторизация fail closed.
- Веб-админка `/admin` (i18n ru/en): провайдеры, токены, балансировка, Langfuse,
  готовые curl-примеры по capabilities модели; правки сохраняются в YAML атомарно (tmp + rename).
- Неблокирующий экспорт трейсов в Langfuse (OTLP/JSON).
- Агрегация `/v1/models` с фоновым обновлением кэша.
- Один бинарь, только стандартная библиотека Go + `gopkg.in/yaml.v3`.

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
    baseurl: http://localhost:11434/v1     # Ollama / openai-совместимый API
    apikey: ""
tokens:
  - id: demo
    token: "<api-токен>"
    scope: models
    models: ["local/qwen3"]
```

⚠️ Конфиг содержит токены в открытом виде — файл не коммитится в git
(в `.gitignore`), права должны оставаться `0600`.

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
и `-X github.com/ITcompressor/any-router/internal/version.BuildTime=<время>`.

**Релизы**: пуш тега `v*` — GitHub Actions сам гоняет тесты, собирает четыре
бинаря с контрольными суммами и публикует Release
([Releases](https://github.com/ITcompressor/any-router/releases)).

## Конфигурация

- Файл: `any-router.yaml` в рабочей директории (создаётся при первом запуске, права 0600).
  Что хранит: `listen`, `token` (админ), `lang` (`ru|en`), `providers` (map:
  `baseurl`, `apikey`), `tokens` (API-токены: `id`/`token`/`scope` — область
  `models`: список конкретных моделей `provider/model`, хостов целиком
  `provider/*` и баланс-алиасов (голые имена); legacy-области
  `all`/`provider`/`model` признаются сервером, кнопка ✎ в админке показывает
  эквивалент и переписывает в `models`), `balances` (баланс-алиасы — групповые
  имена без префикса провайдера, `[{name, models: [provider/model, …]}]`:
  запрос на голое имя без «/» — round-robin по участникам с фейловером;
  scope-токен вправе звать алиас по имени, когда голое имя входит в его область
  `models` или когда его область покрывает всех участников), `langfuse`
  (`enabled` — переключатель выгрузки без удаления настроек, `url`, ключи,
  `environment`, `tags`).
- Правки из админки сохраняются в YAML сразу (атомарно: tmp + rename).
- Оверрайды (флаги > env > дефолты): `-config`, `-addr`, `-port`, `-token`,
  `-base-url`, `-log-level`; env — `ANY_ROUTER_ADDR|PORT|TOKEN|BASE_URL|CONFIG|LOG_LEVEL`.

## Безопасность

- Конфиг `any-router.yaml` содержит токены в открытом виде и не хранится в
  git (в `.gitignore`); сервер не логирует токены и ключи в ответы и в
  сообщения об ошибках.
- Авторизация `/v1` и `/admin` — только Bearer (админ или API-токен), fail closed.

## Документация

Инструкции для агентов — [AGENTS.md](AGENTS.md). Подробные разделы
(устройство, YAML-конфиг, деплой, админка) и PlantUML-схемы — в приватной
монорепе проекта.

## Лицензия

Проект распространяется под [лицензией MIT](LICENSE).
