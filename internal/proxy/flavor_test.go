// flavor_test — flavor-конвертеры: определение по хосту, санитайзер
// запроса под mistral, маппинг capabilities и чистка SSE-чанков.
package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"encoding/json"

	"github.com/ITcompressor/any-router/internal/logging"
)

// mistralUp — апстрим с хостом api.mistral.ai (flavor-детект именно по
// нему): соединение перенаправляется на локальный мок через подмену
// upstreamClient (restore по t.Cleanup). handler отдаёт ответы мока.
func mistralUp(t *testing.T, handler http.HandlerFunc) Upstream {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	up := Upstream{BaseURL: "https://api.mistral.ai/v1", APIKey: "up-key"}
	orig := upstreamClient
	var tr redirectTransport
	tr.dstHost = srv.Listener.Addr().String()
	upstreamClient = &http.Client{Transport: tr}
	t.Cleanup(func() { upstreamClient = orig })
	return up
}

// redirectTransport — все запросы физически уходят на локальный мок
// (https://api.mistral.ai → http://127.0.0.1:port), flavor-детект видит
// оригинальный хост.
type redirectTransport struct{ dstHost string }

func (tr redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = tr.dstHost
	return http.DefaultTransport.RoundTrip(clone)
}

func TestFlavorOf(t *testing.T) {
	for c, want := range map[string]Flavor{
		"https://api.mistral.ai/v1":        FlavorMistral,
		"https://API.MISTRAL.AI/v1":        FlavorMistral,
		"https://api.mistral.ai/v1/extra/": FlavorMistral, // хост совпадает
		"http://10.0.0.4:11434/v1":         FlavorPlain,
		"https://any-router.e7.by/v1":      FlavorPlain,
		"https://api.mistral.ai.evil/v1":   FlavorPlain, // только точное совпадение хоста
		"https://api.z.ai/api/paas/v4":     FlavorZAI,
		"https://api.z.ai/v1":              FlavorZAI, // хост совпадает, путь не важен
		"https://api.z.ai.evil/v4":         FlavorPlain,
		"":                                 FlavorPlain, // неразбираемое — passthrough
	} {
		if got := FlavorOf(c); got != want {
			t.Errorf("FlavorOf(%q): %q, хочу %q", c, got, want)
		}
	}
}

func TestMistralSanitizeChat(t *testing.T) {
	log := logging.NewWriter(os.Stderr, "warning")
	_ = log
	payload := map[string]any{
		"model":    "mistral-small-latest",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
		// несовместимое: убирается
		"options":        map[string]any{"temperature": 0.5},
		"stream_options": map[string]any{},
		"think":          true,
		// совместимое: остаётся
		"temperature":     0.7,
		"max_tokens":      float64(64),
		"stream":          true,
		"tool_choice":     "auto",
		"response_format": map[string]any{"type": "json_object"},
	}
	sanitizeFlavorRequest(FlavorMistral, "/chat/completions", payload, log)
	for _, bad := range []string{"options", "stream_options", "think"} {
		if _, ok := payload[bad]; ok {
			t.Errorf("поле %q осталось — Mistral 422-ит", bad)
		}
	}
	for _, good := range []string{"model", "messages", "temperature", "max_tokens", "stream", "tool_choice", "response_format"} {
		if _, ok := payload[good]; !ok {
			t.Errorf("совместимое поле %q пропало", good)
		}
	}
}

func TestMistralSanitizeCompletions(t *testing.T) {
	log := logging.NewWriter(os.Stderr, "warning")
	_ = log
	payload := map[string]any{
		"model":  "codestral-latest",
		"prompt": "def foo(",
		"suffix": "}",
	}
	sanitizeFlavorRequest(FlavorMistral, "/completions", payload, log)
	for _, keep := range []string{"model", "prompt", "suffix"} {
		if _, ok := payload[keep]; !ok {
			t.Errorf("поле %q пропало (completions whitelist)", keep)
		}
	}
	// messages в legacy completions не существует — вычистится
	payload["messages"] = []any{}
	sanitizeFlavorRequest(FlavorMistral, "/completions", payload, log)
	if _, ok := payload["messages"]; ok {
		t.Errorf("messages должен быть вычищен из legacy completions")
	}
}

func TestMistralSanitizePassthrough(t *testing.T) {
	log := logging.NewWriter(os.Stderr, "warning")
	_ = log
	// passthrough flavor / не-чат-путь: ничего не вычищается
	payload := map[string]any{"model": "m", "input": "hello", "encoding_format": "float"}
	sanitizeFlavorRequest(FlavorPlain, "/chat/completions", payload, log)
	sanitizeFlavorRequest(FlavorMistral, "/embeddings", payload, log)
	for _, keep := range []string{"model", "input", "encoding_format"} {
		if _, ok := payload[keep]; !ok {
			t.Errorf("passthrough: поле %q пропало", keep)
		}
	}
}

