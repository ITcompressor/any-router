// Package store — конфиг-стор any-router: модель файла any-router.yaml
// (задача 2 PRD). Держит провайдеров и токены в памяти под RWMutex;
// изменения атомарно (tmp + rename) сбрасываются в YAML-файл с правами 0600
// (файл содержит токены в открытом виде). Отсутствующий файл создаётся
// при первом запуске с дефолтами: listen :8085, сгенерированный админ-токен.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Provider — OpenAI-совместимый бэкенд: baseurl включает /v1
// (например http://127.0.0.1:11434/v1 или https://api.openai.com/v1).
// Алиасы ручные не нужны: авто-алиасы для path-моделей (id = путь к gguf,
// llama.cpp без --alias) строит роутер из имени файла (см. proxy.PathAlias).
type Provider struct {
	BaseURL string `yaml:"baseurl"`
	APIKey  string `yaml:"apikey,omitempty"` // опционально: Bearer для апстрима
}

// Области доступа API-токена (yaml-поле scope). Форма админки всегда
// создаёт список моделей (конткретные модели, хосты целиком и баланс-алиасы),
// но исторические области старых конфигов продолжают работать:
const (
	ScopeAll      = "all"      // все модели всех провайдеров (админ не ограничен всегда)
	ScopeProvider = "provider" // все модели одного провайдера (yaml: provider)
	ScopeModel    = "model"    // одна модель одного провайдера (yaml: provider + model)
	ScopeModels   = "models"   // список: модели, хосты целиком и баланс-алиасы (yaml: models)
)

// HostWildcard — суффикс wildcard-записи области models («provider/*»):
// весь провайдер целиком — все его модели, наличные и будущие.
const HostWildcard = "*"

// Token — API-токен для /v1 (кроме админского) с областью доступа:
// всё (scope=all, пустые provider/model — обратная совместимость со старыми
// конфигами), провайдер (scope=provider + Provider) или модель (scope=model
// + Provider + Model — id на бэкенде: имя, авто-алиас или путь).
type Token struct {
	ID       string   `yaml:"id"`
	Token    string   `yaml:"token"`
	Scope    string   `yaml:"scope"`
	Provider string   `yaml:"provider,omitempty"` // scope=provider|model
	Model    string   `yaml:"model,omitempty"`    // scope=model: id модели без префикса провайдера
	Models   []string `yaml:"models,omitempty"`   // scope=models: полные формы provider/model
}

// Unlimited — токен без ограничений: любые модели всех провайдеров.
func (t Token) Unlimited() bool { return t.Scope == "" || t.Scope == ScopeAll }

// Ref — полная форма модели области (scope=model): рабочий вид для поля model
// запроса; у path-моделей (Model начинается с "/") — двойной слэш после
// имени провайдера, ровно как их принимает /v1.
func (t Token) Ref() string { return t.Provider + "/" + t.Model }

// DescribeScope — человекочитаемая область прав (таблица админки,
// сообщения об ошибках).
func (t Token) DescribeScope() string {
	switch {
	case t.Unlimited():
		return "все модели"
	case t.Scope == ScopeProvider:
		return "провайдер " + t.Provider
	case t.Scope == ScopeModels:
		return "модели: " + describeModels(t.Models)
	default:
		return "модель " + t.Ref()
	}
}

// describeModels — записи области models по-человечески: «provider/*» —
// весь хост, голое имя — баланс-алиас, остальные — как записаны
// (у path-моделей — «provider//path»).
func describeModels(refs []string) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch p, m, ok := strings.Cut(ref, "/"); {
		case !ok:
			parts = append(parts, "алиас "+ref)
		case m == HostWildcard:
			parts = append(parts, "весь хост "+p)
		default:
			parts = append(parts, ref)
		}
	}
	return strings.Join(parts, ", ")
}

