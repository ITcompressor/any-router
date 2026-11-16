package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ITcompressor/any-router/internal/config"
)

func TestUnknownCommand(t *testing.T) {
	err := Run(context.Background(), []string{"погнали"})
	if err == nil || !strings.Contains(err.Error(), "serve") || !strings.Contains(err.Error(), "help") {
		t.Errorf("ожидал ошибку с подсказкой про serve и help, получили: %v", err)
	}
}

// TestHelp — справка: общий список (команды + флаги) и по подкомандам.
func TestHelp(t *testing.T) {
	var out bytes.Buffer
	if err := help(&out); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"serve", "models", "help serve", "-port", "-addr"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("в общей справке нет %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := help(&out, "serve"); err != nil {
		t.Fatalf("help serve: %v", err)
	}
	for _, want := range []string{"-port", "-addr", "-token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("в справке serve нет %q", want)
		}
	}

	out.Reset()
	if err := help(&out, "models"); err != nil {
		t.Fatalf("help models: %v", err)
	}
	for _, want := range []string{"-provider", "-json"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("в справке models нет %q", want)
		}
	}

	if err := help(&out, "погнали"); err == nil {
		t.Errorf("ожидал ошибку на справку неизвестной подкоманды")
	}
	if err := help(&out, "serve", "models"); err == nil {
		t.Errorf("ожидал ошибку на два аргумента help")
	}
}

// TestRunDashH — -h/-help/--help без подкоманды — та же справка.
func TestRunDashH(t *testing.T) {
	if err := Run(context.Background(), []string{"--help"}); err != nil {
		t.Errorf("Run --help: %v (хочу nil)", err)
	}
}

// TestModelsErrHelp — models -h печатает справку и не считает это ошибкой.
func TestModelsErrHelp(t *testing.T) {
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-h"}); err != nil {
		t.Fatalf("models -h: %v", err)
	}
	if !strings.Contains(out.String(), "-provider") {
		t.Errorf("справка models -h без -provider:\n%s", out.String())
	}
}

// TestServeErrHelp — serve -h печатает справку и не считает это ошибкой.
func TestServeErrHelp(t *testing.T) {
	var out bytes.Buffer
	if err := serve(context.Background(), &out, []string{"-h"}); err != nil {
		t.Fatalf("serve -h: %v", err)
	}
	if !strings.Contains(out.String(), "-port") {
		t.Errorf("справка serve -h без -port:\n%s", out.String())
	}
}

// TestServePortOverride — -port меняет порт поверх -addr: сервер отвечает
// /healthz на зарезервированном порту, не на нулевом из -addr.
func TestServePortOverride(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("резерв порта: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // отпускаем — его займёт serve
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "any-router.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"-config", cfgPath, "-addr", "127.0.0.1:0", "-port", fmt.Sprint(port)})
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	resp, ok := waitHealthz(t, url)
	if !ok {
		cancel()
		t.Fatalf("сервер не отвечает на %s — -port не применился", url)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve не остановился по отмене контекста")
	}
}

// waitHealthz — дожидаемся 200 /healthz (сервер поднимается асинхронно).
func waitHealthz(t *testing.T, url string) (*http.Response, bool) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := client.Get(url)
		if err != nil {
			continue
		}
		if r.StatusCode == http.StatusOK {
			return r, true
		}
		r.Body.Close()
	}
	return nil, false
}

// TestListenOnAllInterfaces — пустой host (':8085') и явные 0.0.0.0/::/* —
// все интерфейсы; loopback-адреса и имя хоста — нет.
func TestListenOnAllInterfaces(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
	}{
		{":8085", true},
		{"0.0.0.0:8085", true},
		{"[::]:8085", true}, // IPv6 any: SplitHostPort → host "::"
		{"[*]:8085", true},
		{"*:8085", true},
		{"", false},        // невалидно, а не «все»
		{":::8085", false}, // too many colons — невалидно
		{"127.0.0.1:8085", false},
		{"localhost:8085", false},
		{"10.0.0.1:8085", false},
		{"[::1]:8085", false},
	}
	for _, c := range cases {
		if got := config.ListenOnAllInterfaces(c.listen); got != c.want {
			t.Errorf("ListenOnAllInterfaces(%q) = %v, хочу %v", c.listen, got, c.want)
		}
	}
}

// TestServeCreatesConfigFileAndExits — serve на отменённом контексте:
// стор создаёт YAML-файл и сервер тут же (gracefully) выключается.
func TestServeCreatesConfigFileAndExits(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "any-router.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, []string{"-config", cfgPath, "-addr", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Run serve: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("конфиг не создан: %v", err)
	}
	if !strings.Contains(string(raw), "listen:") || !strings.Contains(string(raw), "token:") {
		t.Errorf("в созданном конфиге нет listen/token:\n%s", raw)
	}
}
