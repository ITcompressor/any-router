package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthKnownToken — админ и API-токен проходят; нагрузка Identity
// совпавшего кандидата кладётся в контекст (для проверки прав на /v1).
func TestAuthKnownToken(t *testing.T) {
	var seen any
	var sawIdentity bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityOf(r.Context())
		seen, sawIdentity = id.Token, ok
		w.WriteHeader(http.StatusOK)
	})
	h := Auth(func() []Identity {
		return []Identity{{Secret: "admin-token"}, {Secret: "api-token", Token: "tok-payload"}}
	}, next)

	r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	// админ-токен: проходит, нагрузки нет (Token=nil)
	r.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("Bearer admin-token: код %d, хочу 200", rec.Code)
	}
	if !sawIdentity || seen != nil {
		t.Errorf("админ: IdentityOf ok=%v token=%v, хочу ok и nil нагрузку", sawIdentity, seen)
	}
	// API-токен: проходит с нагрузкой
	r.Header.Set("Authorization", "Bearer api-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("Bearer api-token: код %d, хочу 200", rec.Code)
	}
	if !sawIdentity || seen != "tok-payload" {
		t.Errorf("API-токен: IdentityOf token=%v ok=%v, хочу tok-payload", seen, sawIdentity)
	}
}

// TestIdentityOfNoAuth — контекст без Auth: ok=false.
func TestIdentityOfNoAuth(t *testing.T) {
	if _, ok := IdentityOf(context.Background()); ok {
		t.Errorf("IdentityOf без Auth: ok=true, хочу false")
	}
}

func TestAuthUnknownToken401(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Auth(func() []Identity { return []Identity{{Secret: "admin-token"}} }, next)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer чужой")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("чужой токен: код %d, хочу 401", rec.Code)
	}
	// Формат без Bearer
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "admin-token")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("Authorization без 'Bearer ': код %d, хочу 401", rec2.Code)
	}
}

func TestAuthFailClosed(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// пустой набор токенов — всё под 401
	h := Auth(func() []Identity { return nil }, next)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("пустой набор токенов: код %d, хочу 401 (fail closed)", rec.Code)
	}
}

func TestUnauthorizedPayload(t *testing.T) {
	rec := httptest.NewRecorder()
	Unauthorized(rec)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("код %d, хочу 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="any-router"` {
		t.Errorf("WWW-Authenticate: %q", got)
	}
}

func TestBearerExtraction(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if Bearer(r) != "" {
		t.Errorf("без заголовка: %q, хочу пусто", Bearer(r))
	}
	r.Header.Set("Authorization", "Bearer  tok ")
	if got := Bearer(r); got != "tok" {
		t.Errorf("Bearer с пробелами: %q, хочу tok", got)
	}
}