// normalizeScope — привести область токена из yaml к корректной: пустой и
// незнакомый scope → all, некомплектные (provider без провайдера, model без
// провайдера или модели) → all; для scope=model провайдер можно не указывать
// отдельно — он в полной форме provider/model у поля model. Возвращает true,
// если запись менялась (файл перезаписывается). Ручные правки yaml не должны
// мешать старту — поэтому откат к all, а не ошибка.
func (t *Token) normalizeScope() bool {
	oldScope, oldProvider, oldModel, oldModels := t.Scope, t.Provider, t.Model, t.Models
	switch t.Scope {
	case "", ScopeAll:
		t.Scope = ScopeAll
		t.Provider, t.Model, t.Models = "", "", nil
	case ScopeProvider:
		t.Model, t.Models = "", nil
		if t.Provider == "" {
			t.Scope, t.Provider, t.Model, t.Models = ScopeAll, "", "", nil
		}
	case ScopeModel:
		t.Models = nil
		if t.Provider == "" && strings.Contains(t.Model, "/") && !strings.HasPrefix(t.Model, "/") {
			t.Provider, t.Model, _ = strings.Cut(t.Model, "/")
		}
		if t.Provider == "" || t.Model == "" {
			t.Scope, t.Provider, t.Model, t.Models = ScopeAll, "", "", nil
		}
	case ScopeModels:
		t.Provider, t.Model = "", ""
		// валидные записи: полные формы provider/model, wildcard «p/*» и голые
		// имена баланс-алиасов (алиас могла создать позже — несуществующее имя
		// инертно, но намерение ручной правки сохраняем); дубли схлопываются,
		// конкретные модели, покрытые wildcard’ом своего провайдера, канонизируются (как в validModelsScope);
		// пустой итог — область несостоятельна, откат к all
		models := make([]string, 0, len(t.Models))
		seen := map[string]bool{}
		hosts := map[string]bool{}
		for _, ref := range t.Models {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[ref] {
				continue
			}
			p, m, ok := strings.Cut(ref, "/")
			if (ok && p != "" && m != "") || !strings.Contains(ref, "/") {
				seen[ref] = true
				models = append(models, ref)
				if ok && m == HostWildcard {
					hosts[p] = true
				}
			}
		}
		if len(hosts) > 0 {
			minimal := make([]string, 0, len(models))
			for _, ref := range models {
				if p, m, ok := strings.Cut(ref, "/"); ok && m != HostWildcard && hosts[p] {
					continue // p/* покрывает эту модель — запись не нужна
				}
				minimal = append(minimal, ref)
			}
			models = minimal
		}
		if len(models) == 0 {
			t.Scope, t.Provider, t.Model, t.Models = ScopeAll, "", "", nil
		} else {
			t.Models = models
		}
	default:
		t.Scope, t.Provider, t.Model, t.Models = ScopeAll, "", "", nil
	}
	return t.Scope != oldScope || t.Provider != oldProvider || t.Model != oldModel || !equalStrings(t.Models, oldModels)
}

