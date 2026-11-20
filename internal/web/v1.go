// v1 — обработчик /v1/*: маршрутизация JSON-запросов по модели
// "provider/model" к провайдерам из конфиг-стора (задача 3 PRD).
package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/router"
	"github.com/ITcompressor/any-router/internal/store"
)

// chatPaths — POST-маршруты, где модель берётся из JSON-тела.
var chatPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/embeddings":       true,
	"/v1/systemone":        true,
}

// v1 — точки входа /v1/*: Bearer-авторизация выше; POST-маршруты, где модель
// берётся из тела (chat/completions, completions, embeddings, systemone),
// проксируются с перезаписью модели, GET /v1/models — агрегированный список,
// прочее — JSON 404.
func (s *Server) v1(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && chatPaths[r.URL.Path] {
		s.v1Chat(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		s.v1Models(w, r)
		return
	}
	proxy.WriteError(w, http.StatusNotFound,
		"маршрут %s %s: /v1 умеет POST chat|completions|embeddings|systemone и GET /v1/models",
		r.Method, r.URL.Path)
}

// v1Chat — POST с моделью "provider/model" в теле; права API-токена (scopeGate)
// proxy.Chat проверяет до маршрутизации. В langfuse экспорте идут только
// маршрутизированные запросы; IP/токен/провайдер/модель — на выбор sink'а.
func (s *Server) v1Chat(w http.ResponseWriter, r *http.Request) {
	// экспорт: nil, пока в админке не заполнены url/ключи (live — без перезапуска)
	var rec proxy.CallSink
	var tokenID string
	if s.lf.Enabled() {
		rec = s.lf
		// id API-токена (админ — пусто, у него Token{})
		if tok := requestToken(r); tok.ID != "" {
			tokenID = tok.ID
		}
	}
	if err := proxy.Chat(w, r, s, s.log, s.scopeGate(r), tokenID, rec); err != nil {
		var perr proxy.Err
		if errors.As(err, &perr) {
			proxy.WriteError(w, perr.Code, "%s", perr.Msg)
			return
		}
		s.log.Warnf("v1: %s: %v", r.URL.Path, err)
		proxy.WriteError(w, http.StatusBadGateway, "проксирование не удалось: %v", err)
	}
}

// v1Models — агрегированный список моделей провайдеров ("provider/model"):
// у провайдера с алиасами — только алиасы; плюс записи баланс-алиасов
// (balance/members). Список отфильтрован по правам API-токена (modelFilter) —
// у scope-токенов только их область (алиас виден, если область покрывает
// всех участников). Источник — кэш списка моделей (первичная полная загрузка
// при старте serve, далее обновление раз в 10 минут): провайдеры на каждый
// запрос не опрашиваются, кэш авто-алиасов прогревается циклами кэша;
// баланс-алиасы конфига строятся поверх кэша — правки в админке видны сразу.
func (s *Server) v1Models(w http.ResponseWriter, r *http.Request) {
	entries := s.mc.Entries(r.Context(), "")
	entries = append(entries, s.balanceEntries(entries)...)
	proxy.Models(w, r, s, entries, s.modelFilter(r))
}

// ─── права API-токенов (области: all / provider / model / models с хостами) ─

// requestToken — API-токен запроса из контекста (кладёт bearerAuth);
// админский запрос или вызов напрямую — Token{} без ограничений.
func requestToken(r *http.Request) store.Token {
	if id, ok := router.IdentityOf(r.Context()); ok && id.Token != nil {
		if t, ok := id.Token.(store.Token); ok {
			return t
		}
	}
	return store.Token{}
}

// scopeGate — права токена для proxy.Chat (Gate): nil — без ограничений
// (админ, API-токен области all); иначе 403 в OpenAI-формате с описанием прав
// токена (включая запросы за несуществующего провайдера — не раскрываем знаний).
func (s *Server) scopeGate(r *http.Request) proxy.Gate {
	tok := requestToken(r)
	if tok.Unlimited() {
		return nil
	}
	return tokGate{tok: tok}
}

// tokGate — права scope-токена: прямой запрос — совпадение области с моделью
// запроса (у области models — в том числе wildcard «provider/*»); запрос
// через баланс-алиас — область покрывает КАЖДОГО участника группы
// (round-robin вправе отправить запрос любому из них).
type tokGate struct{ tok store.Token }

// denied — единообразный 403 с описанием прав токена (what — что просили).
func (g tokGate) denied(what string) proxy.Err {
	return proxy.Err{
		Code: http.StatusForbidden,
		Msg: fmt.Sprintf("токен %q не даёт права на %s (права токена: %s)",
			g.tok.ID, what, g.tok.DescribeScope()),
	}
}

// Model — прямой запрос к модели.
func (g tokGate) Model(provider, short string) error {
	if tokenAllows(g.tok, provider, short) {
		return nil
	}
	return g.denied(fmt.Sprintf("модель %s/%s", provider, short))
}

// Alias — запрос через баланс-алиас: права есть, если алиас явно выдан в
// области models токена (голое имя в списке — выдача пула по имени; участники
// напрямую при этом не светятся), или область покрывает КАЖДОГО участника
// группы (round-robin вправе отправить запрос любому). Сообщение ошибки
// называет алиас и первого не покрытого участника.
func (g tokGate) Alias(name string, members []string) error {
	if g.tok.Scope == store.ScopeModels && slices.Contains(g.tok.Models, name) {
		return nil
	}
	for _, ref := range members {
		p, m, ok := strings.Cut(ref, "/")
		if !ok || !tokenAllows(g.tok, p, m) {
			return g.denied(fmt.Sprintf("баланс-алиас %q (участник %s вне области)", name, ref))
		}
	}
	return nil
}

// tokenAllows — права scope-токена на модель запроса (provider + short из
// тела). provider — совпадение провайдера; model — провайдер и форма модели
// (modelFormMatches); models — совпадение любой записи списка, в том числе
// wildcard-записи «provider/*» (весь хост целиком).
func tokenAllows(tok store.Token, provider, short string) bool {
	switch tok.Scope {
	case store.ScopeProvider:
		return provider == tok.Provider
	case store.ScopeModel:
		return provider == tok.Provider && modelFormMatches(provider, tok.Model, short)
	case store.ScopeModels:
		return modelsScopeAllows(tok.Models, provider, short)
	}
	return false
}

// modelFormMatches — совпадение модели запроса с записанной в область:
// точная форма (имя/алиас/путь как записан) или алиас↔путь через кэш
// авто-алиасов (без хода в бэкенд: с холодным кэшем работает точная форма —
// обычный случай, клиенты берут модель из /v1/models).
func modelFormMatches(provider, scopeModel, short string) bool {
	if short == scopeModel {
		return true
	}
	if target, ok := proxy.AliasTarget(provider, short); ok && target == scopeModel {
		return true
	}
	if target, ok := proxy.AliasTarget(provider, scopeModel); ok && target == short {
		return true
	}
	return false
}

// modelsScopeAllows — область models: список полных форм provider/model
// (имя, алиас или путь), wildcard-записей «provider/*» (весь хост) и голых
// имён баланс-алиасов (те к прямым запросам отношения не имеют — права
// только через алиас); совпадение любой записи.
func modelsScopeAllows(refs []string, provider, short string) bool {
	for _, ref := range refs {
		p, m, ok := strings.Cut(ref, "/")
		if !ok || p != provider {
			continue // голое имя — баланс-алиас, не прямая модель
		}
		if m == store.HostWildcard {
			return true // весь провайдер — любая его модель
		}
		if modelFormMatches(p, m, short) {
			return true
		}
	}
	return false
}

// modelFilter — фильтр списка /v1/models по правам scope-токена: provider —
// модели своего провайдера; model — ровно своя запись (по ref или raw-пути,
// если область записана путём); models — любая из записей области, включая
// wildcard «provider/*» (весь хост); баланс-алиасы — область покрывает всех
// участников (как в tokGate). nil — без фильтра (админ, all).
func (s *Server) modelFilter(r *http.Request) func(proxy.ModelEntry) bool {
	tok := requestToken(r)
	if tok.Unlimited() {
		return nil
	}
	return func(e proxy.ModelEntry) bool {
		if e.Balance {
			// явная выдача алиаса в области models или покрытие всех участников
			return (tok.Scope == store.ScopeModels && slices.Contains(tok.Models, e.ID)) ||
				balanceCovered(tok, e.Members)
		}
		switch tok.Scope {
		case store.ScopeProvider:
			return e.Provider == tok.Provider
		case store.ScopeModel:
			return e.Provider == tok.Provider && (e.ID == tok.Ref() || e.Raw == tok.Model)
		case store.ScopeModels:
			return modelsScopeEntry(tok.Models, e)
		}
		return false
	}
}

// balanceCovered — область токена покрывает каждого участника
// баланс-алиаса (round-robin вправе отправить запрос любому из них);
// пустой список участников — алиас несостоятелен, не показываем.
func balanceCovered(tok store.Token, members []string) bool {
	if len(members) == 0 {
		return false
	}
	for _, ref := range members {
		p, m, ok := strings.Cut(ref, "/")
		if !ok || !tokenAllows(tok, p, m) {
			return false
		}
	}
	return true
}

// modelsScopeEntry — запись списка /v1/models входит в область models:
// «provider/*» накрывает весь провайдер, иначе — совпадение формы записи
// (ID — та же ссылка, что в области) либо raw-путь алиаса (область может
// быть записана путём). Голые имена — баланс-алиасы: они обрабатываются
// веткой Balance и сюда не доходят.
func modelsScopeEntry(refs []string, e proxy.ModelEntry) bool {
	for _, ref := range refs {
		p, m, ok := strings.Cut(ref, "/")
		if !ok {
			continue // баланс-алиас — не прямая модель
		}
		if m == store.HostWildcard {
			if p == e.Provider {
				return true
			}
			continue
		}
		if e.ID == ref {
			return true
		}
		if e.Raw != "" && ref == e.Provider+"/"+e.Raw {
			return true
		}
	}
	return false
}

// Upstream / ProviderNames — реализация proxy.ProviderSource поверх store.
// Разрешение авто-алиасов (имя файла → путь) делает proxy через кэш.
func (s *Server) Upstream(provider string) (proxy.Upstream, bool) {
	p, ok := s.st.Provider(provider)
	if !ok {
		return proxy.Upstream{}, false
	}
	return proxy.Upstream{BaseURL: p.BaseURL, APIKey: p.APIKey}, true
}

func (s *Server) ProviderNames() []string { return s.st.ProviderNames() }

// ModelCaps — реализация proxy.ModelCapsSource поверх кэша моделей:
// capabilities модели для systemone-fallback (без ensureLoaded — холодный
// кэш читается как unknown, см. proxyModelsCache.Caps).
func (s *Server) ModelCaps(provider, short string) ([]string, bool) {
	return s.mc.Caps(provider, short)
}

// Balance — участники баланс-алиаса (proxy.BalanceSource поверх store;
// ищет proxy.Chat при голом имени модели без «/»).
func (s *Server) Balance(name string) ([]string, bool) { return s.st.Balance(name) }

// balanceEntries — записи баланс-алиасов для списков моделей (/v1/models и
// /admin/api/models): ID — голое имя алиаса (без префикса провайдера — так
// его и видно в поле model запроса), members — участники полными формами,
// capabilities — пересечение кап участников из кэша (гарантия: если в списке
// капа есть — любую модель пула можно звать с ней); участник без записи в
// кэше (провайдер умер) пересечения не сужает — его капы неизвестны.
// Порядок — как в конфиге; вызывается с полным снимком кэша (по нему и капы).
func (s *Server) balanceEntries(entries []proxy.ModelEntry) []proxy.ModelEntry {
	balances := s.st.Balances()
	if len(balances) == 0 {
		return nil
	}
	caps := make(map[string]map[string]bool, len(entries))
	for _, e := range entries {
		if e.Balance || len(e.Capabilities) == 0 {
			continue
		}
		set := make(map[string]bool, len(e.Capabilities))
		for _, c := range e.Capabilities {
			set[c] = true
		}
		caps[e.ID] = set
	}
	out := make([]proxy.ModelEntry, 0, len(balances))
	for _, b := range balances {
		var inter map[string]bool
		for _, ref := range b.Models {
			set, ok := caps[ref]
			if !ok {
				continue // участника нет в кэше — капы неизвестны, не сужаем
			}
			if inter == nil {
				inter = make(map[string]bool, len(set))
				for c := range set {
					inter[c] = true
				}
				continue
			}
			for c := range inter {
				if !set[c] {
					delete(inter, c)
				}
			}
		}
		var capsList []string
		for c := range inter {
			capsList = append(capsList, c)
		}
		sort.Strings(capsList)
		out = append(out, proxy.ModelEntry{
			ID:           b.Name,
			Object:       "model",
			Capabilities: capsList,
			Balance:      true,
			Members:      append([]string(nil), b.Models...),
		})
	}
	return out
}

// cookieToken — значение админ-cookie (пусто, если нет).
func cookieToken(r *http.Request) string {
	c, err := r.Cookie(AdminCookie)
	if err != nil {
		return ""
	}
	return c.Value
}
