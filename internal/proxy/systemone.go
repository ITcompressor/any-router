// systemone — Decision API (/v1/systemone) и его fallback-имитация
// («decision-mode»): модель с capability decision идёт в Decision API
// провайдера как есть; модель без (или с неизвестным набором кап —
// реле, которое на /systemone отвечает ошибкой) — те же вопросы решаются
// обычным чатом со structured output. Клиент формат не меняет: запрос и
// ответ — SystemOneRequest/SystemOneResponse (см. OpenAPI-спеку Ollama
// в монорепе, схемы SystemOneRequest/SystemOneResponse).
//
// Паттерн имитации давно отработан в chat-клиентах Decision API:
//   - промпт «состояние + вопросы + единый JSON-ответ»;
//   - response_format json_schema по типам вопросов (enum choice — из
//     criteria, диапазон score — из длины шкалы), при отказе апстрима —
//     повтор обычным чатом и устойчивый разбор JSON из текста;
//   - семантика ответов как в Decision API: score чата — оценка 1..N,
//     в API-ответе — уровень 0..N-1 (оценка = уровень + 1).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ITcompressor/any-router/internal/logging"
)

// ModelCapsSource — опциональный источник capabilities конкретной модели
// (реализует web.Server поверх ModelsCache). Не-имплементация (старые
// вызовы, тесты) — капы считаются неизвестными: /v1/systemone идёт
// passthrough, fallback срабатывает по ошибке апстрима.
type ModelCapsSource interface {
	// ModelCaps — capabilities модели provider/short из кэша;
	// known=false — формы в кэше нет или капы провайдер не отдаёт.
	ModelCaps(provider, short string) (caps []string, known bool)
}

// decisionCap — capability, которым помечены decision-модели
// (ollama /api/show, llama.cpp /v1/models).
const decisionCap = "decision"

// SystemOneQuestion — вопрос Decision API (choice | noul | score).
type SystemOneQuestion struct {
	Type         string `json:"type"` // choice | noul | score
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// SystemOneAnswer — ответ на один вопрос; поля зависят от типа.
// Указатели — чтобы отличать «нет поля» от осмысленного нуля.
type SystemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// SystemOneResponse — ответ Decision API клиенту (и цель чат-имитации).
type SystemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]SystemOneAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// SystemOneRequest — запрос клиента к /v1/systemone.
type SystemOneRequest struct {
	Model     string                       `json:"model"`
	State     json.RawMessage              `json:"state"`
	Questions map[string]SystemOneQuestion `json:"questions"`
}

// systemoneHandle — Decision API для одной попытки в proxy.Chat:
//   - модель с капой decision (или капы неизвестны) — buffered-вызов
//     /systemone апстрима, при успехе прозрачный ответ клиенту;
//   - модель без капы (или апстрим не отвечает на /systemone,
//     если капы неизвестны) — имитация чатом со structured output
//     (decision-mode). Ответ клиенту всегда в формате Decision API.
//
// capd/call — экспорт langfuse: завершение capture и поля Mode/Output/Usage
// заполняются здесь (успех); при ошибке попытки (фейловер алиаса) —
// call.Failed и возврат Err наружу, без finish (как у forward).
func systemoneHandle(w http.ResponseWriter, r *http.Request, up Upstream,
	payload, attemptBody map[string]any, resolved, provider, short string,
	res ProviderSource, capd *capture, call *Call, log *logging.Logger,
) error {
	// 1. Капы модели из кэша (optional-интерфейс: тесты/старые вызовы —
	// unknown). Известное отсутствие decision — имитация сразу, апстрим
	// не дёргаем; известное наличие — passthrough без fallback'а
	// («decision-модель идет как идет»); неизвестные капы — passthrough
	// с fallback'ом по ошибке апстрима.
	var capsKnown, hasDecision, knownNoDecision bool
	if cs, okc := res.(ModelCapsSource); okc {
		if caps, known := cs.ModelCaps(provider, short); known {
			capsKnown = true
			hasDecision = slices.Contains(caps, decisionCap)
			knownNoDecision = !hasDecision
		}
	}
	if knownNoDecision {
		if call != nil {
			call.Mode = CallModeChat
		}
		return systemoneImitate(w, r.Context(), up, attemptBody, resolved, provider, short, capd, call, log)
	}

	// 2. Passthrough Decision API (буферизованно: ответ — обычный JSON,
	// не SSE; статус нужен до записи клиенту — решение о fallback).
	url := strings.TrimSuffix(up.BaseURL, "/") + "/systemone"
	pb, _ := json.Marshal(attemptBody)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(pb))
	if err != nil {
		return errBad("не собрать запрос /systemone: %v", err)
	}
	req.ContentLength = int64(len(pb))
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, up)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return errUp("апстрим %s недоступен: %v", up.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return errUp("не читается ответ /systemone: %v", err)
	}

	// 2a. Успех — прозрачный ответ (как увидел бы клиент напрямую).
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		if call != nil {
			call.Mode = CallModeAPI
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(raw); err != nil {
			if log != nil {
				log.Warnf("systemone: не отдаётся ответ клиенту: %v", err)
			}
		}
		if capd != nil {
			capd.status = resp.StatusCode
			capd.add(raw)
			capd.finish(call)
		}
		return nil
	}

	// 2b. Ошибка апстрима: у decision-модели — наружу как есть («идет как
	// идет»); у модели с неизвестными капами ошибка 404/405 трактует
	// /systemone как нереализованный маршрут реле — fallback на чат.
	if call != nil {
		call.Failed = fmt.Sprintf("апстрим /systemone ответил %d", resp.StatusCode)
	}
	if capsKnown && hasDecision {
		return errUp("/systemone апстрима %s ответил %d: %.200s", up.BaseURL, resp.StatusCode, string(raw))
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		if log != nil {
			log.Infof("systemone: %s/%s не отвечает /systemone (%d) — decision-mode через чат", provider, short, resp.StatusCode)
		}
		if call != nil {
			call.Mode = CallModeChat
		}
		return systemoneImitate(w, r.Context(), up, attemptBody, resolved, provider, short, capd, call, log)
	}
	return errUp("/systemone апстрима %s ответил %d: %.200s", up.BaseURL, resp.StatusCode, string(raw))
}

