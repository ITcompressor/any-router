// langfuse_test — выгрузчик: построение OTLP/JSON, неблокирующая очередь,
// батч-POST, live-update настроек, дропы на полной очереди.
package langfuse

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ITcompressor/any-router/internal/proxy"
)

// waitServer — сервер, отдающий 200 и сигналящий в канал о каждом POST.
func waitServer(t *testing.T, hits chan<- *http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		if hits != nil {
			hits <- r
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
}

// TestSendBuildsOTLP — send: Basic auth, эндпоинт, версия ingestion, маппинг атрибутов.
func TestSendBuildsOTLP(t *testing.T) {
	hits := make(chan *http.Request, 8)
	srv := waitServer(t, hits)
	defer srv.Close()

	e := New(nil, 4)
	defer e.Shutdown(time.Second)
	e.Update(Config{Enabled: true, URL: srv.URL, PublicKey: "pk-lf-test", SecretKey: "sk-lf-test", Environment: "prod", Tags: []string{"load-test"}})

	in := map[string]any{"model": "ollama/qwen3", "messages": []any{map[string]any{"role": "user", "content": "ping"}}}
	e.send([]proxy.Call{{
		IP: "192.0.2.5", TokenID: "t2", Provider: "ollama",
		Model: "ollama/qwen3", UpstreamModel: "/opt/llm/models/Qwen3.gguf",
		Path: "/v1/chat/completions", Stream: true, Started: time.Now().Add(-time.Second), End: time.Now(),
		Input: in, Output: "понг",
		Usage: proxy.Usage{Input: 5, Output: 2, Total: 7},
	}})

	select {
	case req := <-hits:
		if req.URL.Path != "/api/public/otel/v1/traces" {
			t.Fatalf("путь запроса: %s, хочу /api/public/otel/v1/traces", req.URL.Path)
		}
		user, pass, ok := req.BasicAuth()
		if !ok || user != "pk-lf-test" || pass != "sk-lf-test" {
			t.Fatalf("Basic auth: %q/%q ok=%v, хочу pk/sk", user, pass, ok)
		}
		if req.Header.Get("x-langfuse-ingestion-version") != "4" {
			t.Fatalf("нет x-langfuse-ingestion-version: 4")
		}
		var payload struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
							ArrayValue  struct {
								Values []struct {
									StringValue string `json:"stringValue"`
								} `json:"values"`
							} `json:"arrayValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						TraceId    string `json:"traceId"`
						SpanId     string `json:"spanId"`
						Name       string `json:"name"`
						Kind       int    `json:"kind"`
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
								IntValue    *int64 `json:"intValue"`
								ArrayValue  struct {
									Values []struct {
										StringValue string `json:"stringValue"`
									} `json:"values"`
								} `json:"arrayValue"`
							} `json:"value"`
						} `json:"attributes"`
						Status struct {
							Code    int    `json:"code"`
							Message string `json:"message"`
						} `json:"status"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("тело не OTLP/JSON: %v", err)
		}
		if len(payload.ResourceSpans) != 1 || len(payload.ResourceSpans[0].ScopeSpans) != 1 {
			t.Fatalf("стуктура OTLP: resourceSpans=%d", len(payload.ResourceSpans))
		}
		spans := payload.ResourceSpans[0].ScopeSpans[0].Spans
		if len(spans) != 1 {
			t.Fatalf("спанов: %d, хочу 1", len(spans))
		}
		span := spans[0]
		if len(span.TraceId) != 32 || len(span.SpanId) != 16 {
			t.Errorf("ids: traceId %q (%d) spanId %q (%d), хочу 32/16 hex", span.TraceId, len(span.TraceId), span.SpanId, len(span.SpanId))
		}
		if span.Name != "ollama/qwen3" || span.Kind != 1 {
			t.Errorf("имя/kind: %q/%d", span.Name, span.Kind)
		}
		// карта атрибутов
		attrs := map[string]string{}
		arrays := map[string][]string{}
		for _, a := range span.Attributes {
			if a.Value.StringValue != "" && len(a.Value.ArrayValue.Values) == 0 {
				attrs[a.Key] = a.Value.StringValue
			}
			if len(a.Value.ArrayValue.Values) > 0 {
				for _, v := range a.Value.ArrayValue.Values {
					arrays[a.Key] = append(arrays[a.Key], v.StringValue)
				}
			}
		}
		for key, want := range map[string]string{
			"langfuse.observation.type":                "generation",
			"langfuse.observation.model.name":          "qwen3", // алиасная форма клиента
			"langfuse.observation.input":               `{"messages":[{"content":"ping","role":"user"}],"model":"ollama/qwen3"}`,
			"langfuse.observation.output":              `"понг"`,
			"langfuse.observation.usage_details":       `{"input":5,"output":2,"total":7}`,
			"langfuse.user.id":                         "t2",
			"langfuse.trace.name":                      "chat/completions",
			"langfuse.trace.metadata.ip":               "192.0.2.5",
			"langfuse.trace.metadata.token_id":         "t2",
			"langfuse.trace.metadata.provider":         "ollama",
			"langfuse.trace.metadata.model":            "qwen3",
			"langfuse.trace.metadata.model_ref":        "ollama/qwen3",
			"langfuse.trace.metadata.upstream_model":   "/opt/llm/models/Qwen3.gguf",
			"langfuse.trace.metadata.path":             "/v1/chat/completions",
			"langfuse.trace.metadata.stream":           "true",
			"langfuse.observation.metadata.ip":         "192.0.2.5",
			"langfuse.observation.metadata.token_id":   "t2",
			"langfuse.observation.metadata.provider":   "ollama",
			"langfuse.observation.metadata.model_ref":  "ollama/qwen3",
			"langfuse.observation.metadata.upstream_x": "", // проверка отсечения неизвестных
		} {
			if attrs[key] != want {
				t.Errorf("%s = %q, хочу %q", key, attrs[key], want)
			}
		}
		wantTags := []string{
			"any-router", "provider:ollama", "model:qwen3", "ip:192.0.2.5", "key:t2", "load-test",
		}
		got := arrays["langfuse.trace.tags"]
		if len(got) != len(wantTags) {
			t.Fatalf("tags: %v, хочу %v", got, wantTags)
		}
		for i, tag := range wantTags {
			if got[i] != tag {
				t.Errorf("tags[%d] = %q, хочу %q", i, got[i], tag)
			}
		}
		if attrs["langfuse.environment"] != "prod" || attrs["deployment.environment"] != "prod" {
			t.Errorf("environment: %q/%q, хочу prod", attrs["langfuse.environment"], attrs["deployment.environment"])
		}
		if span.Status.Code != 0 && span.Status.Code != 2 {
			t.Errorf("status.code: %d, хочу 0 (успех)", span.Status.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("POST не дождался")
	}
}

// TestSendErrorLevel — сбой (Failed): level ERROR + status code 2.
func TestSendErrorLevel(t *testing.T) {
	hits := make(chan *http.Request, 8)
	srv := waitServer(t, hits)
	defer srv.Close()

	e := New(nil, 4)
	defer e.Shutdown(time.Second)
	e.Update(Config{Enabled: true, URL: srv.URL, PublicKey: "pk", SecretKey: "sk"})
	e.send([]proxy.Call{{Provider: "ollama", Model: "ollama/q", Path: "/v1/chat/completions",
		Started: time.Now(), End: time.Now(), Failed: "апстрим ответил 502"}})
	select {
	case req := <-hits:
		var span struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Status struct {
							Code    int    `json:"code"`
							Message string `json:"message"`
						} `json:"status"`
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.NewDecoder(req.Body).Decode(&span); err != nil {
			t.Fatalf("тело: %v", err)
		}
		s := span.ResourceSpans[0].ScopeSpans[0].Spans[0]
		if s.Status.Code != 2 || s.Status.Message != "апстрим ответил 502" {
			t.Errorf("status: %+v, хочу code 2 c сообщением", s.Status)
		}
		attrs := map[string]string{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value.StringValue
		}
		if attrs["langfuse.observation.level"] != "ERROR" {
			t.Errorf("level: %q, хочу ERROR", attrs["langfuse.observation.level"])
		}
		if attrs["langfuse.observation.usage_details"] != "" {
			t.Errorf("usage_details при обломе: %q, хочу пусто", attrs["langfuse.observation.usage_details"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("POST не дождался")
	}
}

// TestRecordCallAsyncAndBatch — записи идут через очередь одним батчем,
// дроп на полной очереди не блокирует вызывателя.
func TestRecordCallAsyncAndBatch(t *testing.T) {
	hits := make(chan struct{}, 4)
	// сервер, который пропускает всё и считает POST'ы
	var mu sync.Mutex
	posts := 0
	release := make(chan struct{})
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // держим первый POST — очередь наполняется, вызыватель не блокируется
		mu.Lock()
		posts++
		mu.Unlock()
		hits <- struct{}{}
	}))
	defer srv2.Close()

	e := New(nil, 2)
	defer e.Shutdown(time.Second)
	e.Update(Config{Enabled: true, URL: srv2.URL, PublicKey: "pk", SecretKey: "sk"})
	close(release) // сервер отвечает мгновенно, но воркер спит: флаш по FlushInterval

	start := time.Now()
	call := proxy.Call{Provider: "ollama", Model: "ollama/q", Path: "/v1/chat/completions", Started: time.Now(), End: time.Now()}
	for i := 0; i < 8; i++ {
		e.RecordCall(call)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("RecordCall заблокировал вызывателя: %s (хочу неблокирующий дроп избыточных)", d)
	}
	select {
	case <-hits:
	case <-time.After(3 * time.Second):
		t.Fatal("батч так и не ушёл")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts < 1 {
		t.Fatalf("POST'ов: %d", posts)
	}
}

// TestDisabled — записи при выключенном экспорте не летят: ни при неполной
// конфигурации, ни при снятом переключателе enabled (настройки на месте).
func TestDisabled(t *testing.T) {
	posts := make(chan int, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts <- 1
	}))
	defer srv.Close()

	e := New(nil, 4)
	defer e.Shutdown(time.Second)
	e.Update(Config{URL: srv.URL}) // неполная конфигурация — выключен
	if e.Enabled() {
		t.Fatal("Enabled при неполной настройке = true")
	}
	e.RecordCall(proxy.Call{Provider: "x", Started: time.Now(), End: time.Now()})
	select {
	case <-posts:
		t.Fatal("POST при выключенном экспорте")
	case <-time.After(200 * time.Millisecond):
	}

	// полные url/ключи, но переключатель снят — тоже выключен (настройки
	// остаются в конфиге, включение — одним Update)
	e.Update(Config{URL: srv.URL, PublicKey: "pk", SecretKey: "sk"})
	if e.Enabled() {
		t.Fatal("Enabled с полными настройками, но без переключателя = true")
	}
	e.RecordCall(proxy.Call{Provider: "x", Started: time.Now(), End: time.Now()})
	select {
	case <-posts:
		t.Fatal("POST при снятом переключателе enabled")
	case <-time.After(200 * time.Millisecond):
	}

	// переключатель вернули — выгрузка пошла
	e.Update(Config{Enabled: true, URL: srv.URL, PublicKey: "pk", SecretKey: "sk"})
	if !e.Enabled() {
		t.Fatal("Enabled с переключателем и полными настройками = false")
	}
	e.RecordCall(proxy.Call{Provider: "x", Started: time.Now(), End: time.Now()})
	select {
	case <-posts:
	case <-time.After(3 * time.Second):
		t.Fatal("POST после включения переключателя не дождались")
	}
}

// TestUpdateLive — включение на лету: записи пошли в новый эндпоинт.
func TestUpdateLive(t *testing.T) {
	hits := make(chan *http.Request, 4)
	srv := waitServer(t, hits)
	defer srv.Close()

	e := New(nil, 4)
	defer e.Shutdown(time.Second)
	e.Update(Config{Enabled: true, URL: srv.URL, PublicKey: "pk-lf-a", SecretKey: "sk-lf-a"})
	if !e.Enabled() {
		t.Fatal("Enabled после Update валидной конфигурации")
	}
	e.RecordCall(proxy.Call{Provider: "ollama", Model: "ollama/q", Path: "/v1/chat/completions",
		Started: time.Now(), End: time.Now()})
	select {
	case <-hits:
	case <-time.After(3 * time.Second):
		t.Fatal("POST после включения на лету не дождались")
	}
}

// TestIngestionEndpoint — вывод эндпоинта из базы инстанса.
func TestIngestionEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://10.0.0.3:3000":           "http://10.0.0.3:3000/api/public/otel/v1/traces",
		"http://10.0.0.3:3000/":          "http://10.0.0.3:3000/api/public/otel/v1/traces",
		"https://cloud.langfuse.com":     "https://cloud.langfuse.com/api/public/otel/v1/traces",
		"http://h:3000/api/public/otel":  "http://h:3000/api/public/otel/v1/traces",
		"http://h:3000/api/public/otel/": "http://h:3000/api/public/otel/v1/traces",
	}
	for in, want := range cases {
		if got := ingestionEndpoint(in); got != want {
			t.Errorf("ingestionEndpoint(%q) = %q, хочу %q", in, got, want)
		}
	}
}

