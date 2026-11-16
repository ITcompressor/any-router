// Package web — HTTP-сервер any-router: mux и авторизация.
//
// Дерево маршрутов (задачи 2–3 PRD):
//   - GET  /healthz      — живость, без авторизации;
//   - GET  /api/version  — версия сервиса (ollama-совместимая), без авторизации;
//   - GET  /icon.svg     — иконка проекта (favicon/логотип), без авторизации;
//   - GET  /             — статус, за админским Bearer;
//   - /v1/*              — к OpenAI-совместимым бэкендам, Bearer: админ или API-токен;
//   - /admin             — веб-админка (HTML), админ-токен: Bearer | cookie | ?token=;
//   - прочее             — 404 (за токеном, авторизация раньше роутинга).
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/langfuse"
	"github.com/ITcompressor/any-router/internal/logging"
	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/router"
	"github.com/ITcompressor/any-router/internal/store"
	"github.com/ITcompressor/any-router/internal/version"
)

// (версия сервиса — из internal/version; коммит/время — ldflags при сборке)

// Server — HTTP-сервер any-router.
type Server struct {
	cfg    config.Config
	log    *logging.Logger
	st     *store.Store
	mc     *proxy.ModelsCache // кэш списка моделей (старт + обновление раз в 10 мин)
	lf     *langfuse.Exporter // неблокирующий экспорт трейсов (langfuse, вкладка админки)
	listen string             // адрес слушателя: оверрайд или listen из файла

	onListen func() // колбэк после успешного бинда (кли: печать готовых ссылок)
}

// OnListen — колбэк, вызываемый один раз после успешного бинда
// (cli.serve печатает готовые ссылки админки/v1 уже только по факту
// прослушивания — ссылки не обгоняют сервер).
func (s *Server) OnListen(fn func()) { s.onListen = fn }

// New собирает сервер; listen — разрешённый адрес слушателя
// (оверрайд -addr/env или значение из файла, см. cli.Serve).
// nil-логгер заменяется на дефолтный.
func New(cfg config.Config, log *logging.Logger, st *store.Store, listen string) *Server {
	if log == nil {
		log = logging.New(cfg.LogLevel)
	}
	if listen == "" {
		listen = store.DefaultListen
	}
	s := &Server{cfg: cfg, log: log, st: st, listen: listen}
	// Кэш списка моделей: первичная полная загрузка при старте serve,
	// далее обновление раз в 10 минут; /v1/models и /admin/api/models
	// отдают его снимок без опроса провайдеров на каждый запрос.
	s.mc = proxy.NewModelsCache(s, log, proxy.ModelsRefreshInterval)
	// Экспорт трейсов в Langfuse (если настроен в конфиг-сторе; мутация
	// langfuse-set в админке меняет его на лету, без перезапуска).
	s.lf = langfuse.New(log, langfuse.QueueSize)
	s.lf.Update(langfuseConfig(st.LangfuseCfg()))
	return s
}

// langfuseConfig — store.Langfuse → langfuse.Config.
func langfuseConfig(l store.Langfuse) langfuse.Config {
	return langfuse.Config{
		Enabled: l.Enabled, URL: l.URL, PublicKey: l.PublicKey, SecretKey: l.SecretKey,
		Environment: l.Environment, Tags: l.Tags,
	}
}

// Addr — адрес, который слушает сервер.
func (s *Server) Addr() string { return s.listen }

// ModelsCache — доступ к кэшу списка моделей (в тесте — ручная
// загрузка/обновление без Run: ленивая загрузка сработает на первом
// Entries, если Start не запускался).
func (s *Server) ModelsCache() *proxy.ModelsCache { return s.mc }

