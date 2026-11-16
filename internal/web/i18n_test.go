// Тесты языков админки (i18n.go): ступени выбора (cookie → yaml → ru),
// право-токены на языке интерфейса, локализация logout.

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminPageOf — рендер страницы /admin по запросу (токен в query, cookie —
// язык сессии).
func adminPageOf(t *testing.T, s *Server, target, langCookie string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin"+target, nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	if langCookie != "" {
		req.Header.Set("Cookie", LangCookie+"="+langCookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin%s: код %d", target, rec.Code)
	}
	return rec.Body.String()
}

// TestAdminLangDefaultRU — без cookie и без yaml lang — русский набор
// (значения совпадают со старыми захардкоженными: тесты прав-текстов
// выше по файлу опираются на них).
func TestAdminLangDefaultRU(t *testing.T) {
	s := newTestServer(t)
	html := adminPageOf(t, s, "", "")
	if !strings.Contains(html, ">Провайдеры</button>") {
		t.Errorf("дефолт должен быть ru: %s", sliceAround(html, `class="tabs"`))
	}
	if !strings.Contains(html, `lang="ru"`) {
		t.Errorf("<html lang> должен быть ru")
	}
	if !strings.Contains(html, ">админ-токен</option>") {
		t.Errorf("option админ-токена не на русском")
	}
}

// TestAdminLangCookie — cookie any_router_lang=en → английский набор,
// cookie ru перекрывает yaml en (переключатель шапки действует на сессию).
func TestAdminLangCookie(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.SetLang("en"); err != nil {
		t.Fatalf("SetLang: %v", err)
	}
	html := adminPageOf(t, s, "", "en")
	for _, want := range []string{">Providers</button>", ">admin token</option>", ">— load models —</option>"} {
		if !strings.Contains(html, want) {
			t.Errorf("cookie en: нет %q", want)
		}
	}
	if strings.Contains(html, ">Провайдеры</button>") {
		t.Errorf("cookie en: русская вкладка осталась")
	}
	if html2 := adminPageOf(t, s, "", "ru"); !strings.Contains(html2, ">Провайдеры</button>") {
		t.Errorf("cookie ru должен перекрыть yaml en")
	}
}

// TestAdminLangYAML — lang: en в конфиг-сторе — английская админка без cookie.
func TestAdminLangYAML(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.SetLang("en"); err != nil {
		t.Fatalf("SetLang: %v", err)
	}
	html := adminPageOf(t, s, "", "")
	if !strings.Contains(html, ">Providers</button>") {
		t.Errorf("yaml lang=en: админка не на английском: %s", sliceAround(html, `class="tabs"`))
	}
}

// TestAdminLangUnknownFallsBack — незнакомое значение lang — дефолт ru;
// незнакомый cookie при yaml en — следующая ступень (yaml en).
func TestAdminLangUnknownFallsBack(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.SetLang("fr"); err != nil {
		t.Fatalf("SetLang: %v", err)
	}
	if html := adminPageOf(t, s, "", ""); !strings.Contains(html, ">Провайдеры</button>") {
		t.Errorf("yaml fr: ожидался фолбэк ru")
	}
	if err := s.st.SetLang("en"); err != nil {
		t.Fatalf("SetLang: %v", err)
	}
	if html := adminPageOf(t, s, "", "de"); !strings.Contains(html, ">Providers</button>") {
		t.Errorf("cookie de (незнакомый) при yaml en: ожидается en")
	}
}

// TestAdminLangLogout — страница logout локализована (cookie en).
func TestAdminLangLogout(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/logout", nil)
	req.Header.Set("Authorization", "Bearer "+s.st.AdminToken())
	req.Header.Set("Cookie", LangCookie+"=en")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "You logged out of the admin") {
		t.Errorf("logout не на английском")
	}
}

// sliceAround — фрагмент html вокруг маркера (для сообщений об ошибке).
func sliceAround(html, marker string) string {
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	if i+200 > len(html) {
		return html[i:]
	}
	return html[i : i+200]
}
