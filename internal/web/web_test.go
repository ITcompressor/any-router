package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/store"
	"github.com/ITcompressor/any-router/internal/version"
)

// newTestServer — сервер с временным any-router.yaml (свежие дефолты).
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.File = filepath.Join(t.TempDir(), "any-router.yaml")
	st, err := store.Load(cfg.File)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	return New(cfg, nil, st, cfg.Addr)
}

// TestHealthzOpen — /healthz без авторизации.
func TestHealthzOpen(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz без токена: код %d, хочу 200", rec.Code)
	}
}

// TestVersionOpen — GET /api/version без авторизации: версия сервиса.
func TestVersionOpen(t *testing.T) {
	s := newTestServer(t)
	// без токена — 200 и {"version":"v…"} (internal/version)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/version без токена: код %d, хочу 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело /api/version не JSON: %v", err)
	}
	if body["version"] != version.String() {
		t.Errorf("version = %q, хочу %q", body["version"], version.String())
	}
	// не GET и не POST — 404
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/version", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/version: код %d, хочу 404", rec.Code)
	}
}

// TestVersionBadge — версия бейджем в топбаре админки (internal/version).
func TestVersionBadge(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: код %d, хочу 200", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, `class="version"`) {
		t.Errorf("в топбаре нет бейджа версии")
	}
	if !strings.Contains(html, `>`+version.Short()+`</span>`) {
		t.Errorf("бейдж версии не содержит %q", version.Short())
	}
}

// TestStatusAdminOnly — GET / за админ-токеном: json со статусом.
func TestStatusAdminOnly(t *testing.T) {
	s := newTestServer(t)
	// без токена — 401
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET / без токена: код %d, хочу 401", rec.Code)
	}
	// с админ-токеном — статус
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / с админ-токеном: код %d, хочу 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело / не JSON: %v", err)
	}
	if body["service"] != "any-router" {
		t.Errorf("service: %v, хочу any-router", body["service"])
	}
	if body["providers"] != float64(0) {
		t.Errorf("providers: %v, хочу 0", body["providers"])
	}
}

// TestAPITokenNotForAdmin — API-токен не открывает админку.
func TestAPITokenNotForAdmin(t *testing.T) {
	s := newTestServer(t)
	tok, err := s.st.NewAPIToken(store.Token{ID: "user"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin с API-токеном: код %d, хочу 401", rec.Code)
	}
	// зато /v1 этим токеном открыто (проверка авторизации, не роутинга)
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("GET /v1/models с API-токеном: 401, API-токен пропускается")
	}
}

// TestAdminLoginFlows — ?token= ставит cookie; cookie потом работает;
// несоответствующий токен — 401.
func TestAdminLoginFlows(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	// вход через ?token=
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin?token=: код %d, хочу 200", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == AdminCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("после ?token= нет cookie %s", AdminCookie)
	}
	raw := rec.Header().Get("Set-Cookie")
	if !strings.Contains(raw, "HttpOnly") || !strings.Contains(raw, "SameSite=Lax") {
		t.Errorf("cookie без HttpOnly/SameSite=Lax: %q", raw)
	}
	// админка по cookie
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin по cookie: код %d, хочу 200", rec.Code)
	}
	// кривой cookie — 401
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: AdminCookie, Value: "wrong"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin с неверным cookie: код %d, хочу 401", rec.Code)
	}
}

// TestUnknownPathAuthorized404 — авторизация раньше роутинга: без токена 401, с токеном 404.
func TestUnknownPathAuthorized404(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /nope без токена: код %d, хочу 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope с токеном: код %d, хочу 404", rec.Code)
	}
}