// Call modes для экспорта (langfuse): Call.Mode.
const (
	CallModeAPI  = "api"  // прозрачно в Decision API апстрима
	CallModeChat = "chat" // fallback-имитация чатом (decision-mode)
)

// systemoneImitate — decision-mode: чат со structured output вместо
// Decision API. Ответ клиенту — SystemOneResponse в формате Decision API.
func systemoneImitate(w http.ResponseWriter, ctx context.Context, up Upstream,
	attemptBody map[string]any, resolved, provider, short string, capd *capture, call *Call, log *logging.Logger,
) error {
	var req SystemOneRequest
	if raw, err := json.Marshal(attemptBody); err != nil {
		return errBad("systemone: тело не переименовывается: %v", err)
	} else if err := json.Unmarshal(raw, &req); err != nil {
		return errBad("systemone: тело не в формате SystemOneRequest: %v", err)
	}
	state, err := systemoneState(req.State)
	if err != nil {
		return errBad("systemone: %v", err)
	}
	names := systemoneSortedNames(req.Questions)
	prompt := systemoneChatPrompt(state, names, req.Questions)
	schema, err := systemoneChatSchema(names, req.Questions)
	if err != nil {
		return errBad("systemone: %v", err)
	}

	// Попытка 1: structured output (response_format json_schema).
	// Попытка 2 (при отказе апстрима): обычный чат — формат JSON требует
	// промпт, из текста ответа JSON извлекает скобочный сканер.
	var content string
	var usage Usage
	schemaBody := systemoneChatBody(resolved, prompt, schema)
	content, usage, err = upstreamChatCompletion(ctx, up, schemaBody)
	if err != nil {
		if log != nil {
			log.Infof("systemone: structured output не удался (%v) — повтор чатом без response_format", err)
		}
		plainBody := systemoneChatBody(resolved, prompt, nil)
		content, usage, err = upstreamChatCompletion(ctx, up, plainBody)
		if err != nil {
			return errUp("systemone fallback: %v", err)
		}
	}

	resp, err := systemoneParseReply(content, names, req.Questions)
	if err != nil {
		return errUp("systemone fallback: %v", err)
	}
	resp.Model = provider + "/" + short
	resp.Usage.InputTokens = int64(usage.Input)
	resp.Usage.OutputTokens = int64(usage.Output)

	out, err := json.Marshal(resp)
	if err != nil {
		return errUp("systemone fallback: %v", err)
	}
	if call != nil {
		call.UpstreamModel = resolved
		var pretty map[string]any
		if json.Unmarshal(out, &pretty) == nil {
			call.Output = pretty
		}
		call.Usage = usage
	}
	if capd != nil {
		capd.status = http.StatusOK
		capd.add(out)
		capd.finish(call)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil && log != nil {
		log.Warnf("systemone: не отдаётся ответ клиенту: %v", err)
	}
	return nil
}

// systemoneState — поле state запроса (string | object) в строку промпта.
func systemoneState(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	// Объект/массив контекста — отдаём модели как есть в JSON.
	return string(raw), nil
}

// systemoneSortedNames — имена вопросов в детерминированном порядке
// (промпт и схема не должны зависеть от порядка ключей карты).
func systemoneSortedNames(questions map[string]SystemOneQuestion) []string {
	names := make([]string, 0, len(questions))
	for name := range questions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// systemoneCriteriaMap — criteria вопроса как карта «вариант → описание»
// (choice/noul); criteria другого вида — nil.
func systemoneCriteriaMap(q SystemOneQuestion) map[string]string {
	switch m := q.Criteria.(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = fmt.Sprintf("%v", v)
		}
		return out
	}
	return nil
}

// systemoneCriteriaList — criteria вопроса как список уровней шкалы (score).
func systemoneCriteriaList(q SystemOneQuestion) []string {
	switch l := q.Criteria.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, v := range l {
			out = append(out, fmt.Sprintf("%v", v))
		}
		return out
	}
	return nil
}

