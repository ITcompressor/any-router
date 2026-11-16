// i18n — языки веб-админки any-router (ru/en). Один язык — набор строк
// adminStrings; выбор: cookie any_router_lang (переключатель в шапке,
// действует на сессию) → yaml-поле lang в конфиг-сторе → дефолт ru.
// Значения полей — только простой текст без HTML-сущностей: бандл выводится
// шаблоном (авто-экранирование) и уходит в JS как const STR.
package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ITcompressor/any-router/internal/store"
)

// Коды языков админки (реестр — adminRegistry).
const (
	LangRU = "ru"
	LangEN = "en"
)

// LangDefault — дефолтный язык: продукт исторически русскоязычный.
const LangDefault = LangRU

// LangCookie — cookie с выбором языка интерфейса (переключатель шапки;
// перекрывает yaml lang на сессию, как ralph.lang в одноимённой механике).
const LangCookie = "any_router_lang"

// adminStrings — строки интерфейса админки на одном языке. Поля — контракт
// между шаблоном/JS-динамикой и переводами: незаполненная строка — дыра
// в интерфейсе (фолбэк на дефолт не спасает шаблонные поля с %s).
type adminStrings struct {
	Lang string // код языка (переключатель: data-lang)

	// Шапка и вкладки
	PageTitle    string
	ThemeTitle   string // toggle темы: title
	LangTitle    string // переключатель языка: title
	VersionTitle string // бейдж версии в топбаре: title
	TabProviders string
	TabTokens    string
	TabBalances  string
	TabLangfuse  string
	TabExamples  string

	// Провайдеры
	ProvHead      string // h2
	ThName        string // заголовки таблицы
	ThBaseURL     string
	ThAPIKey      string
	Delete        string // кнопка строки
	ProvEmpty     string // пустой список
	ProvLegend    string // форма добавления: legend
	ProvNameLabel string
	ProvNameHint  string
	ProvBaseLabel string
	ProvBaseHint  string
	ProvKeyLabel  string
	ProvKeyHint   string
	AddBtn        string

	// API-токены
	TokHead        string // h2
	TokHint        string // подсказка «один токен — одно правило»
	TokEmpty       string // пустой список
	TokLegend      string // форма создания/редактирования (legend, при редактировании заменяется JS)
	TokIDLabel     string
	TokIDHint      string
	ThToken        string // заголовок колонки «токен»
	TokRightsLabel string
	TokModelsLabel string // multi-select области models
	TokModelsHint  string // подсказка multi-select (как выбирать несколько)
	TokCreateBtn   string
	EditRowTitle   string // ✎ в строке токена: title
	CancelEditBtn  string // кнопка отмены редактирования
	TokEditingFmt  string // legend при редактировании, %s — id токена
	RefreshTitle   string // ⟳: title
	LoadModelsTok  string // option селекта формы: «— загрузить модели —»
	LoadModelsEx   string // option селекта примеров: «— загрузить —»

	// Балансировка (баланс-алиасы)
	BalHead        string // h2
	BalHint        string // подсказка вкладки
	BalEmpty       string // пустой список
	BalLegend      string // форма создания/редактирования (legend, при редактировании заменяет JS)
	BalNameLabel   string
	BalNameHint    string
	BalModelsLabel string
	BalModelsHint  string
	BalCreateBtn   string
	BalEditingFmt  string // legend при редактировании, %s — имя алиаса
	BalanceGroup   string // optgroup баланс-алиасов в списках моделей (JS)
	HintBalanceFmt string // подсказка примеров для алиаса: %s — имя, %s — участники

	// Langfuse
	LfHead         string // h2
	LfHint         string // подсказка до <code>{url}/api/public/otel/v1/traces</code>
	LfHintPost     string // хвост подсказки после код-тега
	LfStatus       string // «статус:»
	LfOn           string // «включен»
	LfOff          string // «выключен»
	LfOffHint      string // «заполните url и оба ключа»
	LfOffToggled   string // «выключен переключателем — настройки сохранены»
	ThURL          string
	ThPubKey       string
	ThSecKey       string
	LfLegend       string
	LfEnabledLabel string
	LfEnabledHint  string
	LfURLLabel     string
	LfURLHint      string
	LfPubLabel     string
	LfPubHint      string
	LfSecLabel     string
	LfSecHint      string
	LfEnvLabel     string
	LfEnvHint      string
	LfTagsLabel    string
	LfTagsHint     string
	SaveBtn        string

	// Примеры вызова
	ExHead          string // h2
	ExHintFmt       string // подсказка вкладки: %s — base_url (с &lt;url&gt;-сущностью; вывод — template.HTML)
	ExTokenLabel    string
	ExTokenHint     string
	ExModelLabel    string
	AdminOption     string // option «админ-токен»
	ExChatCaption   string // muted-хвосты h3
	ExEmbedCaption  string
	ExSysoneCaption string
	ExModelsCaption string
	CopyBtnTitle    string
	ExTokenSuffix   string // «API: » в option API-токенов

	// Права токена (таблица + option примеров; store.Token)
	RightsAll         string // «все модели»
	RightsProviderFmt string // «провайдер %s»
	RightsModelFmt    string // «модель %s»
	RightsModelsFmt   string // «модели %s» — список записей области
	RightsHostFmt     string // «весь хост %s» — wildcard-запись provider/*
	RightsAliasFmt    string // «алиас %s» — голое имя баланс-алиаса

	// JS-динамика (const STR в скрипте)
	PickModel         string // placeholder модели в curl-примере
	EmptyModels       string // пустой список моделей
	NoModelsForToken  string // список фильтруется в пусто правами токена
	ModelsFailed      string // не удалось загрузить модели — обновите ⟳
	NoProviderGlob    string // optgroup моделей без провайдера
	HostOpt           string // опция «весь хост» в multi-select прав токена (value — provider/*)
	HintModelFmt      string // %s — выбранная модель, %s — raw у провайдера
	HintNoModel       string // модель не выбрана
	ScopeProviderNote string // %s — провайдер
	ScopeModelNote    string // %s — provider/model
	ScopeModelsNote   string // %s — список моделей области models
	FetchFailed       string // /admin/api/models недоступен

	// Уведомления и ошибки мутаций (Go; %s/%q — данные, экранируются)
	NoticeProvSavedFmt string // %q — имя провайдера
	NoticeTokenFmt     string // %q — id, %s — права, %s — вкладка токенов
	NoticeTokenEditFmt string // %q — id, %s — права (op=token-edit)
	NoticeBalSavedFmt  string // %q — имя баланс-алиаса
	NoticeLFOnFmt      string // %s — url
	NoticeLFOff        string
	NoticeLFOffToggled string
	ProblemModelRef    string
	ProblemURLScheme   string
	ProblemUnknownOp   string
	ProblemFormFmt     string // %v — ошибка парсинга формы

	// Прочее
	LogoutFmt       string // страница logout: %s — код-подсказка входа
	SecretEyeTitle  string // глаз в secretCell
	SecretCopyTitle string // копия в secretCell
}

