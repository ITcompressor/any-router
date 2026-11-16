// Package router — HTTP-прослойки any-router: аутентификация по Bearer-токену
// с пробросом личности совпавшего токена в контекст запроса (по ней /v1
// проверяет права на модель).
package router

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Identity — совпавший кандидат авторизации: секрет запроса и нагрузка
// для обработчиков (на /v1 — совпавший API-токен store.Token; админский
// запрос — Token=nil, прав не ограничен). Кладётся в контекст запроса (Auth).
type Identity struct {
	Secret string
	Token  any
}

// identityKey — ключ контекста с совпавшей Identity.
type identityKey struct{}

// Bearer — значение заголовка Authorization: Bearer <...>; пустая строка —
// заголовка нет или формат неверный.
func Bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// SecureEqual — сравнение строк постоянного времени.
func SecureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Auth оборачивает handler проверкой Bearer-токена: get возвращает набор
// допустимых кандидатов на запрос (админ + API-токены — живые, из store;
// у API-токенов с нагрузкой Token). Пустой набор — fail closed: любой
// запрос получает 401. Сравнение токенов — постоянное время; совпавший
// кандидат кладётся в контекст запроса (IdentityOf).
func Auth(get func() []Identity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := Bearer(r)
		var match Identity
		known := false
		for _, id := range get() {
			if bearer != "" && SecureEqual(bearer, id.Secret) {
				match, known = id, true
				break
			}
		}
		if !known {
			Unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, match)))
	})
}

// IdentityOf — Identity совпавшего токена из контекста (кладёт Auth);
// ok=false — запрос шёл мимо Auth (handler тестируют напрямую и т.п.).
func IdentityOf(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Unauthorized отвечает 401 с JSON-телом и WWW-Authenticate.
func Unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("WWW-Authenticate", `Bearer realm="any-router"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
