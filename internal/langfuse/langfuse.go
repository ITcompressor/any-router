// Package langfuse — неблокирующая выгрузка LLM-трейсов any-router в Langfuse
// (админка настраивает url/ключи, store.Data.Langfuse). Актуальный путь
// ingestion — OTLP/HTTP JSON: POST {url}/api/public/otel/v1/traces, Basic
// auth public:secret, заголовок x-langfuse-ingestion-version: 4 (реальное
// время на v4-модели; без него трейсы могли бы задерживаться). Legacy
// POST /api/public/ingestion в Langfuse v4 deprecated и в режиме events_only
// отклоняет trace-события вовсе.
//
// Неблокируемость: Submit кладёт запись в буферизованную очередь (полная —
// дропается с предупреждением не чаще раза в минуту); воркер-горутина
// собирает батчи (по размеру или таймеру) и шлёт их POST'ом с таймаутом.
// Сбой Langfuse на запросы клиентов никак не влияет — это fire-and-forget.
package langfuse

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ITcompressor/any-router/internal/logging"
	"github.com/ITcompressor/any-router/internal/proxy"
)

// Config — настройка экспорта (копия store.Langfuse: переключатель enabled,
// url инстанса, ключи проекта, опциональные environment и тэги трейсов).
// Valid — настройки заполнены (url и оба ключа); On — выгрузка включена.
type Config struct {
	Enabled     bool
	URL         string
	PublicKey   string
	SecretKey   string
	Environment string
	Tags        []string
}

// Valid — настройки заполнены (url и оба ключа); переключатель не учитывается.
func (c Config) Valid() bool { return c.URL != "" && c.PublicKey != "" && c.SecretKey != "" }

// On — выгрузка включена: переключатель enabled И заполнены url/ключи.
func (c Config) On() bool { return c.Enabled && c.Valid() }

// Параметры конвейера экспорта: очередь (полная — дроп), размер батча,
// интервал частичного слива, таймаут POST.
const (
	QueueSize  = 64
	FlushSize  = 32
	PostBudget = 10 * time.Second
)

// Exporter — неблокирующий sink трейсов (реализует proxy.CallSink):
// очередь в памяти + воркер с батч-POST на OTLP-эндпоинт. Настройки
// (Update) меняются на лету — админка включает/выключает экспорт без
// перезапуска.
type Exporter struct {
	log *logging.Logger
	ch  chan proxy.Call

	// live-настройки: RWMutex — Update из обработчиков админки, чтение воркером
	cfgMu sync.RWMutex
	cfg   Config

	client *http.Client

	lastWarn  atomic.Int64  // unix-микросекунды последнего предупреждения (не чаще WarnInterval)
	stopped   atomic.Bool   // Shutdown больше не принимает записи
	done      chan struct{} // сигнал воркеру «финальный слив»
	quit      chan struct{} // воркер закрылся (сбросил очередь)
	closeOnce sync.Once
}

// WarnInterval — не чаще такого интервала предупреждать о дропах/сбоях
// (очередь под нагрузкой не должна забивать лог).
const WarnInterval = time.Minute

// New — экспортер с очередью size (0 — QueueSize); воркер стартует сразу
// (конфиг пуст/выключен — записи дропаются, воркер крутится вхолостую).
func New(log *logging.Logger, size int) *Exporter {
	if size <= 0 {
		size = QueueSize
	}
	e := &Exporter{
		log:  log,
		ch:   make(chan proxy.Call, size),
		done: make(chan struct{}),
		quit: make(chan struct{}),
		client: &http.Client{
			Timeout: PostBudget + 10*time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: PostBudget,
			},
		},
	}
	go e.run()
	return e
}

// Update — новое состояние настроек (админка, мутация langfuse-set);
// действует на лету: следующие батчи пойдут в url нового инстанса, а
// снятие переключателя enabled глушит выгрузку без удаления настроек.
func (e *Exporter) Update(cfg Config) {
	e.cfgMu.Lock()
	e.cfg = cfg
	e.cfgMu.Unlock()
	if log2 := e.log; log2 != nil {
		log2.Debugf("langfuse: настройки обновлены (включен: %v)", cfg.On())
	}
}

// Current — действующие настройки (копия).
func (e *Exporter) Current() Config {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg
}

// Enabled — выгрузка включена (переключатель enabled + url + оба ключа).
func (e *Exporter) Enabled() bool { return e.Current().On() }

