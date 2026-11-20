// systemone_test — тесты Decision API /v1/systemone: passthrough у
// decision-моделей и fallback-имитация чатом («decision-mode») у моделей
// без/с неизвестными капами; парсер ответов чата и генерация схемы.
package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ITcompressor/any-router/internal/logging"
)

// errLogs — сток для логгов тестов (тихо в никуда).
type errLogs struct {
	strings.Builder
}

// capsMock — ProviderSource + ModelCapsSource: капы заданы картой
// «модель → список кап»; известное отсутствие в карте известно только
// для ключей, помеченных в known.
type capsMock struct {
	res     resolverMock
	caps    map[string][]string
	request func(provider, short string) bool
}

func (m capsMock) Upstream(provider string) (Upstream, bool) { return m.res.Upstream(provider) }
func (m capsMock) ProviderNames() []string                   { return m.res.ProviderNames() }
func (m capsMock) ModelCaps(provider, short string) ([]string, bool) {
	if m.request != nil {
		m.request(provider, short)
	}
	key := provider + "/" + short
	caps, ok := m.caps[key]
	if !ok {
		return nil, false
	}
	return caps, true
}

// systemoneUpstream — фейковый апстрим Decision API + чат.
// systemone — обработчик POST /systemone; chat — обработчик
// POST /chat/completions (nil — 500). Счётчики вызовов — для проверок.
type systemoneUpstreams struct {
	systemOneCalls int
	chatCalls      int
	systemOne      func(w http.ResponseWriter, r *http.Request)
	chat           func(w http.ResponseWriter, r *http.Request)
}

func (u *systemoneUpstreams) upstream(t *testing.T) (Upstream, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/systemone":
			u.systemOneCalls++
			u.systemOne(w, r)
		case r.URL.Path == "/v1/chat/completions":
			u.chatCalls++
			u.chat(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Upstream{BaseURL: srv.URL + "/v1", APIKey: "up-key"}, srv
}

// systemoneRequest — тело клиента /v1/systemone: один choice-вопрос.
func systemoneRequest(model string) string {
	return `{"model":"` + model + `","state":"утро, солнце светит","questions":{"mood":{"type":"choice","instructions":"настроение","criteria":{"good":"радость","bad":"грусть"}}}}`
}

// chatAnswer — OpenAI-чата ответ с заданным content (JSON-строка).
func chatAnswer(content string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + jsonString(content) + `}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
}

// jsonString — строка в JSON-литерал.
func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

const moodOK = `{"mood":{"type":"choice","choice":"good","probabilities":{"good":0.9,"bad":0.1}}}`

// TestSystemOnePassthroughCapable — decision-модель (капа известна) идёт
// в Decision API прозрачно; чат не дёргается.
func TestSystemOnePassthroughCapable(t *testing.T) {
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"m1","answers":{"mood":{"type":"choice","choice":"bad"}},"usage":{"input_tokens":5,"output_tokens":2}}`))
		},
		chat: func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("чат не должен вызываться у decision-модели")
		},
	}
	up, _ := u.upstream(t)
	res := capsMock{res: resolverMock{"prov": up}, caps: map[string][]string{"prov/m1": {"completion", decisionCap}}}
	sink := &sinkStub{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	if err := Chat(rec, req, res, testLogger(), nil, "", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if u.systemOneCalls != 1 || u.chatCalls != 0 {
		t.Fatalf("systemone=%d chat=%d", u.systemOneCalls, u.chatCalls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"choice":"bad"`) {
		t.Fatalf("passthrough потерял апстримное тело: %s", rec.Body.String())
	}
	if len(sink.calls) != 1 {
		t.Fatalf("Call не записан: %d", len(sink.calls))
	}
	if mode := sink.calls[0].Mode; mode != CallModeAPI {
		t.Fatalf("Mode=%q, ожидался api", mode)
	}
	if usage := sink.calls[0].Usage; usage.Input != 5 || usage.Output != 2 {
		t.Fatalf("usage %+v", usage)
	}
}

// TestSystemOneFallbackOn404 — реле без /systemone (404) и без кап в кэше:
// fallback-имитация чатом, клиенту — SystemOneResponse.
func TestSystemOneFallbackOn404(t *testing.T) {
	var sawSchema bool
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no route"}`))
		},
		chat: func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["response_format"]; ok {
				sawSchema = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatAnswer(moodOK)))
		},
	}
	up, _ := u.upstream(t)
	res := resolverMock{"prov": up} // без ModelCapsSource — капы неизвестны
	sink := &sinkStub{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	if err := Chat(rec, req, res, testLogger(), nil, "", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !sawSchema {
		t.Fatalf("первый чат без structured output")
	}
	if u.systemOneCalls != 1 || u.chatCalls != 1 {
		t.Fatalf("systemone=%d chat=%d", u.systemOneCalls, u.chatCalls)
	}
	var resp SystemOneResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не SystemOneResponse: %v", err)
	}
	ans := resp.Answers["mood"]
	if ans.Type != "choice" || ans.Choice != "good" {
		t.Fatalf("answer %+v", ans)
	}
	if len(sink.calls) != 1 || sink.calls[0].Mode != CallModeChat {
		t.Fatalf("Mode=%q", sink.calls[0].Mode)
	}
	if usage := sink.calls[0].Usage; usage.Total != 18 {
		t.Fatalf("usage %+v", usage)
	}
}

