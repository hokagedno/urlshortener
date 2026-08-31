package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hokagedno/urlshortener/internal/domain"
)

// ClickCollector — асинхронный сборщик переходов.
//
// Зачем: запись клика не должна задерживать редирект. Хендлер кладёт событие
// в буферизованный канал за наносекунды и сразу отвечает 302, а пул воркеров
// в фоне накапливает батч и вставляет его в Postgres одной транзакцией.
//
// Здесь демонстрируются:
//   - буферизованный канал как очередь с backpressure;
//   - пул горутин (worker pool) — фиксированное число потребителей;
//   - select с default для неблокирующей отправки (дропаем при переполнении,
//     чтобы всплеск трафика не «подвесил» HTTP-хендлеры);
//   - time.Ticker для сброса неполного батча по таймауту;
//   - sync.WaitGroup + закрытие канала для graceful shutdown без потери данных;
//   - sync.Once, чтобы Stop был идемпотентным.
type ClickCollector struct {
	repo       domain.LinkRepository
	log        *slog.Logger
	ch         chan domain.Click
	wg         sync.WaitGroup
	stopOnce   sync.Once
	batchSize  int
	flushEvery time.Duration

	mu      sync.Mutex
	dropped int64 // счётчик потерянных событий при переполнении очереди
}

type CollectorConfig struct {
	BufferSize int
	Workers    int
	BatchSize  int
	FlushEvery time.Duration
}

func NewClickCollector(repo domain.LinkRepository, log *slog.Logger, cfg CollectorConfig) *ClickCollector {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 2 * time.Second
	}

	c := &ClickCollector{
		repo:       repo,
		log:        log,
		ch:         make(chan domain.Click, cfg.BufferSize),
		batchSize:  cfg.BatchSize,
		flushEvery: cfg.FlushEvery,
	}

	c.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go c.worker(i)
	}
	return c
}

// Record кладёт клик в очередь. Никогда не блокирует вызывающую горутину.
func (c *ClickCollector) Record(click domain.Click) {
	select {
	case c.ch <- click:
	default:
		// Очередь переполнена. Аналитика менее важна, чем задержка редиректа,
		// поэтому событие отбрасывается, но факт фиксируется в метрике.
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
	}
}

// Dropped возвращает число отброшенных событий (для /metrics или health).
func (c *ClickCollector) Dropped() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

func (c *ClickCollector) worker(id int) {
	defer c.wg.Done()

	batch := make([]domain.Click, 0, c.batchSize)
	ticker := time.NewTicker(c.flushEvery)
	defer ticker.Stop()

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		// Собственный контекст с таймаутом: батч должен долететь до БД,
		// даже если контекст исходного HTTP-запроса давно отменён.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.repo.SaveClicks(ctx, batch)
		cancel()
		if err != nil {
			c.log.Error("не удалось сохранить клики", "worker", id, "count", len(batch), "err", err)
		} else {
			c.log.Debug("клики сохранены", "worker", id, "count", len(batch), "reason", reason)
		}
		batch = batch[:0] // переиспользуем уже выделенный слайс — без лишних аллокаций
	}

	for {
		select {
		case click, ok := <-c.ch:
			if !ok {
				// Канал закрыт в Stop() — дописываем остаток и выходим.
				flush("shutdown")
				return
			}
			batch = append(batch, click)
			if len(batch) >= c.batchSize {
				flush("full")
			}
		case <-ticker.C:
			flush("timeout")
		}
	}
}

// Stop закрывает очередь и ждёт, пока воркеры допишут накопленное.
// Вызывается при graceful shutdown после остановки HTTP-сервера.
func (c *ClickCollector) Stop() {
	c.stopOnce.Do(func() {
		close(c.ch)
		c.wg.Wait()
	})
}
