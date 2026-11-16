// call — сведения об LLM-запросе для неблокирующего экспорта (langfuse и
// тому подобные приёмники): proxy маршрутизации собирает Call по ходу
// запроса (IP клиента, токен, провайдер, модель, ввод/вывод, usage) и
// отдаёт в CallSink; sink обязан не блокировать обработку (langfuse —
// очередь в памяти). Экспорт включается вкладкой «Langfuse» админки.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// CallSink — приёмник готовых Call (реализует langfuse.Exporter). nil-значение
// интерфейса — экспорт выключен.
type CallSink interface {
	RecordCall(Call)
}

// Usage — токены запроса (OpenAI-формат): input/output/total; нули — не
// извлеклись (стрым апстримам можно и не знать usage — не критично).
type Usage struct {
	Input  int
	Output int
	Total  int
}

// Call — сведения об одном запросе /v1 (chat/completions, completions,
// embeddings, systemone): собирается в proxy.Chat и уходит в sink после
// завершения (или сбоя) проксирования.
type Call struct {
	// контекст запроса (заполняется в начале)
	IP       string // откуда пришёл запрос (X-Real-IP → хвост X-Forwarded-For → RemoteAddr)
	TokenID  string // id API-токена ("" — админ-токен или вызов без токена)
	Alias    string // баланс-алиас запроса ("" — прямой запрос provider/model)
	Provider string // провайдер из конфига (для алиаса — обслужившее попытку)
	Model    string // модель, как прислал клиент: provider/model или голое имя алиаса
	Path     string // путь /v1/...
	Stream   bool   // stream: true в теле запроса
	Started  time.Time

	// результат (заполняется по ходу проксирования)
	UpstreamModel string // модель, реально ушедшая в апстрим (путь/алиас из кэша авто-алиасов)
	End           time.Time
	Input         any    // тело запроса клиента (JSON)
	Output        any    // ответ апстрима: JSON-объект или агрегированный текст SSE
	Usage         Usage  // токены из ответа
	Truncated     bool   // ответ/ввод обрезаны потолком captureLimit
	Failed        string // непустой — ошибка маршрутизации/статус апстрима (level ERROR)
}

// captureLimit — потолок накопления тел для экспорта (буфер capture);
// сверх — усечение с флагом Truncated.
const captureLimit = 192 << 10

// capture — накопитель тела апстрима для экспорта: буфер с потолком,
// статус апстрима, признак SSE. Собирается через teeResponseWriter в
// forward — тела уже после flavor-конвертации, как увидел клиент.
// Пустые апстримные тела (SSE может прилететь куском) не экспортируются.
type capture struct {
	status    int
	stream    bool
	buf       bytes.Buffer
	truncated bool
}

// begin — статус и потоковость ответа апстрима (до копирования).
func (c *capture) begin(resp *http.Response) {
	c.status = resp.StatusCode
	c.stream = looksSSE(resp)
}

// add — кусок тела в буфер (с потолком; overflow — флаг).
func (c *capture) add(p []byte) {
	if len(p) == 0 {
		return
	}
	rem := captureLimit - c.buf.Len()
	if rem <= 0 {
		c.truncated = true
		return
	}
	if len(p) > rem {
		c.buf.Write(p[:rem])
		c.truncated = true
		return
	}
	c.buf.Write(p)
}

// finish — разбор накопленного в поля Call (Output/Usage/Truncated):
// SSE — агрегированный текст генерации + usage последнего чанка;
// JSON — объект (embeddings, не-стримовые чаты) + usage; прочее — строкой.
func (c *capture) finish(call *Call) {
	if c.status >= http.StatusBadRequest && call.Failed == "" {
		call.Failed = fmt.Sprintf("апстрим ответил %d", c.status)
	}
	call.Truncated = c.truncated
	b := c.buf.Bytes()
	if len(b) == 0 {
		return
	}
	if c.stream {
		text, usage := aggregateSSE(b)
		if text != "" {
			call.Output = text
		} else {
			call.Output = string(b) // не разобралось — сырой (уже обрезанный) текст
		}
		call.Usage = usage
		return
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		call.Output = string(b)
		return
	}
	call.Output = body
	call.Usage = usageOf(body["usage"])
}

// aggregateSSE — сборка текста генерации из OpenAI-совместимых SSE-чанков:
// chat — choices[0].delta.content, completions — choices[0].text (контент
// может быть массивом частей — склеиваем text части); usage — из финального
// чанка (есть у апстримов с stream_options.include_usage; иначе нули).
// Неразбираемые чанки пропускаются — экспорт лишь наилучший-усилиями.
func aggregateSSE(b []byte) (string, Usage) {
	var sb strings.Builder
	var usage Usage
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
				Text json.RawMessage `json:"text"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			var m map[string]any
			if json.Unmarshal(chunk.Usage, &m) == nil {
				usage = usageOf(m)
			}
		}
		for _, ch := range chunk.Choices {
			sb.WriteString(contentText(ch.Delta.Content))
			sb.WriteString(contentText(ch.Text))
		}
	}
	out := sb.String()
	if u := usage; u.Input == 0 && u.Output == 0 && u.Total == 0 {
		return out, Usage{}
	}
	return out, usage
}

// contentText — контент чанка в строку: строка — как есть; массив частей
// (мультимодальные) — склейка text-полей; прочее — пусто.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		out := ""
		for _, p := range parts {
			out += p.Text
		}
		return out
	}
	return ""
}

// usageOf — usage OpenAI-формата из JSON-объекта (prompt_tokens/
// completion_tokens/total_tokens или input_tokens/output_tokens);
// total достраивается суммой, если апстрим не дал.
func usageOf(raw any) Usage {
	m, ok := raw.(map[string]any)
	if !ok {
		return Usage{}
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			if f, ok := m[k].(float64); ok {
				return int(f)
			}
		}
		return 0
	}
	u := Usage{
		Input:  num("prompt_tokens", "input_tokens"),
		Output: num("completion_tokens", "output_tokens"),
		Total:  num("total_tokens"),
	}
	if u.Total == 0 && (u.Input > 0 || u.Output > 0) {
		u.Total = u.Input + u.Output
	}
	return u
}

// teeResponseWriter — ResponseWriter-обёртка: копия тела в capture.
// Header/WriteHeader/Flush — пробросом во вложенный (copyResponse требует
// http.Flusher).
type teeResponseWriter struct {
	http.ResponseWriter
	cap *capture
}

func (t teeResponseWriter) Write(p []byte) (int, error) {
	t.cap.add(p)
	return t.ResponseWriter.Write(p)
}

func (t teeResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ClientIP — откуда пришёл запрос: X-Real-IP (наш nginx его перезаписывает
// значением $remote_addr — клиентский заголовок с этим именем подделкой не
// проходит), затем ХВОСТ X-Forwarded-For (последний прыжок — то, что видела
// наша инфраструктура; левый край цепочки клиент может подделать), иначе
// RemoteAddr без порта (прямое обращение, без прокси). Принимаем только
// валидные IP-адреса.
func ClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); validIP(ip) {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.LastIndex(xff, ","); i >= 0 {
			xff = xff[i+1:]
		}
		if ip := strings.TrimSpace(xff); validIP(ip) {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// validIP — непустой валидный IP.
func validIP(s string) bool {
	return s != "" && net.ParseIP(s) != nil
}
