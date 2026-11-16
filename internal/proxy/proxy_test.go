package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ITcompressor/any-router/internal/logging"
)

// resolverMock — proxy.ProviderSource поверх map (для апстримов тестов).
type resolverMock map[string]Upstream

func (m resolverMock) Upstream(provider string) (Upstream, bool) {
	up, ok := m[provider]
	return up, ok
}

func (m resolverMock) ProviderNames() []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	return names
}

// resolverOne — один провайдер (упрощённый мок для авто-алиасов).
type resolverOne struct {
	name string
	up   Upstream
}

func (m resolverOne) Upstream(provider string) (Upstream, bool) {
	if provider == m.name {
		return m.up, true
	}
	return Upstream{}, false
}

func (m resolverOne) ProviderNames() []string { return []string{m.name} }

// upstream — фейковый OpenAI-совместимый бэкенд для тестов.
func upstream(t *testing.T, handler http.HandlerFunc) (Upstream, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return Upstream{BaseURL: srv.URL + "/v1", APIKey: "up-key"}, srv
}

// upstreamEcho — апстрим, печатающий, что получил, в ответе.
func upstreamEcho(t *testing.T) (Upstream, *httptest.Server) {
	t.Helper()
	return upstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s auth=%s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	})
}

// upstreamChat — апстрим chat/completions: разбирает model и отдаёт SSE-стрим.
func upstreamChat(t *testing.T, seenModel *string, seenAuth *string) (Upstream, *httptest.Server) {
	t.Helper()
	return upstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seenModel = body.Model
		*seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "chunk%d\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
}

func TestChatRewritesModelAndPath(t *testing.T) {
	seenModel, seenAuth := "", ""
	up, srv := upstreamChat(t, &seenModel, &seenAuth)
	defer srv.Close()

	body := `{"model":"ollama/qwen3:8b","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("код ответа: %d, хочу 200", got)
	}
	if seenModel != "qwen3:8b" {
		t.Errorf("апстрим получил model=%q, хочу qwen3:8b (без префикса)", seenModel)
	}
	if seenAuth != "Bearer up-key" {
		t.Errorf("апстрим увидел Authorization=%q, хочу Bearer up-key", seenAuth)
	}
	if got := rec.Body.String(); strings.Count(got, "chunk") != 3 {
		t.Errorf("стрим потерял чанки: %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type не скопирован: %q", ct)
	}
}

func TestChatRejectsBadBodies(t *testing.T) {
	up, srv := upstreamEcho(t)
	defer srv.Close()
	cases := []struct {
		name, body, want string
	}{
		{"не JSON", "не json и не модель", "тело не JSON"},
		{"нет model", `{"messages":[]}`, "нет поля model"},
		{"без слэша", `{"model":"qwen3"}`, "не вида provider/model"},
		{"пустой префикс", `{"model":"/qwen3"}`, "не вида provider/model"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(c.body))
		rec := httptest.NewRecorder()
		err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil)
		if err == nil {
			t.Errorf("%s: ошибка ожидается", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ошибка %q, хочу упоминание %q", c.name, err.Error(), c.want)
		}
	}
}

func TestChatStripsV1FromPathKeepsQuery(t *testing.T) {
	up, srv := upstreamEcho(t)
	defer srv.Close()
	// baseurl включает /v1; у запроса путь после strip: апстрим видит
	// сложение baseurl+/v1 и rest=/completions, плюс query.
	req := httptest.NewRequest(http.MethodPost, "/v1/completions?stream=true", strings.NewReader(`{"model":"ollama/m1"}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := rec.Body.String(); !strings.Contains(got, "POST /v1/completions") {
		t.Errorf("путь до апстрима: %q, хочу /v1/completions (baseurl + rest без /v1)", got)
	}
}