// RecordCall — неблокируемая постановка трейса в очередь (proxy.CallSink):
// полная очередь — дроп (warning не чаще WarnInterval). Вызывается из
// обработчика /v1 — блокировать генерацию нельзя ни при каких условиях.
func (e *Exporter) RecordCall(c proxy.Call) {
	if e.stopped.Load() || !e.Enabled() {
		return
	}
	select {
	case e.ch <- c:
	default:
		if e.warnEveryInterval("langfuse: очередь полна — трейс %s не выгружен", e.ref(c)) {
			e.clear()
		}
	}
}

// Shutdown — стоп приёма + ожидание финального слива очереди (не дольше
// wait): вызывается после shutdown HTTP-сервера. Повторный вызов игнорируется.
func (e *Exporter) Shutdown(wait time.Duration) {
	e.closeOnce.Do(func() {
		e.stopped.Store(true)
		close(e.done)
		select {
		case <-e.quit:
		case <-time.After(wait):
			if e.log != nil {
				e.log.Warnf("langfuse: финальный слив не уложился в %s — часть трейсов потеряна", wait)
			}
		}
	})
}

// warnEveryInterval — предупреждение не чаще WarnInterval: true — пора писать
// (и сбросить, clear — очистить канал сброса, чтобы «очередь полна» не мешала).
func (e *Exporter) warnEveryInterval(format string, args ...any) bool {
	now := time.Now().UnixMicro()
	last := e.lastWarn.Load()
	if now-last < int64(WarnInterval) {
		return false
	}
	if !e.lastWarn.CompareAndSwap(last, now) {
		return false
	}
	if e.log != nil {
		e.log.Warnf(format, args...)
	}
	return true
}

// clear — выгребаем очередь, чтобы после сбоя langfuse новые трейсы не
// дропались по полной очереди из старых записей.
func (e *Exporter) clear() {
	for {
		select {
		case <-e.ch:
		default:
			return
		}
	}
}

func (e *Exporter) ref(c proxy.Call) string {
	if c.Model != "" {
		return c.Model
	}
	return c.Path
}

// run — воркер: собирает батч из очереди; флеш по размеру (FlushSize) или
// по таймеру (FlushInterval); по done — финальный слив и выход.
func (e *Exporter) run() {
	defer close(e.quit)
	flushAt := time.NewTicker(FlushInterval)
	defer flushAt.Stop()
	var batch []proxy.Call
	drain := func() []proxy.Call {
		out := batch
		batch = nil
		return out
	}
	for {
		select {
		case c := <-e.ch:
			batch = append(batch, c)
			if len(batch) >= FlushSize {
				e.send(drain())
			}
		case <-flushAt.C:
			if len(batch) > 0 {
				e.send(drain())
			}
		case <-e.done:
			// выгребаем остатки очереди и выходим (RecordCall к этому моменту не пишет)
			for {
				select {
				case c := <-e.ch:
					batch = append(batch, c)
					continue
				default:
					e.send(drain())
					return
				}
			}
		}
	}
}

// send — POST батча OTLP/JSON на {url}/api/public/otel/v1/traces.
// Незнакомые ошибки — warning (не чаще WarnInterval), записи дропаются:
// экспорт не имеет права портить основной поток проксирования.
func (e *Exporter) send(batch []proxy.Call) {
	if len(batch) == 0 {
		return
	}
	cfg := e.Current()
	if !cfg.On() {
		return // выключили на лету (переключатель или настройки) — очередь молча сливается
	}
	payload, err := json.Marshal(otlpPayload(batch, cfg))
	if err != nil {
		e.warnEveryInterval("langfuse: батч не сериализуется — %d трейсов потеряны: %v", len(batch), err)
		return
	}
	endpoint := ingestionEndpoint(cfg.URL)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		e.warnEveryInterval("langfuse: не собрать запрос на %s — %v", endpoint, err)
		return
	}
	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	// актуальный формат ingestion: OTel-данные ложатся на v4-модель без задержки
	req.Header.Set("x-langfuse-ingestion-version", "4")
	resp, err := e.client.Do(req)
	if err != nil {
		e.clear()
		e.warnEveryInterval("langfuse: %s недоступен — %d трейсов потеряны: %v", endpoint, len(batch), err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.clear()
		e.warnEveryInterval("langfuse: %s ответил %d — %d трейсов потеряны", endpoint, resp.StatusCode, len(batch))
		return
	}
	// 207 multi-status: часть событий могла не пройти — перечисляем редко
	var out struct {
		Errors []struct {
			ID      string `json:"id"`
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if len(out.Errors) > 0 {
		e.warnEveryInterval("langfuse: %d из %d трейсов отклонены (статусы 4xx/5xx по отдельным событиям)", len(out.Errors), len(batch))
	}
}

// FlushInterval — частота слива батчей (очередь копится не дольше этого).
const FlushInterval = 2 * time.Second

// ingestionEndpoint — OTLP-эндпоинт из базы инстанса: url с хвостовым «/»
// срезается; если админ уже указал путь с /api/public/otel — как есть,
// иначе добавляем /api/public/otel/v1/traces.
func ingestionEndpoint(base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if u, err := url.Parse(base); err == nil && strings.Contains(u.Path, "/api/public/otel") {
		return base + "/v1/traces"
	}
	return base + "/api/public/otel/v1/traces"
}

// ─── сборка OTLP/JSON ────────────────────────────────────────────────

// attrLimit — потолок значения одного атрибута json-строкой (input/output
// могут быть велики — обрезаем с маркером; langfuse это просто строкой и хранит).
const attrLimit = 160 << 10

// otlpPayload — тело POST: resourceSpans с одним ресурсом/скоупом,
// на каждый Call — один span (root → Langfuse-трейс; langfuse.observation.type
// = generation — наблюдение-генерация). Маппинг атрибутов — langfuse.*-неймспейс
// (см. docs/integrations/native/opentelemetry).
func otlpPayload(calls []proxy.Call, cfg Config) map[string]any {
	spans := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		spans = append(spans, spanOf(c, cfg))
	}
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attrStr("service.name", "any-router")}},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "any-router"},
				"spans": spans,
			}},
		}},
	}
}

