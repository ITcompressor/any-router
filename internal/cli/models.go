// models — CLI-команда any-router: список моделей провайдеров из конфига.
// Ходит напрямую в бэкенды (GET {baseurl}/models, 30 с на каждого),
// печатает таблицу (провайдер | модель | готовая форма provider/model —
// можно копировать в запрос), -provider — фильтр, -json — машиночитаемый.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/store"
)

// modelsRow — строка вывода (и элемент -json).
type modelsRow struct {
	Provider string  `json:"provider"`
	ID       string  `json:"id,omitempty"`
	Request  string  `json:"request,omitempty"` // провайдер/модели — в поле Request модели
	Error    *string `json:"error,omitempty"`
}

// addModelsFlags — собственные флаги models (общие конфигурационные —
// config.AddFlags); та же сборка используется в справке (help models / models -h).
func addModelsFlags(fs *flag.FlagSet) (only *string, jsonOut *bool) {
	return fs.String("provider", "", "ходить только в этого провайдера"),
		fs.Bool("json", false, "вывод в JSON")
}

// models — подкоманда: таблица моделей провайдеров.
func models(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	c := config.FromEnv()
	fs := flag.NewFlagSet("any-router models", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config.AddFlags(fs, &c)
	only, jsonOut := addModelsFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, "models")
		}
		return fmt.Errorf("разбор флагов: %w", err)
	}
	// позиционный аргумент — тоже фильтр провайдера (models ollama)
	if fs.NArg() > 1 {
		return fmt.Errorf("неизвестные аргументы: %s", fs.Args())
	}
	if fs.NArg() == 1 {
		*only = fs.Arg(0)
	}

	st, err := store.Load(c.File)
	if err != nil {
		return err
	}
	names := st.ProviderNames()
	if len(names) == 0 {
		fmt.Fprintln(stderr, "провайдеров нет — добавьте в any-router.yaml или через веб-админку")
		return nil
	}
	// фильтр
	if *only != "" {
		known := false
		for _, n := range names {
			if n == *only {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("провайдер %q не найден (есть: %v)", *only, names)
		}
		names = []string{*only}
	}

	rows := make([]modelsRow, 0, 64)
	okProviders := 0
	for _, name := range names {
		prov, _ := st.Provider(name)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ids, err := proxy.BackendModels(cctx, proxy.Upstream{BaseURL: prov.BaseURL, APIKey: prov.APIKey})
		cancel()
		if err != nil {
			msg := err.Error()
			rows = append(rows, modelsRow{Provider: name, Error: &msg})
			fmt.Fprintln(stderr, name+": недоступен ("+msg+")")
			continue
		}
		okProviders++
		for _, id := range ids {
			idRow, request := id, proxy.ModelRef(name, id)
			if proxy.IsPathID(id) {
				// path-модель (llama.cpp без --alias): авто-алиас = имя файла
				idRow, request = id, proxy.ModelRef(name, proxy.PathAlias(id))
			}
			rows = append(rows, modelsRow{Provider: name, ID: idRow, Request: request})
		}
	}
	if okProviders == 0 {
		return fmt.Errorf("ни один провайдер не ответил (см. ошибки выше)")
	}

	if *jsonOut {
		return json.NewEncoder(stdout).Encode(rows)
	}
	// Таблица печатается, только если есть что показать:
	// ноль моделей при живых провайдерах — тоже результат.
	if onlyErrRows(rows) {
		fmt.Fprintln(stderr, "моделей нет: провайдеры живы, но /v1/models вернул пусто "+
			"(у llama.cpp пусто без загруженной модели; у ollama список всегда полный)")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ПРОВАЙДЕР\tМОДЕЛЬ\tВ ЗАПРОСЕ")
	for _, r := range rows {
		if r.Error != nil {
			fmt.Fprintf(tw, "%s\t(недоступен)\t%s\n", r.Provider, *r.Error)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Provider, r.ID, r.Request)
	}
	return tw.Flush()
}

// onlyErrRows — в выводе только «недоступен» (ни одной модели).
func onlyErrRows(rows []modelsRow) bool {
	for _, r := range rows {
		if r.Error == nil {
			return false
		}
	}
	return true
}
