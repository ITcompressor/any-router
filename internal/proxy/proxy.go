// Package proxy — проброс /v1-запросов на OpenAI-совместимые бэкенды
// (задача 3 PRD): тело с моделью вида provider/model перезаписывается
// на голое имя модели, ответ (включая SSE-стриминг) течёт клиенту чанками.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ITcompressor/any-router/internal/logging"
)

// Upstream — куда и с чем ходить: baseurl провайдера (с /v1 на конце)
// и опциональный apikey апстрима.
type Upstream struct {
	BaseURL string
	APIKey  string
}

// maxBody — предел на чтение тела запроса (LLM-сообщения бывают большими,
// но не бесконечными).
const maxBody = 64 << 20

// upstreamClient — HTTP-клиент для апстримов: таймауты на соединение
// и заголовки (холодный старт llama-сервера занимает минуты), тела стрима
// без таймаута — его гонит контекст запроса клиента.
var upstreamClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	},
}

// WriteError пишет JSON-ошибку в формате OpenAI (error.message).
func WriteError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": fmt.Sprintf(format, args...), "type": "any_router_request_error"},
	})
}

// SplitModel разбирает "provider/model"; ok=false — модель не вида provider/model.
func SplitModel(model string) (provider, short string, ok bool) {
	provider, short, ok = strings.Cut(model, "/")
	return provider, short, ok && provider != "" && short != ""
}

// Gate — права API-токена на модель запроса (реализует web.scopeGate):
// прямой запрос к модели — Model; запрос через баланс-алиас — Alias
// (участники группы передаются целиком: право на алиас есть, только когда
// область токена покрывает каждого — round-robin вправе отправить запрос
// любому из участников).
type Gate interface {
	Model(provider, short string) error
	Alias(name string, members []string) error
}

// BalanceSource — доступ к баланс-алиасам из proxy (реализует web.Server
// поверх store): Balance отдаёт участников алиаса полными формами
// provider/model. Отсутствие реализации (старые тесты, mock'и) — голые
// имена отвергаются 400-й, как раньше.
type BalanceSource interface {
	Balance(name string) ([]string, bool)
}

// ─── баланс-алиасы: round-robin по участникам группы ───
// Запрос на голое имя (без «/»), известное конфигу как баланс-алиас,
// уходит одному из участников; счётчик вращений — у каждого алиаса свой,
// запросы распределяются равномерно. Порядок попыток строится от текущего
// значения счётчика по кругу — это же готовый порядок фейловера.

var balanceRR sync.Map // имя алиаса → *atomic.Uint64 (счётчик запросов)

// ResetBalanceRR — сброс счётчиков round-robin (для тестов).
func ResetBalanceRR() {
	balanceRR = sync.Map{}
}

// balanceCandidates — порядок попыток по участникам алиаса: начиная с
// значения счётчика и по кругу. Один вызов Chat — один инкремент: фейловер
// не балансируется, а просто идёт по кругу от первой попытки.
func balanceCandidates(name string, members []string) []string {
	any, _ := balanceRR.LoadOrStore(name, new(atomic.Uint64))
	start := any.(*atomic.Uint64).Add(1) - 1
	n := uint64(len(members))
	out := make([]string, 0, len(members))
	for i := uint64(0); i < n; i++ {
		out = append(out, members[int((start+i)%n)])
	}
	return out
}

// ModelRef — имя модели в форме провайдера. Path-модели (llama.cpp без
// --alias отдаёт абсолютный путь к gguf как id) — без двойного слэша:
// разбиение по первому "/" в любом случае отделяет провайдера верно,
// а форма "pcbig//opt/..." вводит в заблуждение.
func ModelRef(provider, id string) string {
	if strings.HasPrefix(id, "/") {
		return provider + id
	}
	return provider + "/" + id
}

// ProviderSource — доступ провайдеров из proxy (реализует web.Server):
// Upstream выбирает апстрим по имени; ProviderNames — имена для ошибок
// и списков моделей.
type ProviderSource interface {
	Upstream(provider string) (Upstream, bool)
	ProviderNames() []string
}

// ─── авто-алиасы path-моделей (llama.cpp без --alias) ───
// id бэкенда — путь к gguf; алиас = имя файла без расширения. Соответствие
// «алиас → путь» роутер узнаёт из /v1/models бэкенда и кэширует (TTL),
// иначе каждый chat-запрос ходил бы за списком.