// TestAdminCreateProviderEndToEnd — форма админки → файл → /v1 проксирует.
func TestAdminCreateProviderEndToEnd(t *testing.T) {
	s := newTestServer(t)
	// фейковый OpenAI-совместимый апстрим
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			// path-модели для авто-алиасов (роутер ходит сюда при miss кэша)
			fmt.Fprint(w, `{"object":"list","data":[{"id":"/opt/llm/models/Qwen.gguf"}]}`)
			return
		}
		fmt.Fprintf(w, `{"up":true,"model":%q,"path":%q}`, body.Model, r.URL.Path)
	}))
	defer up.Close()

	admin := s.Handler()
	// 1) добавляем провайдера через админку (cookie-сессия)
	req := postForm(t, "/admin", "op=provider-add&name=mock&baseurl="+urlPath(up.URL+"/v1"))
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mock") {
		t.Fatalf("админка не сохранила провайдера: код %d", rec.Code)
	}
	// 2) провайдер персистентен в yaml
	s2, err := store.Load(s.cfg.File)
	if err != nil {
		t.Fatalf("store.Load после мутации: %v", err)
	}
	if p, ok := s2.Provider("mock"); !ok || p.BaseURL != up.URL+"/v1" {
		t.Fatalf("провайдер не в файле: %+v ok=%v", p, ok)
	}
	// 3) /v1/chat/completions c API-токеном уходит в апстрим
	// (форма алиасов теперь нет — авто-алиасы отдельно в TestChatAutoAliasRoute)
	tok, err := s.st.NewAPIToken(store.Token{ID: "client"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	body := `{"model":"mock/qwen3","messages":[{"role":"user","content":"hi"}]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/chat/completions: код %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Up    bool   `json:"up"`
		Model string `json:"model"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if !got.Up || got.Model != "qwen3" || got.Path != "/v1/chat/completions" {
		t.Errorf("проксирование: up=%v model=%q path=%q", got.Up, got.Model, got.Path)
	}
}

// TestAdminSecretsEyeAndCopy — секреты в таблице по умолчанию маскированы,
// полный value в data-атрибутах (глаз/копия их заберут через JS).
func TestAdminSecretsEyeAndCopy(t *testing.T) {
	s := newTestServer(t)
	_ = s.st.SetProvider("ollama", store.Provider{BaseURL: "http://x/v1", APIKey: "sk-secret-long-key"})
	tok, err := s.st.NewAPIToken(store.Token{ID: "client"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	html := rec.Body.String()
	// client-токен: полный — в data-full (для глаза/копии), видимого текста нет
	if !strings.Contains(html, `data-full="`+tok.Token+`"`) {
		t.Errorf("client-токен не в data-full:\n%s", html)
	}
	if strings.Contains(html, ">"+tok.Token+"<") {
		t.Errorf("client-токен виден в таблице — сначала должна быть маска")
	}
	// глаз и кнопка копирования рядом
	for _, want := range []string{`secretEye(this)">👁`, `secretCopy(this)` + `" data-secret="`, `>⧉</button>`} {
		if !strings.Contains(html, want) {
			t.Errorf("нет кнопки %q", want)
		}
	}
	// apikey провайдера: полный — в data-full, в тексте — только маска
	if !strings.Contains(html, `data-full="sk-secret-long-key"`) {
		t.Errorf("apikey провайдера не в data-full")
	}
	if strings.Contains(html, ">sk-secret-long-key<") {
		t.Errorf("apikey виден открытым — маска сначала")
	}
	if !strings.Contains(html, ">sk-s…ey</code>") {
		t.Errorf("маска apikey не в тексте:\n%s", html)
	}
	// маска client-токена — первые 4 и последние 2
	if !strings.Contains(html, ">"+tok.Token[:4]+"…"+tok.Token[len(tok.Token)-2:]+"</code>") {
		t.Errorf("маска client-токена (abcd…ef) не в тексте")
	}
}

// TestV1ChatErrorMapping — ошибки маршрутизации в OpenAI-формате.
func TestV1ChatErrorMapping(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	tok, err := s.st.NewAPIToken(store.Token{ID: "client"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	cases := []struct {
		name, body, want string
	}{
		{"без провайдера", `{"model":"qwen3"}`, "не вида provider/model"},
		{"неизвестный провайдер", `{"model":"zzz/qwen3"}`, "не найден"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код %d, хочу 400", c.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: тело %q, хочу упоминание %q", c.name, rec.Body.String(), c.want)
		}
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: тело без error-обёртки OpenAI: %q", c.name, rec.Body.String())
		}
	}
}

// TestV1UnsupportedPath404 JSON-404 c API-токеном.
func TestV1UnsupportedPath404(t *testing.T) {
	s := newTestServer(t)
	tok, _ := s.st.NewAPIToken(store.Token{ID: "client"})
	req := httptest.NewRequest(http.MethodPost, "/v1/audio", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /v1/audio: код %d, хочу 404", rec.Code)
	}
}

// postForm — POST с urlencoded формой.
func postForm(t *testing.T, target, form string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// urlPath — кавычки для form-значения URL.
func urlPath(s string) string {
	return strings.ReplaceAll(s, ":", "%3A")
}

// TestAddrDefault — Addr() отдаёт адрес слушателя; тестовый сервер
// создаётся с пустым оверрайдом → дефолт из store.DefaultListen.
func TestAddrDefault(t *testing.T) {
	if got := newTestServer(t).Addr(); got != ":8085" {
		t.Errorf("Addr: %q, хочу :8085", got)
	}
}

// TestStoreFileModeZero — YAML-конфиг не создаётся в cwd тестов: у теста
// свой tmp-путь (см. newTestServer).
func TestStoreFileModeZero(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.File = filepath.Join(dir, "x.yaml")
	st, err := store.Load(cfg.File)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fi, err := os.Stat(cfg.File)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("права %s: %v; err=%v", cfg.File, fi, err)
	}
	_ = st
}

// TestAdminTabs — страница: три вкладки-кнопки, панели, JS переключателя;
// после мутации — ActiveTab на вкладке мутации.
func TestAdminTabs(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	html := rec.Body.String()
	for _, want := range []string{
		`data-pane="providers"`, `data-pane="tokens"`, `data-pane="examples"`,
		`id="pane-providers"`, `id="pane-tokens"`, `id="pane-examples"`,
		`function activateTab(`, `localStorage.setItem`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
	// опции токенов в примерах: админ + созданный API
	tok, err := s.st.NewAPIToken(store.Token{ID: "client"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	html = rec.Body.String()
	if !strings.Contains(html, `>админ-токен</option>`) {
		t.Errorf("нет опции админ-токена")
	}
	if !strings.Contains(html, `>API: client — все модели</option>`) {
		t.Errorf("нет опции API-токена client с правами")
	}
	if !strings.Contains(html, `value="`+tok.Token+`"`) {
		t.Errorf("полный токен не в option (в примеры не подставить)")
	}
	// после token-add — вкладка токенов
	req = postForm(t, "/admin", "op=token-add&id=t9")
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `var initialTab = urlTab || 'tokens'`) {
		t.Errorf("после token-add: не вкладка токенов")
	}
}

// TestAdminExamplesBaseURL — curl-сниппеты вкладки «Примеры вызова»
// строятся на base_url запуска: по умолчанию config.DefaultBaseURL
// (http://localhost:8085), с -base-url — переданный адрес; пустой BaseURL в
// конфиге сервер заменяет на дефолт.
func TestAdminExamplesBaseURL(t *testing.T) {
	htmlOf := func(s *Server) string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin: код %d, хочу 200", rec.Code)
		}
		return rec.Body.String()
	}
	newServer := func(baseURL string) *Server {
		t.Helper()
		cfg := config.Default()
		cfg.File = filepath.Join(t.TempDir(), "any-router.yaml")
		cfg.BaseURL = baseURL
		st, err := store.Load(cfg.File)
		if err != nil {
			t.Fatalf("store.Load: %v", err)
		}
		return New(cfg, nil, st, cfg.Addr)
	}

	// html/template в JS-строке экранирует «/» как «\/» — считываем как есть
	jsEsc := strings.NewReplacer("/", "\\/")

	// дефолт — localhost, а не origin строки браузера
	if html := htmlOf(newServer("") /* пусто — сервер подставит дефолт */); !strings.Contains(html, `var routerBase = '`+jsEsc.Replace(config.DefaultBaseURL)+`'`) {
		t.Errorf("routerBase не дефолт base_url %s (пустой BaseURL)", config.DefaultBaseURL)
	}
	if html := htmlOf(newServer(config.DefaultBaseURL)); !strings.Contains(html, "адрес — base_url запуска ("+config.DefaultBaseURL+";") {
		t.Errorf("в «Примерах вызова» не описан дефолт base_url")
	}

	// -base-url — переданный адрес
	if html := htmlOf(newServer("https://llm.example.by:8443/")); !strings.Contains(html, `var routerBase = '`+jsEsc.Replace("https://llm.example.by:8443/")+`'`) {
		t.Errorf("routerBase не задан -base-url значением")
	}
}

// TestAdminAPIModels — JSON для примеров: алиасный провайдер отдаёт
// только алиасы, raw — целевой id; без админ-авторизации — 401.
func TestAdminAPIModels(t *testing.T) {
	s := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"/opt/llm/models/Qwen3.8-27B.gguf"}]}`)
	}))
	defer up.Close()
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	handler := s.Handler()

	// без токена — 401
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("без админ-токена: код %d, хочу 401", rec.Code)
	}

	// с токеном — JSON с алиасным ref и raw
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models?token="+s.st.AdminToken(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Raw      string `json:"raw"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "mock/Qwen3.8-27B" || body.Data[0].Raw != "/opt/llm/models/Qwen3.8-27B.gguf" {
		t.Errorf("авто-алиас entry: %+v", body.Data)
	}
}

