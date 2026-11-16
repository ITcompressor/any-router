// help — CLI-справка any-router: `any-router help [serve|models]`;
// те же тексты печатает -h/-help/--help (без подкоманды) и -h внутри
// подкоманд (flag.ErrHelp в serve/models).
package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/version"
)

// help печатает справку: без аргументов — список команд и флаги serve,
// serve/models — флаги той подкоманды. Флагсет справки собирается с
// чистыми дефолтами (без env) — текст не зависит от окружения машины.
func help(w io.Writer, args ...string) error {
	head := func(desc string) {
		fmt.Fprintln(w, desc)
		fmt.Fprintln(w, "Флаги (важнее env; порт/токен по умолчанию — из any-router.yaml):")
	}
	footer := func() {
		fmt.Fprintln(w, "Приоритет: флаги > env (ANY_ROUTER_*) > значения any-router.yaml. Подробности: docs/any-router/README.md")
	}
	switch len(args) {
	case 0:
		fmt.Fprint(w, `any-router — маршрутизатор-прокси LLM-запросов: OpenAI-совместимый /v1
(модель provider/model → провайдеры из any-router.yaml) + веб-админка /admin.

Команды:
  serve    HTTP-сервер: API /v1 и веб-админка /admin (подкоманда по умолчанию)
  models   модели провайдеров, прямой опрос бэкендов: models [provider]
  version  версия + метаданные сборки (commit/время — ldflags при сборке)
  help     эта справка; по подкоманде: help serve | help models | help version
`)
		head("Флаги serve:")
		helpFlagSet(w).PrintDefaults()
		footer()
		return nil
	case 1:
		switch args[0] {
		case "serve":
			head("serve — HTTP-сервер: API /v1 (OpenAI-совместимый, модель provider/model) и веб-админка /admin; подкоманда по умолчанию. Оверрайд адреса: -addr целиком или -port — только порт поверх listen из файла.")
			helpFlagSet(w).PrintDefaults()
			footer()
			return nil
		case "models":
			head("models — модели провайдеров: опрос GET {baseurl}/models каждого провайдера (30 с на бэкенда), таблица «провайдер | модель | в запросе». Позиционный аргумент — фильтр провайдера (только он).")
			fs := helpFlagSet(w)
			addModelsFlags(fs)
			fs.PrintDefaults()
			return nil
		case "version":
			fmt.Fprintln(w, "version — версия + метаданные сборки (commit/время вкачиваются при сборке через -ldflags; так же отдаётся GET /api/version и топбар админки).")
			fmt.Fprintln(w, "  "+version.String())
			return nil
		default:
			return fmt.Errorf("неизвестная подкоманда %q (доступно: serve, models, version)", args[0])
		}
	default:
		return fmt.Errorf("help [serve|models] — не больше одного аргумента")
	}
}

// helpFlagSet — флагсет со всеми конфигурационными флагами для справки
// (дефолты config.Default, без env-подстановок); для models дополняется
// addModelsFlags.
func helpFlagSet(w io.Writer) *flag.FlagSet {
	c := config.Default()
	fs := flag.NewFlagSet("any-router", flag.ContinueOnError)
	fs.SetOutput(w)
	config.AddFlags(fs, &c)
	return fs
}