// systemoneCriteriaKeys — ключи карты вариантов по возрастанию.
func systemoneCriteriaKeys(criteria map[string]string) []string {
	keys := make([]string, 0, len(criteria))
	for k := range criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// systemoneChatPromptHeader — общая инструкция chat-промпта decision-вопросов

const systemoneChatPromptHeader = `Ты отвечаешь на вопросы по заданному состоянию (выбор варианта, вероятность или уровень шкалы).
Весь ответ — ЕДИНЫЙ JSON-объект в формате из раздела «Формат ответа»: без пояснений, без Markdown-разметки, без текста до или после JSON.`

// systemoneChatPrompt — промпт: состояние, вопросы и правила ответа.
func systemoneChatPrompt(state string, names []string, questions map[string]SystemOneQuestion) string {
	var b strings.Builder
	b.WriteString(systemoneChatPromptHeader)
	b.WriteString("\n\n# Состояние (state)\n\n")
	b.WriteString(strings.TrimSpace(state))
	b.WriteString("\n\n# Вопросы\n")
	for _, name := range names {
		q := questions[name]
		b.WriteString(fmt.Sprintf("\n## Вопрос %q\n", name))
		if q.Instructions != "" {
			b.WriteString(strings.TrimSpace(q.Instructions))
			b.WriteString("\n")
		}
		switch q.Type {
		case "choice":
			cmap := systemoneCriteriaMap(q)
			b.WriteString("\nВарианты (ключ — описание):\n")
			for _, k := range systemoneCriteriaKeys(cmap) {
				b.WriteString(fmt.Sprintf("- %q — %s\n", k, cmap[k]))
			}
			b.WriteString("\nОтвет по этому вопросу: {\"choice\": <ключ варианта>, " +
				"\"probabilities\": {<ключ>: <уверенность 0..1>, ...}} — выбор одного " +
				"варианта и уверенность по каждому; ключи вероятностей — только из списка.\n")
		case "noul":
			if criteria := systemoneCriteriaMap(q); len(criteria) > 0 {
				b.WriteString("\nИсходы:\n")
				for _, k := range systemoneCriteriaKeys(criteria) {
					b.WriteString(fmt.Sprintf("- %q — %s\n", k, criteria[k]))
				}
			}
			b.WriteString("\nОтвет по этому вопросу: {\"noul\": <число 0..1>} — вероятность " +
				"истинности (0 — точно ложь, 1 — точно истина).\n")
		case "score":
			levels := systemoneCriteriaList(q)
			b.WriteString("\nУровни шкалы (оценка — описание):\n")
			for i, lvl := range levels {
				b.WriteString(fmt.Sprintf("- %d — %s\n", i+1, lvl))
			}
			b.WriteString(fmt.Sprintf("\nОтвет по этому вопросу: {\"score\": <целое 1..%d>} — "+
				"номер уровня шкалы.\n", len(levels)))
		}
	}
	b.WriteString("\n# Формат ответа\n\nЕдиный JSON-объект: ключи верхнего уровня — имена " +
		"вопросов (\"..\") в двойных кавычках, значения — ответы на них.")
	if len(names) == 1 {
		b.WriteString(fmt.Sprintf("\nНапример: {\"%s\": %s}\n", names[0], systemoneAnswerExample(questions[names[0]])))
	}
	return b.String()
}

// systemoneAnswerExample — пример ответа на один вопрос (в фигурных скобках).
func systemoneAnswerExample(q SystemOneQuestion) string {
	switch q.Type {
	case "choice":
		keys := systemoneCriteriaKeys(systemoneCriteriaMap(q))
		if len(keys) == 0 {
			return "{\"choice\": \"вариант\", \"probabilities\": {\"вариант\": 1}}"
		}
		probs := make([]string, 0, len(keys))
		for _, k := range keys {
			probs = append(probs, strconv.Quote(k)+": 0.5")
		}
		return fmt.Sprintf("{\"choice\": %q, \"probabilities\": {%s}}", keys[0], strings.Join(probs, ", "))
	case "noul":
		return "{\"noul\": 0.75}"
	default:
		return "{\"score\": 5}"
	}
}

// systemoneChatSchema — JSON-схема response_format: по типам вопросов
// (enum choice — из criteria, диапазон score — из длины шкалы).
func systemoneChatSchema(names []string, questions map[string]SystemOneQuestion) (json.RawMessage, error) {
	props := map[string]any{}
	for _, name := range names {
		q := questions[name]
		var answer map[string]any
		switch q.Type {
		case "choice":
			criteria := systemoneCriteriaMap(q)
			if len(criteria) == 0 {
				return nil, fmt.Errorf("вопрос %q: choice без criteria", name)
			}
			keys := systemoneCriteriaKeys(criteria)
			probs := map[string]any{}
			for _, k := range keys {
				probs[k] = map[string]any{"type": "number", "minimum": 0, "maximum": 1}
			}
			answer = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"choice": map[string]any{"type": "string", "enum": keys},
					"probabilities": map[string]any{
						"type":                 "object",
						"properties":           probs,
						"additionalProperties": false,
					},
				},
				"required": []string{"choice"},
			}
		case "noul":
			answer = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noul": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
				"required": []string{"noul"},
			}
		case "score":
			n := len(systemoneCriteriaList(q))
			if n == 0 {
				return nil, fmt.Errorf("вопрос %q: score без criteria", name)
			}
			answer = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"score": map[string]any{"type": "integer", "minimum": 1, "maximum": n},
				},
				"required": []string{"score"},
			}
		default:
			return nil, fmt.Errorf("вопрос %q: неизвестный тип %q", name, q.Type)
		}
		props[name] = answer
	}
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             names,
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("собрать JSON-схему ответа: %w", err)
	}
	return raw, nil
}