// equalStrings — списки строк одинаковы (и порядок, и содержимое).
func equalStrings(a, b []string) bool {
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

// Balance — баланс-алиас: групповое имя без префикса провайдера, на которое
// клиенты ссылаются как на обычную модель (голое имя в поле model — без «/»:
// форма provider/model остаётся прямым обращением, голые имена — только
// балансировка; пространство имён чистое, потому что голые имена раньше
// всегда отвергались). Запрос через алиас маршрутизируется на одного из
// участников группы (полные формы provider/model) round-robin'ом; право
// scope-токена на алиас — только при покрытии всех участников.
type Balance struct {
	Name   string   `yaml:"name"`
	Models []string `yaml:"models"` // участники: полные формы provider/model
}

// Langfuse — неблокирующий экспорт LLM-трейсов в Langfuse (админка:
// вкладка «Langfuse»). url — база инстанса (langfuse-web или облако,
// например http://10.0.0.3:3000 или https://cloud.langfuse.com);
// public_key/secret_key — ключи проекта (pk-lf-…/sk-lf-…, Settings → API Keys).
// enabled — переключатель вкл/выкл: выключает выгрузку без удаления url/ключей
// (настройки остаются в файле, включение — одним сохранением). Старые файлы
// без enabled — семантика «включено, когда заполнены url и ключи» (Load
// дозаполняет флаг явно).
// Экспорт — OTLP/JSON на {url}/api/public/otel/v1/traces (актуальный
// путь ingestion v4; legacy /api/public/ingestion deprecated).
type Langfuse struct {
	Enabled     bool     `yaml:"enabled"` // переключатель выгрузки (вкл/выкл без удаления настроек)
	URL         string   `yaml:"url"`
	PublicKey   string   `yaml:"public_key"`
	SecretKey   string   `yaml:"secret_key"`
	Environment string   `yaml:"environment"` // пометка окружения на трейсах ("production", "dev"…)
	Tags        []string `yaml:"tags"`        // дополнительные тэги трейсов (к служебным any-router/provider:*)
}

// Valid — настройки заполнены: url и оба ключа проекта
// (environment/tags — опционально; переключатель не учитывается).
func (l Langfuse) Valid() bool {
	return l.URL != "" && l.PublicKey != "" && l.SecretKey != ""
}

// On — экспорт включен: переключатель enabled И заполнены url/ключи.
func (l Langfuse) On() bool { return l.Enabled && l.Valid() }

// Normalize — обводки строк, хвостовой слэш url, чистый список тэгов
// (из формы админки тэги приходят запятой — "prod, load-test").
func (l Langfuse) Normalize() Langfuse {
	l.URL = strings.TrimSpace(l.URL)
	l.URL = strings.TrimSuffix(l.URL, "/")
	l.PublicKey = strings.TrimSpace(l.PublicKey)
	l.SecretKey = strings.TrimSpace(l.SecretKey)
	l.Environment = strings.TrimSpace(l.Environment)
	tags := l.Tags[:0]
	seen := map[string]bool{}
	for _, t := range l.Tags {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	l.Tags = tags
	return l
}

// Data — модель YAML-файла any-router.yaml. Все ключи присутствуют
// всегда (пустые — как нулевые значения), чтобы файл был читаемым образцом.
type Data struct {
	Listen    string              `yaml:"listen"`
	Token     string              `yaml:"token"`
	Lang      string              `yaml:"lang"` // язык админки: ru|en (пусто — ru; cookie any_router_lang перекрывает per-сессию)
	Providers map[string]Provider `yaml:"providers"`
	Tokens    []Token             `yaml:"tokens"`
	Balances  []Balance           `yaml:"balances"` // баланс-алиасы: групповые имена без префикса провайдера
	Langfuse  Langfuse            `yaml:"langfuse"`
}

// DefaultListen — адрес HTTP-сервера по умолчанию.
const DefaultListen = ":8085"

// Store — потокобезопасный конфиг-стор: data в памяти + персистентность в файл.
type Store struct {
	mu   sync.RWMutex
	path string
	data Data
}

// Load читает YAML-файл по пути path; если его нет — создаёт с дефолтами
// (listen :8085, свежий hex32-админ-токен). Пустые обязательные поля
// (listen, token) дозаполняются; при дозаполнении или создании файл
// перезаписывается.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	created := false
	legacyLF := false
	switch {
	case os.IsNotExist(err):
		s.data = defaultData()
		created = true
	case err != nil:
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	default:
		if err := yaml.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("разбор %s: %w", path, err)
		}
		// langfuse: старый файл без ключа enabled — прежняя семантика («включено,
		// когда заполнены url и ключи»); явное enabled в файле действует как
		// записано. Если флаг пришлось дозаполнить — файл перезапишется с
		// явным переключателем (legacyLF).
		var probe struct {
			Langfuse struct {
				Enabled *bool `yaml:"enabled"`
			} `yaml:"langfuse"`
		}
		if err := yaml.Unmarshal(raw, &probe); err == nil && probe.Langfuse.Enabled == nil && s.data.Langfuse.Valid() {
			s.data.Langfuse.Enabled = true
			legacyLF = true
		}
	}
	// Нормализация: провайдеры/токены всегда non-nil, обязательные поля не пусты.
	// created — файла не было, его надо материализовать на диске.
	dirty := created || legacyLF || s.data.Listen == "" || s.data.Token == ""
	if s.data.Listen == "" {
		s.data.Listen = DefaultListen
	}
	if s.data.Token == "" {
		s.data.Token = NewToken()
	}
	// язык админки: приводим к нижнему регистру (RU → ru); незнакомое значение
	// не ошибка — web сфолбэчит в ru (в файле остаётся как записали)
	s.data.Lang = strings.ToLower(strings.TrimSpace(s.data.Lang))
	if s.data.Providers == nil {
		s.data.Providers = make(map[string]Provider)
	}
	if s.data.Tokens == nil {
		s.data.Tokens = []Token{}
	}
	if s.data.Balances == nil {
		s.data.Balances = []Balance{}
	}
	for i := range s.data.Tokens {
		if s.data.Tokens[i].normalizeScope() {
			dirty = true
		}
	}
	// баланс-алиасы: чистка кривых записей yaml (имя с «/», пустые/дубли —
	// см. функцию); ручные правки не валят старт — кривое выпадает молча
	if cleaned, changed := normalizeBalances(s.data.Balances); changed {
		s.data.Balances = cleaned
		dirty = true
	}
	if dirty {
		if err := s.Save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// defaultData — содержимое свежесозданного any-router.yaml.
func defaultData() Data {
	return Data{
		Listen:    DefaultListen,
		Token:     NewToken(),
		Providers: make(map[string]Provider),
		Tokens:    []Token{},
		Balances:  []Balance{},
		Langfuse:  Langfuse{},
	}
}

// Save записывает данные в YAML-файл атомарно: временный файл (0600)
// → rename поверх целевого.
func (s *Store) Save() error {
	s.mu.RLock()
	raw, err := yaml.Marshal(&s.data)
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("маршалинг %s: %w", s.path, err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("переименование %s: %w", s.path, err)
	}
	return nil
}

// SetAdminToken перезаписывает админ-токен (флаг -token, задача 2 PRD).
func (s *Store) SetAdminToken(token string) error {
	s.mu.Lock()
	if s.data.Token == token {
		s.mu.Unlock()
		return nil
	}
	s.data.Token = token
	s.mu.Unlock()
	return s.Save()
}

// AdminToken — текущий админ-токен.
func (s *Store) AdminToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Token
}

// Listen — адрес HTTP-сервера из файла.
func (s *Store) Listen() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Listen
}

