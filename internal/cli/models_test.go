package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/store"
)

// modelsConfigDir — временный any-router.yaml с провайдерами.
func modelsConfigDir(t *testing.T, urlFor map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	st, err := store.Load(path)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	for name, url := range urlFor {
		if err := st.SetProvider(name, store.Provider{BaseURL: url}); err != nil {
			t.Fatalf("SetProvider(%s): %v", name, err)
		}
	}
	return path
}

// modelsUpstream — OpenAI-совместимый /models.
func modelsUpstream(t *testing.T, ids []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []struct {
			ID string `json:"id"`
		}
		for _, id := range ids {
			data = append(data, struct {
				ID string `json:"id"`
			}{id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func TestModelsTable(t *testing.T) {
	path := modelsConfigDir(t, map[string]string{
		"ollama": modelsUpstream(t, []string{"llama3", "qwen3:8b"}),
		// llama.cpp без alias: id модели — путь к gguf
		"pcbig": modelsUpstream(t, []string{"/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"}),
		"mock":  modelsUpstream(t, []string{"gpt-x"}),
	})
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", path}); err != nil {
		t.Fatalf("models: %v", err)
	}
	if strings.Contains(errB.String(), "недоступен") {
		t.Errorf("провайдеры живы, а в stderr ошибки: %q", errB.String())
	}
	got := out.String()
	// tabwriter выравнивает колонки (паддинги) — сравниваем по токенам
	for _, want := range []string{
		"ПРОВАЙДЕР", "МОДЕЛЬ", "В ЗАПРОСЕ",
		"llama3", "ollama/llama3",
		"qwen3:8b", "ollama/qwen3:8b",
		"gpt-x", "mock/gpt-x",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в таблице нет %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "ollama/llama3"), strings.Index(got, "ollama/qwen3:8b"); i == -1 || j == -1 || j < i {
		t.Errorf("порядок моделей нарушен\n%s", got)
	}
}

func TestModelsProviderDown(t *testing.T) {
	// закрытый порт — недоступен
	closed := fmt.Sprintf("http://127.0.0.1:%d/v1", freePort(t))
	live := modelsUpstream(t, []string{"llama3"})
	path := modelsConfigDir(t, map[string]string{"dead": closed, "ollama": live})
	var out, errB bytes.Buffer
	err := models(context.Background(), &out, &errB, []string{"-config", path})
	if err != nil {
		t.Fatalf("один живой провайдер: не ошибка: %v", err)
	}
	if !strings.Contains(errB.String(), "dead: недоступен") {
		t.Errorf("stderr без предупреждения о dead: %q", errB.String())
	}
	if !strings.Contains(out.String(), "ollama/llama3") {
		t.Errorf("живой провайдер потерян:\n%s", out.String())
	}
}

func TestModelsAllDown(t *testing.T) {
	closed := fmt.Sprintf("http://127.0.0.1:%d/v1", freePort(t))
	path := modelsConfigDir(t, map[string]string{"dead": closed})
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", path}); err == nil {
		t.Fatalf("все провайдеры мертвы — ошибка ожидается")
	}
}

func TestModelsJSON(t *testing.T) {
	path := modelsConfigDir(t, map[string]string{
		"ollama": modelsUpstream(t, []string{"llama3"}),
	})
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", path, "-json"}); err != nil {
		t.Fatalf("models -json: %v", err)
	}
	var rows []modelsRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("не JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0].Request != "ollama/llama3" {
		t.Errorf("rows: %+v", rows)
	}
}

func TestModelsProviderFilter(t *testing.T) {
	path := modelsConfigDir(t, map[string]string{
		"ollama": modelsUpstream(t, []string{"llama3"}),
		"mock":   modelsUpstream(t, []string{"gpt-x"}),
	})
	// флагом
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", path, "-provider", "mock"}); err != nil {
		t.Fatalf("models -provider: %v", err)
	}
	if strings.Contains(out.String(), "ollama") {
		t.Errorf("фильтр не сработал:\n%s", out.String())
	}
	// позиционным
	out.Reset()
	if err := models(context.Background(), &out, &errB, []string{"-config", path, "ollama"}); err != nil {
		t.Fatalf("models ollama: %v", err)
	}
	if !strings.Contains(out.String(), "ollama/llama3") || strings.Contains(out.String(), "mock/") {
		t.Errorf("позиционный фильтр:\n%s", out.String())
	}
	// неизвестный — ошибка
	if err := models(context.Background(), &out, &errB, []string{"-config", path, "zzz"}); err == nil {
		t.Fatalf("неизвестный провайдер — ошибка ожидается")
	}
}

func TestModelsNoProviders(t *testing.T) {
	dir := t.TempDir()
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", filepath.Join(dir, "any-router.yaml")}); err != nil {
		t.Fatalf("нет провайдеров: не ошибка: %v", err)
	}
	if !strings.Contains(errB.String(), "провайдеров нет") {
		t.Errorf("stderr: %q", errB.String())
	}
}

// freePort — свободный порт для «мертвого» апстрима.
func freePort(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	parts := strings.Split(srv.URL, ":")
	port := parts[len(parts)-1]
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatalf("порт из %q: %v", srv.URL, err)
	}
	return n
}

// path-модели (llama.cpp без --alias): авто-алиас в «В ЗАПРОСЕ»,
// полный путь — в колонке МОДЕЛЬ; без двойных слэшей.
func TestModelsPathAutoAlias(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	path := modelsConfigDir(t, map[string]string{
		"pcbig": modelsUpstream(t, []string{"/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"}),
	})
	var out, errB bytes.Buffer
	if err := models(context.Background(), &out, &errB, []string{"-config", path}); err != nil {
		t.Fatalf("models: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "pcbig//opt") {
		t.Errorf("двойной слэш:\n%s", got)
	}
	if !strings.Contains(got, "/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf") {
		t.Errorf("путь не в колонке МОДЕЛЬ:\n%s", got)
	}
	if !strings.Contains(got, "pcbig/Qwen3.8-27B-UD-IQ4_XS") {
		t.Errorf("авто-алиас не в В ЗАПРОСЕ:\n%s", got)
	}
}
