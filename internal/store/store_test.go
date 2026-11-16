package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hex32regex — сгенерированные токены: ровно 32 hex-символа (16 байт).
var hex32regex = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestLoadCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load отсутствующего файла: %v", err)
	}
	if s.Listen() != DefaultListen {
		t.Errorf("Listen: %q, хочу %q", s.Listen(), DefaultListen)
	}
	if !hex32regex.MatchString(s.AdminToken()) {
		t.Errorf("админ-токен: %q, хочу hex32", s.AdminToken())
	}
	if len(s.ProviderNames()) != 0 || len(s.Tokens()) != 0 {
		t.Errorf("свежий стор не пуст: провайдеры %v, токены %v", s.ProviderNames(), s.Tokens())
	}
	// Файл создан, права 0600 (в YAML — все ключи, даже пустые).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat созданного файла: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("права файла: %o, хочу 600", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, key := range []string{"listen:", "token:", "providers:", "tokens:"} {
		if !contains(string(raw), key) {
			t.Errorf("в YAML нет ключа %q:\n%s", key, raw)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestLoadNormalizesPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	// Файл без listen и token — дозаполняются и сохраняются обратно.
	if err := os.WriteFile(path, []byte("providers:\n  ollama:\n    baseurl: http://127.0.0.1:11434/v1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Listen() != DefaultListen {
		t.Errorf("Listen дозаполнен: %q, хочу %q", s.Listen(), DefaultListen)
	}
	if !hex32regex.MatchString(s.AdminToken()) {
		t.Errorf("админ-токен дозаполнен: %q, хочу hex32", s.AdminToken())
	}
	if p, ok := s.Provider("ollama"); !ok || p.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Errorf("провайдер из файла потерян: %+v ok=%v", p, ok)
	}
}

func TestProviderCRUDPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetProvider("ollama", Provider{BaseURL: "http://127.0.0.1:11434/v1", APIKey: "k"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	// Пустые поля — ошибка
	if err := s.SetProvider("", Provider{BaseURL: "http://x"}); err == nil {
		t.Errorf("SetProvider без имени: ошибка ожидается")
	}
	if err := s.SetProvider("bad", Provider{}); err == nil {
		t.Errorf("SetProvider без baseurl: ошибка ожидается")
	}
	if _, ok := s.Provider("ollama"); !ok {
		t.Errorf("провайдер ollama не найден после SetProvider")
	}
	// Персистентность: новый стор из того же файла видит изменения
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if p, ok := s2.Provider("ollama"); !ok || p.APIKey != "k" {
		t.Errorf("SetProvider не сохранился в файл: %+v ok=%v", p, ok)
	}
	// Перезапись и удаление
	if err := s.SetProvider("ollama", Provider{BaseURL: "http://elsewhere/v1"}); err != nil {
		t.Fatalf("перезапись ollama: %v", err)
	}
	if p, _ := s.Provider("ollama"); p.BaseURL != "http://elsewhere/v1" {
		t.Errorf("перезапись не применилась: %+v", p)
	}
	if ok, err := s.DeleteProvider("ollama"); err != nil || !ok {
		t.Fatalf("DeleteProvider ollama: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DeleteProvider("ollama"); ok {
		t.Errorf("DeleteProvider несуществующего: ok=true")
	}
}

func TestAPITokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t1, err := s.NewAPIToken(Token{})
	if err != nil {
		t.Fatalf("NewAPIToken(пустой): %v", err)
	}
	if t1.ID != "t1" {
		t.Errorf("авто-ID: %q, хочу t1", t1.ID)
	}
	if !hex32regex.MatchString(t1.Token) {
		t.Errorf("токен: %q, хочу hex32", t1.Token)
	}
	if t1.Scope != ScopeAll || !t1.Unlimited() {
		t.Errorf("Scope: %q, хочу all", t1.Scope)
	}
	t2, err := s.NewAPIToken(Token{ID: "tests"})
	if err != nil {
		t.Fatalf("NewAPIToken(tests): %v", err)
	}
	if _, err := s.NewAPIToken(Token{ID: "tests"}); err == nil {
		t.Errorf("дубликат ID: ошибка ожидается")
	}
	if !s.BearerKnown(t1.Token) || !s.BearerKnown(t2.Token) || !s.BearerKnown(s.AdminToken()) {
		t.Errorf("BearerKnown не признаёт сгенерированные/админ-токен")
	}
	if s.BearerKnown("нету") {
		t.Errorf("BearerKnown признаёт посторонний токен")
	}
	// Персистентность
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if got := len(s2.Tokens()); got != 2 {
		t.Errorf("токенов после перезагрузки: %d, хочу 2", got)
	}
	// Удаление
	if ok, err := s.DeleteToken("tests"); err != nil || !ok {
		t.Fatalf("DeleteToken(tests): ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DeleteToken("tests"); ok {
		t.Errorf("DeleteToken несуществующего: ok=true")
	}
}

// TestNewAPITokenScopes — области при создании: провайдер должен существовать
// в конфиге; model — нужны оба поля; незаполненные области откатываются к all.
func TestNewAPITokenScopes(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(filepath.Join(dir, "any-router.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// провайдер для области не добавлен — ошибка
	if _, err := s.NewAPIToken(Token{ID: "a", Scope: ScopeProvider, Provider: "ollama"}); err == nil {
		t.Errorf("scope=provider без провайдера в конфиге: ошибка ожидается")
	}
	if err := s.SetProvider("ollama", Provider{BaseURL: "http://x/v1"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	// model без модели — ошибка
	if _, err := s.NewAPIToken(Token{ID: "b", Scope: ScopeModel, Provider: "ollama"}); err == nil {
		t.Errorf("scope=model без модели: ошибка ожидается")
	}
	// корректные области
	pTok, err := s.NewAPIToken(Token{ID: "c", Scope: ScopeProvider, Provider: "ollama"})
	if err != nil {
		t.Fatalf("NewAPIToken(provider): %v", err)
	}
	if pTok.Scope != ScopeProvider || pTok.Provider != "ollama" || pTok.Model != "" {
		t.Errorf("scope=provider сохранён неверно: %+v", pTok)
	}
	mTok, err := s.NewAPIToken(Token{ID: "d", Scope: ScopeModel, Provider: "ollama", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("NewAPIToken(model): %v", err)
	}
	if mTok.Ref() != "ollama/llama3.2" || mTok.Unlimited() {
		t.Errorf("scope=model: %+v", mTok)
	}
	if mTok.DescribeScope() != "модель ollama/llama3.2" || pTok.DescribeScope() != "провайдер ollama" {
		t.Errorf("DescribeScope: %q / %q", mTok.DescribeScope(), pTok.DescribeScope())
	}
	if err := s.SetProvider("pcbig", Provider{BaseURL: "http://y/v1"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	// область models: без моделей, с чужим провайдером и с записью без
	// провайдера — ошибки из формы (в yaml кривые записи выкидываются, см.
	// TestLoadNormalizesTokenScopes)
	if _, err := s.NewAPIToken(Token{ID: "g", Scope: ScopeModels}); err == nil {
		t.Errorf("scope=models без моделей: ошибка ожидается")
	}
	if _, err := s.NewAPIToken(Token{ID: "g", Scope: ScopeModels, Models: []string{"нету/x"}}); err == nil {
		t.Errorf("scope=models с неизвестным провайдером: ошибка ожидается")
	}
	if _, err := s.NewAPIToken(Token{ID: "g", Scope: ScopeModels, Models: []string{"кривая-без-слэша"}}); err == nil {
		t.Errorf("scope=models с записью без провайдера: ошибка ожидается")
	}
	// дубли и обводки схлопываются, порядок сохраняется
	multiTok, err := s.NewAPIToken(Token{ID: "h", Scope: ScopeModels,
		Models: []string{"ollama/llama3.2", " ollama/llama3.2 ", "pcbig/Q", "pcbig/Q"}})
	if err != nil {
		t.Fatalf("NewAPIToken(models): %v", err)
	}
	if !equalStrings(multiTok.Models, []string{"ollama/llama3.2", "pcbig/Q"}) ||
		multiTok.Unlimited() || multiTok.DescribeScope() != "модели: ollama/llama3.2, pcbig/Q" {
		t.Errorf("scope=models: %+v describe=%q", multiTok, multiTok.DescribeScope())
	}
	// wildcard-запись «provider/*» — весь хост: без провайдера и «/*» без
	// имени — ошибка; конкретная модель, покрытая wildcard’ом своего же
	// провайдера, канонизируется (p/* покрывает её всё равно)
	if _, err := s.NewAPIToken(Token{ID: "w", Scope: ScopeModels, Models: []string{"нету/*"}}); err == nil {
		t.Errorf("wildcard без провайдера: ошибка ожидается")
	}
	if _, err := s.NewAPIToken(Token{ID: "w", Scope: ScopeModels, Models: []string{"/*"}}); err == nil {
		t.Errorf("wildcard без имени провайдера: ошибка ожидается")
	}
	wTok, err := s.NewAPIToken(Token{ID: "w2", Scope: ScopeModels, Models: []string{"ollama/*", "ollama/llama3.2", "pcbig/Q"}})
	if err != nil {
		t.Fatalf("NewAPIToken(wildcard): %v", err)
	}
	if !equalStrings(wTok.Models, []string{"ollama/*", "pcbig/Q"}) ||
		wTok.DescribeScope() != "модели: весь хост ollama, pcbig/Q" {
		t.Errorf("scope=models с wildcard: %+v describe=%q", wTok, wTok.DescribeScope())
	}
	// незнакомая область — откат к all, не ошибка; неполная (model без
	// провайдера) — из формы ошибка, а не молчаливый all
	tok, err := s.NewAPIToken(Token{ID: "e", Scope: "ерунда"})
	if err != nil {
		t.Fatalf("NewAPIToken(ерунда): %v", err)
	}
	if tok.Scope != ScopeAll || !tok.Unlimited() {
		t.Errorf("незнакомая область: %+v, хочу all", tok)
	}
	if _, err := s.NewAPIToken(Token{ID: "f", Scope: ScopeModel, Model: "llama3.2"}); err == nil {
		t.Errorf("scope=model без провайдера: ошибка ожидается")
	}
	// персистентность областей: перечитываем файл того же стора
	s2, err := Load(filepath.Join(dir, "any-router.yaml"))
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	tokens := s2.Tokens()
	hTok := Token{}
	for _, t2 := range tokens {
		if t2.ID == "h" {
			hTok = t2
		}
	}
	if hTok.Scope != ScopeModels || !equalStrings(hTok.Models, []string{"ollama/llama3.2", "pcbig/Q"}) {
		t.Errorf("scope=models в файле: %+v", hTok)
	}
	for _, want := range []Token{pTok, mTok} {
		found := false
		for _, got := range tokens {
			if got.ID == want.ID {
				found = true
				if got.Scope != want.Scope || got.Provider != want.Provider || got.Model != want.Model {
					t.Errorf("токен %s в файле: %+v, хочу %+v", want.ID, got, want)
				}
			}
		}
		if !found {
			t.Errorf("токен %s не в перечитанном конфиге", want.ID)
		}
	}
}

// TestLoadNormalizesTokenScopes — yaml руками: пустой scope → all, полная
// форма provider/model у поля model разбирается, кривые области не валят старт.
func TestLoadNormalizesTokenScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "any-router.yaml")
	yaml := `listen: ":8085"
token: abc
tokens:
  - id: legacy
    token: tok1
  - id: prefixed
    token: tok2
    scope: model
    model: ollama/llama3.2
  - id: pathmodel
    token: tok3
    scope: model
    provider: pcbig
    model: /opt/models/Qwen.gguf
  - id: broken
    token: tok4
    scope: provider
  - id: unknown
    token: tok5
    scope: что-то ещё
  - id: multimodel
    token: tok6
    scope: models
    models:
      - ollama/llama3.2
      - «алиас-без-слэша»
      - ollama/llama3.2
  - id: multimodel-empty
    token: tok7
    scope: models
  - id: multimodel-bad
    token: tok8
    scope: models
    models:
      - баланс-алиас-имя
  - id: hostwild
    token: tok9
    scope: models
    models:
      - ollama/llama3.2
      - ollama/*
      - ollama/llama3.2
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load кривого yaml: %v", err)
	}
	byID := map[string]Token{}
	for _, t := range s.Tokens() {
		byID[t.ID] = t
	}
	if got := byID["legacy"]; got.Scope != ScopeAll || !got.Unlimited() {
		t.Errorf("legacy без scope: %+v, хочу all", got)
	}
	if got := byID["prefixed"]; got.Scope != ScopeModel || got.Provider != "ollama" || got.Model != "llama3.2" {
		t.Errorf("prefixed provider/model: %+v", got)
	}
	if got := byID["pathmodel"]; got.Scope != ScopeModel || got.Model != "/opt/models/Qwen.gguf" || got.Ref() != "pcbig//opt/models/Qwen.gguf" {
		t.Errorf("pathmodel: %+v", got)
	}
	if got := byID["broken"]; got.Scope != ScopeAll {
		t.Errorf("broken (provider без провайдера): %+v, хочу all", got)
	}
	if got := byID["unknown"]; got.Scope != ScopeAll {
		t.Errorf("unknown (незнакомая scope): %+v, хочу all", got)
	}
	// голые имена в области models сохраняются (явная выдача баланс-алиаса:
	// алиас мог быть создан позже — несуществующее имя инертно)
	if got := byID["multimodel"]; got.Scope != ScopeModels ||
		!equalStrings(got.Models, []string{"ollama/llama3.2", "«алиас-без-слэша»"}) || got.Provider != "" || got.Model != "" {
		t.Errorf("multimodel: %+v, хочу models с моделью и голым именем алиаса", got)
	}
	if got := byID["multimodel-empty"]; got.Scope != ScopeAll {
		t.Errorf("multimodel-empty: %+v, хочу all", got)
	}
	if got := byID["multimodel-bad"]; got.Scope != ScopeModels ||
		!equalStrings(got.Models, []string{"баланс-алиас-имя"}) {
		t.Errorf("multimodel-bad (только голое имя): %+v, хочу models", got)
	}
	// wildcard-записи хостов сохраняются, покрытые ими конкретные модели
	// канонизируются при загрузке (yaml переписывается минимальной формой)
	if got := byID["hostwild"]; got.Scope != ScopeModels ||
		!equalStrings(got.Models, []string{"ollama/*"}) ||
		got.DescribeScope() != "модели: весь хост ollama" {
		t.Errorf("hostwild: %+v describe=%q", got, got.DescribeScope())
	}
	// нормализация перезаписана в файл (idempotent при повторном чтении)
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if got := len(s2.Tokens()); got != 9 {
		t.Errorf("токенов после перечтения: %d, хочу 9", got)
	}
}

// TestUpdateTokenScope — редактирование прав существующего токена: id и
// секрет не меняются, меняется область; несуществующий токен и кривая
// область — ошибки (область не трогается).
func TestUpdateTokenScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = s.SetProvider("ollama", Provider{BaseURL: "http://x/v1"})
	_ = s.SetProvider("pcbig", Provider{BaseURL: "http://y/v1"})
	tok, err := s.NewAPIToken(Token{ID: "t", Scope: ScopeProvider, Provider: "ollama"})
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	// → models из двух провайдеров
	upd, err := s.UpdateTokenScope("t", Token{Scope: ScopeModels, Models: []string{"ollama/llama3.2", "pcbig/Q"}})
	if err != nil {
		t.Fatalf("UpdateTokenScope(models): %v", err)
	}
	if upd.ID != "t" || upd.Token != tok.Token || upd.Scope != ScopeModels ||
		!equalStrings(upd.Models, []string{"ollama/llama3.2", "pcbig/Q"}) {
		t.Errorf("UpdateTokenScope: %+v (секрет хочу %s)", upd, tok.Token)
	}
	// → all
	upd, err = s.UpdateTokenScope("t", Token{})
	if err != nil {
		t.Fatalf("UpdateTokenScope(all): %v", err)
	}
	if upd.Scope != ScopeAll || upd.Provider != "" || upd.Model != "" || len(upd.Models) != 0 {
		t.Errorf("UpdateTokenScope(all): %+v", upd)
	}
	// кривая область — ошибка, прежняя остается в файле
	if _, err := s.UpdateTokenScope("t", Token{Scope: ScopeProvider, Provider: "нету"}); err == nil {
		t.Errorf("UpdateTokenScope(чужой провайдер): ошибка ожидается")
	}
	if got, _ := s2Token(s, "t"); got.Scope != ScopeAll {
		t.Errorf("после неудачного UpdateTokenScope область изменилась: %+v", got)
	}
	// несуществующий токен — ошибка
	if _, err := s.UpdateTokenScope("nope", Token{Scope: ScopeProvider, Provider: "ollama"}); err == nil {
		t.Errorf("UpdateTokenScope(nope): ошибка ожидается")
	}
	// перечитанный файл согласен
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	var inFile Token
	for _, t2 := range s2.Tokens() {
		if t2.ID == "t" {
			inFile = t2
		}
	}
	if inFile.Scope != ScopeAll || inFile.Token != tok.Token {
		t.Errorf("после перечтения: %+v", inFile)
	}
}

// s2Token — токен по id (помощник тестов).
func s2Token(s *Store, id string) (Token, bool) {
	for _, t := range s.Tokens() {
		if t.ID == id {
			return t, true
		}
	}
	return Token{}, false
}

func TestSetAdminTokenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetAdminToken("new-admin-token"); err != nil {
		t.Fatalf("SetAdminToken: %v", err)
	}
	// Тот же токен — второй записи не надо (idempotent)
	if err := s.SetAdminToken("new-admin-token"); err != nil {
		t.Fatalf("SetAdminToken повторно: %v", err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if s2.AdminToken() != "new-admin-token" {
		t.Errorf("админ-токен после перезагрузки: %q, хочу new-admin-token", s2.AdminToken())
	}
}

func TestNoTmpLeftAfterSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetProvider("x", Provider{BaseURL: "http://x/v1"}); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp-файл остался после атомарной записи: %v", err)
	}
}

// TestLangfuseSetPersist — настройки langfuse: сохранение в yaml, чтение,
// нормализация (обводка/хвостовые слэши), Valid по заполненности,
// переключатель enabled — вкл/выкл без удаления настроек.
func TestLangfuseSetPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.LangfuseCfg().On() {
		t.Fatal("свежий конфиг — экспорт langfuse должен быть выключен")
	}
	// url с хвостом и пробелами — нормализация
	if err := s.SetLangfuse(Langfuse{Enabled: true, URL: " http://10.0.0.3:3000/ ", PublicKey: "pk-lf-1", SecretKey: "sk-lf-1"}); err != nil {
		t.Fatalf("SetLangfuse: %v", err)
	}
	if got := s.LangfuseCfg(); got.URL != "http://10.0.0.3:3000" || got.PublicKey != "pk-lf-1" {
		t.Errorf("прочитано: %+v", got)
	}
	if !s.LangfuseCfg().On() {
		t.Fatal("полная конфигурация с переключателем — хочу включен")
	}
	// переживает перезагрузку стора (включенность — по явным yaml)
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if l := s2.LangfuseCfg(); l.URL != "http://10.0.0.3:3000" || !l.On() {
		t.Errorf("после перезагрузки: %+v", l)
	}
	// переключатель снят, url/ключи на месте — выключен, настройки не потеряны
	if err := s.SetLangfuse(Langfuse{URL: "http://10.0.0.3:3000", PublicKey: "pk-lf-1", SecretKey: "sk-lf-1"}); err != nil {
		t.Fatalf("SetLangfuse (выкл): %v", err)
	}
	if l := s.LangfuseCfg(); !l.Valid() || l.On() {
		t.Errorf("снят переключатель: %+v, хочу Valid=true On=false", l)
	}
	// частичная конфигурация — выключен (запись не должна терять остальные поля)
	if err := s.SetLangfuse(Langfuse{URL: "http://x"}); err != nil {
		t.Fatalf("SetLangfuse частично: %v", err)
	}
	if s.LangfuseCfg().Valid() {
		t.Error("без ключей — экспорт должен быть выключен")
	}
	// тэги из формы — запятой в одной записи, чистка пустых/дублей
	if err := s.SetLangfuse(Langfuse{URL: "http://10.0.0.3:3000/", PublicKey: "pk", SecretKey: "sk",
		Environment: " prod ", Tags: []string{" load-test ", "", "prod", " load-test "}}); err != nil {
		t.Fatalf("SetLangfuse tags: %v", err)
	}
	l := s.LangfuseCfg()
	if l.Environment != "prod" || l.URL != "http://10.0.0.3:3000" {
		t.Errorf("нормализация env/url: %+v", l)
	}
	if len(l.Tags) != 2 || l.Tags[0] != "load-test" || l.Tags[1] != "prod" {
		t.Errorf("тэги нормализованы: %v, хочу [load-test prod]", l.Tags)
	}
}

// TestLangfuseLegacyEnabled — старый файл без ключа enabled: прежняя семантика
// («включено, когда заполнены url и ключи»), после первого Load файл
// перезаписывается с явным переключателем; явное enabled: false — выключает
// выгрузку без удаления url/ключей.
func TestLangfuseLegacyEnabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	base := "listen: \":8085\"\ntoken: admin-token\n"
	lf := "langfuse:\n    url: http://10.0.0.3:3000\n    public_key: pk-lf-1\n    secret_key: sk-lf-1\n"

	// без enabled, url/ключи заполнены — включено, флаг материализован в файле
	if err := os.WriteFile(path, []byte(base+lf), 0o600); err != nil {
		t.Fatalf("запись legacy-файла: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if l := s.LangfuseCfg(); !l.Enabled || !l.On() {
		t.Fatalf("legacy с url/ключами — хочу включен: %+v", l)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение файла после Load: %v", err)
	}
	if !strings.Contains(string(raw), "enabled: true") {
		t.Errorf("файл не перезаписан с явным enabled: %s", raw)
	}

	// явное enabled: false — выключено, url/ключи не потеряны
	if err := os.WriteFile(path, []byte(base+"langfuse:\n    enabled: false\n    url: http://10.0.0.3:3000\n    public_key: pk-lf-1\n    secret_key: sk-lf-1\n"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}
	s, err = Load(path)
	if err != nil {
		t.Fatalf("Load (enabled: false): %v", err)
	}
	if l := s.LangfuseCfg(); !l.Valid() || l.On() {
		t.Errorf("явное enabled: false — хочу Valid=true On=false: %+v", l)
	}
	// файл с явным false не должен переписываться в true
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "enabled: false") {
		t.Errorf("явное enabled: false перезаписано: %s", raw)
	}
}

// TestBalancesCRUD — баланс-алиасы: валидация SetBalance (имя без «/», хотя
// бы одна модель, провайдеры из конфига), upsert по имени без дублей записей,
// порядок сохранения (важен round-robin'у) и персистентность.
func TestBalancesCRUD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = s.SetProvider("ollama", Provider{BaseURL: "http://a/v1"})
	_ = s.SetProvider("pcbig", Provider{BaseURL: "http://b/v1"})

	// валидация
	if err := s.SetBalance("  ", []string{"ollama/m1"}); err == nil {
		t.Error("SetBalance без имени: ошибка ожидается")
	}
	if err := s.SetBalance("a/b", []string{"ollama/m1"}); err == nil {
		t.Error("SetBalance с «/» в имени: ошибка ожидается (пространство прямых обращений)")
	}
	if err := s.SetBalance("fast", []string{}); err == nil {
		t.Error("SetBalance без моделей: ошибка ожидается")
	}
	if err := s.SetBalance("fast", []string{"   "}); err == nil {
		t.Error("SetBalance с пустыми записями: ошибка ожидается")
	}
	if err := s.SetBalance("fast", []string{"ghost/m1"}); err == nil {
		t.Error("SetBalance с неизвестным провайдером: ошибка ожидается")
	}
	// создание: дубль участника схлопывается, порядок сохраняется
	if err := s.SetBalance("fast", []string{"ollama/m2", "pcbig/Q", " ollama/m2 "}); err != nil {
		t.Fatalf("SetBalance: %v", err)
	}
	if members, ok := s.Balance("fast"); !ok || members[0] != "ollama/m2" || members[1] != "pcbig/Q" || len(members) != 2 {
		t.Errorf("участники: %v ok=%v, хочу [ollama/m2 pcbig/Q]", members, ok)
	}
	// перезапись по имени: дублей записей нет
	if err := s.SetBalance("fast", []string{"pcbig/Q"}); err != nil {
		t.Fatalf("перезапись fast: %v", err)
	}
	if bs := s.Balances(); len(bs) != 1 || bs[0].Name != "fast" || len(bs[0].Models) != 1 {
		t.Errorf("после перезаписи: %+v, хочу одну запись fast с одним участником", bs)
	}
	// порядок записей = порядок создания (таблица/round-robin)
	if err := s.SetBalance("aaa", []string{"ollama/m2"}); err != nil {
		t.Fatalf("SetBalance aaa: %v", err)
	}
	bs := s.Balances()
	if bs[0].Name != "fast" || bs[1].Name != "aaa" {
		t.Errorf("порядок: %v,%v, хочу fast,aaa", bs[0].Name, bs[1].Name)
	}
	// персистентность
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if bs := s2.Balances(); len(bs) != 2 || bs[1].Models[0] != "ollama/m2" {
		t.Errorf("SetBalance не сохранился в файл: %+v", bs)
	}
	// удаление
	if ok, err := s.DeleteBalance("fast"); err != nil || !ok {
		t.Fatalf("DeleteBalance fast: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DeleteBalance("fast"); ok {
		t.Error("DeleteBalance несуществующего: ok=true")
	}
	if len(s.Balances()) != 1 {
		t.Errorf("после удаления: %d записей, хочу 1", len(s.Balances()))
	}
}

// TestLoadNormalizesBalances — кривые записи баланс-алиасов в yaml (имя с «/»,
// пустое имя, без участников, дубли имени/участников) выпадают молча, файл
// перезаписывается исправленным — ручные правки не валят старт.
func TestLoadNormalizesBalances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	raw := `providers:
  ollama:
    baseurl: http://a/v1
balances:
  - name: good
    models:
      - ollama/m1
      - "  ollama/m1  "
      - ollama/m1
      - ollama/*
  - name: bad/name
    models:
      - ollama/m1
  - name: ""
    models:
      - ollama/m1
  - name: empty
  - name: good
    models:
      - ollama/zzz
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load кривого yaml: %v", err)
	}
	bs := s.Balances()
	if len(bs) != 1 || bs[0].Name != "good" {
		t.Fatalf("после нормализации: %+v, хочу одну запись good", bs)
	}
	if members := bs[0].Models; len(members) != 1 || members[0] != "ollama/m1" {
		t.Errorf("участники после чистки: %v, хочу [ollama/m1] (wildcard не бывает участником)", members)
	}
	// исправленное сохранено: повторный Load — то же содержимое, файл стабилен
	cleaned, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	if bs := s2.Balances(); len(bs) != 1 || bs[0].Name != "good" {
		t.Errorf("повторный Load: %+v", bs)
	}
	if !contains(string(cleaned), "balances:") {
		t.Errorf("в переписанном yaml нет ключа balances:\n%s", cleaned)
	}
}

// TestTokenScopeBalanceAlias — явная выдача баланс-алиаса в области models:
// голое имя существующего алиаса проходит валидацию (как и модели), несущест-
// вующего — ошибка; участники баланс-алиаса — только полные формы (голое имя
// участника — ошибка; алиас-в-алиас не бывает).
func TestTokenScopeBalanceAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "any-router.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = s.SetProvider("ollama", Provider{BaseURL: "http://a/v1"})
	_ = s.SetProvider("pcbig", Provider{BaseURL: "http://b/v1"})
	_ = s.SetBalance("fast-pool", []string{"ollama/m1", "pcbig/m2"})

	// алиас в области models — ок (вперемешку с моделями)
	tok, err := s.NewAPIToken(Token{ID: "t1", Scope: ScopeModels,
		Models: []string{"ollama/x", " fast-pool "}})
	if err != nil {
		t.Fatalf("NewAPIToken с алиасом: %v", err)
	}
	if !equalStrings(tok.Models, []string{"ollama/x", "fast-pool"}) {
		t.Errorf("область с алиасом: %v", tok.Models)
	}
	// несуществующий алиас — ошибка
	if _, err := s.NewAPIToken(Token{ID: "t2", Scope: ScopeModels, Models: []string{"no-such-alias"}}); err == nil {
		t.Error("несуществующий алиас: ошибка ожидается")
	}
	// редактирование прав тоже понимает алиас
	if upd, err := s.UpdateTokenScope("t1", Token{Scope: ScopeModels, Models: []string{"fast-pool"}}); err != nil ||
		!equalStrings(upd.Models, []string{"fast-pool"}) {
		t.Errorf("UpdateTokenScope с алиасом: %+v err=%v", upd, err)
	}
	// перечитанный файл согласен
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("второй Load: %v", err)
	}
	var inFile Token
	for _, t2 := range s2.Tokens() {
		if t2.ID == "t1" {
			inFile = t2
		}
	}
	if !equalStrings(inFile.Models, []string{"fast-pool"}) {
		t.Errorf("область с алиасом в файле: %+v", inFile)
	}
	// участник алиаса — только полная форма: голое имя участника rejected
	if err := s.SetBalance("nested", []string{"fast-pool"}); err == nil {
		t.Error("участник-алиас (голое имя) в SetBalance: ошибка ожидается")
	}
}