// (Подсказка вкладки «Примеры вызова» с живым адресом — не поле бандла:
// она строится вместе с base_url в adminData.ExHint.)

// adminRegistry — реестр языков админки.
var adminRegistry = map[string]*adminStrings{
	LangRU: &ruAdmin,
	LangEN: &enAdmin,
}

// adminStringsFor — выбор набора строк по запросу: cookie LangCookie →
// yaml lang (store) → дефолт ru; пустое/незнакомое значение — шаг ниже.
func (s *Server) adminStringsFor(r *http.Request) *adminStrings {
	for _, v := range []string{cookie(r, LangCookie), s.st.Lang()} {
		if b := adminRegistry[strings.ToLower(strings.TrimSpace(v))]; b != nil {
			return b
		}
	}
	return adminRegistry[LangDefault]
}

// cookie — значение cookie по имени (пусто — нет cookie).
func cookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// scopeRights — человекочитаемые права токена на языке интерфейса
// (store.DescribeScope — русская, тексты её слов живут здесь).
func scopeRights(a *adminStrings, tok store.Token) string {
	switch tok.Scope {
	case store.ScopeProvider:
		return fmt.Sprintf(a.RightsProviderFmt, tok.Provider)
	case store.ScopeModel:
		// полная форма области: provider/… или provider//path (path-модели)
		return fmt.Sprintf(a.RightsModelFmt, tok.Provider+"/"+tok.Model)
	case store.ScopeModels:
		// записи области словами: «p/*» — весь хост, голые — алиасы,
		// остальные — как записаны (у path-моделей — provider//path)
		parts := make([]string, 0, len(tok.Models))
		for _, ref := range tok.Models {
			switch p, m, ok := strings.Cut(ref, "/"); {
			case !ok:
				parts = append(parts, fmt.Sprintf(a.RightsAliasFmt, ref))
			case m == store.HostWildcard:
				parts = append(parts, fmt.Sprintf(a.RightsHostFmt, p))
			default:
				parts = append(parts, ref)
			}
		}
		return fmt.Sprintf(a.RightsModelsFmt, strings.Join(parts, ", "))
	default: // all (в т.ч. пустые/незнакомые — normalizeScope)
		return a.RightsAll
	}
}