// TestV1ModelsServedFromCache — /v1/models отдаётся из кэша списка моделей:
// провайдер опрашивается один раз (первичная загрузка), следующие запросы —
// из снимка; Refresh (кнопка ⟳ в админке) — новый опрос.
func TestV1ModelsServedFromCache(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	s := newTestServer(t)
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			atomic.AddInt32(&hits, 1)
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"}]}`)
		case r.URL.Path == "/api/show":
			http.NotFound(w, r)
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer up.Close()
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	handler := s.Handler()

	v1Models := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 3; i++ {
		rec := v1Models()
		if rec.Code != http.StatusOK {
			t.Fatalf("запрос %d: код %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"id":"mock/m1"`) {
			t.Fatalf("запрос %d: нет модели в списке: %s", i+1, rec.Body.String())
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("опросов /v1/models у провайдера: %d, хочу 1 (первичная загрузка, дальше кэш)", n)
	}
	// Refresh — немедленное обновление: новый опрос, список из свежего снимка.
	s.ModelsCache().Refresh(context.Background())
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("после Refresh опросов: %d, хочу 2", n)
	}
	if rec := v1Models(); !strings.Contains(rec.Body.String(), `"id":"mock/m1"`) {
		t.Errorf("список после Refresh: %s", rec.Body.String())
	}
}

// TestAdminAPIModelsRefreshParam — GET /admin/api/models?refresh=1 запускает
// немедленный полный цикл обновления кэша; без параметра — текущий снимок
// без дополнительных сетевых запросов к провайдерам.
func TestAdminAPIModelsRefreshParam(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	s := newTestServer(t)
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			atomic.AddInt32(&hits, 1)
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"}]}`)
		case r.URL.Path == "/api/show":
			http.NotFound(w, r)
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer up.Close()
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	handler := s.Handler()
	tok := s.st.AdminToken()

	apiModels := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}
	// Первый запрос — первичная загрузка кэша.
	if rec := apiModels("/admin/api/models?token=" + tok); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	before := atomic.LoadInt32(&hits)
	if before == 0 {
		t.Fatalf("первичная загрузка не опросила провайдера")
	}
	// Повторный без refresh — снимок, новых запросов к провайдерам нет.
	if rec := apiModels("/admin/api/models?token=" + tok); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != before {
		t.Errorf("опросов без refresh: %d, хочу %d (снимок кэша)", n, before)
	}
	// refresh=1 — немедленный полный цикл обновления.
	if rec := apiModels("/admin/api/models?token=" + tok + "&refresh=1"); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != before+1 {
		t.Errorf("опросов после refresh=1: %d, хочу %d", n, before+1)
	}
}

// TestChatAutoAliasRoute — сквозной авто-алиас: модель «имя-файла» уходит
// в апстрим полным путём (роутер кэширует список /v1/models провайдера).
func TestChatAutoAliasRoute(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	path := "/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"
	seenModel := ""
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, path)
		case r.URL.Path == "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		default:
			http.Error(w, "неожиданный маршрут", http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	tok, err := s.st.NewAPIToken(store.Token{ID: "client"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	handler := s.Handler()
	// 1) прогреем кэш списком моделей
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"id":"mock/Qwen3.8-27B-UD-IQ4_XS"`) {
		t.Fatalf("авто-алиас не в /v1/models: %s", rec.Body.String())
	}
	// 2) chat по красивому имени → апстрим видит путь
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock/Qwen3.8-27B-UD-IQ4_XS","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: код %d: %s", rec.Code, rec.Body.String())
	}
	if seenModel != path {
		t.Errorf("апстрим получил %q, хочу путь %s", seenModel, path)
	}
}

// ─── права API-токенов (области: all / provider / model / models с хостами) ─

// TestTokenScopesChat — области токенов на chat: provider — свой провайдер
// проходит, чужой 403 (и несуществующий тоже — знания о провайдерах не
// раскрываем); model — своя модель проходит, чужая 403. Админ не ограничен.
func TestTokenScopesChat(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	seenModel := ""
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		http.Error(w, "неожиданный маршрут", http.StatusNotFound)
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	handler := s.Handler()
	provTok, err := s.st.NewAPIToken(store.Token{ID: "prov", Scope: store.ScopeProvider, Provider: "mock"})
	if err != nil {
		t.Fatalf("token provider: %v", err)
	}
	modTok, err := s.st.NewAPIToken(store.Token{ID: "mod", Scope: store.ScopeModel, Provider: "mock", Model: "qwen3"})
	if err != nil {
		t.Fatalf("token model: %v", err)
	}
	hostTok, err := s.st.NewAPIToken(store.Token{ID: "host", Scope: store.ScopeModels, Models: []string{"mock/*"}})
	if err != nil {
		t.Fatalf("token host: %v", err)
	}

	post := func(tok, model string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// provider-scope: свой — в апстрим под голым именем модели
	seenModel = ""
	if rec := post(provTok.Token, `mock/qwen3`); rec.Code != http.StatusOK || seenModel != "qwen3" {
		t.Errorf("provider-scope свой: код %d модель %q: %s", rec.Code, seenModel, rec.Body.String())
	}
	// чужой провайдер и несуществующий — единообразный 403 в OpenAI-формате
	for _, model := range []string{`other/qwen3`, `zzz/qwen3`} {
		rec := post(provTok.Token, model)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"error"`) ||
			!strings.Contains(rec.Body.String(), "права токена") {
			t.Errorf("provider-scope %q: код %d тело %s, хочу 403 с правами", model, rec.Code, rec.Body.String())
		}
	}
	// model-scope: своя — ок, чужая — 403
	seenModel = ""
	if rec := post(modTok.Token, `mock/qwen3`); rec.Code != http.StatusOK || seenModel != "qwen3" {
		t.Errorf("model-scope своя: код %d модель %q", rec.Code, seenModel)
	}
	if rec := post(modTok.Token, `mock/llama3.2`); rec.Code != http.StatusForbidden {
		t.Errorf("model-scope чужая модель: код %d, хочу 403", rec.Code)
	}
	// host-scope (wildcard «mock/*»): любая модель провайдера — ок, чужой — 403
	seenModel = ""
	if rec := post(hostTok.Token, `mock/любая-модель`); rec.Code != http.StatusOK || seenModel != "любая-модель" {
		t.Errorf("host-scope своя: код %d модель %q", rec.Code, seenModel)
	}
	if rec := post(hostTok.Token, `other/qwen3`); rec.Code != http.StatusForbidden {
		t.Errorf("host-scope чужой провайдер: код %d, хочу 403", rec.Code)
	}
	// all-scope — как раньше: несуществующий провайдер даёт обычный 400
	allTok, _ := s.st.NewAPIToken(store.Token{ID: "all"})
	if rec := post(allTok.Token, `zzz/q`); rec.Code != http.StatusBadRequest {
		t.Errorf("all-scope: код %d, хочу 400 (провайдер не найден)", rec.Code)
	}
	// админ не ограничен
	if rec := post(s.st.AdminToken(), `zzz/q`); rec.Code != http.StatusBadRequest {
		t.Errorf("админ: код %d, хочу 400 (провайдер не найден)", rec.Code)
	}
}