// Handler собирает дерево маршрутов (см. описание пакета).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Живость — открыт.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Иконка проекта (favicon + логотип админки) — открыта: браузер
	// запрашивает favicon до входа по cookie.
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(iconSVG)
	})

	// Версия — открыта (ollama-совместимая, GET /api/version).
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"version": version.String()})
	})

	// Статус — за админ-токеном (Bearer или cookie после входа через ?token=).
	mux.Handle("/", s.adminAuth(http.HandlerFunc(s.status)))

	// /v1/* — Bearer: админ или API-токен, затем проксирование.
	v1 := http.HandlerFunc(s.v1)
	mux.Handle("/v1/", s.bearerAuth(v1))

	// /admin — веб-админка, админ-токен (Bearer | cookie | ?token=).
	mux.Handle("/admin", s.adminAuth(http.HandlerFunc(s.admin)))
	mux.Handle("/admin/", s.adminAuth(http.HandlerFunc(s.admin)))

	return mux
}

// Run запускает HTTP-сервер и блокируется до ошибки или завершения ctx;
// при ctx.Done() сервер аккуратно выключается (shutdown с таймаутом).
// Кэш списка моделей стартует до прослушивания: первичная полная загрузка
// (в фоне — /healthz отвечает сразу) + фоновые обновления раз в 10 минут.
// Листенер создаётся до Serve: занятый порт — ошибка до печати готовых
// ссылок печатной-cli (иначе ссылки ведут на чужой инстанс на этом же
// порту — вход в админку даёт 401 с их токеном). После успешного бинда
// кликается s.onListen (кли печатает ссылки).
func (s *Server) Run(ctx context.Context) error {
	s.mc.Start(ctx)
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("слушаю %s: %w (занят? — возможно, уже работает другой экземпляр any-router)", s.listen, err)
	}
	if s.onListen != nil {
		s.onListen()
	}
	srv := &http.Server{
		Addr:              s.listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Infof("HTTP-сервер слушает %s (конфиг %s)", s.listen, s.cfg.File)
		errCh <- srv.Serve(ln)
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		// финальный слив очереди экспорта langfuse (не дольше 3 с)
		s.lf.Shutdown(3 * time.Second)
		return err
	}
}

// bearerAuth — Bearer: админ или любой API-токен (/v1, задача 3 PRD).
// Совпавший кандидат кладётся в контекст (router.Identity) с API-токеном
// как нагрузкой — v1 по нему проверяет права на модель (scopeGate/modelFilter).
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return router.Auth(func() []router.Identity {
		api := s.st.Tokens()
		ids := make([]router.Identity, 0, len(api)+1)
		ids = append(ids, router.Identity{Secret: s.st.AdminToken()}) // админ: Token=nil — без ограничений
		for _, t := range api {
			ids = append(ids, router.Identity{Secret: t.Token, Token: t})
		}
		return ids
	}, next)
}

// adminAuth — админ-токен одним из способов: Bearer, cookie any_router_admin
// (ставится после входа через ?token=, HttpOnly + SameSite=Lax) или ?token=.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidates := map[string]bool{}
		for _, t := range []string{router.Bearer(r), r.URL.Query().Get("token"), cookieToken(r)} {
			if t != "" {
				candidates[t] = true
			}
		}
		ok := false
		for t := range candidates {
			if router.SecureEqual(t, s.st.AdminToken()) {
				ok = true
				break
			}
		}
		if !ok {
			router.Unauthorized(w)
			return
		}
		// Вход через ?token= запоминаем в cookie — навигация по админке без ?token.
		if r.URL.Query().Get("token") != "" && cookieToken(r) == "" {
			http.SetCookie(w, &http.Cookie{
				Name:     AdminCookie,
				Value:    s.st.AdminToken(),
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   12 * 3600,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// status — служебный JSON о сервисе (GET /).
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "any-router",
		"status":    "ok",
		"version":   version.Short(),
		"listen":    s.listen,
		"config":    s.cfg.File,
		"providers": len(s.st.ProviderNames()),
		"tokens":    len(s.st.Tokens()),
		"langfuse":  s.lf.Enabled(),
	})
}

// LangfuseExporter — экспортер трейсов (for tests: подмена настроек на лету).
func (s *Server) LangfuseExporter() *langfuse.Exporter { return s.lf }

// writeJSON пишет JSON-ответ с заданным кодом.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
