// flavor — диалекты API апстримов-провайдеров. Дефолт — OpenAI-совместимый
// passthrough (ollama, llama.cpp, openai-реле); для провайдеров со своими
// заморочками — узкие конвертеры по хосту (registry, новый формат
// добавляется без переписывания общей логики). Сейчас — mistral:
// api.mistral.ai строгой pydantic-валидацией отвергает любые незнакомые
// поля тела (422 extra_forbidden) и сыплет в SSE-чанки нестандартное
// поле «p» (prefix-hash) — запросы чистятся до белого списка полей,
// нестандартные поля стрима выкидываются из ответа.
package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ITcompressor/any-router/internal/logging"
)

// Flavor — диалект API апстрима («openai» — passthrough).
type Flavor string

const (
	FlavorPlain   Flavor = ""        // OpenAI-совместимый — как есть
	FlavorMistral Flavor = "mistral" // api.mistral.ai — см. sanitizeFlavorRequest
	FlavorZAI     Flavor = "zai"     // api.z.ai — см. zaiCaps
)

// flavorHosts — registry «хост апстрима → flavor». Новый формат
// провайдера — добавить запись (и конвертеры ниже), общая логика не трогается.
var flavorHosts = map[string]Flavor{
	"api.mistral.ai": FlavorMistral,
	"api.z.ai":       FlavorZAI,
}

// FlavorOf — flavor по baseurl провайдера (хост без учёта регистра;
// неразбираемый/незнакомый — passthrough FlavorPlain).
func FlavorOf(baseurl string) Flavor {
	u, err := url.Parse(baseurl)
	if err != nil {
		return FlavorPlain
	}
	if f, ok := flavorHosts[strings.ToLower(u.Hostname())]; ok {
		return f
	}
	return FlavorPlain
}

// mistralChatFields / mistralCompletionsFields — белые списки полей
// (официальная спека api.mistral.ai/v1): всё прочее апстрим отвечает
// 422 «Extra inputs are not permitted», значит — вычищаем до отправки.
var (
	mistralChatFields = map[string]bool{
		"model": true, "messages": true, "temperature": true, "top_p": true,
		"max_tokens": true, "stream": true, "stop": true, "random_seed": true,
		"response_format": true, "tools": true, "tool_choice": true,
		"parallel_tool_calls": true, "safe_prompt": true,
	}
	mistralCompletionsFields = map[string]bool{
		"model": true, "prompt": true, "suffix": true, "temperature": true,
		"top_p": true, "max_tokens": true, "stream": true, "stop": true,
		"random_seed": true,
	}
)

// sanitizeFlavorRequest — подстройка тела запроса под диалект апстрима
// (payload на месте); для FlavorPlain — passthrough. Удалённые поля
// логгируются warning'ом (видно, какие клиенты шлют несовместимое).
func sanitizeFlavorRequest(fl Flavor, rest string, payload map[string]any, log *logging.Logger) {
	var allowed map[string]bool
	switch {
	case fl != FlavorMistral:
		return
	case rest == "/chat/completions":
		allowed = mistralChatFields
	case rest == "/completions":
		allowed = mistralCompletionsFields
	default:
		// embeddings и прочее — Mistral принимает совместимо
		// (input строкой/массивом, encoding_format — проверено живым вызовом)
		return
	}
	for k := range payload {
		if !allowed[k] {
			delete(payload, k)
			log.Warnf("flavor mistral: поле %q апстрим 422-ит (strict) — вычищено из запроса", k)
		}
	}
}