func TestModelsAggregatesAndPrefixes(t *testing.T) {
	a, aSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") != "Bearer key-a" {
				t.Errorf("apikey a не дошёл: %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"object":"list","data":[{"id":"llama3"},{"id":"qwen2"}]}`)
			return
		}
		// capabilities-допрос: у этого бэкенда /api/show нет — 404
		if r.URL.Path == "/api/show" {
			http.NotFound(w, r)
			return
		}
		t.Errorf("апстрим а получил %s %q", r.Method, r.URL.Path)
	})
	defer aSrv.Close()
	b, bSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "взрыв", http.StatusInternalServerError)
	})
	defer bSrv.Close()
	c, cSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-x"}]}`)
	})
	defer cSrv.Close()
	upA := Upstream{BaseURL: a.BaseURL, APIKey: "key-a"}
	upB := Upstream{BaseURL: b.BaseURL}
	upC := Upstream{BaseURL: c.BaseURL}

	log := logging.NewWriter(&strings.Builder{}, "error")
	res := resolverMock{"c": upC, "a": upA, "b": upB}
	entries := CollectModels(context.Background(), res, log, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	Models(rec, req, res, entries, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	var ids []string
	for _, e := range body.Data {
		ids = append(ids, e.ID)
	}
	want := "[a/llama3 a/qwen2 c/gpt-x]"
	if fmt.Sprint(ids) != want {
		t.Errorf("модели: %v, хочу %s", ids, want)
	}
}

func TestModelsSingleProviderPrefixed(t *testing.T) {
	a, aSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"llama3"}]}`)
	})
	defer aSrv.Close()
	res := resolverMock{"a": {BaseURL: a.BaseURL}}
	log := logging.NewWriter(&strings.Builder{}, "error")
	entries := CollectModels(context.Background(), res, log, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models?provider=a", nil)
	Models(rec, req, res, entries, nil)
	// ref префиксован и для single-запроса — как есть копируется в model
	if !strings.Contains(rec.Body.String(), `"id":"a/llama3"`) {
		t.Errorf("single-список: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder() // неизвестный провайдер — 400
	req = httptest.NewRequest(http.MethodGet, "/v1/models?provider=zz", nil)
	Models(rec, req, res, entries, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("неизвестный провайдер: код %d, хочу 400", rec.Code)
	}
}

func TestModelsAllProvidersDown502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "взрыв", http.StatusInternalServerError)
	}))
	defer srv.Close()
	res := resolverMock{"a": {BaseURL: srv.URL + "/v1"}}
	// все провайдеры недоступны → снимок кэша пуст → 502
	entries := CollectModels(context.Background(), res, logging.NewWriter(&strings.Builder{}, "error"), "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	Models(rec, req, res, entries, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("все провайдеры недоступны: код %d, хочу 502", rec.Code)
	}
}

func TestSplitModel(t *testing.T) {
	cases := []struct {
		in   string
		p, m string
		ok   bool
	}{
		{"ollama/qwen3:8b", "ollama", "qwen3:8b", true},
		{"openai/gpt-x", "openai", "gpt-x", true},
		{"noid/slash", "noid", "slash", true},
		{"/lead", "", "lead", false},
		{"openai/", "openai", "", false},
	}
	for _, c := range cases {
		p, m, ok := SplitModel(c.in)
		if ok != c.ok || p != c.p || m != c.m {
			t.Errorf("SplitModel(%q)=%q,%q,%v, хочу %q,%q,%v", c.in, p, m, ok, c.p, c.m, c.ok)
		}
	}
}

// Авто-алиасы: Chat с коротким именем path-модели ходит за /v1/models
// бэкенда (кэш) и уходит в апстрим с полным путём.
func TestChatAutoAlias(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	seenModel := ""
	path := "/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, path)
		default:
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel = body.Model
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	res := resolverOne{name: "pcbig", up: Upstream{BaseURL: srv.URL + "/v1"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pcbig/Qwen3.8-27B-UD-IQ4_XS","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if seenModel != path {
		t.Errorf("авто-алиас не разрешён в путь: %q (хочу %s)", seenModel, path)
	}
	// второй запрос — из кэша (без повторного fetch, но нам достаточно результата)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pcbig/Qwen3.8-27B-UD-IQ4_XS","messages":[]}`))
	if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat из кэша: %v", err)
	}
	if seenModel != path {
		t.Errorf("кэш сломался: %q", seenModel)
	}
}