// ru — русский набор (дефолт).
var ruAdmin = adminStrings{
	Lang: LangRU,

	PageTitle:    "any-router — админка",
	ThemeTitle:   "переключить тему (светлая/тёмная)",
	LangTitle:    "язык интерфейса (дефолт — lang из any-router.yaml)",
	VersionTitle: "версия any-router (метаданные — команда any-router version)",
	TabProviders: "Провайдеры",
	TabTokens:    "API-токены",
	TabBalances:  "Балансировка",
	TabLangfuse:  "Langfuse",
	TabExamples:  "Примеры вызова",

	// Провайдеры
	ProvHead:      "Провайдеры (baseurl — OpenAI-совместимый апстрим, включая /v1)",
	ThName:        "имя",
	ThBaseURL:     "baseurl",
	ThAPIKey:      "apikey",
	Delete:        "удалить",
	ProvEmpty:     "провайдеров нет — добавьте форму ниже",
	ProvLegend:    "Добавить провайдера",
	ProvNameLabel: "имя",
	ProvNameHint:  "— в модели provider/model",
	ProvBaseLabel: "baseurl",
	ProvBaseHint:  "— включая /v1",
	ProvKeyLabel:  "apikey",
	ProvKeyHint:   "— опционально",
	AddBtn:        "добавить",

	// API-токены
	TokHead:        "API-токены (/v1)",
	TokHint:        "Один токен — список моделей: конкретные модели (в т.ч. разных провайдеров), хосты целиком («весь хост» — все его модели) и/или баланс-алиасы. Права меняются кнопкой «✎» без пересоздания токена.",
	TokEmpty:       "API-токенов нет — /v1 доступен пока только админскому токену",
	TokLegend:      "Создать API-токен",
	TokIDLabel:     "id",
	TokIDHint:      "— необязательно, будет t1, t2, …",
	ThToken:        "токен",
	TokRightsLabel: "права",
	TokModelsLabel: "модели",
	TokModelsHint:  "— «весь хост» (все его модели), конкретные модели и/или баланс-алиасы (голые имена — выдача пула), ctrl/cmd или выбор мышью",
	TokCreateBtn:   "создать",
	EditRowTitle:   "изменить права токена",
	CancelEditBtn:  "отмена",
	TokEditingFmt:  "изменение прав токена «%s»",
	RefreshTitle:   "обновить список моделей",
	LoadModelsTok:  "— загрузить модели —",
	LoadModelsEx:   "— загрузить —",

	// Балансировка
	BalHead:        "Балансировка — алиасы групп моделей",
	BalHint:        "Баланс-алиас — голое имя без префикса провайдера (без «/»): клиенты пишут его в поле model вместо provider/model, и запрос уходит одной из моделей группы\n(round-robin; при недоступности апстрима — следующая модель). Голая форма отличает обращение\nчерез балансировщик от прямых обращений к моделям — в langfuse такие запросы помечаются тегом\nalias:<имя> и метаданными alias. Права scope-токена на алиас — явная выдача его в области models\n(участники напрямую не светятся) или покрытие всех участников группы.\nРазговор закрепляется за участником (липкость: работает кэш промпта апстрима): отпечаток —\nзаголовок X-Session-Id или первые два сообщения chat-цепочки (TTL 30 мин).",
	BalEmpty:       "баланс-алиасов нет — форма ниже создаёт первый",
	BalLegend:      "Добавить баланс-алиас",
	BalNameLabel:   "имя",
	BalNameHint:    "— без префикса провайдера (без «/»), ровно как уйдет в поле model",
	BalModelsLabel: "модели",
	BalModelsHint:  "— участники группы (запросы распределяются по очереди), ctrl/cmd или выбор мышью",
	BalCreateBtn:   "создать",
	BalEditingFmt:  "изменение баланс-алиаса «%s»",
	BalanceGroup:   "балансировка",
	HintBalanceFmt: "«%s» — баланс-алиас: запрос уйдет одной из моделей-участниц: %s",

	// Langfuse
	LfHead:         "Langfuse — экспорт трейсов",
	LfHint:         "Неблокирующая выгрузка трейсов LLM-запросов (chat/completions, completions, embeddings, systemone):\nв трейсе — IP клиента, id API-токена, провайдер и модель, ввод/вывод и usage. Выгружается, когда включен переключатель\n«экспорт» и заполнены url и оба ключа; смена настроек действует сразу (без\nперезапуска). Эндпоинт — OTLP/JSON POST",
	LfHintPost:     "(актуальный путь ingestion v4).",
	LfStatus:       "статус:",
	LfOn:           "включен",
	LfOff:          "выключен",
	LfOffHint:      "заполните url и оба ключа",
	LfOffToggled:   "выключен переключателем — настройки сохранены",
	ThURL:          "url",
	ThPubKey:       "public key",
	ThSecKey:       "secret key",
	LfLegend:       "Настройки экспорта",
	LfEnabledLabel: "экспорт",
	LfEnabledHint:  "— переключатель вкл/выкл (настройки остаются в конфиге)",
	LfURLLabel:     "url",
	LfURLHint:      "— база инстанса, без /api",
	LfPubLabel:     "public key",
	LfPubHint:      "— pk-lf-…",
	LfSecLabel:     "secret key",
	LfSecHint:      "— sk-lf-…",
	LfEnvLabel:     "environment",
	LfEnvHint:      "— опционально (prod/dev…)",
	LfTagsLabel:    "tags",
	LfTagsHint:     "— опционально, через запятую",
	SaveBtn:        "сохранить",

	// Примеры вызова
	ExHead:          "Примеры вызова",
	ExHintFmt:       "Выберите токен и модель — curl подставит их автоматически;\nнабор примеров определяется capabilities модели (completion → чат,\nembedding → эмбеддинги, decision → systemone; capabilities неизвестны —\nпоказан чат). адрес — base_url запуска (%s;\nпереопределить: -base-url &lt;url&gt;).",
	ExTokenLabel:    "токен",
	ExTokenHint:     "— права фильтруют модели",
	ExModelLabel:    "модель",
	AdminOption:     "админ-токен",
	ExChatCaption:   "(проверка модели строкой «ping»)",
	ExEmbedCaption:  "(эмбеддинг строки «ping»)",
	ExSysoneCaption: "(decision-API бэкенда)",
	ExModelsCaption: "(что доступно)",
	CopyBtnTitle:    "копировать пример",
	ExTokenSuffix:   "API: ",

	// Права токена
	RightsAll:         "все модели",
	RightsProviderFmt: "провайдер %s",
	RightsModelFmt:    "модель %s",
	RightsModelsFmt:   "модели: %s",
	RightsHostFmt:     "весь хост %s",
	RightsAliasFmt:    "алиас %s",

	// JS-динамика
	PickModel:         "<выберите модель>",
	EmptyModels:       "моделей нет — добавьте провайдера",
	NoModelsForToken:  "доступных токену моделей нет",
	ModelsFailed:      "не удалось загрузить модели — обновите ⟳",
	NoProviderGlob:    "без провайдера",
	HostOpt:           "весь хост — все модели",
	HintModelFmt:      "модель «%s»: у роутера уйдет провайдеру как «%s»",
	HintNoModel:       "выберите модель из списка (у провайдера с алиасами — только алиасы)",
	ScopeProviderNote: "токен ограничен провайдером «%s» — чужие модели скрыты",
	ScopeModelNote:    "токен ограничен моделью «%s»",
	ScopeModelsNote:   "токен ограничен списком моделей: %s",
	FetchFailed:       "не удалось получить модели (смотрите лог)",

	// Уведомления и ошибки мутаций
	NoticeProvSavedFmt: "провайдер %q сохранён",
	NoticeTokenFmt:     "создан API-токен %q — %s (вкладка «%s», глаз — показать)",
	NoticeTokenEditFmt: "права API-токена %q обновлены — %s",
	NoticeBalSavedFmt:  "баланс-алиас %q сохранён",
	NoticeLFOnFmt:      "экспорт langfuse включен — %s",
	NoticeLFOff:        "настройки langfuse сохранены (экспорт выключен — пусты url или ключи)",
	NoticeLFOffToggled: "настройки langfuse сохранены — экспорт выключен переключателем",
	ProblemModelRef:    "модель должна быть вида провайдер/модель — выберите из списка моделей",
	ProblemURLScheme:   "url должен начинаться с http:// или https://",
	ProblemUnknownOp:   "неизвестная операция",
	ProblemFormFmt:     "форма не разбирается: %v",

	// Прочее
	LogoutFmt:       "Вы вышли из админки. Вход заново — %s или Bearer.",
	SecretEyeTitle:  "показать/скрыть",
	SecretCopyTitle: "скопировать",
}