// TestSendAliasTag — запрос через баланс-алиас: тэг alias:<имя> в трейсе
// (обращения через балансировщик отделяются от прямых), метаданные alias,
// provider — обслужившее попытку, upstream_model — участник.
func TestSendAliasTag(t *testing.T) {
	hits := make(chan *http.Request, 4)
	srv := waitServer(t, hits)
	defer srv.Close()

	e := New(nil, 4)
	defer e.Shutdown(time.Second)
	e.Update(Config{Enabled: true, URL: srv.URL, PublicKey: "pk-lf-test", SecretKey: "sk-lf-test"})

	e.send([]proxy.Call{{
		IP: "192.0.2.5", TokenID: "t2", Alias: "pool", Provider: "ollama",
		Model: "pool", UpstreamModel: "m1", Path: "/v1/chat/completions",
		Started: time.Now().Add(-time.Second), End: time.Now(),
		Input: map[string]any{"model": "pool", "messages": []any{}},
	}})

	select {
	case req := <-hits:
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name       string `json:"name"`
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
								ArrayValue  struct {
									Values []struct {
										StringValue string `json:"stringValue"`
									} `json:"values"`
								} `json:"arrayValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("тело не OTLP/JSON: %v", err)
		}
		span := payload.ResourceSpans[0].ScopeSpans[0].Spans[0]
		if span.Name != "pool" {
			t.Errorf("имя спана: %q, хочу голое имя алиаса pool", span.Name)
		}
		attrs := map[string]string{}
		arrays := map[string][]string{}
		for _, a := range span.Attributes {
			if len(a.Value.ArrayValue.Values) > 0 {
				for _, v := range a.Value.ArrayValue.Values {
					arrays[a.Key] = append(arrays[a.Key], v.StringValue)
				}
				continue
			}
			attrs[a.Key] = a.Value.StringValue
		}
		// тэги: any-router, provider, model (голая форма), alias + IP и токен
		wantTags := []string{"any-router", "provider:ollama", "model:pool", "alias:pool", "ip:192.0.2.5", "key:t2"}
		got := arrays["langfuse.trace.tags"]
		if len(got) != len(wantTags) {
			t.Fatalf("tags: %v, хочу %v", got, wantTags)
		}
		for i := range wantTags {
			if got[i] != wantTags[i] {
				t.Errorf("tags[%d] = %q, хочу %q", i, got[i], wantTags[i])
			}
		}
		// метаданные алиаса — на трейсе и на наблюдении (v4: непересекающиеся)
		for _, key := range []string{"langfuse.trace.metadata.alias", "langfuse.observation.metadata.alias"} {
			if attrs[key] != "pool" {
				t.Errorf("metadata %s: %q, хочу pool", key, attrs[key])
			}
		}
		if attrs["langfuse.trace.metadata.upstream_model"] != "m1" {
			t.Errorf("metadata upstream_model: %q, хочу m1 (обслуживший участник)", attrs["langfuse.trace.metadata.upstream_model"])
		}
		if attrs["langfuse.observation.model.name"] != "pool" {
			t.Errorf("observation model.name: %q, хочу pool", attrs["langfuse.observation.model.name"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("langfuse не принял батч")
	}
}