// Путь модели прямо в запросе (старая форма) — уходит как есть.
func TestChatPathModelAsIs(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	seenModel := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Error(w, "не должен вызываться", http.StatusNotFound)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenModel = body.Model
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	res := resolverOne{name: "pcbig", up: Upstream{BaseURL: srv.URL + "/v1"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pcbig//opt/llm/models/Qwen3.gguf","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if seenModel != "/opt/llm/models/Qwen3.gguf" {
		t.Errorf("path в запросе исказился: %q", seenModel)
	}
}

// TestModelsPathAliasEntries — path-модели: ref = авто-алиас (имя файла),
// raw = полный путь; именные модели — обычные префиксованные.
func TestModelsPathAliasEntries(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	a, aSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"},{"id":"llama3"}]}`)
	})
	defer aSrv.Close()
	res := resolverMock{"pcbig": {BaseURL: a.BaseURL}}
	entries := CollectModels(context.Background(), res, logging.NewWriter(&strings.Builder{}, "error"), "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	Models(rec, req, res, entries, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	got := rec.Body.String()
	for _, want := range []string{
		`"id":"pcbig/Qwen3.8-27B-UD-IQ4_XS"`,
		`"raw":"/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"`,
		`"id":"pcbig/llama3"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q:\n%s", want, got)
		}
	}
}

// TestModelsCapabilitiesLlamaCpp — llama.cpp прикладывает к /v1/models
// массив models [{name, model, capabilities}] и meta (n_ctx/n_ctx_train) —
// роутер прокидывает их как есть и не дёргает /api/show («multimodal»
// доканчивается «tools» — llama.cpp с mmproj не репортит tool-calls,
// см. canonCaps).
func TestModelsCapabilitiesLlamaCpp(t *testing.T) {
	ResetShowMiss()
	t.Cleanup(ResetShowMiss)
	a, aSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			t.Errorf("llama.cpp: /api/show не должен дёргаться, когда capabilities/meta есть в списке")
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"/opt/llm/models/vl-32b.gguf",
			"meta":{"n_ctx":16384,"n_ctx_train":131072}}],
			"models":[{"name":"/opt/llm/models/vl-32b.gguf","model":"/opt/llm/models/vl-32b.gguf",
			"capabilities":["completion","multimodal"]}]}`)
	})
	defer aSrv.Close()
	// ResetAliasCache: path-модель заводит авто-алиас (общий кэш тестов)
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	res := resolverMock{"pcbig": {BaseURL: a.BaseURL}}
	entries := CollectModels(context.Background(), res, logging.NewWriter(&strings.Builder{}, "error"), "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	Models(rec, req, res, entries, nil)
	var body struct {
		Data []struct {
			ID           string         `json:"id"`
			Raw          string         `json:"raw"`
			Capabilities []string       `json:"capabilities"`
			Meta         map[string]any `json:"meta"`
		} `json:"data"`
		Models []struct {
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v: %s", err, rec.Body.String())
	}
	wantCaps := []string{"completion", "multimodal", "tools"}
	if len(body.Data) != 1 || body.Data[0].ID != "pcbig/vl-32b" {
		t.Fatalf("data: %+v", body.Data)
	}
	if fmt.Sprint(body.Data[0].Capabilities) != fmt.Sprint(wantCaps) {
		t.Errorf("data capabilities: %v, хочу %v", body.Data[0].Capabilities, wantCaps)
	}
	// meta llama.cpp — passthrough как есть
	if body.Data[0].Meta["n_ctx"] != float64(16384) || body.Data[0].Meta["n_ctx_train"] != float64(131072) {
		t.Errorf("data meta: %v, хочу n_ctx=16384 n_ctx_train=131072", body.Data[0].Meta)
	}
	if len(body.Models) != 1 || body.Models[0].Model != "pcbig/vl-32b" {
		t.Fatalf("models: %+v", body.Models)
	}
	// name — identity на бэкенде (путь gguf)
	if body.Models[0].Name != "/opt/llm/models/vl-32b.gguf" {
		t.Errorf("models[0].name: %q, хочу путь gguf", body.Models[0].Name)
	}
	if fmt.Sprint(body.Models[0].Capabilities) != fmt.Sprint(wantCaps) {
		t.Errorf("models capabilities: %v, хочу %v", body.Models[0].Capabilities, wantCaps)
	}
}

// TestModelsCapabilitiesOllamaShow — ollama-совместимый бэкенд capabilities
// и контекст в /v1/models не даёт; роутер доспрашивает /api/show: capabilities
// + нативный потолок контекста (model_info) → синтетический meta.n_ctx_train.
func TestModelsCapabilitiesOllamaShow(t *testing.T) {
	ResetShowMiss()
	t.Cleanup(ResetShowMiss)
	seenShow := ""
	a, aSrv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-oss:20b"}]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/show" {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenShow = body.Model
			fmt.Fprint(w, `{"capabilities":["completion","tools"],
				"model_info":{"general.architecture":"gpt-oss","gpt-oss.context_length":131072}}`)
			return
		}
		t.Errorf("лишний запрос: %s %s", r.Method, r.URL.Path)
	})
	defer aSrv.Close()
	res := resolverMock{"ollama": {BaseURL: a.BaseURL}}
	entries := CollectModels(context.Background(), res, logging.NewWriter(&strings.Builder{}, "error"), "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	Models(rec, req, res, entries, nil)
	var body struct {
		Data []struct {
			ID           string         `json:"id"`
			Capabilities []string       `json:"capabilities"`
			Meta         map[string]any `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v: %s", err, rec.Body.String())
	}
	wantCaps := []string{"completion", "tools"}
	if len(body.Data) != 1 || body.Data[0].ID != "ollama/gpt-oss:20b" {
		t.Fatalf("data: %+v", body.Data)
	}
	if fmt.Sprint(body.Data[0].Capabilities) != fmt.Sprint(wantCaps) {
		t.Errorf("capabilities: %v, хочу %v", body.Data[0].Capabilities, wantCaps)
	}
	// контекст из model_info — синтетический meta.n_ctx_train
	if body.Data[0].Meta["n_ctx_train"] != float64(131072) {
		t.Errorf("meta: %v, хочу n_ctx_train=131072", body.Data[0].Meta)
	}
	if seenShow != "gpt-oss:20b" {
		t.Errorf("в /api/show ушёл model=%q, хочу голый id бэкенда", seenShow)
	}
}

func TestShowCapabilitiesNegativeCache(t *testing.T) {
	ResetShowMiss()
	t.Cleanup(ResetShowMiss)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
			return
		}
		if r.URL.Path == "/api/show" {
			hits++
			http.NotFound(w, r)
			return
		}
	}))
	defer srv.Close()
	up := Upstream{BaseURL: srv.URL + "/v1"}
	ctx := context.Background()
	if _, err := ShowModelInfo(ctx, up, "m1"); !errors.Is(err, errNoShow) {
		t.Fatalf("ShowModelInfo: %v, хочу errNoShow", err)
	}
	if hits != 1 {
		t.Fatalf("допросов /api/show: %d, хочу 1", hits)
	}
	// повтор — из негативного кэша
	if _, err := ShowModelInfo(ctx, up, "m1"); !errors.Is(err, errNoShow) {
		t.Fatalf("ShowModelInfo из кэша: %v, хочу errNoShow", err)
	}
	if hits != 1 {
		t.Errorf("кэш не работает: допросов %d, хочу 1", hits)
	}

	// И в CollectModels негативный кэш съедает допросы второй модели
	collect := CollectModels(ctx, resolverOne{name: "up", up: up}, logging.NewWriter(&strings.Builder{}, "error"), "up")
	if len(collect) != 2 {
		t.Fatalf("CollectModels: %d моделей, хочу 2", len(collect))
	}
	for _, e := range collect {
		if len(e.Capabilities) != 0 {
			t.Errorf("у %q capabilities %v — у бэкенда их нет", e.ID, e.Capabilities)
		}
	}
	if hits != 1 {
		t.Errorf("допросов /api/show: %d, хочу 1 (кэш на весь провайдер)", hits)
	}
}

func TestModelRef(t *testing.T) {
	if got := ModelRef("ollama", "llama3"); got != "ollama/llama3" {
		t.Errorf("обычная модель: %q", got)
	}
	if got := ModelRef("pcbig", "/opt/m.gguf"); got != "pcbig/opt/m.gguf" {
		t.Errorf("path-модель — без двойного слэша: %q", got)
	}
}

func TestPathAlias(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf", "Qwen3.8-27B-UD-IQ4_XS"},
		{"/a/b/model.gguf", "model"},
		{"/a/b/нет-расширения", "нет-расширения"},
		{"/", ""},
		{"/.", ""},
	}
	for _, c := range cases {
		if got := PathAlias(c.in); got != c.want {
			t.Errorf("PathAlias(%q)=%q, хочу %q", c.in, got, c.want)
		}
	}
	if !IsPathID("/opt/x.gguf") || IsPathID("llama3") {
		t.Errorf("IsPathID сломан")
	}
}

// Синк кэша авто-алиасов: модель удалили на бэкенде, свежий список
// (CollectModels — то же, что кнопка «обновить модели» в админке)
// перезаписывает кэш: удалённый алиас больше не резолвится.
func TestAliasCacheSyncOnFreshList(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	path := "/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"
	list := path // мутируемое «состояние» бэкенда
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if list == "" {
				fmt.Fprint(w, `{"object":"list","data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, list)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"echo_model":%q}`, body.Model)
	}))
	defer srv.Close()
	res := resolverOne{name: "pcbig", up: Upstream{BaseURL: srv.URL + "/v1"}}
	ctx := context.Background()
	log := logging.NewWriter(&strings.Builder{}, "error")

	// populate через CollectModels: алиас появился в кэше
	CollectModels(ctx, res, log, "pcbig")
	if _, ok := aliasLookup("pcbig", "Qwen3.8-27B-UD-IQ4_XS"); !ok {
		t.Fatalf("алиас не в кэше после свежего списка")
	}

	// «модель удалили»: пустой список → кэш перезаписан пусто
	list = ""
	CollectModels(ctx, res, log, "pcbig")
	if _, ok := aliasLookup("pcbig", "Qwen3.8-27B-UD-IQ4_XS"); ok {
		t.Errorf("удалённый алиас всё ещё в кэше после свежего списка")
	}
	// resolveModel: miss → fetch → пусто → имя уходит апстриму как есть
	up, _ := res.Upstream("pcbig")
	if got := resolveModel(ctx, res, "pcbig", up, "Qwen3.8-27B-UD-IQ4_XS"); got != "Qwen3.8-27B-UD-IQ4_XS" {
		t.Errorf("resolveModel: %q, хочу имя как есть (модели больше нет)", got)
	}
}

