package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ITcompressor/any-router/internal/logging"
)

// modelsUpstream — фейковый бэкенд: /v1/models отдаёт текущее тело из body
// (счётчик запросов), при broken — 502; /api/show — 404 (capabilities нет).
func modelsUpstream(t *testing.T, body *string, broken *bool) (Upstream, *httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			atomic.AddInt32(&hits, 1)
			if broken != nil && *broken {
				http.Error(w, "взрыв", http.StatusBadGateway)
				return
			}
			if body != nil {
				fmt.Fprint(w, *body)
			}
		case r.URL.Path == "/api/show":
			http.NotFound(w, r)
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return Upstream{BaseURL: srv.URL + "/v1"}, srv, &hits
}

func modelIDs(entries []ModelEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestModelsCacheStartAndRefresh — Start: первичная полная загрузка
// немедленно, далее обновление раз в Interval (здесь 50 мс): изменение на
// бэкенде (появилась новая модель) подхватывается фоновым циклом,
// без ручного Refresh и без новых запросов к провайдерам от клиентов.
func TestModelsCacheStartAndRefresh(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	body := `{"object":"list","data":[{"id":"m1"}]}`
	up, _, _ := modelsUpstream(t, &body, nil)
	res := resolverMock{"a": up}
	log := logging.NewWriter(&strings.Builder{}, "error")
	mc := NewModelsCache(res, log, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mc.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if hasID(modelIDs(mc.Entries(ctx, "")), "a/m1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("первичная загрузка не завершилась: %v", modelIDs(mc.Entries(ctx, "")))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Фоновое обновление подхватывает появление новой модели.
	body = `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`
	deadline = time.Now().Add(5 * time.Second)
	for {
		if hasID(modelIDs(mc.Entries(ctx, "")), "a/m2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("фоновое обновление не подхватило новую модель: %v", modelIDs(mc.Entries(ctx, "")))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestModelsCacheStaleOnFailure — сбой провайдера при обновлении не чистит
// снимок: возвращается прежний список, пока провайдер не оправится;
// оправился — снимок авторитетный (и пустой список тоже).
func TestModelsCacheStaleOnFailure(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	body := `{"object":"list","data":[{"id":"m1"}]}`
	broken := false
	up, _, _ := modelsUpstream(t, &body, &broken)
	res := resolverMock{"a": up}
	log := logging.NewWriter(&strings.Builder{}, "error")
	mc := NewModelsCache(res, log, ModelsRefreshInterval)
	ctx := context.Background()

	if !hasID(modelIDs(mc.Entries(ctx, "")), "a/m1") {
		t.Fatalf("первичная загрузка: %v", modelIDs(mc.Entries(ctx, "")))
	}
	// Провайдер лег: обновляемся — прежний снимок остаётся.
	broken = true
	mc.Refresh(ctx)
	if ids := modelIDs(mc.Entries(ctx, "")); !hasID(ids, "a/m1") || len(ids) != 1 {
		t.Fatalf("после сбоя провайдера: %v, хочу прежний снимок [a/m1]", ids)
	}
	// Провайдер оправился и модель удалил: авторитетный пустой снимок.
	broken = false
	body = `{"object":"list","data":[]}`
	mc.Refresh(ctx)
	if ids := modelIDs(mc.Entries(ctx, "")); len(ids) != 0 {
		t.Fatalf("после оправления: %v, хочу пустой список", ids)
	}
}

// TestModelsCacheLazyLoad — без Start (тест без Run, либо первый запрос
// до завершения фоновой загрузки): первое Entries запускает первичную
// загрузку (одна волна опроса провайдеров), следующие Entries — из снимка,
// провайдеры повторно не опрашиваются.
func TestModelsCacheLazyLoad(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	body := `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`
	up, _, hits := modelsUpstream(t, &body, nil)
	res := resolverMock{"a": up}
	log := logging.NewWriter(&strings.Builder{}, "error")
	mc := NewModelsCache(res, log, ModelsRefreshInterval)
	ctx := context.Background()

	if ids := modelIDs(mc.Entries(ctx, "")); len(ids) != 2 {
		t.Fatalf("ленивая загрузка: %v, хочу 2 модели", ids)
	}
	mc.Entries(ctx, "")
	mc.Entries(ctx, "")
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("опросов /v1/models: %d, хочу 1 (дальше — из кэша)", n)
	}
	// Refresh — ручной цикл: новый опрос.
	mc.Refresh(ctx)
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("после Refresh опросов: %d, хочу 2", n)
	}
}

// TestModelsCacheRemovedProvider — провайдер, удалённый из конфига,
// выпадает из кэша на следующем обновлении.
func TestModelsCacheRemovedProvider(t *testing.T) {
	ResetAliasCache()
	t.Cleanup(ResetAliasCache)
	bodyA := `{"object":"list","data":[{"id":"ma"}]}`
	bodyB := `{"object":"list","data":[{"id":"mb"}]}`
	upA, _, _ := modelsUpstream(t, &bodyA, nil)
	upB, _, _ := modelsUpstream(t, &bodyB, nil)
	res := resolverMock{"a": upA, "b": upB}
	log := logging.NewWriter(&strings.Builder{}, "error")
	mc := NewModelsCache(res, log, ModelsRefreshInterval)
	ctx := context.Background()

	if ids := modelIDs(mc.Entries(ctx, "")); len(ids) != 2 {
		t.Fatalf("первичная загрузка: %v, хочу 2 модели", ids)
	}
	delete(res, "b")
	mc.Refresh(ctx)
	if ids := modelIDs(mc.Entries(ctx, "")); !hasID(ids, "a/ma") || hasID(ids, "b/mb") {
		t.Fatalf("после удаления провайдера: %v, хочу только [a/ma]", ids)
	}
	// Фильтр по одному провайдеру тоже из снимка.
	if ids := modelIDs(mc.Entries(ctx, "b")); len(ids) != 0 {
		t.Errorf("Entries(only=b): %v, хочу пусто", ids)
	}
}

// TestModelsCacheIntervalDefault — период по умолчанию (≤0) — 10 минут.
func TestModelsCacheIntervalDefault(t *testing.T) {
	mc := NewModelsCache(resolverMock{}, logging.NewWriter(&strings.Builder{}, "error"), 0)
	if mc.Interval != 10*time.Minute {
		t.Errorf("дефолтный период: %v, хочу 10 минут", mc.Interval)
	}
}