// TestSystemOneImitationKnownNoDecision — капа известна как «не decision»:
// имитация сразу, /systemone не вызывается вовсе.
func TestSystemOneImitationKnownNoDecision(t *testing.T) {
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("/systemone не должен вызываться")
		},
		chat: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatAnswer(moodOK)))
		},
	}
	up, _ := u.upstream(t)
	res := capsMock{res: resolverMock{"prov": up}, caps: map[string][]string{"prov/m1": {"completion"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	if err := Chat(rec, req, res, testLogger(), nil, "", &sinkStub{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if u.chatCalls != 1 {
		t.Fatalf("chat=%d", u.chatCalls)
	}
	if !strings.Contains(rec.Body.String(), `"choice":"good"`) {
		t.Fatalf("ответ не SystemOneResponse: %s", rec.Body.String())
	}
}

// TestSystemOneRetryWithoutSchema — апстрим не берёт structured output
// (400 на response_format): повтор обычным чатом без response_format.
func TestSystemOneRetryWithoutSchema(t *testing.T) {
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) },
		chat: func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["response_format"]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unsupported"}`))
				return
			}
			// Ответ с Markdown-заграждением и лишним текстом — скобочный
			// сканер обязан вытащить JSON.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatAnswer("Вот ответ:\n```json\n" + moodOK + "\n``` готово")))
		},
	}
	up, _ := u.upstream(t)
	res := resolverMock{"prov": up}
	sink := &sinkStub{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	if err := Chat(rec, req, res, testLogger(), nil, "", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if u.chatCalls != 2 {
		t.Fatalf("chat=%d — повтор без schema не сделан", u.chatCalls)
	}
	var resp SystemOneResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не SystemOneResponse: %v", err)
	}
	if resp.Answers["mood"].Choice != "good" {
		t.Fatalf("answer %+v", resp.Answers["mood"])
	}
}

// TestSystemOneKnownDecisionErrorNoFallback — у decision-модели ошибка
// апстрима не превращается в fallback («идет как идет»).
func TestSystemOneKnownDecisionErrorNoFallback(t *testing.T) {
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		chat:      func(w http.ResponseWriter, r *http.Request) { t.Fatal("чат не должен вызываться") },
	}
	up, _ := u.upstream(t)
	res := capsMock{res: resolverMock{"prov": up}, caps: map[string][]string{"prov/m1": {"completion", decisionCap}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	err := Chat(rec, req, res, testLogger(), nil, "", &sinkStub{})
	if err == nil {
		t.Fatalf("ожидалась ошибка 502, ответ %d", rec.Code)
	}
	var perr Err
	if !asProxyErr(err, &perr) || perr.Code != 502 {
		t.Fatalf("err=%v", err)
	}
}

// TestSystemOnePassthroughNonDecisionRoute404 — капы неизвестны и ошибка
// апстрима не 404/405 (например 500 на валидном Decision API): fallback
// НЕ делается — наружу ошибка апстрима.
func TestSystemOnePassthroughNonDecisionRoute404(t *testing.T) {
	u := &systemoneUpstreams{
		systemOne: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		chat:      func(w http.ResponseWriter, r *http.Request) { t.Fatal("чат не должен вызываться") },
	}
	up, _ := u.upstream(t)
	res := resolverMock{"prov": up}
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemoneRequest("prov/m1")))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, res, testLogger(), nil, "", &sinkStub{}); err == nil {
		t.Fatalf("ожидалась ошибка")
	}
}