// TestCanonCaps — нормализация capabilities (точка сборки ModelEntry):
// «multimodal» влечёт «tools» (llama.cpp с mmproj не репортит tools —
// потребители фильтра «completion+tools» зря отбраковывали модель),
// дубликаты/пустые срезаются, пустой список — как есть.
func TestCanonCaps(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"completion", "multimodal"}, []string{"completion", "multimodal", "tools"}},
		{[]string{"multimodal", "tools"}, []string{"multimodal", "tools"}},                                           // уже есть — не дублируем
		{[]string{"completion"}, []string{"completion"}},                                                             // без multimodal — не трогаем
		{[]string{"completion", "multimodal", "tools", "multimodal"}, []string{"completion", "multimodal", "tools"}}, // дедуп
		{[]string{"completion", "", "vision"}, []string{"completion", "vision"}},                                     // пустая capability — мимо
		{nil, nil},
	}
	for _, c := range cases {
		if got := canonCaps(c.in); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("canonCaps(%v) = %v, хочу %v", c.in, got, c.want)
		}
	}
}

// balanceResolver — resolverMock + баланс-алиасы (proxy.BalanceSource для tests).
type balanceResolver struct {
	resolverMock
	balances map[string][]string
}

func (b balanceResolver) Balance(name string) ([]string, bool) {
	ms, ok := b.balances[name]
	return ms, ok
}