// Lang — язык интерфейса админки из файла (пустое/незнакомое значение —
// web сфолбэчит в ru).
func (s *Store) Lang() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Lang
}

// SetLang сохраняет язык интерфейса админки (yaml lang; ввод нормализуется —
// регистр/пробелы). Значение не валидируется: незнакомое — web фолбэчит в ru.
func (s *Store) SetLang(lang string) error {
	lang = strings.ToLower(strings.TrimSpace(lang))
	s.mu.Lock()
	s.data.Lang = lang
	s.mu.Unlock()
	return s.Save()
}

// LangfuseCfg — настройки экспорта в Langfuse (копия).
func (s *Store) LangfuseCfg() Langfuse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Langfuse
}

// normalizeBalances — чистка списка баланс-алиасов из yaml: имя — не пустое
// и без «/» (алиасы живут в голых именах, «/» — пространство прямых обращений),
// участники — полные формы provider/model (обводки, пустые записи и дубли
// выбрасываются — порядок важен round-robin'у), запись без валидных
// участников несостоятельна и выпадает, дубль имени — остаётся первая запись
// (по ней и роутинг). Неизвестные имена провайдеров участников НЕ выбрасываем:
// списки бэкендов динамические (у токенов в области — та же семантика),
// маршрутизация в момент запроса участников с мёртвым провайдером пропускает.
// Возвращает чистый список и признак изменений (файл перезаписывается,
// чтобы исправленное не правилось при каждом старте).
func normalizeBalances(balances []Balance) ([]Balance, bool) {
	out := make([]Balance, 0, len(balances))
	seen := map[string]bool{}
	changed := false
	for _, b := range balances {
		name := strings.TrimSpace(b.Name)
		members := cleanModelRefs(b.Models)
		if name == "" || strings.Contains(name, "/") || len(members) == 0 || seen[name] ||
			name != b.Name || !equalStrings(members, b.Models) {
			changed = true
		}
		if name == "" || strings.Contains(name, "/") || len(members) == 0 || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Balance{Name: name, Models: members})
	}
	return out, changed
}