// TestTokenScopeModelAliasPath — у scope-модели-алиаса путевая форма запроса
// тоже проходит (алиас→путь из прогретого кэша авто-алиасов).
func TestTokenScopeModelAliasPath(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	path := "/opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf"
	seenModel := ""
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, path)
		case r.URL.Path == "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		default:
			http.Error(w, "неожиданный маршрут", http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	tok, err := s.st.NewAPIToken(store.Token{ID: "t", Scope: store.ScopeModel, Provider: "mock", Model: "Qwen3.8-27B-UD-IQ4_XS"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	handler := s.Handler()
	// 1) прогрев кэша: список моделей токеном — только его область
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"mock/Qwen3.8-27B-UD-IQ4_XS"`) {
		t.Fatalf("модели токена: код %d: %s", rec.Code, rec.Body.String())
	}
	// 2) chat путевой формой — проходит по кэшу, в апстрим уходит путь
	seenModel = ""
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock//opt/llm/models/Qwen3.8-27B-UD-IQ4_XS.gguf","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat путевой формой: код %d: %s", rec.Code, rec.Body.String())
	}
	if seenModel != path {
		t.Errorf("апстрим получил %q, хочу путь %s", seenModel, path)
	}
}

// TestTokenScopesModels — /v1/models фильтруется по правам токена
// (включая llama-массив models); пустой список под фильтром — 200, не 502.
func TestTokenScopesModels(t *testing.T) {
	proxy.ResetShowMiss()
	t.Cleanup(proxy.ResetShowMiss)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"alpha"},{"id":"beta"},{"id":"/opt/models/Path.gguf"}]}`)
			return
		}
		http.Error(w, "неожиданный маршрут", http.StatusNotFound)
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	_ = s.st.SetProvider("other", store.Provider{BaseURL: up.URL + "/v1"})
	handler := s.Handler()
	provTok, _ := s.st.NewAPIToken(store.Token{ID: "prov", Scope: store.ScopeProvider, Provider: "mock"})
	modTok, _ := s.st.NewAPIToken(store.Token{ID: "mod", Scope: store.ScopeModel, Provider: "mock", Model: "alpha"})
	pathTok, _ := s.st.NewAPIToken(store.Token{ID: "path", Scope: store.ScopeModel, Provider: "mock", Model: "/opt/models/Path.gguf"})
	emptyTok, _ := s.st.NewAPIToken(store.Token{ID: "empty", Scope: store.ScopeModel, Provider: "mock", Model: "no-such-model"})
	hostTok, _ := s.st.NewAPIToken(store.Token{ID: "host", Scope: store.ScopeModels, Models: []string{"mock/*", "other/alpha"}})

	get := func(tok string) (int, []string, int) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("тело /v1/models не JSON: %v: %s", err, rec.Body.String())
		}
		ids := make([]string, 0, len(body.Data))
		for _, e := range body.Data {
			ids = append(ids, e.ID)
		}
		return rec.Code, ids, len(body.Models)
	}
	// админ видит оба провайдера (6 записей: 3 имени/алиаса × 2 провайдера)
	code, ids, llama := get(s.st.AdminToken())
	if code != http.StatusOK || len(ids) != 6 || llama != 6 {
		t.Errorf("админ: код %d, data %d, models %d; хочу 6/6/6: %v", code, len(ids), llama, ids)
	}
	// provider-scope: только свой провайдер
	_, ids, llama = get(provTok.Token)
	if len(ids) != 3 || llama != 3 {
		t.Errorf("provider-scope: %d записей, models %d, хочу по 3: %v", len(ids), llama, ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "mock/") {
			t.Errorf("provider-scope: чужая запись %q", id)
		}
	}
	// model-scope по имени: ровно одна
	_, ids, llama = get(modTok.Token)
	if len(ids) != 1 || ids[0] != "mock/alpha" || llama != 1 {
		t.Errorf("model-scope: %v (models %d), хочу только mock/alpha", ids, llama)
	}
	// model-scope по пути: запись с raw-путём этой модели (ID — алиас)
	_, ids, llama = get(pathTok.Token)
	if len(ids) != 1 || ids[0] != "mock/Path" || llama != 1 {
		t.Errorf("model-scope path: %v (models %d), хочу только mock/Path", ids, llama)
	}
	// у scope-токена пустой результат — 200 (область не совпала с провайдерами)
	code, ids, _ = get(emptyTok.Token)
	if code != http.StatusOK || len(ids) != 0 {
		t.Errorf("пустая область: код %d, %d записей; хочу 200/0", code, len(ids))
	}
	// wildcard «mock/*» — все три записи mock (имена и path-алиас)
	// плюс конкретная other/alpha: 3 + 1
	_, ids, llama = get(hostTok.Token)
	if len(ids) != 4 || llama != 4 {
		t.Errorf("host-scope: %d записей, models %d, хочу по 4: %v", len(ids), llama, ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "mock/") && id != "other/alpha" {
			t.Errorf("host-scope: чужая запись %q", id)
		}
	}
}

