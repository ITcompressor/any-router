package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	// Addr/Token пусты — их берут из any-router.yaml (оверрайдов нет).
	if c.Addr != "" {
		t.Errorf("Addr по умолчанию: %q, хочу пусто (listen из файла)", c.Addr)
	}
	// Port 0 = не менять (порт из listen/-addr).
	if c.Port != 0 {
		t.Errorf("Port по умолчанию: %d, хочу 0", c.Port)
	}
	if c.Token != "" {
		t.Errorf("Token по умолчанию: %q, хочу пусто (token из файла)", c.Token)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL по умолчанию: %q, хочу %q", c.BaseURL, DefaultBaseURL)
	}
	if c.File != DefaultFile {
		t.Errorf("File по умолчанию: %q, хочу %q", c.File, DefaultFile)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel по умолчанию: %q, хочу info", c.LogLevel)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	t.Setenv("ANY_ROUTER_TOKEN", "env-token")
	t.Setenv("ANY_ROUTER_LOG_LEVEL", "debug")
	c, err := Load([]string{"-addr", ":9000", "-token", "flag-token"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":9000" {
		t.Errorf("Addr: %q, хочу :9000", c.Addr)
	}
	// флаг важнее env
	if c.Token != "flag-token" {
		t.Errorf("Token: %q, хочу flag-token (флаг > env)", c.Token)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel: %q, хочу debug (env)", c.LogLevel)
	}
}

func TestLoadEnvOnly(t *testing.T) {
	t.Setenv("ANY_ROUTER_ADDR", ":7777")
	t.Setenv("ANY_ROUTER_TOKEN", "env-token")
	t.Setenv("ANY_ROUTER_CONFIG", "/tmp/other.yaml")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if c.Addr != ":7777" {
		t.Errorf("Addr из env: %q, хочу :7777", c.Addr)
	}
	if c.Token != "env-token" {
		t.Errorf("Token из env: %q, хочу env-token", c.Token)
	}
	if c.File != "/tmp/other.yaml" {
		t.Errorf("File из env: %q, хочу /tmp/other.yaml", c.File)
	}
}

// TestLoadBaseURL — base_url в «Примерах вызова» админки: флаг > env.
func TestLoadBaseURL(t *testing.T) {
	t.Setenv("ANY_ROUTER_BASE_URL", "https://env.example/")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseURL != "https://env.example/" {
		t.Errorf("BaseURL из env: %q, хочу https://env.example/", c.BaseURL)
	}
	// флаг важнее env
	c, err = Load([]string{"-base-url", "https://flag.example/"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseURL != "https://flag.example/" {
		t.Errorf("BaseURL из флага: %q, хочу https://flag.example/ (флаг > env)", c.BaseURL)
	}
}

// TestLoadBaseURLDefault — без env/флага base_url — дефолт прод-домена.
func TestLoadBaseURLDefault(t *testing.T) {
	os.Unsetenv("ANY_ROUTER_BASE_URL")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL по умолчанию: %q, хочу %q", c.BaseURL, DefaultBaseURL)
	}
}

func TestLoadConfigFlag(t *testing.T) {
	if _, err := Load([]string{"-config", "/tmp/x.yaml"}); err != nil {
		t.Fatalf("Load с -config: %v", err)
	}
}

func TestLoadRejectsUnknownArgs(t *testing.T) {
	if _, err := Load([]string{"serve", "extra"}); err == nil {
		t.Errorf("ожидал ошибку на позиционный аргумент, получили nil")
	}
}

func TestLoadRejectsBadLogLevel(t *testing.T) {
	if _, err := Load([]string{"-log-level", "loud"}); err == nil {
		t.Errorf("ожидал ошибку на неизвестный уровень логирования, получили nil")
	}
}

// TestLoadPort — оверрайд порта: флаг > env; без обоих — 0 (порт из файла).
func TestLoadPort(t *testing.T) {
	t.Setenv("ANY_ROUTER_PORT", "9002")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if c.Port != 9002 {
		t.Errorf("Port из env: %d, хочу 9002", c.Port)
	}
	// флаг важнее env
	c, err = Load([]string{"-port", "9003"})
	if err != nil {
		t.Fatalf("Load(-port): %v", err)
	}
	if c.Port != 9003 {
		t.Errorf("Port из флага: %d, хочу 9003 (флаг > env)", c.Port)
	}
}

// TestLoadPortBad — кривая строка env и порт вне диапазона — ошибки.
func TestLoadPortBad(t *testing.T) {
	t.Setenv("ANY_ROUTER_PORT", "abc")
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "ANY_ROUTER_PORT") {
		t.Errorf("ожидал ошибку на нечисловой ANY_ROUTER_PORT, получили: %v", err)
	}
	os.Unsetenv("ANY_ROUTER_PORT")
	for _, port := range []string{"-1", "70000"} {
		if _, err := Load([]string{"-port", port}); err == nil {
			t.Errorf("ожидал ошибку на порт %s вне диапазона 1–65535, получили nil", port)
		}
	}
}

// TestOverridePort — -port меняет только порт: host любого слушателя
// (выкачанный, IPv6 any, нулевой порт) сохраняется; пустой/без порта — ошибка.
func TestOverridePort(t *testing.T) {
	cases := []struct {
		listen string
		port   int
		want   string
	}{
		{":8085", 9000, ":9000"},
		{"127.0.0.1:8085", 9000, "127.0.0.1:9000"},
		{"localhost:8085", 8081, "localhost:8081"},
		{"[::]:8085", 9000, "[::]:9000"},
		{"10.0.0.1:0", 12345, "10.0.0.1:12345"},
	}
	for _, c := range cases {
		got, err := OverridePort(c.listen, c.port)
		if err != nil {
			t.Errorf("OverridePort(%q, %d): %v", c.listen, c.port, err)
			continue
		}
		if got != c.want {
			t.Errorf("OverridePort(%q, %d) = %q, хочу %q", c.listen, c.port, got, c.want)
		}
	}
	for _, c := range []struct {
		listen string
		port   int
	}{{"", 9000}, {"localhost", 9000}, {":8085", 0}, {":8085", 70000}} {
		if _, err := OverridePort(c.listen, c.port); err == nil {
			t.Errorf("OverridePort(%q, %d): ожидал ошибку, получили nil", c.listen, c.port)
		}
	}
}

// TestLoadEnvMissingEnvVars — env не задан: оверрайды пустые.
func TestLoadEnvMissingEnvVars(t *testing.T) {
	for _, k := range []string{"ANY_ROUTER_ADDR", "ANY_ROUTER_TOKEN", "ANY_ROUTER_CONFIG"} {
		os.Unsetenv(k)
	}
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if c.Addr != "" || c.Token != "" {
		t.Errorf("без env оверрайды пусты, получили Addr=%q Token=%q", c.Addr, c.Token)
	}
	if c.File != DefaultFile {
		t.Errorf("File: %q, хочу %q", c.File, DefaultFile)
	}
}