// cleanModelRefs — чистка списка полных форм provider/model (участники
// баланс-алиасов): обводки, пустые записи, дубли и wildcard-записи
// (участник — всегда конкретная модель, «provider/*» не бывает) выбрасываются
// (порядок сохраняется — он важен round-robin’у).
func cleanModelRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		p, m, ok := strings.Cut(ref, "/")
		if !ok || p == "" || m == "" || m == HostWildcard || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// Balances — копия списка баланс-алиасов (в порядке конфига — он важен
// для таблицы и round-robin).
func (s *Store) Balances() []Balance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Balance, len(s.data.Balances))
	copy(out, s.data.Balances)
	return out
}

// Balance — участники баланс-алиаса по имени; ok=false, если алиаса нет.
// Участники как записаны в конфиге (полные формы provider/model).
func (s *Store) Balance(name string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.data.Balances {
		if b.Name == name {
			return append([]string(nil), b.Models...), true
		}
	}
	return nil, false
}

// SetBalance добавляет/перезаписывает баланс-алиас (форма админки:
// balance-add и balance-edit) и сохраняет файл. Имя — не пустое и без «/»
// (голые имена — пространство балансировки); участники — хотя бы одна полная
// форма provider/model, провайдеры записей должны существовать в конфиге
// (сами модели существовать не обязаны — списки бэкендов динамические).
func (s *Store) SetBalance(name string, models []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("нужно имя баланс-алиаса")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("имя баланс-алиаса %q не должно содержать «/» — голые имена без префикса провайдера отличают балансировку от прямых обращений", name)
	}
	hasModel := false
	for _, ref := range models {
		if strings.TrimSpace(ref) != "" {
			hasModel = true
			break
		}
	}
	if !hasModel {
		return fmt.Errorf("баланс-алиасу %q нужна хотя бы одна модель — выберите из списка моделей", name)
	}
	s.mu.Lock()
	members, err := validModelsScope(models, s.data.Providers, nil) // участники — без баланс-алиасов
	if err != nil {
		s.mu.Unlock()
		return err
	}
	replaced := false
	for i, b := range s.data.Balances {
		if b.Name == name {
			s.data.Balances[i] = Balance{Name: name, Models: members}
			replaced = true
			break
		}
	}
	if !replaced {
		s.data.Balances = append(s.data.Balances, Balance{Name: name, Models: members})
	}
	s.mu.Unlock()
	return s.Save()
}

// DeleteBalance удаляет баланс-алиас по имени; ok=false, если его не было.
func (s *Store) DeleteBalance(name string) (bool, error) {
	s.mu.Lock()
	for i, b := range s.data.Balances {
		if b.Name == name {
			s.data.Balances = append(s.data.Balances[:i], s.data.Balances[i+1:]...)
			s.mu.Unlock()
			return true, s.Save()
		}
	}
	s.mu.Unlock()
	return false, nil
}

// SetLangfuse сохраняет настройки экспорта в Langfuse; выключен — при снятом
// переключателе enabled или пустых url/ключях (environment/tags — опционально,
// на включенность не влияют). Ввод нормализуется (обводки, url без хвостового
// «/», чистка тэгов).
func (s *Store) SetLangfuse(l Langfuse) error {
	l = l.Normalize()
	s.mu.Lock()
	s.data.Langfuse = l
	s.mu.Unlock()
	return s.Save()
}

// BearerKnown — токен подходит для /v1: это админ-токен или API-токен
// из массива tokens (сравнения постоянного времени делает router.Auth).
func (s *Store) BearerKnown(bearer string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if bearer == s.data.Token {
		return true
	}
	for _, t := range s.data.Tokens {
		if t.Token == bearer {
			return true
		}
	}
	return false
}

// ProviderNames — имена провайдеров по алфавиту.
func (s *Store) ProviderNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data.Providers))
	for name := range s.data.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Provider — провайдер по имени; ok=false, если такого нет.
func (s *Store) Provider(name string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Providers[name]
	return p, ok
}