const aliasTTL = 5 * time.Minute

// aliasRec — запись кэша: цель (путь/id) и время появления.
type aliasRec struct {
	target string
	at     time.Time
}

var (
	aliasMu    sync.RWMutex
	aliasCache = map[string]aliasRec{}
)

// ResetAliasCache — сброс кэша авто-алиасов (для тестов).
func ResetAliasCache() {
	aliasMu.Lock()
	aliasCache = map[string]aliasRec{}
	aliasMu.Unlock()
}

func aliasKey(provider, alias string) string {
	return provider + "\x00" + alias
}

// IsPathID — ид модели — путь на бэкенде (llama.cpp без --alias).
func IsPathID(id string) bool {
	return strings.HasPrefix(id, "/")
}

// PathAlias — авто-алиас пути: basename без расширения
// ("/opt/m/Qwen3.8.gguf" → "Qwen3.8").
func PathAlias(pathID string) string {
	base := filepath.Base(pathID)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// aliasLookup — цель для алиаса из кэша (срок не истёк).
func aliasLookup(provider, alias string) (string, bool) {
	aliasMu.RLock()
	rec, ok := aliasCache[aliasKey(provider, alias)]
	aliasMu.RUnlock()
	if !ok || time.Since(rec.at) > aliasTTL {
		return "", false
	}
	return rec.target, true
}

// aliasPopulate — синк кэша авто-алиасов по снимку списка моделей бэкенда:
// успешный список — авторитетный снимок (модель удалили — запись кэша
// улетает с ним же, добавили — появляется сразу). Path-модели — алиас
// (имя файла без расширения) → путь; именные — identity (чтобы не ходить
// за списком на каждый чат). Просроченные записи провайдера чистятся
// (иначе мапа растёт вечно).
func aliasPopulate(provider string, ids []string) {
	aliasMu.Lock()
	defer aliasMu.Unlock()
	now := time.Now()
	keep := map[string]bool{}
	for _, id := range ids {
		alias := id
		if IsPathID(id) {
			if PathAlias(id) == "" {
				continue
			}
			alias = PathAlias(id)
		}
		if alias == "" {
			continue
		}
		key := aliasKey(provider, alias)
		aliasCache[key] = aliasRec{target: id, at: now}
		keep[key] = true
	}
	for k, rec := range aliasCache {
		if strings.HasPrefix(k, provider+"\x00") && (time.Since(rec.at) > aliasTTL || !keep[k]) {
			delete(aliasCache, k)
		}
	}
}

// AliasTarget — цель алиаса из кэша (алиас/имя → id/путь бэкенда), без хода
// в бэкенд: на miss вернёт ok=false. Нужен проверкам прав API-токенов на
// модель — совпадение алиасной и путевой формы одной модели (см. web).
func AliasTarget(provider, alias string) (string, bool) {
	return aliasLookup(provider, alias)
}

// resolveModel — модель для апстрима: путь — как есть; короткое имя —
// алиас из кэша, при miss — один fetch /v1/models и повторный поиск;
// иначе имя как есть (апстрим сам отвергнет, если не знает).
// Кэш: TTL 5 мин, и любой свежий список моделей (chat-fetch, /v1/models,
// админка) его перезаписывает — удалённые модели живут в кэше не дольше.
func resolveModel(ctx context.Context, res ProviderSource, provider string, up Upstream, short string) string {
	if IsPathID(short) {
		// путь-модель (старые формы с полным путём) — без изменений
		return short
	}
	if target, ok := aliasLookup(provider, short); ok {
		return target
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ids, err := BackendModels(cctx, up)
	cancel()
	if err != nil {
		return short
	}
	aliasPopulate(provider, ids)
	if target, ok := aliasLookup(provider, short); ok {
		return target
	}
	return short
}

// ─── липкая маршрутизация (affinity) ───
// Разговор, уже ходивший на участника алиаса, продолжает ходить на него же:
// апстрим держит кэш промпта/KV-кэш — переключение участника между ходами
// цепочки его сбрасывает. Отпечаток беседы: заголовок X-Session-Id (сильнее,
// работает для всех путей, включая systemone) или хеш первых двух сообщений
// chat-тела (вся цепочка одной беседы начинается с них — отпечаток стабилен
// от хода к ходу). Привязки в памяти: TTL на разговор + потолок количества
// (переполнение — самые старые выбрасываются); фейловер перезакрепляет
// беседу за тем, кто реально ответил; выпавший из группы участник — привязка
// сбрасывается (round-robin заново).

// SessionHeader — заголовок клиента с id сессии/разговора (для липкости).
const SessionHeader = "X-Session-Id"

const (
	stickyTTL        = 30 * time.Minute // срок жизни привязки беседы к участнику
	stickyMaxEntries = 4096             // потолок привязок на один алиас
)

var (
	stickyMu    sync.Mutex
	stickyCache = map[string]map[string]stickyRec{} // алиас → отпечаток → участник
)

type stickyRec struct {
	member string
	at     time.Time
}

// ResetStickyCache — очистка липких привязок (для тестов).
func ResetStickyCache() {
	stickyMu.Lock()
	stickyCache = map[string]map[string]stickyRec{}
	stickyMu.Unlock()
}

// fingerprintKeys — ключи липкости ходов: [0] — собственный отпечаток хода
// (под ним беседа запоминается), далее — унаследованный ключ открытия цепочки
// (ход после «одиночного» сообщения-открытия ищет беседу, запомнившуюся по
// первому сообщению). X-Session-Id — всегда один ключ; поле messages не
// массив/пусто (completions, embeddings, systemone без заголовка) — ключей
// нет (round-robin).
func fingerprintKeys(r *http.Request, payload map[string]any) []string {
	if sid := r.Header.Get(SessionHeader); sid != "" {
		return []string{"S:" + sid}
	}
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil
	}
	if len(msgs) > 2 {
		msgs = msgs[:2]
	}
	keys := []string{hashOf(msgs)}
	if len(msgs) > 1 {
		keys = append(keys, hashOf(msgs[:1]))
	}
	return keys
}

// hashOf — стабильный хеш JSON части тела (8 байт sha256, префикс «F:»).
func hashOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "F:" + hex.EncodeToString(sum[:8])
}

