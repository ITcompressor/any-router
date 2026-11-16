# any-router

Маршрутизатор-прокси LLM-запросов (public-репо; разрабатывается внутри монорепы
`project-phoenix` и подключается туда git-сабмодулем). Go-модуль
`github.com/ITcompressor/any-router` — только стандартная библиотека +
`gopkg.in/yaml.v3`.

Проектная документация ведётся в монорепе (`docs/any-router/`), схемы PlantUML
там же. Продуктовый обзор — лендинг https://any-router.e7.by/ .
Подробно про YAML-конфиг — раздел «Конфиг» ниже; см. также `README.md`.

## Структура

```
.
├── cmd/any-router/main.go    точка входа: SIGINT/SIGTERM → graceful stop → cli.Run
└── internal/
    ├── cli/                  команды: serve (по умолчанию), models, version, help
    ├── version/              версия + метаданные сборки (ldflags; CLI, /api/version, топбар админки)
    ├── config/               флаги > env > дефолты (-config/-addr/-port/-token/-base-url/-log-level)
    ├── logging/              leveled-логгер в stderr (RFC3339, DEBUG..ERROR)
    ├── store/                конфиг-стор any-router.yaml (RWMutex, tmp+rename, 0600):
    │                         провайдеры, токены, баланс-алиасы, langfuse
    ├── langfuse/             неблокирующий экспорт трейсов: очередь → батчи → OTLP/JSON
    ├── router/               аутентификация: Bearer (админ | API-токен), fail closed;
    │                         identity токена — в контексте запроса
    ├── proxy/                перепись модели (авто-алиасы path-моделей, кэш TTL),
    │                         баланс-алиасы (голое имя без «/» — липкость бесед:
    │                         X-Session-Id/первые два сообщения, TTL 30 мин,
    │                         поверх round-robin по участникам с фейловером), стриминг SSE,
    │                         агрегация /v1/models (+ ModelsCache:
    │                         первичная загрузка при старте + фоновое обновление раз в 10 минут)
    └── web/                  mux: /healthz, /api/version, /v1, /admin (вкладки:
                              провайдеры, токены, балансировка, langfuse, примеры)
                              + HTML-шаблон, i18n ru/en
```

## Команды

```bash
go build -o any-router ./cmd/any-router        # локальная сборка
go test ./...                                  # тесты
go install github.com/ITcompressor/any-router/cmd/any-router@latest   # установка из репы
GOOS=linux GOARCH=amd64 go build -o any-router-linux-amd64 ./cmd/any-router   # кросс-сборка
./any-router serve                             # HTTP-сервер: API /v1 и веб-админка /admin
./any-router models                            # список моделей провайдеров (ходит в бэкенды напрямую)
./any-router version                           # версия + commit/время сборки
```

Бинари `any-router`, `any-router-linux-amd64` и конфиг `any-router.yaml`
в git не попадают (см. `.gitignore`).

## Конфиг

- Файл — `any-router.yaml` в текущей директории (создаётся при первом запуске,
  права 0600). **Не коммитится в git** — в нём токены и ключи провайдеров
  в открытом виде.
- Структура: `listen`, `token` (админ), `lang` (ru|en), `providers` (map:
  `baseurl`, `apikey`), `tokens` (API-токены: `id`/`token`/`scope` —
  область `models`: список конкретных моделей `provider/model`, хостов
  целиком `provider/*` и баланс-алиасов (голые имена); legacy-области
  `all`/`provider`/`model` признаются сервером, ✎ в админке показывает
  эквивалент и переписывает в `models`), `balances` (баланс-алиасы —
  групповые имена без префикса провайдера, `[{name, models: [provider/model,
  …]}]`: запрос на голое имя без «/» — round-robin по участникам с
  фейловером), `langfuse` (`enabled` — переключатель выгрузки без удаления
  настроек, `url`, ключи, `environment`, `tags`).
- Оверрайды: `-config`, `-addr`, `-port`, `-token`, `-base-url`, `-log-level`; env —
  `ANY_ROUTER_ADDR|PORT|TOKEN|BASE_URL|CONFIG|LOG_LEVEL`. Приоритет: флаги > env > дефолты.
- Правки из админки сохраняются в YAML сразу (атомарно: tmp + rename).

## Правила

- Конфиг `any-router.yaml` с секретами — не в репозитории; не коммитить,
  не логировать токены и ключи в ответы и в сообщения об ошибках.
- Ответы `/v1` — OpenAI-формат (`error.message` для 400/403), SSE отдаётся клиенту
  сразу (flush).
- Версия в админке и `GET /api/version` — из `internal/version` (-ldflags при сборке),
  не хардкодить.
- Лендинг https://any-router.e7.by/ собирает бинари этого модуля — путь
  к пакету `./cmd/any-router` менять только согласованно (в монорепе он
  зашит в run.sh и `media/any-router.e7.by/build.sh`).