// systemoneChatBody — тело /v1/chat/completions для имитации; schema —
// nil означает повтор без structured output.
func systemoneChatBody(model, prompt string, schema json.RawMessage) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	}
	if schema != nil {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "decision_answer",
				"schema": json.RawMessage(schema),
			},
		}
	}
	return body
}

// upstreamChatCompletion — buffered-вызов /chat/completions апстрима:
// статус → контент choices[0].message.content и usage. Общие поля flavor
// чистят так же, как в forward (sanitizeFlavorRequest).
func upstreamChatCompletion(ctx context.Context, up Upstream, body map[string]any) (string, Usage, error) {
	sanitizeFlavorRequest(FlavorOf(up.BaseURL), "chat/completions", body, nil)
	raw, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("тело не собирается: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(up.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", Usage{}, fmt.Errorf("не собрать запрос: %w", err)
	}
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, up)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("апстрим недоступен: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", Usage{}, fmt.Errorf("не читается ответ: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", Usage{}, fmt.Errorf("статус %d: %.200s", resp.StatusCode, string(rb))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return "", Usage{}, fmt.Errorf("ответ не JSON: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("в ответе нет choices")
	}
	content := contentText(parsed.Choices[0].Message.Content)
	if strings.TrimSpace(content) == "" {
		return "", Usage{}, fmt.Errorf("в ответе пустой content")
	}
	return content, usageOf(parsed.Usage), nil
}

// systemoneParseReply — разбор ответа модели в SystemOneResponse.
//
// Поддерживаются два вида ответа:
//   - вложенный (по схеме): {"<вопрос>": {"choice"/"noul"/"score": ...}, ...};
//   - плоский: числа прямо в ключах вопросов (только score-вопросы).
func systemoneParseReply(reply string, names []string, questions map[string]SystemOneQuestion) (SystemOneResponse, error) {
	obj, err := extractJSONObject(reply)
	if err != nil {
		return SystemOneResponse{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(obj, &top); err != nil {
		return SystemOneResponse{}, fmt.Errorf("ответ содержит не объект: %w", err)
	}
	resp := SystemOneResponse{Answers: map[string]SystemOneAnswer{}}
	for _, name := range names {
		q := questions[name]
		rawAnswer, ok := top[name]
		if !ok {
			missing := make([]string, 0, len(top))
			for k := range top {
				missing = append(missing, k)
			}
			sort.Strings(missing)
			return SystemOneResponse{}, fmt.Errorf("в ответе нет вопроса %q (есть: %v)", name, missing)
		}
		answer, err := systemoneDecodeAnswer(q, rawAnswer)
		if err != nil {
			return SystemOneResponse{}, fmt.Errorf("вопрос %q: %w", name, err)
		}
		resp.Answers[name] = answer
	}
	return resp, nil
}

// systemoneDecodeAnswer — ответ на один вопрос в SystemOneAnswer.
func systemoneDecodeAnswer(q SystemOneQuestion, raw json.RawMessage) (SystemOneAnswer, error) {
	answer := SystemOneAnswer{Type: q.Type}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		// Плоский ответ: значение — сразу число (только score-вопросы).
		if q.Type == "score" {
			if v, ok := systemoneNumber(strings.TrimSpace(string(raw))); ok {
				answer.Score = systemoneLevel(v, len(systemoneCriteriaList(q)))
				return answer, nil
			}
		}
		return answer, fmt.Errorf("ответ не объект: %.100s", string(raw))
	}
	switch q.Type {
	case "choice":
		choice, _ := body["choice"].(string)
		if choice = systemoneMatchCriteria(choice, systemoneCriteriaKeys(systemoneCriteriaMap(q))); choice == "" {
			return answer, fmt.Errorf("choice %q не входит в варианты", body["choice"])
		}
		answer.Choice = choice
		answer.Probabilities = systemoneProbabilities(body["probabilities"])
	case "noul":
		v, ok := systemoneNumber(body["noul"])
		if !ok {
			return answer, fmt.Errorf("в ответе нет поля 'noul'")
		}
		answer.Noul = systemoneF(clampF(v, 0, 1))
	case "score":
		v, ok := systemoneNumber(body["score"])
		if !ok {
			return answer, fmt.Errorf("в ответе нет поля 'score'")
		}
		// Чат отвечает оценкой 1..N, Decision API — уровнем 0..N-1
		// (оценка = уровень + 1): пересчитываем, чтобы семантика ответа
		// совпала с нативным Decision API.
		answer.Score = systemoneLevel(v, len(systemoneCriteriaList(q)))
	}
	return answer, nil
}

// systemoneMatchCriteria — ключ варианта: точное совпадение; фолбэки —
// регистр и крайние знаки ("Anger." → "anger"); иначе пустая строка (ошибка).
func systemoneMatchCriteria(choice string, keys []string) string {
	for _, k := range keys {
		if choice == k {
			return k
		}
	}
	trimmed := strings.Trim(strings.TrimSpace(strings.ToLower(choice)), ".,!?:;\"'`")
	for _, k := range keys {
		if trimmed == strings.ToLower(k) {
			return k
		}
	}
	return ""
}

// systemoneProbabilities — вероятности по вариантам; нечисловые значения
// отбрасываются.
func systemoneProbabilities(raw any) map[string]float64 {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]float64{}
	for k, v := range m {
		if f, ok := systemoneNumber(v); ok {
			out[k] = f
		}
	}
	return out
}

// systemoneNumber — число из JSON-значения (float64/json.Number/строка).
func systemoneNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case json.Number:
		if f, err := n.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
	}
	return 0, false
}