// hashOf — стабильный хеш JSON части тела (8 байт sha256, префикс «F:»).

// stickyRoute — участник алиаса, к которому беседа привязана (первый ключ
// с hit'ом; TTL не истёк, участник ещё в актуальной группе). ok=false —
// round-robin.
func stickyRoute(alias string, fps []string, members []string) (string, bool) {
	for _, fp := range fps {
		member, ok := stickyLookup(alias, fp, members)
		if ok {
			return member, true
		}
	}
	return "", false
}

// stickyLookup — одна попытка по ключу: TTL не истёк и участник ещё в группе.
func stickyLookup(alias, fp string, members []string) (string, bool) {
	if fp == "" {
		return "", false
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	m, ok := stickyCache[alias]
	if !ok {
		return "", false
	}
	rec, ok := m[fp]
	if !ok || time.Since(rec.at) > stickyTTL {
		return "", false
	}
	for _, ref := range members {
		if ref == rec.member {
			return rec.member, true
		}
	}
	delete(m, fp) // участник выбыл из группы — привязка не имеет смысла
	return "", false
}

// stickyRemember — закрепление беседы за обслужившим участником; при
// переполнении — выбрасывается половина самых старых записей (просроченные
// улетают вместе с ними).
func stickyRemember(alias, fp, member string) {
	if fp == "" || member == "" {
		return
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	m, ok := stickyCache[alias]
	if !ok {
		m = map[string]stickyRec{}
		stickyCache[alias] = m
	}
	if _, exists := m[fp]; !exists && len(m) >= stickyMaxEntries {
		type kv struct {
			fp string
			at time.Time
		}
		all := make([]kv, 0, len(m))
		for k, v := range m {
			all = append(all, kv{k, v.at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for i, e := range all {
			if i >= len(all)/2 {
				break
			}
			delete(m, e.fp)
		}
	}
	m[fp] = stickyRec{member: member, at: time.Now()}
}

// Chat пересылает JSON-запрос (chat/completions, completions, embeddings,
// systemone) на апстрим. Модель из тела — либо "provider/model" (прямой
// запрос: выбор провайдера и перезапись модели без префикса), либо голое
// имя без «/» — баланс-алиас (липкость беседы + round-robin по участникам
// группы с фейловером на недоступности апстрима). Ответ копируется
// стримингом (SSE-чанки отдаются сразу). allow — необязательная проверка прав API-токена (Gate) ДО
// маршрутизации: прямой запрос — Model, алиас — Alias; не-nil ошибка
// (proxy.Err с HTTP-кодом, см. web.scopeGate) отклоняет запрос; nil — проверка
// не нужна (админ, API-токен области all).
func Chat(w http.ResponseWriter, r *http.Request, res ProviderSource, log *logging.Logger,
	allow Gate, tokenID string, rec CallSink) error {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return errBad("не читается тело запроса: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return errBad("тело не JSON: %v", err)
	}
	model, _ := payload["model"].(string)
	if model == "" {
		return errBad("в теле нет поля model")
	}
	provider, short, ok := SplitModel(model)
	aliasName, members := "", []string(nil)
	if !ok {
		// голое имя (без «/»): баланс-алиас из конфига — или прежний 400
		if bs, isBal := res.(BalanceSource); isBal {
			if ms, found := bs.Balance(model); found && len(ms) > 0 {
				aliasName, members = model, ms
			}
		}
		if aliasName == "" {
			return errBad("модель %q не вида provider/model и не баланс-алиас — /v1/models подскажет допустимые", model)
		}
	}
	// Права токена — до всего остального: единообразный 403 на чужие области,
	// без раскрытия знания про провайдеров (неизвестный провайдер тоже 403).
	if allow != nil {
		if aliasName != "" {
			if err := allow.Alias(aliasName, members); err != nil {
				return err
			}
		} else if err := allow.Model(provider, short); err != nil {
			return err
		}
	}
	// Call для экспорта (langfuse) — только когда запрос реально маршрутизируется:
	// 400 (кривое тело) и 403 (вне прав) — не экспортируем. Копия payload — плоская
	// (Input); тело для попыток — отдельные копии (flavor-чистка удаляет ключи),
	// вложенные поля (messages, в т.ч. оригинальный payload) не мутируются.
	// Для алиаса Model — голое имя, как прислал клиент; Provider уточняется
	// выбранным участником попытки (round-robin), UpstreamModel — ушедшая модель.
	var call *Call
	if rec != nil {
		ref := model
		if aliasName == "" {
			ref = ModelRef(provider, short)
		}
		call = &Call{
			IP:      ClientIP(r),
			TokenID: tokenID,
			Model:   ref,
			Alias:   aliasName,
			Path:    r.URL.Path,
			Stream:  payload["stream"] == true,
			Started: start,
			Input:   maps.Clone(payload),
		}
		defer func() {
			call.End = time.Now()
			rec.RecordCall(*call)
		}()
	}
	// Порядок попыток (провайдер, модель): прямое обращение — одна (как
	// раньше); алиас — липкая привязка беседы (affinity, кэш апстрима) или
	// round-robin по участникам от общего счётчика; остальные участники —
	// по кругу (фейловер). Кривые записи конфига (не provider/model)
	// пропускаются; провайдер, удалённый из конфига, выкидывает попытку
	// участника — алиас отвечает и на частично живой группе.
	type attempt struct{ provider, short string }
	candidates := make([]attempt, 0, 1)
	var fp []string
	if aliasName == "" {
		candidates = append(candidates, attempt{provider, short})
	} else {
		fp = fingerprintKeys(r, payload)
		var order []string
		if sticky, ok := stickyRoute(aliasName, fp, members); ok {
			order = append(order, sticky) // беседа уже на участнике — продолжаем на нём
		}
		for _, ref := range balanceCandidates(aliasName, members) {
			if !slices.Contains(order, ref) {
				order = append(order, ref)
			}
		}
		for _, ref := range order {
			if p, s, okc := SplitModel(ref); okc {
				candidates = append(candidates, attempt{p, s})
			}
		}
		if len(candidates) == 0 {
			return errBad("баланс-алиас %q не содержит валидных участников (форма provider/model)", aliasName)
		}
	}
	var lastErr error
	for _, c := range candidates {
		up, known := res.Upstream(c.provider)
		if !known {
			lastErr = errBad("провайдер %q не найден (есть: %s)", c.provider, strings.Join(orNone(res.ProviderNames()), ", "))
			if aliasName == "" {
				return lastErr
			}
			continue // участник алиаса с мёртвым/удалённым провайдером — пропускаем
		}
		var capd *capture
		if call != nil {
			capd = &capture{} // захват тела апстрима для экспорта — только под rec
			call.Provider, call.Failed, call.UpstreamModel = c.provider, "", ""
		}
		// Авто-алиас: "pcbig/Qwen3.8-27B" → "/opt/llm/models/…gguf" (см. файл).
		resolved := resolveModel(r.Context(), res, c.provider, up, c.short)
		// у алиаса UpstreamModel всегда: по ней в langfuse видно, какой именно
		// участник обслужил (c.Model — голое имя алиаса, не путается)
		if call != nil && (aliasName != "" || resolved != c.short) {
			call.UpstreamModel = resolved
		}
		// тело попытки — своя копия payload: model перезаписана, flavor-чистка
		// удаляет ключи — попытка не должна видеть чистку прошлой попытки
		attemptBody := maps.Clone(payload)
		attemptBody["model"] = resolved
		log.Debugf("chat: %s → %s (модель %s)", c.provider, up.BaseURL, attemptBody["model"])
		// flavor-конвертеры: строгие апстримы (mistral) 422-ят незнакомые
		// поля — тело чатится под диалект апстрима (см. flavor.go)
		sanitizeFlavorRequest(FlavorOf(up.BaseURL), strings.TrimPrefix(r.URL.Path, "/v1"), attemptBody, log)
		rewritten, err := json.Marshal(attemptBody)
		if err != nil {
			if call != nil {
				call.Failed = fmt.Sprintf("тело не переименовывается: %v", err)
			}
			return errBad("тело не переименовывается: %v", err)
		}
		if err := forward(w, r, up, rewritten, capd); err != nil {
			if call != nil {
				call.Failed = err.Error()
			}
			lastErr = err
			if aliasName == "" {
				return err
			}
			continue // фейловер: следующая модель группы
		}
		if capd != nil {
			capd.finish(call)
		}
		if aliasName != "" && len(fp) > 0 {
			// беседа закрепляется за обслужившим участником: следующие ходы
			// цепочки пойдут на него же (переиспользуется кэш апстрима);
			// после фейловера — перезакрепление за реально ответившим
			stickyRemember(aliasName, fp[0], ModelRef(c.provider, c.short))
		}
		return nil
	}
	// у алиаса все попытки провалены/пропущены — наружу последняя ошибка
	return lastErr
}

// Err — HTTP-ошибка маршрутизации (бэд-боди → 400, недоступный апстрим → 502).
type Err struct {
	Code int
	Msg  string
}

func (e Err) Error() string { return e.Msg }

func errBad(format string, args ...any) Err {
	return Err{Code: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

func errUp(format string, args ...any) Err {
	return Err{Code: http.StatusBadGateway, Msg: fmt.Sprintf(format, args...)}
}

// writeJSON пишет JSON-ответ с заданным кодом.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// orNone — для сообщений об ошибках: пустой список → "(нет)".
func orNone(list []string) []string {
	if len(list) == 0 {
		return []string{"(нет)"}
	}
	return list
}

// BackendModel — модель бэкенда: голый id, capabilities и meta (если бэкенд
// их отдаёт: llama.cpp прикладывает к /v1/models массив models и meta —
// n_ctx (серверный -c) и n_ctx_train).
type BackendModel struct {
	ID           string
	Capabilities []string
	Meta         map[string]any
}

// BackendModels — GET {baseurl}/models у провайдера: список голых id моделей.
// Используется resolveModel и CLI-командой models.
func BackendModels(ctx context.Context, up Upstream) ([]string, error) {
	bms, err := BackendModelsFull(ctx, up)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(bms))
	for _, bm := range bms {
		ids = append(ids, bm.ID)
	}
	return ids, nil
}

// BackendModelsFull — то же с capabilities и meta: llama.cpp в /v1/models
// кладёт массив models [{name, model, capabilities}] и meta с размером
// контекста (n_ctx/n_ctx_train) — capabilities мапятся на id по name и по
// model, meta переписывается как есть; ollama и openai-реле в списке
// их не дают — Collector доспрашивает через ShowModelInfo.
func BackendModelsFull(ctx context.Context, up Upstream) ([]BackendModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(up.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	applyAuth(req, up)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("статус %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID   string         `json:"id"`
			Meta map[string]any `json:"meta"`
			// mistral: capabilities булевым объектом + потолок контекста
			// (llama.cpp/ollama эти поля не шлют — nil/0, не мешают)
			Capabilities     map[string]any `json:"capabilities"`
			MaxContextLength int64          `json:"max_context_length"`
		} `json:"data"`
		Models []struct { // llama.cpp: capabilities поимённо
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("тело не JSON: %w", err)
	}
	f := FlavorOf(up.BaseURL)
	caps := map[string][]string{}
	for _, m := range body.Models {
		if len(m.Capabilities) == 0 {
			continue
		}
		for _, key := range []string{m.Name, m.Model} {
			if key != "" {
				caps[key] = m.Capabilities
			}
		}
	}
	bms := make([]BackendModel, 0, len(body.Data))
	for _, d := range body.Data {
		bm := BackendModel{ID: d.ID, Capabilities: caps[d.ID], Meta: d.Meta}
		switch f {
		case FlavorMistral:
			// capabilities у mistral — объектом прямо в записи списка,
			// мапятся в наши значения (completion/tools/thinking/vision/audio);
			// потолок контекста — в синтетический meta.n_ctx_train
			if lc := mistralCaps(d.ID, d.Capabilities); len(lc) > 0 {
				bm.Capabilities = lc
			}
			if d.MaxContextLength > 0 {
				bm.Meta = map[string]any{"n_ctx_train": d.MaxContextLength}
			}
		case FlavorZAI:
			// у Z.AI капы в списке не отдаются вовсе — синтез по id (docs.z.ai)
			if lc := zaiCaps(d.ID); len(lc) > 0 {
				bm.Capabilities = lc
			}
		}
		bms = append(bms, bm)
	}
	return bms, nil
}

// showTTL — срок негативного кэша «/api/show нет» (бэкенды без ollama-
// совместимого маршрута отвечают 404/405 — не долбим их заново).
const showTTL = 10 * time.Minute

// showMiss — негативный кэш: base → до какого момента /api/show не дёргать.
var showMiss sync.Map

// ResetShowMiss — сброс негативного кэша /api/show (для тестов).
func ResetShowMiss() { showMiss = sync.Map{} }

var errNoShow = errors.New("нет /api/show")

// showBase — база бэкенда без суффикса /v1 ("http://h:11434/v1" →
// "http://h:11434"): ollama-совместимые маршруты живут вне /v1.
func showBase(baseurl string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(baseurl, "/"), "/v1")
	return strings.TrimSuffix(base, "/")
}

// ShowModelInfo — сведения модели от бэкенда: ollama-совместимый
// POST {base}/api/show {"model": id} — capabilities и нативный потолок
// контекста (model_info["<arch>.context_length"]). Бэкенды без такого
// маршрута (llama.cpp meta отдаёт прямо в /v1/models, openai-реле не дают
// вовсе) отвечают 404/405 — запоминаем на showTTL, дальше не спрашиваем
// (errNoShow).
type ShowModel struct {
	Capabilities  []string
	ContextLength int64
}

func ShowModelInfo(ctx context.Context, up Upstream, model string) (*ShowModel, error) {
	base := showBase(up.BaseURL)
	if at, ok := showMiss.Load(base); ok {
		if t, ok := at.(time.Time); ok && time.Now().Before(t) {
			return nil, errNoShow
		}
		showMiss.Delete(base)
	}
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, up)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		showMiss.Store(base, time.Now().Add(showTTL))
		return nil, errNoShow
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("статус %d", resp.StatusCode)
	}
	var body struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("тело не JSON: %w", err)
	}
	return &ShowModel{
		Capabilities:  body.Capabilities,
		ContextLength: contextLength(body.ModelInfo),
	}, nil
}

// contextLength — нативный потолок контекста из model_info /api/show:
// ключ "<arch>.context_length" (архитектура в general.architecture; если
// архитектура не указана — ищем любой *.context_length).
func contextLength(modelInfo map[string]any) int64 {
	num := func(v any) (int64, bool) {
		f, ok := v.(float64)
		if !ok || f <= 0 {
			return 0, false
		}
		return int64(f), true
	}
	if arch, ok := modelInfo["general.architecture"].(string); ok && arch != "" {
		if n, ok := num(modelInfo[arch+".context_length"]); ok {
			return n
		}
	}
	for k, v := range modelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := num(v); ok {
				return n
			}
		}
	}
	return 0
}

// applyAuth — апстриму идёт apikey провайдера (или ничего); токен клиента
// наружу не утекает.
func applyAuth(req *http.Request, up Upstream) {
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
}

// forward строит запрос на апстрим (baseurl + путь после /v1), копирует
// заголовки и стримингово отдаёт ответ клиенту. cap — захват тела для
// экспорта (nil — без захвата: overhead записи в буфер не нужен).
func forward(w http.ResponseWriter, r *http.Request, up Upstream, body []byte, capd *capture) error {
	rest := strings.TrimPrefix(r.URL.Path, "/v1")
	url := strings.TrimSuffix(up.BaseURL, "/") + rest
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, strings.NewReader(string(body)))
	if err != nil {
		return errBad("не собрать запрос апстриму: %v", err)
	}
	req.ContentLength = int64(len(body))
	// тело всегда JSON (маршруты /v1), но клиента-заголовок не пересылаем —
	// ставим сами: без Content-Type строгие апстримы (Mistral) парсят тело
	// как строку → 422 «Input should be a valid dictionary or object»
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, up)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return errUp("апстрим %s недоступен: %v", up.BaseURL, err)
	}
	if capd != nil {
		capd.begin(resp)
		w = teeResponseWriter{ResponseWriter: w, cap: capd}
	}
	copyResponse(w, resp, FlavorOf(up.BaseURL))
	return nil
}