// spanOf — один LLM-вызов: трейс по запросу /v1 (root span), атрибуты —
// langfuse.*: наблюдение-generation (модель, ввод/вывод, usage, уровень),
// метаданные трейса (фильтруемые): ip, token_id, провайдер, модель и пр.
// cfg добавляет environment и тэги из настроек (environment на трейсе —
// root span, поэтому достаточно атрибутов этого спана).
func spanOf(c proxy.Call, cfg Config) map[string]any {
	start := c.Started
	if start.IsZero() {
		start = time.Now()
	}
	end := c.End
	if end.Before(start) {
		end = start
	}
	name := c.Model
	if name == "" {
		name = c.Path
	}
	// поле model наблюдения — алиасная форма, как прислал клиент (если есть
	// алиас — алиас, у path-формы — путь); исполнение/путь — в метаданных
	// (upstream_model). Комбинированная форма — имя спана/трейса и model_ref.
	modelField := c.Model
	if _, short, ok := strings.Cut(c.Model, "/"); ok {
		modelField = short
	}
	attrs := []any{
		attrStr("langfuse.observation.type", "generation"),
		attrStr("langfuse.observation.model.name", modelField),
	}
	// трейсовые атрибуты root span → свойства Langfuse-трейса
	if n := strings.TrimPrefix(c.Path, "/v1/"); n != c.Path && n != "" {
		attrs = append(attrs, attrStr("langfuse.trace.name", n))
	}
	// окружение из настроек (фильтр prod/dev в UI, Observations-фильтры)
	if cfg.Environment != "" {
		attrs = append(attrs,
			attrStr("langfuse.environment", cfg.Environment),
			attrStr("deployment.environment", cfg.Environment))
	}
	// тэги — основное поле фильтрации/группировки в списках Langfuse:
	// провайдер, модель (алиасная форма), IP клиента и id API-токена
	// + настроенные в админке. Для запросов через баланс-алиас — и тэг
	// алиаса (alias:<имя>): по нему в Langfuse отделяются обращения
	// через балансировщик от прямых обращений к моделям (те идут
	// только с формой provider/model); provider здесь — обслуживший запрос
	// участник (round-robin), поэтому доп. тэг полезен для группировки.
	tags := []string{
		"any-router",
	}
	if c.Provider != "" {
		tags = append(tags, "provider:"+c.Provider)
	}
	tags = append(tags, "model:"+modelField)
	if c.Alias != "" {
		tags = append(tags, "alias:"+c.Alias)
	}
	if c.IP != "" {
		tags = append(tags, "ip:"+c.IP)
	}
	if c.TokenID != "" {
		tags = append(tags, "key:"+c.TokenID)
	}
	tags = append(tags, cfg.Tags...)
	attrs = append(attrs, attrArr("langfuse.trace.tags", tags))
	if c.TokenID != "" {
		attrs = append(attrs, attrStr("langfuse.user.id", c.TokenID))
	}
	if model := attrJSON(c.Input); model != "" {
		attrs = append(attrs, attrStr("langfuse.observation.input", model))
	}
	if out := attrJSON(c.Output); out != "" {
		attrs = append(attrs, attrStr("langfuse.observation.output", out))
	}
	if usage := usageDetails(c.Usage); usage != "" {
		attrs = append(attrs, attrStr("langfuse.observation.usage_details", usage))
	}
	if params := modelParameters(c.Input); params != "" {
		attrs = append(attrs, attrStr("langfuse.observation.model.parameters", params))
	}
	// метаданные (фильтруемые): и в Langfuse-трейсе (langfuse.trace.metadata.*),
	// и в карточке наблюдения (langfuse.observation.metadata.*) — в v4 эти
	// ключи не пересекаются: trace.metadata не виден на наблюдении.
	// Оставлены как дубли тэгов/источник точных данных: ip/token_id — аудит,
	// model/model_ref/provider — формы модели, upstream_model — путь,
	// реально ушедший в апстрим (если алиас разрешался).
	md := map[string]string{
		"ip":        c.IP,
		"token_id":  c.TokenID,
		"provider":  c.Provider,
		"model":     modelField,
		"model_ref": c.Model,
		"path":      c.Path,
	}
	if c.Alias != "" {
		md["alias"] = c.Alias // баланс-алиас запроса (метаданные — доп. фильтр к тэгу)
	}
	if c.UpstreamModel != "" && c.UpstreamModel != c.Model {
		md["upstream_model"] = c.UpstreamModel
	}
	if c.Stream {
		md["stream"] = "true"
	}
	if c.Truncated {
		md["truncated"] = "true"
	}
	for _, k := range sortedMetadataKeys(md) {
		if v := md[k]; v != "" {
			attrs = append(attrs,
				attrStr("langfuse.trace.metadata."+k, v),
				attrStr("langfuse.observation.metadata."+k, v))
		}
	}
	// уровень/статус — в attrs, затем attributes ставятся в span одним куском
	if c.Failed != "" {
		attrs = append(attrs, attrStr("langfuse.observation.level", "ERROR"),
			attrStr("langfuse.observation.status_message", c.Failed))
	}
	span := map[string]any{
		"traceId":           randHex(16),
		"spanId":            randHex(8),
		"name":              name,
		"kind":              1, // INTERNAL
		"startTimeUnixNano": strconv.FormatInt(start.UnixNano(), 10),
		"endTimeUnixNano":   strconv.FormatInt(end.UnixNano(), 10),
		"attributes":        attrs,
	}
	if c.Failed != "" {
		span["status"] = map[string]any{"code": 2, "message": c.Failed} // STATUS_CODE_ERROR
	} else {
		span["status"] = map[string]any{}
	}
	return span
}