// SetProvider добавляет/перезаписывает провайдера и сохраняет файл.
// Имя и baseurl обязательны.
func (s *Store) SetProvider(name string, p Provider) error {
	if name == "" || p.BaseURL == "" {
		return fmt.Errorf("нужны имя и baseurl провайдера")
	}
	s.mu.Lock()
	s.data.Providers[name] = p
	s.mu.Unlock()
	return s.Save()
}

// DeleteProvider удаляет провайдера; ok=false, если его не было.
func (s *Store) DeleteProvider(name string) (bool, error) {
	s.mu.Lock()
	if _, ok := s.data.Providers[name]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	delete(s.data.Providers, name)
	s.mu.Unlock()
	return true, s.Save()
}

// Tokens — копия списка API-токенов.
func (s *Store) Tokens() []Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Token, len(s.data.Tokens))
	copy(out, s.data.Tokens)
	return out
}

// NewAPIToken создаёт API-токен: генерирует hex32, пустой ID заменяет
// на t1, t2, ... (первый свободный). Область (Scope/Provider/Model из формы)
// проверяется: незнакомый scope трактуется как all; для provider/model
// провайдер обязателен и должен существовать в конфиге (модель существовать
// не обязана — списки динамические); незаполненная область откатывается
// к all. Сохраняет файл.
func (s *Store) NewAPIToken(req Token) (Token, error) {
	s.mu.Lock()
	scope, err := resolveScope(req, s.data.Providers, balanceNames(s.data))
	if err != nil {
		s.mu.Unlock()
		return Token{}, err
	}
	req.Scope, req.Provider, req.Model, req.Models = scope.Scope, scope.Provider, scope.Model, scope.Models
	id := req.ID
	if id == "" {
		id = freeTokenID(s.data.Tokens)
	}
	for _, t := range s.data.Tokens {
		if t.ID == id {
			s.mu.Unlock()
			return Token{}, fmt.Errorf("токен с id %q уже есть", id)
		}
	}
	req.ID = id
	req.Token = NewToken()
	s.data.Tokens = append(s.data.Tokens, req)
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return Token{}, err
	}
	return req, nil
}

// resolveScope — проверка/нормализация области прав (token-add и token-edit):
// незнакомый scope откатывается к all (без ошибки); для provider/model нужен
// провайдер (для model — ещё и модель); для models — хотя бы одна запись,
// у каждой — существующий провайдер: полные формы provider/model,
// wildcard-записи provider/* (весь хост) или существующий баланс-алиас
// (голое имя — явная выдача пула токену); сами модели существовать не
// обязаны — списки бэкендов динамические. Возвращает нормализованную область.
func resolveScope(req Token, providers map[string]Provider, aliasNames map[string]bool) (Token, error) {
	switch req.Scope {
	case ScopeProvider, ScopeModel:
		if req.Provider == "" {
			return Token{}, fmt.Errorf("области %s нужен провайдер", req.Scope)
		}
		if _, ok := providers[req.Provider]; !ok {
			return Token{}, fmt.Errorf("провайдера %q нет — добавьте его во вкладке «Провайдеры»", req.Provider)
		}
		if req.Scope == ScopeModel && req.Model == "" {
			return Token{}, fmt.Errorf("области %s нужна модель", req.Scope)
		}
		req.Models = nil
	case ScopeModels:
		models, err := validModelsScope(req.Models, providers, aliasNames)
		if err != nil {
			return Token{}, err
		}
		req.Provider, req.Model, req.Models = "", "", models
	default:
		req = Token{Scope: ScopeAll}
	}
	return req, nil
}

