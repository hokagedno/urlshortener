package service_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hokagedno/urlshortener/internal/domain"
	"github.com/hokagedno/urlshortener/internal/service"
)

func TestClickCollector_FlushesOnBatchSize(t *testing.T) {
	repo := newStubRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	c := service.NewClickCollector(repo, log, service.CollectorConfig{
		BufferSize: 100,
		Workers:    1,
		BatchSize:  10,
		FlushEvery: time.Hour, // таймер отключаем, проверяем именно сброс по размеру
	})

	for i := 0; i < 10; i++ {
		c.Record(domain.Click{LinkID: 1, IP: "10.0.0.1"})
	}
	c.Stop() // Stop дожидается воркеров — данные обязаны быть в репозитории

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.clicks) != 10 {
		t.Errorf("ожидали 10 сохранённых кликов, получили %d", len(repo.clicks))
	}
}

// Главная гарантия graceful shutdown: неполный батч не теряется.
func TestClickCollector_FlushesRemainderOnStop(t *testing.T) {
	repo := newStubRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	c := service.NewClickCollector(repo, log, service.CollectorConfig{
		BufferSize: 100, Workers: 2, BatchSize: 1000, FlushEvery: time.Hour,
	})

	for i := 0; i < 7; i++ {
		c.Record(domain.Click{LinkID: int64(i + 1)})
	}
	c.Stop()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.clicks) != 7 {
		t.Errorf("остаток батча потерян: сохранено %d из 7", len(repo.clicks))
	}
}

func TestClickCollector_StopIsIdempotent(t *testing.T) {
	repo := newStubRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := service.NewClickCollector(repo, log, service.CollectorConfig{Workers: 1})

	c.Stop()
	c.Stop() // sync.Once: повторный вызов не должен паниковать на закрытом канале
}

// Переполненная очередь не блокирует вызывающего — только увеличивает счётчик.
func TestClickCollector_DropsWhenFull(t *testing.T) {
	repo := newStubRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	c := service.NewClickCollector(repo, log, service.CollectorConfig{
		BufferSize: 1, Workers: 1, BatchSize: 1000, FlushEvery: time.Hour,
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			c.Record(domain.Click{LinkID: 1})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Record заблокировался при переполненной очереди")
	}
	c.Stop()
}
