// Бинарник any-router: маршрутизатор-прокси LLM-запросов.
// Без аргументов (или с подкомандой serve) поднимает HTTP-сервер
// (listen из any-router.yaml, по умолчанию :8085); созданый конфиг-стор
// держит провайдеров и токены; SIGINT/SIGTERM аккуратно останавливают.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ITcompressor/any-router/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "any-router:", err)
		os.Exit(1)
	}
}