// systemoneLevel — оценка 1..N → уровень 0..N-1 с привязкой к диапазону.
func systemoneLevel(v float64, n int) *float64 {
	level := clampF(v-1, 0, math.Max(float64(n-1), 0))
	return systemoneF(level)
}

// systemoneF — указатель на float64.
func systemoneF(v float64) *float64 { return &v }

// clampF — значение в диапазоне [lo, hi].
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// extractJSONObject — первый валидный JSON-объект из текста ответа
// (скобочный сканер с пропуском строковых литералов): строки
// пропускаются целиком, каждый сбалансированный объект проверяется
// json.Valid; невалидные кандидаты пропускаются.
func extractJSONObject(text string) (json.RawMessage, error) {
	var (
		depth int
		start = -1
	)
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"':
			i += skipQuoted(text[i:]) - 1
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}' && depth > 0:
			depth--
			if depth == 0 && start >= 0 {
				if candidate := text[start : i+1]; json.Valid([]byte(candidate)) {
					return json.RawMessage(candidate), nil
				}
				start = -1
			}
		}
	}
	return nil, fmt.Errorf("в ответе модели нет валидного JSON-объекта: %.200q", strings.TrimSpace(text))
}

// skipQuoted — длина строкового литерала с экранированием (со стартовой
// кавычки до закрывающей включительно); незакрытая строка — до конца текста.
func skipQuoted(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(s)
}