// TestSystemOneParseReplyNestedAndFlat — парсер: вложенный ответ (по схеме),
// плоский числовой (score), Markdown-заграждение, фолбэк совпадения choice.
func TestSystemOneParseReplyNestedAndFlat(t *testing.T) {
	questions := map[string]SystemOneQuestion{
		"mood": {Type: "choice", Criteria: map[string]string{"good": "г", "bad": "п"}},
		"safe": {Type: "noul"},
		"lvl":  {Type: "score", Criteria: []string{"низкий", "средний", "высокий"}},
	}
	// Вложенный: choice в другом регистре ("Good."), score — оценка 3 (уровень 2).
	nested := `{"mood":{"choice":"Good","probabilities":{"good":0.8}},"safe":{"noul":1.2},"lvl":{"score":3}}`
	resp, err := systemoneParseReply(nested, []string{"lvl", "mood", "safe"}, questions)
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if resp.Answers["mood"].Choice != "good" {
		t.Fatalf("choice match: %+v", resp.Answers["mood"])
	}
	if n := *resp.Answers["safe"].Noul; n != 1.0 {
		t.Fatalf("noul clamp: %v", n)
	}
	if lvl := *resp.Answers["lvl"].Score; lvl != 2 {
		t.Fatalf("score level: %v", lvl)
	}
	// Плоский: score — число прямо в ключе вопроса.
	flatResp, flatErr := systemoneParseReply(`{"lvl": 2}`, []string{"lvl"}, questions_scoreOnly())
	if flatErr != nil {
		t.Fatalf("flat: %v", flatErr)
	}
	if lvl := *flatResp.Answers["lvl"].Score; lvl != 1 {
		t.Fatalf("flat level: %v", lvl)
	}
}

// questions_scoreOnly — вопросы только со score (для плоского разбора).
func questions_scoreOnly() map[string]SystemOneQuestion {
	return map[string]SystemOneQuestion{
		"lvl": {Type: "score", Criteria: []string{"a", "b", "c"}},
	}
}

// TestSystemOneChatSchema — схема содержит enum варианто и required.
func TestSystemOneChatSchema(t *testing.T) {
	questions := map[string]SystemOneQuestion{
		"mood": {Type: "choice", Criteria: map[string]string{"good": "г", "bad": "п"}},
		"lvl":  {Type: "score", Criteria: []string{"a", "b", "c"}},
	}
	schema, err := systemoneChatSchema([]string{"lvl", "mood"}, questions)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	s := string(schema)
	for _, want := range []string{`"enum":["bad","good"]`, `"maximum":3`, `"required":["choice"]`, `"required":["lvl","mood"]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("схема без %s: %s", want, s)
		}
	}
	// choice без criteria — ошибка сборки схемы.
	if _, err := systemoneChatSchema([]string{"mood"}, map[string]SystemOneQuestion{"mood": {Type: "choice"}}); err == nil {
		t.Fatalf("ожидалась ошибка choice без criteria")
	}
}

// TestModelsCacheCaps — Caps поверх снапшота: точная форма, алиас (Raw),
// отсутствие. (models_cache_test.go того же пакета — здесь для связности
// с systemone-fallback.)
func TestModelsCacheCaps(t *testing.T) {
	mc := NewModelsCache(resolverMock{}, testLogger(), -1)
	mc.snapshots["prov"] = []ModelEntry{
		{ID: "prov/m1", Provider: "prov", Capabilities: []string{"completion", decisionCap}},
		{ID: "prov/alias", Provider: "prov", Raw: "/models/m2.gguf", Capabilities: nil},
	}
	caps, known := mc.Caps("prov", "m1")
	if !known || !slices.Contains(caps, decisionCap) {
		t.Fatalf("m1: caps=%v known=%v", caps, known)
	}
	// raw-путь: короткое имя — целевой id бэкенда.
	caps2, known2 := mc.Caps("prov", "/models/m2.gguf")
	if !known2 {
		t.Fatalf("m2 raw: known=false")
	}
	if len(caps2) != 0 {
		t.Fatalf("m2 caps=%v", caps2)
	}
	if _, known3 := mc.Caps("prov", "zz"); known3 {
		t.Fatalf("zz: known=true")
	}
}

// testLogger — тихий логгер тестов (error-уровень в builder).
func testLogger() *logging.Logger { return logging.NewWriter(&errLogs{}, "error") }

// asProxyErr — приведение ошибки к proxy.Err.
func asProxyErr(err error, target *Err) bool {
	var e Err
	if !errors.As(err, &e) {
		return false
	}
	*target = e
	return true
}