// usageDetails — usageDetails-строка {"input":…,"output":…,"total":…}
// (в нулях не указаны ключи); пусто — токены извлечь не удалось.
func usageDetails(u proxy.Usage) string {
	if u.Input == 0 && u.Output == 0 && u.Total == 0 {
		return ""
	}
	m := map[string]int{"input": u.Input, "output": u.Output}
	if u.Total > 0 {
		m["total"] = u.Total
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// modelParameters — параметры генерации из тела запроса (temperature,
// top_p, max_tokens): langfuse.observation.model.parameters JSON-строкой.
func modelParameters(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	params := map[string]any{}
	for _, k := range []string{"temperature", "top_p", "max_tokens"} {
		if v, ok := m[k]; ok && v != nil {
			params[k] = v
		}
	}
	if len(params) == 0 {
		return ""
	}
	b, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	return string(b)
}

// attrJSON — значение атрибута «(JSON) string»: any → JSON; сверх attrLimit —
// обрезаем с маркером (строка уже не JSON — langfuse покажет её как текст).
func attrJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if len(b) <= attrLimit {
		return string(b)
	}
	return string(b[:attrLimit]) + "…[обрезано langfuse-export]"
}

// attr-конструкторы OTLP-JSON: KeyValue {key, value:{stringValue…}}.
func attrStr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}

func attrArr(k string, vs []string) map[string]any {
	values := make([]any, 0, len(vs))
	for _, v := range vs {
		values = append(values, map[string]any{"stringValue": v})
	}
	return map[string]any{"key": k, "value": map[string]any{"arrayValue": map[string]any{"values": values}}}
}

// sortedMetadataKeys — детерминированный порядок метаданных.
func sortedMetadataKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// randHex — n случайных байт hex-строкой (traceId: 32 hex, spanId: 16 hex).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
