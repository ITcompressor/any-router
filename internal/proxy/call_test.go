// call_test — экспорт Call (langfuse): capture тел, usage, ClientIP.
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ITcompressor/any-router/internal/logging"
)

// sinkStub — CallSink-мок: копит записанные Call.
type sinkStub struct {
	calls []Call
}

func (s *sinkStub) RecordCall(c Call) { s.calls = append(s.calls, c) }

// upstreamAnswer — апстрим отдаёт заданный content-type + тело.
func upstreamAnswer(t *testing.T, ctype, body string) (Upstream, *httptest.Server) {
	t.Helper()
	return upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

// TestChatRecordJSON — не-стримовый чат: Call с вводом/выводом/usage.
func TestChatRecordJSON(t *testing.T) {
	up, srv := upstreamAnswer(t, "application/json",
		`{"id":"c1","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	defer srv.Close()
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[{"role":"user","content":"ping"}],"temperature":0.5}`))
	req.Header.Set("X-Real-IP", "10.0.0.9")
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "t2", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("записей Call: %d, хочу 1", len(sink.calls))
	}
	c := sink.calls[0]
	if c.Provider != "ollama" || c.Model != "ollama/qwen3" {
		t.Errorf("провайдер/модель: %q/%q, хочу ollama/ollama/qwen3", c.Provider, c.Model)
	}
	if c.IP != "10.0.0.9" {
		t.Errorf("IP: %q, хочу из X-Real-IP", c.IP)
	}
	if c.TokenID != "t2" {
		t.Errorf("TokenID: %q, хочу t2", c.TokenID)
	}
	if c.Path != "/v1/chat/completions" {
		t.Errorf("Path: %q", c.Path)
	}
	m, _ := c.Input.(map[string]any)
	if m == nil || m["model"] != "ollama/qwen3" {
		t.Errorf("Input: %v — модель должна быть как прислал клиент", c.Input)
	}
	if c.Output == nil {
		t.Fatal("Output пуст")
	}
	if c.Usage != (Usage{Input: 11, Output: 7, Total: 15}) && c.Usage != (Usage{Input: 11, Output: 7, Total: 18}) {
		t.Errorf("Usage: %+v, хочу input=11 output>=7 total", c.Usage)
	}
	if c.Failed != "" {
		t.Errorf("Failed: %q, хочу пусто", c.Failed)
	}
	if c.End.Before(c.Started) {
		t.Errorf("End < Started")
	}
	_ = c.End.Sub(c.Started)
}

// TestChatRecordSSE — стрим: агрегация дельт + usage финального чанка.
func TestChatRecordSSE(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"по\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"нг\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	up, srv := upstreamAnswer(t, "text/event-stream", sse)
	defer srv.Close()
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "t1", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	c := sink.calls[0]
	if !c.Stream {
		t.Errorf("Stream: false, хочу true")
	}
	if text, _ := c.Output.(string); text != "понг" {
		t.Errorf("агрегированный текст SSE: %q, хочу «понг»", c.Output)
	}
	if c.Usage != (Usage{Input: 5, Output: 2, Total: 7}) {
		t.Errorf("Usage: %+v, хочу 5/2/7", c.Usage)
	}
}

// TestChatRecordUpstreamError — недоступный апстрим: Call с Failed.
func TestChatRecordUpstreamError(t *testing.T) {
	up, srv := upstreamAnswer(t, "application/json", `{"error":"boom"}`)
	_ = up
	srv.Close() // порт ещё может жить — но проще: заведомо мёртвый апстрим
	up = Upstream{BaseURL: "http://127.0.0.1:1/v1"}
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "t1", sink); err == nil {
		t.Fatal("Chat на мёртвом апстриме должен вернуть ошибку")
	}
	if len(sink.calls) != 1 {
		t.Fatalf("записей Call: %d, хочу 1 (ошибка апстрима тоже экспортируется)", len(sink.calls))
	}
	if sink.calls[0].Failed == "" {
		t.Errorf("Failed пуст — хочу текст ошибки")
	}
}

// TestChatRecordUpstreamHTTPStatus — апстрим с 500-кой: Failed со статусом.
func TestChatRecordUpstreamHTTPStatus(t *testing.T) {
	up, srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	defer srv.Close()
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "t1", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	c := sink.calls[0]
	if c.Failed != "апстрим ответил 500" {
		t.Errorf("Failed: %q, хочу «апстрим ответил 500»", c.Failed)
	}
}

// TestChatNoRecordWithoutSink — rec=nil: экспорт выключен, ничего не падает.
func TestChatNoRecordWithoutSink(t *testing.T) {
	up, srv := upstreamAnswer(t, "application/json", `{"choices":[],"usage":{}}`)
	defer srv.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

// TestChatCaptureTruncated — тело апстрима сверх captureLimit усекается.
func TestChatCaptureTruncated(t *testing.T) {
	big := strings.Repeat("x", captureLimit*2)
	up, srv := upstreamAnswer(t, "application/json", `{"choices":[],"pad":"`+big+`"}`)
	defer srv.Close()
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/qwen3","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, resolverMock{"ollama": up}, logging.NewWriter(&strings.Builder{}, "error"), nil, "t1", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	c := sink.calls[0]
	if !c.Truncated {
		t.Errorf("Truncated: false, хочу true (тело %d байт)", len(big))
	}
	// буфер усечён до потолка, а JSON невалиден — output строкой
	if _, ok := c.Output.(string); !ok {
		t.Errorf("Output: %T, хочу строку (обрезанный JSON)", c.Output)
	}
}

// TestChatRecordAliasUpstreamModel — авто-алиас: UpstreamModel — путь.
func TestChatRecordAliasUpstreamModel(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	path := "/opt/llm/models/Qwen3.gguf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + path + `"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	res := resolverOne{name: "pcbig", up: Upstream{BaseURL: srv.URL + "/v1"}}
	sink := &sinkStub{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pcbig/Qwen3","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := Chat(rec, req, res, logging.NewWriter(&strings.Builder{}, "error"), nil, "t1", sink); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	c := sink.calls[0]
	if c.UpstreamModel != path {
		t.Errorf("UpstreamModel: %q, хочу %s", c.UpstreamModel, path)
	}
}

// ─── ClientIP ─────────────────────────────────────────────────────────

// TestClientIP — приоритет: X-Real-IP → хвост XFF → RemoteAddr.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.7", "X-Forwarded-For": "1.1.1.1"}, "203.0.113.7"},
		{"хвост XFF", map[string]string{"X-Forwarded-For": "9.9.9.9, 203.0.113.9"}, "203.0.113.9"},
		{"одиночный XFF", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "203.0.113.9"},
		{"кривой XFF — RemoteAddr", map[string]string{"X-Forwarded-For": "не-айпи"}, "192.0.2.1"},
		{"без заголовков", map[string]string{}, "192.0.2.1"},
		{"кривой X-Real-IP → XFF", map[string]string{"X-Real-IP": "инъекция", "X-Forwarded-For": "10.0.0.5, 192.0.2.7"}, "192.0.2.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := ClientIP(req); got != tc.want {
				t.Errorf("ClientIP = %q, хочу %q", got, tc.want)
			}
		})
	}
}

// TestClientIPRemoteAddrNoPort — IPv6/порт в RemoteAddr срезается.
func TestClientIPRemoteAddrNoPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "[2001:db8::1]:41234"
	if got := ClientIP(req); got != "2001:db8::1" {
		t.Errorf("ClientIP = %q, хочу 2001:db8::1", got)
	}
}
