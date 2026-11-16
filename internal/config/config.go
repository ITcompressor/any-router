// Package config — конфигурация any-router: дефолты, переменные окружения
// и флаги командной строки. Приоритет: флаги > env > дефолты.
// Порт и админ-токен (задача 2 PRD) живут в any-router.yaml (конфиг-стор);
// Addr/Port/Token здесь — оверрайды на запуск (пусто/0 = значение из файла):
// -addr/-port переопределяют только на время процесса, а -token перезаписывает
// значение в файле (см. cli.Serve). BaseURL — адрес в «Примерах вызова»
// админки (файл не правит; пусто — DefaultBaseURL).
package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config — настройки запуска any-router.
type Config struct {
	Addr     string // оверрайд адреса HTTP-сервера (host:port); пусто — listen из файла
	Port     int    // оверрайд только порта (host — из listen/-addr); 0 — порт из файла/адреса
	Token    string // оверрайд админ-токена (env/флаг); пусто — token из файла
	BaseURL  string // base_url в «Примерах вызова» админки; пусто — DefaultBaseURL
	File     string // путь к YAML-конфигу (any-router.yaml), создается при отсутствии
	LogLevel string // уровень логирования: debug|info|warn|error
}

// DefaultFile — путь к конфигу по умолчанию (текущая директория).
const DefaultFile = "any-router.yaml"

// DefaultBaseURL — адрес, который подставляется в «Примеры вызова» админки,
// если -base-url/env не заданы (локальный сервер на дефолтном порту 8085;
// прод-API llm.e7.by пробрасывается явно при локальном запуске — промо
// any-router.e7.by раздаёт только статику, без API-пробросов).
const DefaultBaseURL = "http://localhost:8085"

// Default — настройки по умолчанию: INFO, any-router.yaml в cwd;
// Addr/Token пусты — берутся из файла, BaseURL — локальный адрес.
func Default() Config {
	return Config{Addr: "", Token: "", BaseURL: DefaultBaseURL, File: DefaultFile, LogLevel: "info"}
}

// FromEnv дополняет дефолты переменными окружения ANY_ROUTER_*.
func FromEnv() Config {
	c := Default()
	if v := os.Getenv("ANY_ROUTER_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("ANY_ROUTER_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("ANY_ROUTER_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("ANY_ROUTER_CONFIG"); v != "" {
		c.File = v
	}
	if v := os.Getenv("ANY_ROUTER_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	return c
}

// AddFlags описывает флаги конфигурации (значения по умолчанию — уже с env).
func AddFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.Addr, "addr", c.Addr, "оверрайд адреса HTTP-сервера (host:port); пусто — listen из "+DefaultFile)
	fs.IntVar(&c.Port, "port", c.Port, "оверрайд только порта HTTP-сервера (host — из listen из файла или -addr); 0 — порт из файла/адреса; применяется в serve")
	fs.StringVar(&c.Token, "token", c.Token, "перезаписать админ-токен в YAML-конфиге; пусто — token из файла")
	fs.StringVar(&c.BaseURL, "base-url", c.BaseURL, "base_url в «Примерах вызова» админки (по умолчанию "+DefaultBaseURL+")")
	fs.StringVar(&c.File, "config", c.File, fmt.Sprintf("путь к YAML-конфигу (по умолчанию %s в текущей директории)", DefaultFile))
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "уровень логирования: debug|info|warn|error")
}

// Load строит конфиг из env и флагов args (например "-addr :9000 -port 8086").
// Остаточные позиционные аргументы считаются ошибкой.
func Load(args []string) (Config, error) {
	c := FromEnv()
	// Порт из env: ANY_ROUTER_PORT (парс с проверкой — отсюда, а не из FromEnv,
	// чтобы вернуть ошибку на кривую строку).
	if v := os.Getenv("ANY_ROUTER_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("env ANY_ROUTER_PORT=%q: не число", v)
		}
		c.Port = port
	}
	fs := flag.NewFlagSet("any-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	AddFlags(fs, &c)
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("разбор флагов: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return Config{}, fmt.Errorf("неизвестные аргументы: %s", strings.Join(rest, " "))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("неизвестный уровень логирования %q (debug|info|warn|error)", c.LogLevel)
	}
	if c.Port < 0 || c.Port > 65535 {
		return Config{}, fmt.Errorf("порт %d вне диапазона 1–65535", c.Port)
	}
	return c, nil
}

// OverridePort заменяет порт в слушателе host:port (оверрайд -port/port-env);
// хост не трогается, поэтому -port работает поверх listen из файла и -addr.
// Слушатель без порта (или пустой) и порт вне 1–65535 — ошибка.
func OverridePort(listen string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("порт %d вне диапазона 1–65535", port)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("оверрайд порта поверх %q: нет host:port — укажите -addr или listen в конфиге", listen)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// ListenOnAllInterfaces — слушатель не ограничен loopback: пустой хост
// (":8085") биндится на все интерфейсы, как и явные 0.0.0.0/::/*.
// Дефолт из PRD (межхостовый прокси), но о таком запуске надо предупредить.
func ListenOnAllInterfaces(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// пустая строка или кривая форма — это не «все», это ошибка
		return false
	}
	return host == "" || host == "0.0.0.0" || host == "::" || host == "*"
}