// TestChatBalanceRoundRobin — запросы на баланс-алиас ходят по участникам
// по кругу (round-robin со счётчиком на алиас); апстрим видит модель
// участника без префикса провайдера; в Call — голое имя алиаса, alias и
// обслужившая модель участника.
func TestChatBalanceRoundRobin(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR() })

	var mu sync.Mutex
	var seen []string // "label/model" — что увидел апстрим
	mk := func(label string) Upstream {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/chat/completions" {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, label+"/"+body.Model)
				mu.Unlock()
				fmt.Fprint(w, `{"ok":true}`)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		return Upstream{BaseURL: srv.URL + "/v1"}
	}
	res := balanceResolver{
		resolverMock{"a": mk("a"), "b": mk("b"), "c": mk("c")},
		map[string][]string{"pool": {"a/m1", "b/m2", "c/m3"}},
	}
	sink := &sinkStub{}
	rec := httptest.NewRecorder()
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"pool","messages":[]}`))
		if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", sink); err != nil {
			t.Fatalf("запрос %d: %v", i+1, err)
		}
	}
	// round-robin: цикл a/m1 → b/m2 → c/m3 с произвольного сдвига (счётчик
	// общий на процесс — сверяем относительный порядок)
	roster := [3]string{"a/m1", "b/m2", "c/m3"}
	start := 0
	for i, s := range roster {
		if seen[0] == s {
			start = i
			break
		}
	}
	for i, got := range seen {
		if want := roster[(start+i)%3]; got != want {
			t.Fatalf("round-robin: цикл должен быть %v от %d: got %q want %q (seen %v)", roster, start, got, want, seen)
		}
	}
	// Call: голое имя алиаса, alias, обслужившая попытка (провайдер+модель участника)
	if len(sink.calls) != 6 {
		t.Fatalf("записей Call: %d, хочу 6", len(sink.calls))
	}
	c := sink.calls[0]
	if c.Model != "pool" || c.Alias != "pool" {
		t.Errorf("Call.Model/Alias: %q/%q, хочу pool/pool", c.Model, c.Alias)
	}
	provider, model, _ := strings.Cut(seen[0], "/")
	if c.Provider != provider {
		t.Errorf("Call.Provider: %q, хочу обслужившего %q", c.Provider, provider)
	}
	if c.UpstreamModel != model {
		t.Errorf("Call.UpstreamModel: %q, хочу %q (модель участника без префикса)", c.UpstreamModel, model)
	}
}

// TestChatBalanceFailover — недоступный участник пропускается: запрос уходит
// следующему, клиент получает ответ; счётчик round-robin двигается один раз
// за запрос (фейловер не балансируется).
func TestChatBalanceFailover(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR() })

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("мёртвый апстрим принял запрос")
	}))
	deadURL := dead.URL
	dead.Close() // соединение будет отклоняться

	var hits int
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			hits++
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer alive.Close()

	res := balanceResolver{
		resolverMock{"dead": {BaseURL: deadURL + "/v1"}, "b": {BaseURL: alive.URL + "/v1"}},
		map[string][]string{"pool": {"dead/lagged", "b/poolmodel"}},
	}
	rec := httptest.NewRecorder()
	for i := 0; i < 3; i++ {
		if err := Chat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"pool","messages":[]}`)),
			res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
			t.Fatalf("запрос %d: %v (фейловер не сработал)", i+1, err)
		}
	}
	if hits != 3 {
		t.Errorf("живой апстрим получил %d запросов, хочу 3 (каждый запрос добирался до него)", hits)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("клиент не увидел ответ живого участника: %s", rec.Body.String())
	}
}