func TestMistralCapsMapping(t *testing.T) {
	got := mistralCaps("mistral-small-latest", map[string]any{
		"completion_chat": true, "function_calling": true, "reasoning": true,
		"vision": true, "audio": true,
		"completion_fim": true, "fine_tuning": true, "ocr": true,
		"classification": true, "unified_resources": true,
		"audio_transcription": false,
	})
	want := []string{"audio", "completion", "thinking", "tools", "vision"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("caps: %v, хочу %v", got, want)
	}
	if got := mistralCaps("mistral-small-latest", map[string]any{}); got != nil {
		t.Errorf("пустые capabilities: %v, хочу nil", got)
	}
	// флага «эмбеддинги» в схеме mistral нет: у embed-моделей все флаги false —
	// капа синтезируется по подстроке «embed» в id (любой регистр, даже без
	// capabilities-объекта)
	embedRaw := map[string]any{"completion_chat": false, "vision": false, "audio": false}
	if got := mistralCaps("mistral-embed", embedRaw); strings.Join(got, ",") != "embedding" {
		t.Errorf("embed-модель (все флаги false): %v, хочу [embedding]", got)
	}
	if got := mistralCaps("Codestral-Embed", map[string]any{}); strings.Join(got, ",") != "embedding" {
		t.Errorf("embed-модель без capabilities-объекта: %v, хочу [embedding]", got)
	}
	if got := mistralCaps("mistral-small-latest", embedRaw); got != nil {
		t.Errorf("не-embed модель с одними false: %v, хочу nil", got)
	}
}

// TestMistralModelsCaps — BackendModelsFull у mistral-провайдера берёт
// capabilities из объекта записи и max_context_length → meta.n_ctx_train.
func TestMistralModelsCaps(t *testing.T) {
	up := mistralUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmtJSON := `{"object":"list","data":[
			{"id":"mistral-small-latest","capabilities":{"completion_chat":true,"function_calling":true,"reasoning":true},"max_context_length":131072},
			{"id":"mistral-embed","capabilities":{}}
		]}`
		_, _ = io.WriteString(w, fmtJSON)
	})

	bms, err := BackendModelsFull(context.Background(), up)
	if err != nil {
		t.Fatalf("BackendModelsFull: %v", err)
	}
	if len(bms) != 2 {
		t.Fatalf("моделей %d, хочу 2", len(bms))
	}
	c1 := strings.Join(bms[0].Capabilities, ",")
	if c1 != "completion,thinking,tools" {
		t.Errorf("caps: %q, хочу completion,thinking,tools", c1)
	}
	if v, ok := bms[0].Meta["n_ctx_train"].(int64); !ok || v != 131072 {
		t.Errorf("meta.n_ctx_train: %v (хочу int64 131072)", bms[0].Meta["n_ctx_train"])
	}
	if c2 := strings.Join(bms[1].Capabilities, ","); c2 != "embedding" {
		t.Errorf("caps embed-модели: %q, хочу [embedding] (синтез по id)", c2)
	}
}

// TestZaiCaps — синтез capabilities Z.AI по id модели (docs.z.ai:
// GLM-чат — completion+tools, thinking — с GLM-4.6; не-чат — без кап).
func TestZaiCaps(t *testing.T) {
	for id, want := range map[string]string{
		"glm-4.5":              "completion,tools",
		"glm-4.5-air":          "completion,tools",
		"glm-4.6":              "completion,thinking,tools",
		"glm-4.7":              "completion,thinking,tools",
		"glm-5":                "completion,thinking,tools",
		"glm-5-turbo":          "completion,thinking,tools",
		"glm-5.1":              "completion,thinking,tools",
		"glm-5.3-flash":        "completion,thinking,tools",
		"glm-5.3-flashx-legal": "completion,thinking,tools",
		"glm-image":            "", // image-генерация — не чат
		"glm-asr-2512":         "", // аудио — не чат
		"glm-ocr":              "",
		"cogvideox-2":          "", // чужой префикс
		"glm-embed-v2":         "embedding",
	} {
		got := zaiCaps(id)
		if strings.Join(got, ",") != want {
			t.Errorf("zaiCaps(%q): %q, хочу %q", id, strings.Join(got, ","), want)
		}
	}
}