// TestAdminTokenAddScopeForm — форма создания токена на селектах: области
// пишутся в yaml; scope=model приходит полной формой provider/model
// (path-модели — с двойным слэшем) и разбирается; неизвестный провайдер — ошибка.
func TestAdminTokenAddScopeForm(t *testing.T) {
	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: "http://x/v1"})
	handler := s.Handler()
	admin := s.st.AdminToken()
	post := func(form string) *httptest.ResponseRecorder {
		req := postForm(t, "/admin?token="+admin, form)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// provider-scope: провайдер из селекта
	if rec := post("op=token-add&id=prov&scope=provider&provider=mock"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `создан API-токен "prov" — провайдер mock`) {
		t.Errorf("provider-scope: код %d: %s", rec.Code, rec.Body.String())
	}
	// model-scope: имя — полная форма в поле model
	_ = post("op=token-add&id=mod&scope=model&model=mock%2FQwen3.8-27B")
	// model-scope: path-модель — двойной слэш в форме
	_ = post("op=token-add&id=path&scope=model&model=mock%2F%2Fopt%2Fmodels%2FQ.gguf")
	// пустая область (селект не тронут) — all
	_ = post("op=token-add&id=plain")
	// неизвестный провайдер — ошибка на странице
	if rec := post("op=token-add&id=bad&scope=provider&provider=nope"); !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("неизвестный провайдер: тело без ошибки: %s", rec.Body.String())
	}
	// область models: multi-select присылает несколько полных форм
	_ = post("op=token-add&id=multi&scope=models&models=mock%2FQwen3.8-27B&models=mock%2F%2Fopt%2Fmodels%2FQ.gguf")
	// область models с wildcard-хостом: «provider/*» пишется как есть,
	// покрытая им модель того же(provider) провайдера канонизируется
	_ = post("op=token-add&id=host&scope=models&models=mock%2F*&models=mock%2Falpha")
	// wildcard с неизвестным провайдером — ошибка
	if rec := post("op=token-add&id=host-bad&scope=models&models=nope%2F*"); !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("wildcard с неизвестным провайдером: тело без ошибки: %s", rec.Body.String())
	}
	// кривые записи области models: чужой провайдер и пустой список — ошибки
	if rec := post("op=token-add&id=multi-bad&scope=models&models=nope%2Fx"); !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("models с неизвестным провайдером: тело без ошибки: %s", rec.Body.String())
	}
	if rec := post("op=token-add&id=multi-empty&scope=models"); !strings.Contains(rec.Body.String(), "хотя бы одна модель") {
		t.Errorf("models без моделей: тело без ошибки: %s", rec.Body.String())
	}
	// в yaml всё записалось верно
	s2, err := store.Load(s.cfg.File)
	if err != nil {
		t.Fatalf("Load после мутаций: %v", err)
	}
	byID := map[string]store.Token{}
	for _, tok := range s2.Tokens() {
		byID[tok.ID] = tok
	}
	if got := byID["prov"]; got.Scope != store.ScopeProvider || got.Provider != "mock" {
		t.Errorf("prov: %+v", got)
	}
	if got := byID["mod"]; got.Scope != store.ScopeModel || got.Provider != "mock" || got.Model != "Qwen3.8-27B" {
		t.Errorf("mod: %+v", got)
	}
	if got := byID["path"]; got.Scope != store.ScopeModel || got.Model != "/opt/models/Q.gguf" || got.Ref() != "mock//opt/models/Q.gguf" {
		t.Errorf("path: %+v", got)
	}
	if got := byID["plain"]; got.Scope != store.ScopeAll || got.Provider != "" || got.Model != "" {
		t.Errorf("plain: %+v", got)
	}
	if got := byID["multi"]; got.Scope != store.ScopeModels ||
		len(got.Models) != 2 || got.Models[0] != "mock/Qwen3.8-27B" || got.Models[1] != "mock//opt/models/Q.gguf" {
		t.Errorf("multi: %+v", got)
	}
	if got := byID["host"]; got.Scope != store.ScopeModels || !equalStringsLocal(got.Models, []string{"mock/*"}) {
		t.Errorf("host: %+v, хочу [mock/*] (конкретная mock/alpha покрыта wildcard’ом)", got)
	}
	// примеры: option API-токенов несёт data-атрибуты прав
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+admin, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	html := rec.Body.String()
	for _, want := range []string{
		`data-scope="provider" data-provider="mock"`, `>API: prov — провайдер mock<`,
		`data-scope="model" data-provider="mock" data-model="Qwen3.8-27B" data-models="">API: mod — модель mock/Qwen3.8-27B<`,
		`data-scope="model" data-provider="mock" data-model="/opt/models/Q.gguf" data-models="">API: path — модель mock//opt/models/Q.gguf<`,
		`data-scope="models" data-provider="" data-model="" data-models="mock/Qwen3.8-27B`, `>API: multi — модели: mock/Qwen3.8-27B, mock//opt/models/Q.gguf<`,
		`data-models="mock/*">API: host — модели: весь хост mock<`,
		// форма токена без выбора «права»: скрытая область models + один multi-select
		`name="scope" value="models"`, `id="tok-models-select"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
}

// TestAdminTokenEditScopeForm — ✎ в строке токена (op=token-edit): права
// меняются без пересоздания (id и секрет сохраняются), ошибки — на странице.
func TestAdminTokenEditScopeForm(t *testing.T) {
	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: "http://x/v1"})
	_ = s.st.SetProvider("other", store.Provider{BaseURL: "http://y/v1"})
	handler := s.Handler()
	admin := s.st.AdminToken()
	post := func(form string) *httptest.ResponseRecorder {
		req := postForm(t, "/admin?token="+admin, form)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	orig, err := s.st.NewAPIToken(store.Token{ID: "t1", Scope: store.ScopeModel, Provider: "mock", Model: "alpha"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	// область models из формы редактирования
	if rec := post("op=token-edit&edit-id=t1&scope=models&models=mock%2Falpha&models=other%2Fbeta"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `права API-токена "t1" обновлены — модели: mock/alpha, other/beta`) {
		t.Errorf("token-edit models: код %d: %s", rec.Code, rec.Body.String())
	}
	// в yaml: секрет и id не тронуты, область заменена
	s2, err := store.Load(s.cfg.File)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got store.Token
	for _, tk := range s2.Tokens() {
		if tk.ID == "t1" {
			got = tk
		}
	}
	if got.Scope != store.ScopeModels || len(got.Models) != 2 ||
		got.Models[0] != "mock/alpha" || got.Models[1] != "other/beta" ||
		got.Token != orig.Token {
		t.Errorf("после token-edit: %+v (хочу models-область, секрет %s)", got, orig.Token)
	}
	// откат к all
	if rec := post("op=token-edit&edit-id=t1&scope=all"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `обновлены — все модели`) {
		t.Errorf("token-edit all: код %d: %s", rec.Code, rec.Body.String())
	}
	// несуществующий токен и кривая область — ошибки
	if rec := post("op=token-edit&edit-id=nope&scope=all"); !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("token-edit nope: тело без ошибки: %s", rec.Body.String())
	}
	if rec := post("op=token-edit&edit-id=t1&scope=provider&provider=zzz"); !strings.Contains(rec.Body.String(), "zzz") {
		t.Errorf("token-edit кривой провайдер: тело без ошибки: %s", rec.Body.String())
	}
	// option примеров несёт обновлённую область
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+admin, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `data-scope="all" data-provider="" data-model="" data-models="">API: t1`) {
		t.Errorf("option t1 без обновлённой области: %s", rec.Body.String())
	}
}

// TestTokenScopeModelsChat — область models на chat: разрешает модели из
// списка (в т.ч. других провайдеров), чужие — 403 с описанием области;
// /v1/models отфильтрован по списку (включая raw-форму path-моделей).
func TestTokenScopeModelsChat(t *testing.T) {
	proxy.ResetAliasCache()
	proxy.ResetShowMiss()
	t.Cleanup(proxy.ResetAliasCache)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"alpha"},{"id":"beta"},{"id":"/opt/models/Path.gguf"}]}`)
		case r.URL.Path == "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":true,"model":%q}`, body.Model)
		default:
			http.Error(w, "неожиданный маршрут", http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	_ = s.st.SetProvider("other", store.Provider{BaseURL: up.URL + "/v1"})
	tok, err := s.st.NewAPIToken(store.Token{ID: "mix", Scope: store.ScopeModels,
		Models: []string{"mock/alpha", "other/beta", "mock//opt/models/Path.gguf"}})
	if err != nil {
		t.Fatalf("NewAPIToken models: %v", err)
	}
	handler := s.Handler()
	post := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, model := range []string{"mock/alpha", "other/beta", "mock//opt/models/Path.gguf"} {
		if rec := post(model); rec.Code != http.StatusOK {
			t.Errorf("models-scope %s: код %d: %s", model, rec.Code, rec.Body.String())
		}
	}
	// алиасная форма path-модели из списка — тоже своя
	if rec := post("mock/Path"); rec.Code != http.StatusOK {
		t.Errorf("models-scope алиас mock/Path: код %d: %s", rec.Code, rec.Body.String())
	}
	// чужие модели/провайдеры — 403 с описанием области
	for _, model := range []string{"mock/beta", "other/alpha", "zzz/alpha"} {
		rec := post(model)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "модели: mock/alpha, other/beta") {
			t.Errorf("models-scope %s: код %d, тело без области: %s", model, rec.Code, rec.Body.String())
		}
	}
	// /v1/models отфильтрован: свои три записи (path-модель — алиасной записью)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var body struct {
		Data []struct {
			ID  string `json:"id"`
			Raw string `json:"raw"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, e := range body.Data {
		ids = append(ids, e.ID)
	}
	if len(ids) != 3 || strings.Join(ids, ",") != "mock/alpha,mock/Path,other/beta" {
		// порядок записей — группировкой по провайдерам (mock раньше other);
		// у path-моделей ID — алиас
		t.Errorf("модели models-scope: %v, хочу [mock/alpha mock/Path other/beta]", ids)
	}
}