// validModelsScope — чистка и проверка списка моделей области models:
// каждая запись — полная форма provider/model, wildcard «provider/*» (весь
// хост; провайдер существует) или голое имя баланс-алиаса (существует в
// конфиге — явная выдача пула токену; права только через алиас, участники
// напрямую не светятся). Пустые и повторяющиеся записи выкидываются;
// каноническая минимальная форма: конкретные модели, покрытые wildcard’ом
// своего же провайдера, выбрасываются (p/* покрывает их всё равно).
// aliasNames — имена существующих алиасов; nil — голые имена запрещены
// (участники баланс-алиасов: группа плоская, алиас-в-алиас не бывает,
// wildcards тоже — их тут не предлагают).
func validModelsScope(models []string, providers map[string]Provider, aliasNames map[string]bool) ([]string, error) {
	out := make([]string, 0, len(models))
	seen := map[string]bool{}
	hosts := map[string]bool{} // wildcard-записи «p/*» уже в списке
	for _, ref := range models {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		if p, m, ok := strings.Cut(ref, "/"); ok && p != "" && m != "" {
			// полные формы и wildcard-записи хостов — provider должен существовать
			if _, ok := providers[p]; !ok {
				return nil, fmt.Errorf("провайдера %q нет — добавьте его во вкладке «Провайдеры»", p)
			}
			if aliasNames == nil && m == HostWildcard {
				return nil, fmt.Errorf("запись %q не бывает участником баланс-алиаса — выберите конкретные модели", ref)
			}
			seen[ref] = true
			out = append(out, ref)
			if m == HostWildcard {
				hosts[p] = true
			}
			continue
		}
		if aliasNames[ref] {
			seen[ref] = true
			out = append(out, ref)
			continue
		}
		if aliasNames == nil {
			return nil, fmt.Errorf("участник %q должен быть вида провайдер/модель — выберите из списка моделей", ref)
		}
		return nil, fmt.Errorf("баланс-алиас %q не найден — выберите из списка моделей (группы создаются во вкладке «Балансировка»)", ref)
	}
	// канонизация: конкретные модели, покрытые wildcard’ом своего провайдера, лишние
	if len(hosts) > 0 {
		minimal := make([]string, 0, len(out))
		for _, ref := range out {
			if p, m, ok := strings.Cut(ref, "/"); ok && m != HostWildcard && hosts[p] {
				continue // p/* покрывает эту модель — запись не нужна
			}
			minimal = append(minimal, ref)
		}
		out = minimal
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("области %s нужна хотя бы одна модель", ScopeModels)
	}
	return out, nil
}

// UpdateTokenScope — изменить область прав существующего API-токена
// (админка: ✎ в строке токена, op=token-edit): id и секрет не меняются,
// меняются только поля области (scope/provider/model/models). Валидация —
// та же, что при создании (resolveScope); модель существовать не обязана.
func (s *Store) UpdateTokenScope(id string, req Token) (Token, error) {
	s.mu.Lock()
	idx := -1
	for i, t := range s.data.Tokens {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return Token{}, fmt.Errorf("токена %q нет", id)
	}
	scope, err := resolveScope(req, s.data.Providers, balanceNames(s.data))
	if err != nil {
		s.mu.Unlock()
		return Token{}, err
	}
	t := s.data.Tokens[idx]
	t.Scope, t.Provider, t.Model, t.Models = scope.Scope, scope.Provider, scope.Model, scope.Models
	s.data.Tokens[idx] = t
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return Token{}, err
	}
	return t, nil
}

// balanceNames — имена баланс-алиасов конфига (для проверки областей tokens:
// запись области может быть явной выдачей алиаса — голое имя).
func balanceNames(d Data) map[string]bool {
	names := make(map[string]bool, len(d.Balances))
	for _, b := range d.Balances {
		names[b.Name] = true
	}
	return names
}

// freeTokenID — первый свободный номер t1.. с учётом занятых ID.
func freeTokenID(existing []Token) string {
	taken := make(map[string]bool, len(existing))
	for _, t := range existing {
		taken[t.ID] = true
	}
	for n := 1; ; n++ {
		id := fmt.Sprintf("t%d", n)
		if !taken[id] {
			return id
		}
	}
}

// DeleteToken удаляет API-токен по ID; ok=false, если его не было.
func (s *Store) DeleteToken(id string) (bool, error) {
	s.mu.Lock()
	for i, t := range s.data.Tokens {
		if t.ID == id {
			s.data.Tokens = append(s.data.Tokens[:i], s.data.Tokens[i+1:]...)
			s.mu.Unlock()
			return true, s.Save()
		}
	}
	s.mu.Unlock()
	return false, nil
}

// NewToken — hex32: 16 байт из crypto/rand, hex-кодировка (32 символа).
func NewToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
