// Package cli — точка входа CLI any-router: подкоманды и конфигурация.
// serve (по умолчанию без аргументов) — HTTP-сервер, models — список
// моделей провайдеров, help — справка (и по подкомандам); сюда же лягут
// будущие команды управления.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/logging"
	"github.com/ITcompressor/any-router/internal/store"
	"github.com/ITcompressor/any-router/internal/version"
	"github.com/ITcompressor/any-router/internal/web"
)

// Run разбирает первую позиционную подкоманду и запускает её.
// Без подкоманды (или с `serve`) — запуск HTTP-сервера;
// -h/-help/--help — та же справка, что и подкоманда help.
func Run(ctx context.Context, args []string) error {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "serve":
		return serve(ctx, os.Stdout, args)
	case "models":
		return models(ctx, os.Stdout, os.Stderr, args)
	case "version":
		fmt.Fprintln(os.Stdout, version.String())
		return nil
	case "help":
		return help(os.Stdout, args...)
	case "-h", "-help", "--help":
		return help(os.Stdout)
	default:
		return fmt.Errorf("неизвестная подкоманда %q (доступно: serve, models, version; справка: any-router help)", cmd)
	}
}

// serve грузит конфиг (env + флаги), конфиг-стор any-router.yaml
// (создаёт при отсутствии) и запускает HTTP-сервер до сигнала.
func serve(ctx context.Context, stdout io.Writer, args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, "serve")
		}
		return err
	}
	st, err := store.Load(cfg.File)
	if err != nil {
		return err
	}
	// Оверрайд токена перезаписывает значение в файле (задача 2 PRD).
	if cfg.Token != "" {
		if err := st.SetAdminToken(cfg.Token); err != nil {
			return err
		}
	}
	// Адрес слушателя: оверрайд (-addr/env) или listen из файла;
	// -port/env-порт заменяет только порт (host не трогается).
	listen := st.Listen()
	if cfg.Addr != "" {
		listen = cfg.Addr
	}
	if cfg.Port != 0 {
		listen, err = config.OverridePort(listen, cfg.Port)
		if err != nil {
			return err
		}
	}
	log := logging.New(cfg.LogLevel)
	if config.ListenOnAllInterfaces(listen) {
		log.Warnf("слушаю ВСЕ интерфейсы (%s): /v1 и /admin доступны по сети — "+
			"ограничьте listen на 127.0.0.1:8085 в %s, если доступ снаружи не нужен", listen, cfg.File)
	}
	if cfg.Token != "" {
		log.Infof("админ-токен перезаписан из -token/env")
	}
	log.Infof("конфиг: %s (провайдеров: %d, API-токенов: %d)",
		cfg.File, len(st.ProviderNames()), len(st.Tokens()))
	srv := web.New(cfg, log, st, listen)
	// Готовые ссылки с токеном: из лога запуска сразу заходят в админку.
	// Печать — после успешного бинда (OnListen): при занятом порту ссылки
	// не печатаются вовсе — иначе они вели бы на чужой инстанс на том же
	// порту (чужой админ-токен → 401 при входе из лога), а сам процесс
	// умирал на бинде, печатая ссылки в никуда.
	srv.OnListen(func() {
		for _, u := range adminURLs(listen, st.AdminToken()) {
			log.Infof("админка: %s", u)
		}
		log.Infof("v1: %s (Bearer: админ или API-токен, модель provider/model)",
			strings.Join(v1URLs(listen), " · "))
	})
	return srv.Run(ctx)
}