// en — английский набор.
var enAdmin = adminStrings{
	Lang: LangEN,

	PageTitle:    "any-router — admin",
	ThemeTitle:   "toggle theme (light/dark)",
	LangTitle:    "UI language (default — lang from any-router.yaml)",
	VersionTitle: "any-router version (see any-router version)",
	TabProviders: "Providers",
	TabTokens:    "API tokens",
	TabBalances:  "Load balancing",
	TabLangfuse:  "Langfuse",
	TabExamples:  "Call examples",

	// Providers
	ProvHead:      "Providers (baseurl — OpenAI-compatible upstream, including /v1)",
	ThName:        "name",
	ThBaseURL:     "baseurl",
	ThAPIKey:      "apikey",
	Delete:        "delete",
	ProvEmpty:     "no providers yet — add one with the form below",
	ProvLegend:    "Add provider",
	ProvNameLabel: "name",
	ProvNameHint:  "— in the provider/model model",
	ProvBaseLabel: "baseurl",
	ProvBaseHint:  "— including /v1",
	ProvKeyLabel:  "apikey",
	ProvKeyHint:   "— optional",
	AddBtn:        "add",

	// API tokens
	TokHead:        "API tokens (/v1)",
	TokHint:        "One token — a list of models: individual models (possibly across providers), whole hosts («whole host» — all of its models) and/or balance aliases. Change the scope with ✎ — no need to recreate the token.",
	TokEmpty:       "no API tokens yet — /v1 is available to the admin token only",
	TokLegend:      "Create API token",
	TokIDLabel:     "id",
	TokIDHint:      "— optional; t1, t2, … if empty",
	ThToken:        "token",
	TokRightsLabel: "scope",
	TokModelsLabel: "models",
	TokModelsHint:  "— «whole host» (all of its models), individual models and/or balance aliases (bare names grant the pool), multi-select: ctrl/cmd or plain click",
	TokCreateBtn:   "create",
	EditRowTitle:   "edit token scope",
	CancelEditBtn:  "cancel",
	TokEditingFmt:  "editing the scope of token «%s»",
	RefreshTitle:   "refresh the model list",
	LoadModelsTok:  "— load models —",
	LoadModelsEx:   "— load —",

	// Load balancing
	BalHead:        "Load balancing — model group aliases",
	BalHint:        "A balance alias is a bare name without the provider prefix (no “/”): clients put it in the model field\ninstead of provider/model, and the request goes to one of the group's models (round-robin; on an\nunreachable upstream — the next one). The bare form distinguishes balancer calls from direct\nmodel calls — langfuse marks them with the alias:<name> tag and the alias metadata.\nA scope token may use an alias when it is explicitly granted in the token's models scope (the\nmembers stay hidden) or when the scope covers every group member. Conversation chains\nstick to the member they already use (upstream prompt-cache reuse): fingerprint is the\nX-Session-Id header or the first two messages of the chat body (TTL 30 min).",
	BalEmpty:       "no balance aliases yet — the form below creates the first one",
	BalLegend:      "Add balance alias",
	BalNameLabel:   "name",
	BalNameHint:    "— no provider prefix (no “/”), exactly as it goes into the model field",
	BalModelsLabel: "models",
	BalModelsHint:  "— group members (requests take turns), multi-select: ctrl/cmd or plain click",
	BalCreateBtn:   "create",
	BalEditingFmt:  "editing the balance alias “%s”",
	BalanceGroup:   "balancing",
	HintBalanceFmt: "“%s” is a balance alias: the request goes to one of the member models: %s",

	// Langfuse
	LfHead:         "Langfuse — trace export",
	LfHint:         "Non-blocking upload of LLM request traces (chat/completions, completions, embeddings, systemone):\na trace carries the client IP, API-token id, provider and model, input/output and usage. Uploads when the\n«export» switch is on and url and both keys are set; setting changes apply immediately\n(no restart). Endpoint — OTLP/JSON POST",
	LfHintPost:     "(the current ingestion path, v4).",
	LfStatus:       "status:",
	LfOn:           "on",
	LfOff:          "off",
	LfOffHint:      "fill in url and both keys",
	LfOffToggled:   "disabled by the switch — settings are kept",
	ThURL:          "url",
	ThPubKey:       "public key",
	ThSecKey:       "secret key",
	LfLegend:       "Export settings",
	LfEnabledLabel: "export",
	LfEnabledHint:  "— on/off switch (settings are kept in the config)",
	LfURLLabel:     "url",
	LfURLHint:      "— instance base, without /api",
	LfPubLabel:     "public key",
	LfPubHint:      "— pk-lf-…",
	LfSecLabel:     "secret key",
	LfSecHint:      "— sk-lf-…",
	LfEnvLabel:     "environment",
	LfEnvHint:      "— optional (prod/dev…)",
	LfTagsLabel:    "tags",
	LfTagsHint:     "— optional, comma-separated",
	SaveBtn:        "save",

	// Call examples
	ExHead:          "Call examples",
	ExHintFmt:       "Pick the token and model — curl substitutes them automatically;\nthe set of examples is determined by the model's capabilities (completion → chat,\nembedding → embeddings, decision → systemone; if capabilities are unknown —\nchat is shown). the address is the run's base_url (%s;\noverride: -base-url &lt;url&gt;).",
	ExTokenLabel:    "token",
	ExTokenHint:     "— scope filters the models",
	ExModelLabel:    "model",
	AdminOption:     "admin token",
	ExChatCaption:   "(model check with the string «ping»)",
	ExEmbedCaption:  "(embedding of the string «ping»)",
	ExSysoneCaption: "(backend decision API)",
	ExModelsCaption: "(what is available)",
	CopyBtnTitle:    "copy example",
	ExTokenSuffix:   "API: ",

	// Token scope
	RightsAll:         "all models",
	RightsProviderFmt: "provider %s",
	RightsModelFmt:    "model %s",
	RightsModelsFmt:   "models: %s",
	RightsHostFmt:     "whole host %s",
	RightsAliasFmt:    "alias %s",

	// JS dynamics
	PickModel:         "<pick a model>",
	EmptyModels:       "no models — add a provider",
	NoModelsForToken:  "no models available to the token",
	ModelsFailed:      "failed to load models — refresh ⟳",
	NoProviderGlob:    "without provider",
	HostOpt:           "whole host — all models",
	HintModelFmt:      "model «%s»: the router passes it to the provider as «%s»",
	HintNoModel:       "pick a model from the list (for a provider with aliases — aliases only)",
	ScopeProviderNote: "token is restricted to the provider «%s» — other providers' models are hidden",
	ScopeModelNote:    "token is restricted to the model «%s»",
	ScopeModelsNote:   "token is restricted to the models list: %s",
	FetchFailed:       "failed to get models (see the log)",

	// Mutation notices and problems
	NoticeProvSavedFmt: "provider %q saved",
	NoticeTokenFmt:     "API token %q created — %s (the %s tab, the eye shows it)",
	NoticeTokenEditFmt: "API token %q scope updated — %s",
	NoticeBalSavedFmt:  "balance alias %q saved",
	NoticeLFOnFmt:      "langfuse export enabled — %s",
	NoticeLFOff:        "langfuse settings saved (export disabled — url or keys are empty)",
	NoticeLFOffToggled: "langfuse settings saved — export disabled by the switch",
	ProblemModelRef:    "model must be provider/model — pick one from the model list",
	ProblemURLScheme:   "url must start with http:// or https://",
	ProblemUnknownOp:   "unknown operation",
	ProblemFormFmt:     "cannot parse the form: %v",

	// Misc
	LogoutFmt:       "You logged out of the admin. To log in again — %s or Bearer.",
	SecretEyeTitle:  "show/hide",
	SecretCopyTitle: "copy",
}
