// models_cache — кэш полного списка моделей всех провайдеров (PRD:
// «Оптимизация загрузки списка моделей и кэширование»). Первичная полная
// загрузка при старте serve (в фоне — /healthz не ждёт) + фоновое обновление
// раз в 10 минут; /v1/models и /admin/api/models отдают снимок без опроса
// провайдеров на каждый запрос. Сбой провайдера при обновлении оставляет
// прежний снимок (stale-while-revalidate), провайдер, удалённый из конфига,
// вылетает из кэша на следующем цикле.
package proxy

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ITcompressor/any-router/internal/logging"
)

// ModelsRefreshInterval — период фоновых обновлений кэша списка моделей
// (критерий PRD: первичная полная загрузка, далее раз в 10 минут).
const ModelsRefreshInterval = 10 * time.Minute

// lazyLoadTimeout — потолок ленивой первичной загрузки (запрос к кэшу,
// который ещё ни разу не загружался — тесты без Run; серверный цикл
// использует свой ctx).
const lazyLoadTimeout = 5 * time.Minute

// ModelsCache — снимки списка моделей по провайдерам: имя → []ModelEntry
// (в форме CollectModels: алиасы path-моделей, capabilities, meta).
// Чтение — RWMutex; циклы обновления сериализованы refreshMu.
type ModelsCache struct {
	res      ProviderSource
	log      *logging.Logger
	Interval time.Duration // период фоновых обновлений (≤0 — ModelsRefreshInterval)

	mu        sync.RWMutex
	snapshots map[string][]ModelEntry
	loadedAt  time.Time // момент завершения последнего полного цикла

	loadOnce  sync.Once // первичная полная загрузка ровно один раз
	refreshMu sync.Mutex
}

// NewModelsCache создаёт пустой кэш; interval — период фоновых обновлений
// (≤0 — ModelsRefreshInterval).
func NewModelsCache(res ProviderSource, log *logging.Logger, interval time.Duration) *ModelsCache {
	if interval <= 0 {
		interval = ModelsRefreshInterval
	}
	return &ModelsCache{
		res:       res,
		log:       log,
		Interval:  interval,
		snapshots: map[string][]ModelEntry{},
	}
}

// Start — фоновый режим: первичная полная загрузка немедленно, далее
// обновление раз в Interval; цикл останавливается по ctx (serve-shutdown).
func (c *ModelsCache) Start(ctx context.Context) {
	go func() {
		c.ensureLoaded()
		ticker := time.NewTicker(c.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refreshAll(ctx)
			}
		}
	}()
}

// Caps — capabilities одной модели provider/short из готового снапшота
// (без ensureLoaded: холодный кэш не блокирует чтение — known=false,
// systemone-fallback отработает по ошибке апстрима). Форма ищется:
// id "provider/short" (алиас или голый), Raw — целевой id бэкенда.
// known=true с пустыми капами («провайдер список отдал, капы — нет»),
// известное отсутствие decision — честный ответ для systemone-fallback.
func (c *ModelsCache) Caps(provider, short string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.snapshots[provider] {
		if e.ID == provider+"/"+short || e.Raw == short {
			return e.Capabilities, true
		}
	}
	return nil, false
}

// ensureLoaded — первичная полная загрузка ровно один раз: первый вызов
// запускает цикл обновления, остальные ждут его завершения (sync.Once
// блокирует до возврата функции). Ленивая загрузка идёт на своём
// ограниченном ctx — отменённый запрос клиента не убивает прогрев кэша.
func (c *ModelsCache) ensureLoaded() {
	c.loadOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), lazyLoadTimeout)
		defer cancel()
		c.refreshAll(ctx)
	})
}

// Entries — текущий снимок списка: only — один провайдер, "" — все (в
// отсортированном порядке, как CollectModels). Если первичная загрузка
// ещё идёт (сервер поднялся, первый цикл не завершился), ждёт её.
// Возвращает внутренние срезы — только для чтения.
func (c *ModelsCache) Entries(ctx context.Context, only string) []ModelEntry {
	c.ensureLoaded()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if only != "" {
		return c.snapshots[only]
	}
	names := make([]string, 0, len(c.snapshots))
	for n := range c.snapshots {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []ModelEntry
	for _, n := range names {
		out = append(out, c.snapshots[n]...)
	}
	return out
}

// Refresh — один полный цикл обновления синхронно (кнопка ⟳ в админке,
// изменение провайдеров в конфиге): сериализуется с фоновыми обновлениями.
func (c *ModelsCache) Refresh(ctx context.Context) {
	c.refreshAll(ctx)
}

// LoadedAt — момент завершения последнего полного цикла обновления
// (пусто, если кэш ещё не загружался).
func (c *ModelsCache) LoadedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadedAt
}

// refreshAll — полный обход провайдеров из конфига: успешный список
// перезаписывает снимок (даже пустой — авторитетный снимок), сбой
// оставляет прежний снимок, провайдер, удалённый из конфига, выпадает.
// Частичный результат (ctx отменён посреди обхода) снимок не перезаписывает.
func (c *ModelsCache) refreshAll(ctx context.Context) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	names := c.res.ProviderNames()
	sort.Strings(names)
	fresh := make(map[string][]ModelEntry, len(names))
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		entries, err := collectProvider(ctx, c.res, name)
		if err != nil {
			c.log.Warnf("models: кэш: провайдер %s недоступен: %v — сохраняю прежний снимок", name, err)
			if prev, ok := c.snapshot(name); ok {
				fresh[name] = prev
			}
			continue
		}
		fresh[name] = entries
	}
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	c.snapshots = fresh
	c.loadedAt = time.Now()
	c.mu.Unlock()
	total := 0
	for _, entries := range fresh {
		total += len(entries)
	}
	c.log.Infof("models: кэш обновлён: %d моделей у %d провайдеров", total, len(fresh))
}

// snapshot — прежний снимок провайдера (для stale-while-revalidate).
func (c *ModelsCache) snapshot(name string) ([]ModelEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.snapshots[name]
	return e, ok
}