// TestZaiModelsCaps — BackendModelsFull у zai-провайдера: список /v4/models
// без капов — синтез по id (upstream зaiUp — хост api.z.ai, flavor-детект).
func TestZaiModelsCaps(t *testing.T) {
	up := zaiUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"glm-5.3","object":"model","created":1786636800,"owned_by":"z-ai"},
			{"id":"glm-4.5","object":"model","created":1753632000,"owned_by":"z-ai"},
			{"id":"glm-asr-2512","object":"model","created":1,"owned_by":"z-ai"}
		]}`)
	})

	bms, err := BackendModelsFull(context.Background(), up)
	if err != nil {
		t.Fatalf("BackendModelsFull: %v", err)
	}
	if len(bms) != 3 {
		t.Fatalf("моделей %d, хочу 3", len(bms))
	}
	if c := strings.Join(bms[0].Capabilities, ","); c != "completion,thinking,tools" {
		t.Errorf("caps glm-5.3: %q, хочу completion,thinking,tools", c)
	}
	if c := strings.Join(bms[1].Capabilities, ","); c != "completion,tools" {
		t.Errorf("caps glm-4.5: %q, хочу completion,tools", c)
	}
	if bms[2].Capabilities != nil {
		t.Errorf("caps glm-asr-2512: %v, хочу nil (не чат)", bms[2].Capabilities)
	}
}

// zaiUp — апстрим с хостом api.z.ai (flavor-детект именно по нему):
// соединение перенаправляется на локальный мок (как mistralUp).
func zaiUp(t *testing.T, handler http.HandlerFunc) Upstream {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	up := Upstream{BaseURL: "https://api.z.ai/api/paas/v4", APIKey: "zai-key"}
	orig := upstreamClient
	var tr redirectTransport
	tr.dstHost = srv.Listener.Addr().String()
	upstreamClient = &http.Client{Transport: tr}
	t.Cleanup(func() { upstreamClient = orig })
	return up
}

// TestMistralSSEClean — стрим mistral: поле «p» выкидывается из чанков,
// [DONE] и не-data строки — как есть; plain-флавор — passthrough.
func TestMistralSSEClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Ч\"}}],\"p\":\"abc123\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"total_tokens\":5},\"p\":\"xyz\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET mock: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	copyResponse(rec, resp, FlavorMistral)
	body := rec.Body.String()
	if strings.Count(body, "\"p\"") != 0 {
		t.Errorf("поле p осталось в стриме: %q", body)
	}
	if strings.Count(body, "\"delta\":[") > 0 || !strings.Contains(body, "\"content\":\"Ч\"") {
		t.Errorf("контент чанка потерян: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("[DONE] потерян: %q", body)
	}
	if strings.Count(body, "\n") != 6 {
		t.Errorf("структура SSE (строка + разделитель) сломана: %q", body)
	}

	// plain: как есть, вместе с p
	resp2, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET mock (plain): %v", err)
	}
	defer resp2.Body.Close()
	rec2 := httptest.NewRecorder()
	copyResponse(rec2, resp2, FlavorPlain)
	if strings.Count(rec2.Body.String(), "\"p\"") != 2 {
		t.Errorf("plain-флавор должен копировать как есть: %q", rec2.Body.String())
	}
}

// TestMistralRoundtrip — сквозной: провайдер mistral-flavor, клиент шлёт
// лишние поля — апстрим получает вычищенное тело с Content-Type:
// application/json и валидным JSON (без flavor — 422-строгая валидация
// апстрима). Апстрим-мок имитирует strict-валидацию Mistral.
func TestMistralRoundtrip(t *testing.T) {
	var gotCT, gotAuth string
	var gotPayload map[string]any
	up := mistralUp(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		if json.Unmarshal(b, &gotPayload) != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "тело не JSON")
			return
		}
		// strict: неизвестное поле — «422 extra_forbidden»
		bad := []string{"options", "think", "stream_options"}
		for _, k := range bad {
			if _, ok := gotPayload[k]; ok {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, "{\"detail\":[{\"type\":\"extra_forbidden\",\"loc\":[\"body\",\""+k+"\"]}]}")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}]}`)
	})

	body := `{"model":"mistral-andrew/mistral-small-latest","messages":[{"role":"user","content":"ping"}],"options":{"temperature":0.5},"think":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"mistral-andrew": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type апстрима: %q, хочу application/json", gotCT)
	}
	if gotAuth != "Bearer up-key" {
		t.Errorf("Authorization апстрима: %q", gotAuth)
	}
	if _, ok := gotPayload["options"]; ok {
		t.Errorf("поле options доехало до апстрима")
	}
	if _, ok := gotPayload["think"]; ok {
		t.Errorf("поле think доехало до апстрима")
	}
	m, _ := gotPayload["model"].(string)
	if m != "mistral-small-latest" {
		t.Errorf("модель не перезаписана: %q", m)
	}
	if !strings.Contains(rec.Body.String(), "\"content\":\"pong\"") {
		t.Errorf("ответ апстрима потерян: %q", rec.Body.String())
	}
}

// TestForwardContentTypePlain — Content-Type ставится и для passthrough
// провайдеров (ollama и т.п. — фикс общего уровня, не только mistral).
func TestForwardContentTypePlain(t *testing.T) {
	gotCT := ""
	up, srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}
		gotCT = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `choices: []`)
	})
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ollama/q"}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type апстрима: %q", gotCT)
	}
}