// TestChatBalanceUnknownName — голое имя не из конфига — прежний 400; алиас,
// все участники которого ссылаются на удалённых провайдеров — тоже 400.
func TestChatBalanceUnknownName(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR() })

	up, srv := upstreamAnswer(t, "application/json", `{"ok":true}`)
	defer srv.Close()

	// не вид provider/model и не алиас
	rec := httptest.NewRecorder()
	err := Chat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"nosuch","messages":[]}`)),
		balanceResolver{resolverMock{"a": up}, nil},
		logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil)
	var perr Err
	if !errors.As(err, &perr) || perr.Code != http.StatusBadRequest || !strings.Contains(perr.Msg, "баланс-алиас") {
		t.Errorf("голое имя без алиаса: err=%v, хочу 400 с подсказкой про баланс-алиас", err)
	}
	// алиас весь из мёртвых провайдеров
	err = Chat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pool","messages":[]}`)),
		balanceResolver{nil, map[string][]string{"pool": {"ghost/m1", "ghost2/m2"}}},
		logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil)
	if !errors.As(err, &perr) || perr.Code != http.StatusBadRequest || !strings.Contains(perr.Msg, "провайдер") {
		t.Errorf("алиас без живых провайдеров: err=%v, хочу 400 «провайдер не найден»", err)
	}
}