// TestV1SystemoneRoute — POST /v1/systemone (decision): маршрутизация как
// у chatPaths — модель provider/model из тела, апстрим получает
// перезаписанную модель и путь /v1/systemone.
func TestV1SystemoneRoute(t *testing.T) {
	s := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"id":"sysone","model":%q,"path":%q}`, body.Model, r.URL.Path)
	}))
	defer up.Close()
	if err := s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	body := `{"model":"mock/decision-9b","state":{"task":"t"},"questions":{"done":{"type":"noul","instructions":"готово?"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/systemone: код %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if got.ID != "sysone" || got.Model != "decision-9b" || got.Path != "/v1/systemone" {
		t.Errorf("проксирование: id=%q model=%q path=%q", got.ID, got.Model, got.Path)
	}
}

// TestAdminExamplesCapabilityBlocks — примеры вызова capability-aware:
// три блока (chat/embeddings/systemone) с ids row-* (embed/sysone скрыты
// по умолчанию), options моделей несут capabilities (dataset.caps),
// сниппеты systemone/embeddings и capability-логика строит на клиенте.
func TestAdminExamplesCapabilityBlocks(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: код %d", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{
		`id="row-chat"`, `id="row-embed" hidden`, `id="row-sysone" hidden`,
		`id="ex-embeddings"`, `id="ex-systemone"`, `copyPre('ex-systemone'`,
		`o.dataset.caps = m.capabilities.join(',')`, `function capsOf(`,
		`/v1/systemone`, `caps && !hasCap(caps, 'completion')`,
		`capsOf(model)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
}

// TestLangfuseTab — вкладка «Langfuse»: рендер, мутация langfuse-set
// (сохранение в yaml + live-обновление экспортера), статус в /.
func TestLangfuseTab(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	auth := "Bearer " + s.st.AdminToken()

	// GET /admin: вкладка есть (кнопка + панель)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	html := rec.Body.String()
	if !strings.Contains(html, `data-pane="langfuse"`) || !strings.Contains(html, `id="pane-langfuse"`) {
		t.Fatalf("вкладки Langfuse нет на странице")
	}
	if !strings.Contains(html, "статус: <b>выключен</b>") {
		t.Errorf("свежий конфиг — статус хочу «выключен»")
	}

	// частичная настройка (без ключей) — сохраняется, экспорт выключен
	rec = httptest.NewRecorder()
	req = postForm(t, "/admin", "op=langfuse-set&url=http%3A%2F%2F10.0.0.3%3A3000")
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `var initialTab = urlTab || 'langfuse'`) {
		t.Errorf("после langfuse-set: не вкладка langfuse")
	}
	if !strings.Contains(rec.Body.String(), "экспорт выключен") {
		t.Errorf("частичная настройка — хочу notice «выключен»")
	}
	if s.lf.Enabled() {
		t.Errorf("экспортер включен при частичной настройке")
	}
	// url с хвостом нормализован и пережил запись в yaml
	if l := s.st.LangfuseCfg(); l.URL != "http://10.0.0.3:3000" {
		t.Errorf("в yaml: %+v", l)
	}

	// полная настройка с включенным переключателем — включен (+ environment/tгgs из формы)
	rec = httptest.NewRecorder()
	req = postForm(t, "/admin", `op=langfuse-set&enabled=on&url=http%3A%2F%2F10.0.0.3%3A3000&public_key=pk-lf-x&secret_key=sk-lf-x&environment=prod&tags=load-test%2C+smoke`)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "экспорт langfuse включен") {
		t.Errorf("полная настройка — хочу notice «включен»: %s", rec.Body.String())
	}
	if !s.lf.Enabled() {
		t.Error("экспортер не включен на лету после langfuse-set")
	}

	// GET /admin: значения на странице + маска секретного ключа
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	html = rec.Body.String()
	for _, want := range []string{"value=\"http://10.0.0.3:3000\"", "value=\"pk-lf-x\""} {
		if !strings.Contains(html, want) {
			t.Errorf("нет %q на странице", want)
		}
	}
	if !strings.Contains(html, "статус: <b>включен</b>") {
		t.Errorf("статус хочу «включен»")
	}

	// кривой url (без схемы) при включенном переключателе — problem, настройки не применяются
	rec = httptest.NewRecorder()
	req = postForm(t, "/admin", "op=langfuse-set&enabled=on&url=10.0.0.3%3A3000&public_key=pk&secret_key=sk")
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "url должен начинаться") {
		t.Errorf("нет problem о схеме url")
	}
	if l := s.st.LangfuseCfg(); l.PublicKey != "pk-lf-x" {
		t.Errorf("кривая настройка перезаписала корректную: %+v", l)
	}
	if l := s.st.LangfuseCfg(); l.Environment != "prod" || len(l.Tags) != 2 || l.Tags[1] != "smoke" {
		t.Errorf("environment/tags не сохранились: %+v", l)
	}

	// переключатель снят — выключен без удаления настроек (url/ключи на месте;
	// форма шлёт все поля, как в UI)
	rec = httptest.NewRecorder()
	req = postForm(t, "/admin", `op=langfuse-set&url=http%3A%2F%2F10.0.0.3%3A3000&public_key=pk-lf-x&secret_key=sk-lf-x&environment=prod&tags=load-test%2C+smoke`)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "экспорт выключен переключателем") {
		t.Errorf("снятие переключателя — хочу notice «выключен переключателем»")
	}
	if s.lf.Enabled() {
		t.Error("экспортер включен при снятом переключателе")
	}
	if l := s.st.LangfuseCfg(); l.Enabled || l.On() || l.URL != "http://10.0.0.3:3000" || l.PublicKey != "pk-lf-x" {
		t.Errorf("настройки не сохранились при снятии переключателя: %+v", l)
	}
	// на странице — статус «выключен переключателем — настройки сохранены»
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "выключен переключателем — настройки сохранены") {
		t.Errorf("нет статуса «выключен переключателем» на странице")
	}
	// на странице: поля environment/tags заполнены
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", auth)
	handler.ServeHTTP(rec, req)
	html = rec.Body.String()
	for _, want := range []string{`name="environment"`, `value="prod"`, `value="load-test, smoke"`} {
		if !strings.Contains(html, want) {
			t.Errorf("нет %q на странице", want)
		}
	}
}

// ─── балансировка (баланс-алиасы) ───

// TestAdminBalanceCRUD — вкладка «Балансировка»: создание/редактирование/
// удаление алиаса формами (op=balance-add|edit|delete), ошибки валидации —
// на странице, таблица и yaml актуальны.
func TestAdminBalanceCRUD(t *testing.T) {
	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: "http://x/v1"})
	handler := s.Handler()
	admin := s.st.AdminToken()
	post := func(form string) *httptest.ResponseRecorder {
		req := postForm(t, "/admin?token="+admin, form)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// multi-select присылает несколько полей models
	if rec := post("op=balance-add&name=fast&models=mock%2Fm1&models=mock%2Fm2"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `баланс-алиас "fast" сохранён`) {
		t.Fatalf("создание: код %d: %s", rec.Code, rec.Body.String())
	}
	if members, ok := s.st.Balance("fast"); !ok || len(members) != 2 || members[0] != "mock/m1" {
		t.Fatalf("запись в конфиге: %v ok=%v", members, ok)
	}
	// страница: вкладка и строка таблицы
	req := httptest.NewRequest(http.MethodGet, "/admin?token="+admin, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	html := rec.Body.String()
	for _, want := range []string{
		`data-pane="balances"`, `id="bal-name"`, `id="bal-models-select"`, `value="balance-add" id="bal-op"`,
		`data-name="fast"`, `mock/m1, mock/m2`, `onclick="editBalance(this)"`, `value="balance-delete"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
	// редактирование через ✎: op=balance-edit — имя из скрытого поля, участники заменены
	if rec := post("op=balance-edit&edit-name=fast&name=fast&models=mock%2Fm2"); rec.Code != http.StatusOK {
		t.Fatalf("редактирование: %s", rec.Body.String())
	}
	if members, _ := s.st.Balance("fast"); len(members) != 1 || members[0] != "mock/m2" {
		t.Fatalf("после редактирования: %v", members)
	}
	if bs := s.st.Balances(); len(bs) != 1 || bs[0].Name != "fast" {
		t.Fatalf("перезапись создала дубль записи: %+v", bs)
	}
	// валидация: имя с «/» и без моделей — проблемы на странице
	for _, form := range []string{
		"op=balance-add&name=a%2Fb&models=mock%2Fm1",
		"op=balance-add&name=alone",
		"op=balance-add&name=badprov&models=nope%2Fm",
	} {
		if rec := post(form); !strings.Contains(rec.Body.String(), `"error"`) && !strings.Contains(rec.Body.String(), "need") {
			if !strings.Contains(rec.Body.String(), `<div class="notice err">`) {
				t.Errorf("form %s: нет сообщения об ошибке: %s", form, rec.Body.String())
			}
		}
	}
	// удаление
	if rec := post("op=balance-delete&name=fast"); rec.Code != http.StatusOK {
		t.Fatalf("удаление: %s", rec.Body.String())
	}
	if _, ok := s.st.Balance("fast"); ok {
		t.Errorf("алиас не удалён")
	}
	s2, err := store.Load(s.cfg.File)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s2.Balances()) != 0 {
		t.Errorf("в yaml остались баланс-алиасы: %+v", s2.Balances())
	}
}

// TestBalanceModelsList — записи баланс-алиасов в /v1/models и /admin/api/models:
// голое имя, баланс-пометка, участники и пересечение кап; scope-токен видит
// алиас, только когда его область покрывает всех участников.
func TestBalanceModelsList(t *testing.T) {
	proxy.ResetAliasCache()
	t.Cleanup(proxy.ResetAliasCache)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}],`+
				`"models":[{"name":"m1","model":"m1","capabilities":["completion","tools"]},`+
				`{"name":"m2","model":"m2","capabilities":["completion"]}]}`)
		default:
			http.Error(w, "неожиданный маршрут", http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t)
	if err := s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	if err := s.st.SetBalance("pool", []string{"mock/m1", "mock/m2"}); err != nil {
		t.Fatalf("SetBalance: %v", err)
	}
	s.ModelsCache().Refresh(context.Background())
	handler := s.Handler()

	entry := func(tok string) (map[string]any, bool) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("тело /v1/models не JSON: %v: %s", err, rec.Body.String())
		}
		for _, e := range body.Data {
			if id, _ := e["id"].(string); id == "pool" {
				return e, true
			}
		}
		return nil, false
	}
	// админ: запись алиаса с баланс-пометкой, участниками и пересечением кап
	e, ok := entry(s.st.AdminToken())
	if !ok {
		t.Fatalf("записи «pool» нет в /v1/models")
	}
	if b, _ := e["balance"].(bool); !b {
		t.Errorf("balance: %v, хочу true", e["balance"])
	}
	members := fmt.Sprint(e["members"])
	if !strings.Contains(members, "mock/m1") || !strings.Contains(members, "mock/m2") {
		t.Errorf("members: %v, хочу [mock/m1 mock/m2]", e["members"])
	}
	caps := fmt.Sprint(e["capabilities"])
	if !strings.Contains(caps, "completion") || strings.Contains(caps, "tools") {
		t.Errorf("capabilities: %v, хочу пересечение [completion]", e["capabilities"])
	}
	// частичное покрытие — алиас скрыт; полное — виден
	if _, err := s.st.NewAPIToken(store.Token{ID: "t1", Scope: store.ScopeModels, Models: []string{"mock/m1"}}); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if _, err := s.st.NewAPIToken(store.Token{ID: "t2", Scope: store.ScopeModels,
		Models: []string{"mock/m1", "mock/m2", "mock/m3"}}); err != nil {
		t.Fatalf("t2: %v", err)
	}
	toks := s.st.Tokens()
	t1, t2 := toks[0].Token, toks[1].Token
	if _, ok := entry(t1); ok {
		t.Errorf("частичное покрытие: алиас не должен быть виден токену с правами на одну модель")
	}
	if _, ok := entry(t2); !ok {
		t.Errorf("полное покрытие: алиас не виден токену с правами на всех участников")
	}
	// /admin/api/models тоже несёт запись алиаса
	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"id":"pool"`) || !strings.Contains(rec.Body.String(), `"balance":true`) {
		t.Errorf("/admin/api/models без записи алиаса: %s", rec.Body.String())
	}
}