// hopHeaders — hop-by-hop заголовки, через проксю не живущие (RFC 7230 §6.1);
// ключи в канонической MIME-форме.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

// copyResponse — статус + фильтрованные заголовки + тело апстрима клиенту.
// SSE/стримы: каждый чанк отдаётся сразу (flush), чтобы клиент видел
// генерацию в реальном времени.
// sortedAliasNames — имена алиасов по алфавиту (детерминированный вывод).
func sortedAliasNames(aliases map[string]string) []string {
	out := make([]string, 0, len(aliases))
	for name := range aliases {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ModelEntry — модель в списке: ID — форма для поля model запроса
// ("provider/model" или "provider/alias"), Raw — целевой id/путь бэкенда
// (для алиасов), Capabilities — возможности модели (llama.cpp — passthrough
// из /v1/models, ollama-совместимые — из /api/show; у реле — пусто),
// Meta — llama.cpp-оверрайд с контекстом (llama.cpp — passthrough n_ctx/
// n_ctx_train, ollama-совместимые — синтез n_ctx_train из /api/show).
type ModelEntry struct {
	ID           string         `json:"id"`
	Object       string         `json:"object"`
	Provider     string         `json:"provider,omitempty"`
	Raw          string         `json:"raw,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
	Balance      bool           `json:"balance,omitempty"`
	Members      []string       `json:"members,omitempty"`
}

// collectProvider — модели одного провайдера (формат — см. ModelEntry):
// у path-моделей ref — авто-алиас, raw — целевой путь; capabilities/meta —
// passthrough из /v1/models или доспрос /api/show. Сбой — ошибка (решает
// вызывающий: пропустить — CollectModels, сохранить прежний снимок — кэш).
func collectProvider(ctx context.Context, res ProviderSource, name string) ([]ModelEntry, error) {
	up, known := res.Upstream(name)
	if !known {
		return nil, fmt.Errorf("провайдер %s не в конфиге", name)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	bms, err := BackendModelsFull(cctx, up)
	cancel()
	if err != nil {
		return nil, err
	}
	// Синк кэша авто-алиасов: любой свежий список (цикл кэша, кнопка
	// «обновить модели» в админке) перезаписывает мапу — добавленные
	// модели подхватываются сразу, удалённые вылетают с TTL.
	ids := make([]string, 0, len(bms))
	for _, bm := range bms {
		ids = append(ids, bm.ID)
	}
	aliasPopulate(name, ids)
	// Бюджет на допрос capabilities всех моделей провайдера (один ctx
	// на всех — суммарно не дольше, чем на сам список).
	pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
	defer pcancel()
	out := make([]ModelEntry, 0, len(bms))
	for _, bm := range bms {
		id := bm.ID
		ref := id
		raw := ""
		if IsPathID(id) {
			// path-модель: авто-алиас = имя файла без расширения
			ref = PathAlias(id)
			raw = id
		}
		if ref == "" {
			continue
		}
		caps := bm.Capabilities
		meta := bm.Meta
		if len(caps) == 0 || len(meta) == 0 {
			// llama.cpp-style capabilities/meta нет в списке — доспрашиваем
			// /api/show (ollama отвечает быстро; llama.cpp и реле — 404,
			// дальше уже не спрашиваем): capabilities + нативный потолок
			// контекста (уходит в синтетический meta.n_ctx_train)
			if info, err := ShowModelInfo(pctx, up, id); err == nil && info != nil {
				if len(caps) == 0 && len(info.Capabilities) > 0 {
					caps = info.Capabilities
				}
				if len(meta) == 0 && info.ContextLength > 0 {
					meta = map[string]any{"n_ctx_train": info.ContextLength}
				}
			}
		}
		out = append(out, ModelEntry{
			ID:           ModelRef(name, ref),
			Object:       "model",
			Provider:     name,
			Raw:          raw,
			Capabilities: canonCaps(caps),
			Meta:         meta,
		})
	}
	return out, nil
}

// canonCaps — нормализация capabilities модели в точке сборки ModelEntry:
// «multimodal» влечёт «tools». Почему: llama.cpp с mmproj-проектором
// (Qwen3.8/VL-семейство) в /v1/models отдаёт [«completion», «multimodal»]
// и не репортит «tools» (детект Jinja зависит от флагов запуска), хотя
// чат-модели с jinja-шаблоном заведомо умеют tool-calls. Без дописывания
// потребители фильтра «completion+tools» (ralph, вкладка «Админка») зря
// отбраковывают работающие модели. Остальное — passthrough без дедупа из
// пейлода (dedupe тут же).
func canonCaps(caps []string) []string {
	if len(caps) == 0 {
		return caps
	}
	seen := make(map[string]bool, len(caps)+1)
	out := make([]string, 0, len(caps)+1)
	for _, c := range caps {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if seen["multimodal"] && !seen["tools"] {
		seen["tools"] = true
		out = append(out, "tools")
	}
	return out
}

// CollectModels — модели всех провайдеров (или только only) в одном списке:
// у провайдера с алиасами — только алиасы, сырые id — если алиасов нет.
// Недоступный провайдер логгируется и пропускается. Движок разового опроса
// (CLI models, обновление кэша списка — см. ModelsCache).
func CollectModels(ctx context.Context, res ProviderSource, log *logging.Logger, only string) []ModelEntry {
	out := make([]ModelEntry, 0, 64)
	names := res.ProviderNames()
	sort.Strings(names)
	for _, name := range names {
		if only != "" && name != only {
			continue
		}
		entries, err := collectProvider(ctx, res, name)
		if err != nil {
			log.Warnf("models: провайдер %s недоступен: %v", name, err)
			continue
		}
		out = append(out, entries...)
	}
	return out
}

// Models — веб-версия /v1/models: entries — снимок кэша списка моделей
// (первичная полная загрузка при старте serve, далее раз в 10 минут — см.
// ModelsCache; провайдеры на каждый запрос не опрашиваются), allow —
// необязательный фильтр по правам scope-токена (nil — без фильтра). Пустой
// список под фильтром — это 200 (область токена не обязана что-то совпадать
// с тем, что отдали провайдеры); 502 «ни один провайдер не отдал модели» —
// только для нефильтрованного запроса.
func Models(w http.ResponseWriter, r *http.Request, res ProviderSource, entries []ModelEntry, allow func(ModelEntry) bool) {
	only := r.URL.Query().Get("provider")
	if only != "" {
		found := false
		for _, n := range res.ProviderNames() {
			if n == only {
				found = true
				break
			}
		}
		if !found {
			WriteError(w, http.StatusBadRequest, "провайдер %q не найден", only)
			return
		}
	}
	out := make([]ModelEntry, 0, len(entries))
	for _, e := range entries {
		if only != "" && e.Provider != only {
			continue
		}
		out = append(out, e)
	}
	if allow != nil {
		kept := make([]ModelEntry, 0, len(out))
		for _, e := range out {
			if allow(e) {
				kept = append(kept, e)
			}
		}
		out = kept
	}
	if allow == nil && only == "" && len(out) == 0 {
		WriteError(w, http.StatusBadGateway, "ни один провайдер не отдал модели (проверьте их в админке)")
		return
	}
	// llama.cpp-совместимая надстройка над стандартным списком: массив
	// models [{name, model, capabilities}] — ответ /v1/models (клиенты в монорепе project-phoenix)
	// берёт capabilities отсюда, match по model (форма provider/model).
	// name — identity на бэкенде (путь/alias для path-моделей, иначе ref).
	llama := make([]map[string]any, 0, len(out))
	for _, e := range out {
		name := e.ID
		if e.Raw != "" {
			name = e.Raw
		}
		entry := map[string]any{"name": name, "model": e.ID}
		if len(e.Capabilities) > 0 {
			entry["capabilities"] = e.Capabilities
		}
		llama = append(llama, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "models": llama})
}