// TestChatBalanceStickyChain — липкость разговоров: ходы одной беседы
// (в теле каждого следующего — история предыдущих, первые два сообщения
// общие) ходят в одного и того же участника алиаса (кэш промпта апстрима
// переиспользуется); разные беседы распределяются; без отпечатка
// (пустые messages) — честный round-robin.
func TestChatBalanceStickyChain(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	ResetStickyCache()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR(); ResetStickyCache() })

	var mu sync.Mutex
	var seen []string // "label|model" — что увидел апстрим (только chat-путь)
	mk := func(label string) Upstream {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/chat/completions":
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, label+"|"+body.Model)
				mu.Unlock()
				fmt.Fprint(w, `{"ok":true}`)
			case "/v1/models":
				fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"},{"id":"m3"},{"id":"m4"},{"id":"m5"},{"id":"m6"}]}`)
			default:
				http.Error(w, "неожиданный маршрут", http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return Upstream{BaseURL: srv.URL + "/v1"}
	}
	res := balanceResolver{
		resolverMock{"a": mk("a"), "b": mk("b"), "c": mk("c")},
		map[string][]string{"pool": {"a/m1", "b/m2", "c/m3"}},
	}
	// три хода одной беседы: история растёт, первые два сообщения постоянны
	turns := []string{
		`{"model":"pool","messages":[{"role":"user","content":"вопрос 1"}]}`,
		`{"model":"pool","messages":[{"role":"user","content":"вопрос 1"},{"role":"assistant","content":"ответ 1"},{"role":"user","content":"вопрос 2"}]}`,
		`{"model":"pool","messages":[{"role":"user","content":"вопрос 1"},{"role":"assistant","content":"ответ 1"},{"role":"user","content":"вопрос 2"},{"role":"assistant","content":"ответ 2"},{"role":"user","content":"вопрос 3"}]}`,
	}
	mu.Lock()
	before := len(seen)
	mu.Unlock()
	for _, body := range turns {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
			t.Fatalf("ход беседы: %v", err)
		}
	}
	mu.Lock()
	chain := append([]string(nil), seen[before:]...)
	mu.Unlock()
	if len(chain) != 3 || labelOf(chain[0]) != labelOf(chain[1]) || labelOf(chain[1]) != labelOf(chain[2]) {
		t.Fatalf("цепочка беседы разбежалась по участникам: %v — хочу одного обслужившего", chain)
	}
	t.Logf("липкая цепочка: %v", chain)
	// разные беседы (разные открытия, по одному ходу) — round-robin распределяет:
	// за шесть свежих бесед встречаются как минимум два разных участника
	servingSet := map[string]bool{}
	for i := 0; i < 6; i++ {
		body := fmt.Sprintf(`{"model":"pool","messages":[{"role":"user","content":"разговор %d"}]}`, i)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
			t.Fatalf("свежая беседа %d: %v", i+1, err)
		}
	}
	mu.Lock()
	fresh := append([]string(nil), seen[len(seen)-6:]...)
	mu.Unlock()
	for _, s := range fresh {
		servingSet[labelOf(s)] = true
	}
	if len(servingSet) < 2 {
		t.Errorf("одновходовые беседы не распределяются: %v", fresh)
	}
	// ходы без истории (пустые messages) — round-robin без привязки
	anonServing := map[string]bool{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"pool","messages":[]}`))
		rec := httptest.NewRecorder()
		if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
			t.Fatalf("ход без истории %d: %v", i+1, err)
		}
	}
	mu.Lock()
	last3 := append([]string(nil), seen[len(seen)-3:]...)
	mu.Unlock()
	for _, s := range last3 {
		anonServing[labelOf(s)] = true
	}
	if len(anonServing) != 3 {
		t.Errorf("без отпечатка (messages: []): %d разных участников за 3 запроса, хочу 3 (round-robin)", len(anonServing))
	}
}

// labelOf — метка участника из записи "label|model".
func labelOf(s string) string {
	i := strings.IndexByte(s, '|')
	if i < 0 {
		return s
	}
	return s[:i]
}

// TestChatBalanceStickySessionHeader — X-Session-Id сильнее отпечатка тела:
// те же сообщения под разными сессиями — разные участники, разные тела под
// одной сессией (например, systemone — ход цепочки без messages) — один.
func TestChatBalanceStickySessionHeader(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	ResetStickyCache()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR(); ResetStickyCache() })

	var mu sync.Mutex
	var seen []string
	mk := func(label string) Upstream {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/chat/completions":
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, label+"|"+body.Model)
				mu.Unlock()
				fmt.Fprint(w, `{"ok":true}`)
			case "/v1/models":
				fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
			default:
				http.Error(w, "неожиданный маршрут", http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return Upstream{BaseURL: srv.URL + "/v1"}
	}
	res := balanceResolver{
		resolverMock{"a": mk("a"), "b": mk("b")},
		map[string][]string{"pool": {"a/m1", "b/m2"}},
	}
	chat := func(sid, body string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		rec := httptest.NewRecorder()
		if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
			t.Fatalf("chat: %v", err)
		}
	}
	// одна сессия — один участник, тела (прогресс systemone) меняются
	chat("s1", `{"model":"pool"}`)
	chat("s1", `{"model":"pool","progress":"next"}`)
	mu.Lock()
	s1 := seen[len(seen)-2:]
	mu.Unlock()
	if labelOf(s1[0]) != labelOf(s1[1]) {
		t.Errorf("одна сессия разошлась: %v, хочу один участник", s1)
	}
	// новые сессии — привязки нет, round-robin: за восемь свежих сессий
	// встречаются оба участника
	members := map[string]bool{}
	for i := 0; i < 8; i++ {
		chat(fmt.Sprintf("s%d", i+2), `{"model":"pool"}`)
	}
	mu.Lock()
	fresh := append([]string(nil), seen[len(seen)-8:]...)
	mu.Unlock()
	for _, s := range fresh {
		members[labelOf(s)] = true
	}
	if len(members) != 2 {
		t.Errorf("свежие сессии не распределяются: %v → %v", fresh, members)
	}
}

// TestChatBalanceStickyRemovedMember — участник выбыл из группы (алиас
// отредактировали): старая привязка сбрасывается, запрос уходит действующему.
func TestChatBalanceStickyRemovedMember(t *testing.T) {
	ResetAliasCache()
	ResetBalanceRR()
	ResetStickyCache()
	t.Cleanup(func() { ResetAliasCache(); ResetBalanceRR(); ResetStickyCache() })

	var mu sync.Mutex
	var seen []string
	mk := func(label string) Upstream {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/chat/completions":
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, label+"|"+body.Model)
				mu.Unlock()
				fmt.Fprint(w, `{"ok":true}`)
			case "/v1/models":
				fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
			default:
				http.Error(w, "неожиданный маршрут", http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return Upstream{BaseURL: srv.URL + "/v1"}
	}
	res := balanceResolver{
		resolverMock{"a": mk("a"), "b": mk("b")},
		map[string][]string{"pool": {"a/m1", "b/m2"}},
	}
	turn := `{"model":"pool","messages":[{"role":"user","content":"привет"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(turn))
	if err := Chat(httptest.NewRecorder(), req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("первый ход: %v", err)
	}
	mu.Lock()
	pinned := labelOf(seen[len(seen)-1])
	mu.Unlock()
	// редактирование алиаса: участник выбыл
	res.balances["pool"] = []string{"b/m2"}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(turn))
	if err := Chat(httptest.NewRecorder(), req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("ход после правки алиаса: %v", err)
	}
	mu.Lock()
	last := labelOf(seen[len(seen)-1])
	mu.Unlock()
	if last == pinned {
		t.Errorf("запрос снова ушел выбывшему участнику %q", last)
	}
}