// mistralCaps — маппинг capabilities-объекта из /v1/models mistral
// (булевы поля) в список капабилити (как у llama.cpp/ollama бэкендов):
// в админку просачиваются только совместимые с нашей моделью значения.
// Флага «эмбеддинги» в схеме mistral нет в принципе — у embed-моделей
// все булевы поля false, поддержка кодируется только подстрокой «embed»
// в id: синтезируем капу сами (иначе в агрегате /v1/models пустой список,
// а потребители считают caps неизвестными — показывают чат для
// embeddings-модели).
func mistralCaps(id string, raw map[string]any) []string {
	var out []string
	for k, v := range raw {
		b, ok := v.(bool)
		if !ok || !b {
			continue
		}
		switch k {
		case "completion_chat":
			out = append(out, "completion")
		case "function_calling":
			out = append(out, "tools")
		case "reasoning":
			out = append(out, "thinking")
		case "vision":
			out = append(out, "vision")
		case "audio":
			out = append(out, "audio")
		}
	}
	if strings.Contains(strings.ToLower(id), "embed") {
		out = append(out, "embedding")
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// zaiCaps — capabilities моделей Z.AI (docs.z.ai), синтез по id: список
// /v4/models не отдаёт капы (только id/created/owned_by), /api/show у них
// нет (404 — негативный кэш). По их докам: все GLM-чат-модели умеют
// function calling; thinking (deep thinking) — с GLM-4.6. Не-чат-модели
// (image/video/asr/ocr — свои маршруты, не /chat/completions) и чужие
// префиксы id — без кап (потребители считают их неизвестными).
func zaiCaps(id string) []string {
	l := strings.ToLower(id)
	switch {
	case strings.Contains(l, "embed"):
		return []string{"embedding"}
	case strings.Contains(l, "image") || strings.Contains(l, "video") ||
		strings.Contains(l, "asr") || strings.Contains(l, "ocr") ||
		strings.Contains(l, "whisper"):
		return nil
	case !strings.HasPrefix(l, "glm"):
		return nil
	}
	out := []string{"completion", "tools"}
	if glmVersionAtLeast(id, 4, 6) {
		out = append(out, "thinking")
	}
	sort.Strings(out)
	return out
}

// glmVersionAtLeast — версия GLM-модели по id ("glm-4.5-air" → 4.5,
// "glm-5-turbo" → 5.0): не меньше wantMajor.wantMinor.
func glmVersionAtLeast(id string, wantMajor, wantMinor int) bool {
	s := strings.TrimPrefix(strings.ToLower(id), "glm-")
	parts := strings.SplitN(s, ".", 2)
	major, ok := leadingInt(parts[0])
	if !ok {
		return false
	}
	if major != wantMajor {
		return major > wantMajor
	}
	minor := 0
	if len(parts) > 1 {
		if m, ok2 := leadingInt(parts[1]); ok2 {
			minor = m
		}
	}
	return minor >= wantMinor
}

// leadingInt — ведущие цифры строки ("5-air" → 5); без цифр — ok=false.
func leadingInt(s string) (int, bool) {
	n, digits := 0, 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	return n, true
}

// copyResponse — статус + фильтрованные заголовки + тело апстрима клиенту.
// SSE/стримы: каждый чанк отдаётся сразу (flush). Для flavor-провайдеров —
// трансформ-копия: у mistral в SSE-чанках нестандартное поле «p»
// (prefix-hash) — выкидывается, чтобы клиент получал канон OpenAI;
// не-стримовые ответы mistral совместимы — passthrough.
func copyResponse(w http.ResponseWriter, resp *http.Response, fl Flavor) {
	for k, vv := range resp.Header {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	if fl == FlavorMistral && looksSSE(resp) {
		copySSEFlat(w, resp.Body, flusher, canFlush)
		_ = resp.Body.Close()
		return
	}
	r := bufio.NewReader(resp.Body)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
}

// looksSSE — поток ли это: content-type text/event-stream (либо его нет,
// но тело начинается с data:) — решаем по заголовку (тело не трогаем,
// чтобы не ломать буферизацию).
func looksSSE(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "text/event-stream")
}

// copySSEFlat — построчная SSE-прокачка с чисткой нестандартных полей:
// «data: {…}» — JSON-чанк (поле top-level «p» у mistral выкидывается,
// чанк пересобирается), «data: [DONE]» и прочие строки — как есть.
// По строке write+flush: клиент видит генерацию в реальном времени.
func copySSEFlat(w http.ResponseWriter, body interface{ Read([]byte) (int, error) }, flusher http.Flusher, canFlush bool) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		out := sc.Text()
		out = strings.TrimSuffix(out, "\r")
		if payload, ok := strings.CutPrefix(out, "data: "); ok && payload != "[DONE]" {
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if _, has := chunk["p"]; has {
					delete(chunk, "p")
					if b, err := json.Marshal(chunk); err == nil {
						out = "data: " + string(b)
					}
				}
			}
		}
		if _, err := w.Write([]byte(out + "\n")); err != nil {
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}
}