// TestTokenScopeBalanceChat — права scope-токена на запрос через алиас:
// непокрытая область (хоть один участник вне прав) — 403 с алиасом в тексте,
// полное покрытие — 200 и round-robin по участникам.
func TestTokenScopeBalanceChat(t *testing.T) {
	proxy.ResetAliasCache()
	proxy.ResetBalanceRR()
	t.Cleanup(func() { proxy.ResetAliasCache(); proxy.ResetBalanceRR() })

	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seen = append(seen, body.Model)
			mu.Unlock()
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		http.Error(w, "неожиданный маршрут", http.StatusNotFound)
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	_ = s.st.SetBalance("pool", []string{"mock/m1", "mock/m2", "mock/m3"})
	handler := s.Handler()
	if _, err := s.st.NewAPIToken(store.Token{ID: "t1", Scope: store.ScopeModels, Models: []string{"mock/m1"}}); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if _, err := s.st.NewAPIToken(store.Token{ID: "t2", Scope: store.ScopeModels,
		Models: []string{"mock/m1", "mock/m2", "mock/m3"}}); err != nil {
		t.Fatalf("t2: %v", err)
	}
	toks := s.st.Tokens()
	partial, full := toks[0].Token, toks[1].Token

	post := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"pool","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// частичное покрытие: 403 — алиас в сообщении (round-robin мог уйти на m2/m3)
	if rec := post(partial); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "баланс-алиас") || !strings.Contains(rec.Body.String(), "права токена") {
		t.Errorf("частичное покрытие: код %d: %s", rec.Code, rec.Body.String())
	}
	if seenNow := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }(); len(seenNow) != 0 {
		t.Errorf("403 без маршрутизации, а апстрим получил %v", seenNow)
	}
	// полное покрытие: 200, запросы ротацией
	for i := 0; i < 3; i++ {
		if rec := post(full); rec.Code != http.StatusOK {
			t.Fatalf("запрос %d: %s", i+1, rec.Body.String())
		}
	}
	// round-robin: снимок seen — до следующего post (не держим mu во время запроса)
	mu.Lock()
	seenCopy := append([]string(nil), seen...)
	mu.Unlock()
	if len(seenCopy) != 3 || (seenCopy[0] == seenCopy[1] || seenCopy[1] == seenCopy[2]) {
		t.Errorf("round-robin по участникам: seen %v, хочу три разные подряд", seenCopy)
	}
	// админ не ограничен
	if rec := post(s.st.AdminToken()); rec.Code != http.StatusOK {
		t.Errorf("админ: код %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBalancesTabI18N — вкладка «Балансировка» на обоих языках: заголовок,
// подсказка и легенда формы из бандла строк (ru — дефолт, en — cookie).
func TestBalancesTabI18N(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	get := func(langCookie string) string {
		req := httptest.NewRequest(http.MethodGet, "/admin?token="+s.st.AdminToken(), nil)
		req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
		if langCookie != "" {
			req.AddCookie(&http.Cookie{Name: LangCookie, Value: langCookie})
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	for _, want := range []string{"Балансировка — алиасы групп моделей", "Баланс-алиас — голое имя", "Добавить баланс-алиас"} {
		if !strings.Contains(get(""), want) {
			t.Errorf("в русской админке нет %q", want)
		}
	}
	html := get("en")
	for _, want := range []string{"Load balancing — model group aliases", `data-pane="balances"`, "Add balance alias"} {
		if !strings.Contains(html, want) {
			t.Errorf("в английской админке нет %q", want)
		}
	}
}

// TestTokenScopeAliasGrant — явная выдача баланс-алиаса в области models:
// токен может звать алиас по имени (участники напрямую — 403), алиас виден
// ему в /v1/models и /admin…; токен без выдачи и без полного покрытия — 403
// и в списке алиаса нет.
func TestTokenScopeAliasGrant(t *testing.T) {
	proxy.ResetAliasCache()
	proxy.ResetBalanceRR()
	proxy.ResetStickyCache()
	t.Cleanup(func() { proxy.ResetAliasCache(); proxy.ResetBalanceRR(); proxy.ResetStickyCache() })

	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seen = append(seen, body.Model)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
		default:
			http.Error(w, "неожиданный маршрут", http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: up.URL + "/v1"})
	_ = s.st.SetBalance("fast-pool", []string{"mock/m1", "mock/m2"})
	handler := s.Handler()
	grantTok, err := s.st.NewAPIToken(store.Token{ID: "grant", Scope: store.ScopeModels, Models: []string{"fast-pool"}})
	if err != nil {
		t.Fatalf("токен с алиасом: %v", err)
	}
	_, _ = s.st.NewAPIToken(store.Token{ID: "stranger", Scope: store.ScopeModels, Models: []string{"mock/m1"}})
	toks := s.st.Tokens()
	stranger := toks[1].Token

	post := func(tok, model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	ids := func(tok string) []string {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("/v1/models: %v", err)
		}
		var out []string
		for _, e := range body.Data {
			out = append(out, e.ID)
		}
		return out
	}
	// алиас виден токену с явной выдачей; участники напрямую в списке НЕ
	// светятся (выдача пула не открывает его модели по отдельности)
	hasAlias, hasMember := false, false
	for _, id := range ids(grantTok.Token) {
		if id == "fast-pool" {
			hasAlias = true
		}
		if id == "mock/m1" {
			hasMember = true
		}
	}
	if !hasAlias {
		t.Errorf("токен с выдачей алиаса: /v1/models не содержит алиас")
	}
	if hasMember {
		t.Errorf("выдача пула не должна раскрывать участников в /v1/models")
	}
	// чат через алиас — 200 (право выдано по имени)
	if rec := post(grantTok.Token, "fast-pool"); rec.Code != http.StatusOK {
		t.Errorf("chat через явно выданный алиас: %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	served := append([]string(nil), seen...)
	mu.Unlock()
	if len(served) != 1 || (served[0] != "m1" && served[0] != "m2") {
		t.Errorf("маршрутизация алиаса: %v", served)
	}
	// участник напрямую — 403: выдача пула не даёт прав на его модели по отдельности
	if rec := post(grantTok.Token, "mock/m1"); rec.Code != http.StatusForbidden {
		t.Errorf("прямой вызов участника при выданном алиасе: %d, хочу 403", rec.Code)
	}
	// токен без выдачи алиаса и без полного покрытия: алиас скрыт, вызов — 403
	strangerHas := false
	for _, id := range ids(stranger) {
		if id == "fast-pool" {
			strangerHas = true
		}
	}
	if strangerHas {
		t.Errorf("токен без выдачи алиаса видит его в /v1/models")
	}
	if rec := post(stranger, "fast-pool"); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "баланс-алиас") {
		t.Errorf("чат через невыданный алиас: %d, хочу 403 с алиасом: %s", rec.Code, rec.Body.String())
	}
	if rec := post(grantTok.Token, "fast-poolX"); rec.Code != http.StatusBadRequest {
		t.Errorf("несуществующий алиас: %d, хочу 400", rec.Code)
	}
	// форма области models: multi-select содержит баланс-алиасы
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `id="tok-models-select"`) {
		t.Errorf("нет multi-select области models")
	}
}

// TestTokenScopeAliasGrantForm — token-add с областью models, содержащей
// баланс-алиас, пишется в yaml; неизвестный алиас — ошибка на странице.
func TestTokenScopeAliasGrantForm(t *testing.T) {
	s := newTestServer(t)
	_ = s.st.SetProvider("mock", store.Provider{BaseURL: "http://x/v1"})
	_ = s.st.SetBalance("fast-pool", []string{"mock/m1", "mock/m2"})
	handler := s.Handler()
	admin := s.st.AdminToken()
	post := func(form string) *httptest.ResponseRecorder {
		req := postForm(t, "/admin?token="+admin, form)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("op=token-add&id=g&scope=models&models=fast-pool&models=mock%2Fm1"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `создан API-токен "g" — модели: алиас fast-pool, mock/m1`) {
		t.Errorf("token-add с алиасом: %s", rec.Body.String())
	}
	if rec := post("op=token-add&id=bad&scope=models&models=nosuch-pool"); !strings.Contains(rec.Body.String(), "nosuch-pool") {
		t.Errorf("неизвестный алиас: тело без ошибки: %s", rec.Body.String())
	}
	s2, err := store.Load(s.cfg.File)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got store.Token
	for _, t2 := range s2.Tokens() {
		if t2.ID == "g" {
			got = t2
		}
	}
	if got.Scope != store.ScopeModels || !equalStringsLocal(got.Models, []string{"fast-pool", "mock/m1"}) {
		t.Errorf("область с алиасом в yaml: %+v", got)
	}
}

// equalStringsLocal — сравнение списков строк в тестах web.
func equalStringsLocal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
