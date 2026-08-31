package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hokagedno/urlshortener/internal/domain"
	"github.com/hokagedno/urlshortener/internal/service"
)

// --- стабы вместо Postgres и Redis ---------------------------------------
//
// Интерфейсы в domain позволяют тестировать бизнес-логику без БД:
// тесты выполняются за миллисекунды и не требуют docker-compose.

type stubRepo struct {
	mu       sync.Mutex
	links    map[string]domain.Link
	nextID   int64
	clicks   []domain.Click
	failNext error

	// Поля для проверки Shortener.Top: стаб запоминает, с каким limit
	// его позвали, и отдаёт заранее заготовленный результат.
	topLimit  int
	topResult []domain.TopLink
	topErr    error
}

func newStubRepo() *stubRepo {
	return &stubRepo{links: make(map[string]domain.Link), nextID: 1}
}

func (r *stubRepo) NextID(context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	return id, nil
}

func (r *stubRepo) Create(_ context.Context, l *domain.Link) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return err
	}
	if _, exists := r.links[l.Code]; exists {
		return domain.ErrCodeTaken
	}
	r.links[l.Code] = *l
	return nil
}

func (r *stubRepo) GetByCode(_ context.Context, code string) (domain.Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.links[code]
	if !ok {
		return domain.Link{}, domain.ErrNotFound
	}
	return l, nil
}

func (r *stubRepo) DeleteByCode(_ context.Context, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.links[code]; !ok {
		return domain.ErrNotFound
	}
	delete(r.links, code)
	return nil
}

func (r *stubRepo) List(context.Context, int, int) ([]domain.Link, error) { return nil, nil }

func (r *stubRepo) StatsByCode(context.Context, string) (domain.LinkStats, error) {
	return domain.LinkStats{}, nil
}

func (r *stubRepo) Top(_ context.Context, limit int) ([]domain.TopLink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topLimit = limit
	return r.topResult, r.topErr
}

func (r *stubRepo) SaveClicks(_ context.Context, cs []domain.Click) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clicks = append(r.clicks, cs...)
	return nil
}

type stubCache struct {
	mu      sync.Mutex
	data    map[string]domain.CachedLink
	gets    int
	sets    int
	deletes int
}

func newStubCache() *stubCache { return &stubCache{data: make(map[string]domain.CachedLink)} }

func (c *stubCache) GetLink(_ context.Context, code string) (domain.CachedLink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	v, ok := c.data[code]
	if !ok {
		return domain.CachedLink{}, domain.ErrCacheMiss
	}
	return v, nil
}

func (c *stubCache) SetLink(_ context.Context, code string, l domain.CachedLink, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	c.data[code] = l
	return nil
}

func (c *stubCache) Delete(_ context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	delete(c.data, code)
	return nil
}

func (c *stubCache) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return true, nil
}

type stubClicks struct {
	mu   sync.Mutex
	list []domain.Click
}

func (s *stubClicks) Record(c domain.Click) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, c)
}

func (s *stubClicks) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

func newSUT(t *testing.T) (*service.Shortener, *stubRepo, *stubCache, *stubClicks) {
	t.Helper()
	repo, cache, clicks := newStubRepo(), newStubCache(), &stubClicks{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.NewShortener(repo, cache, clicks, log, service.WithCacheTTL(time.Minute))
	return svc, repo, cache, clicks
}

// --- тесты ----------------------------------------------------------------

func TestCreate_GeneratesCode(t *testing.T) {
	svc, _, _, _ := newSUT(t)

	link, err := svc.Create(context.Background(), service.CreateInput{OriginalURL: "https://example.com/page"})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if link.Code == "" {
		t.Fatal("код не сгенерирован")
	}
	if link.OriginalURL != "https://example.com/page" {
		t.Errorf("URL изменился: %q", link.OriginalURL)
	}
}

func TestCreate_RejectsBadURL(t *testing.T) {
	svc, _, _, _ := newSUT(t)

	cases := map[string]string{
		"пустой":           "",
		"без схемы":        "example.com",
		"неподдерж. схема": "ftp://example.com",
		"без хоста":        "https://",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), service.CreateInput{OriginalURL: raw})
			if !errors.Is(err, domain.ErrInvalidURL) {
				t.Errorf("ожидали ErrInvalidURL, получили %v", err)
			}
		})
	}
}

func TestCreate_CustomAliasConflict(t *testing.T) {
	svc, _, _, _ := newSUT(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, service.CreateInput{OriginalURL: "https://a.example", Alias: "my-link"}); err != nil {
		t.Fatalf("первое создание не должно падать: %v", err)
	}
	_, err := svc.Create(ctx, service.CreateInput{OriginalURL: "https://b.example", Alias: "my-link"})
	if !errors.Is(err, domain.ErrCodeTaken) {
		t.Errorf("ожидали ErrCodeTaken, получили %v", err)
	}
}

// Ключевой тест кэширования: второй Resolve не должен ходить в репозиторий.
func TestResolve_UsesCacheOnSecondCall(t *testing.T) {
	svc, repo, cache, clicks := newSUT(t)
	ctx := context.Background()

	link, _ := svc.Create(ctx, service.CreateInput{OriginalURL: "https://example.com"})

	if _, err := svc.Resolve(ctx, link.Code, domain.Click{}); err != nil {
		t.Fatalf("первый resolve: %v", err)
	}
	if cache.sets != 1 {
		t.Errorf("после промаха ожидали 1 запись в кэш, получили %d", cache.sets)
	}

	// Убираем ссылку из «БД»: если второй вызов пройдёт — значит, ответ из кэша.
	repo.mu.Lock()
	delete(repo.links, link.Code)
	repo.mu.Unlock()

	got, err := svc.Resolve(ctx, link.Code, domain.Click{})
	if err != nil {
		t.Fatalf("второй resolve должен был попасть в кэш: %v", err)
	}
	if got != "https://example.com" {
		t.Errorf("получили %q", got)
	}

	// Переход, обслуженный из кэша, тоже обязан попасть в статистику:
	// иначе счётчики теряли бы подавляющее большинство кликов.
	if clicks.count() != 2 {
		t.Errorf("ожидали 2 зарегистрированных клика, получили %d", clicks.count())
	}
}

func TestResolve_ExpiredLink(t *testing.T) {
	repo, cache, clicks := newStubRepo(), newStubCache(), &stubClicks{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc := service.NewShortener(repo, cache, clicks, log,
		service.WithClock(func() time.Time { return now }))

	past := now.Add(-time.Hour)
	link, err := svc.Create(context.Background(), service.CreateInput{
		OriginalURL: "https://example.com", ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("создание: %v", err)
	}

	if _, err := svc.Resolve(context.Background(), link.Code, domain.Click{}); !errors.Is(err, domain.ErrExpired) {
		t.Errorf("ожидали ErrExpired, получили %v", err)
	}
}

func TestDelete_InvalidatesCache(t *testing.T) {
	svc, _, cache, _ := newSUT(t)
	ctx := context.Background()

	link, _ := svc.Create(ctx, service.CreateInput{OriginalURL: "https://example.com"})
	_, _ = svc.Resolve(ctx, link.Code, domain.Click{}) // прогреваем кэш

	if err := svc.Delete(ctx, link.Code); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if cache.deletes != 1 {
		t.Errorf("кэш не инвалидирован, deletes=%d", cache.deletes)
	}
	if _, err := svc.Resolve(ctx, link.Code, domain.Click{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("после удаления ожидали ErrNotFound, получили %v", err)
	}
}

func TestResolve_RecordsClick(t *testing.T) {
	svc, _, _, clicks := newSUT(t)
	ctx := context.Background()

	link, _ := svc.Create(ctx, service.CreateInput{OriginalURL: "https://example.com"})
	if _, err := svc.Resolve(ctx, link.Code, domain.Click{IP: "1.2.3.4"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if clicks.count() != 1 {
		t.Errorf("ожидали 1 зарегистрированный клик, получили %d", clicks.count())
	}
}

// Тест на гонки: запускать с `go test -race ./...`.
// Сервис не хранит изменяемого состояния, поэтому обязан быть безопасен
// при одновременном использовании из множества горутин.
func TestResolve_ConcurrentSafe(t *testing.T) {
	svc, _, _, _ := newSUT(t)
	ctx := context.Background()
	link, _ := svc.Create(ctx, service.CreateInput{OriginalURL: "https://example.com"})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.Resolve(ctx, link.Code, domain.Click{IP: "1.1.1.1"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("параллельный resolve вернул ошибку: %v", err)
	}
}

// --- ЗАДАНИЕ 1: тесты для Shortener.Top -----------------------------------
//
// Эти тесты падают, пока метод не реализован. Их менять не нужно —
// они описывают требуемое поведение.

func TestTop_NormalizesLimit(t *testing.T) {
	cases := []struct {
		name     string
		given    int
		expected int
	}{
		{"ноль заменяется значением по умолчанию", 0, 10},
		{"отрицательный заменяется значением по умолчанию", -5, 10},
		{"разумный передаётся как есть", 7, 7},
		{"слишком большой ограничивается сотней", 500, 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, _, _ := newSUT(t)

			if _, err := svc.Top(context.Background(), tc.given); err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}

			repo.mu.Lock()
			got := repo.topLimit
			repo.mu.Unlock()

			if got != tc.expected {
				t.Errorf("в репозиторий ушёл limit=%d, ожидали %d", got, tc.expected)
			}
		})
	}
}

func TestTop_ReturnsRepositoryResult(t *testing.T) {
	svc, repo, _, _ := newSUT(t)
	repo.topResult = []domain.TopLink{
		{Link: domain.Link{Code: "aaa", OriginalURL: "https://a.example"}, Clicks: 42},
		{Link: domain.Link{Code: "bbb", OriginalURL: "https://b.example"}, Clicks: 7},
	}

	got, err := svc.Top(context.Background(), 10)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("получили %d строк, ожидали 2", len(got))
	}
	if got[0].Link.Code != "aaa" || got[0].Clicks != 42 {
		t.Errorf("первая строка отчёта: %+v", got[0])
	}
}

func TestTop_PropagatesRepositoryError(t *testing.T) {
	svc, repo, _, _ := newSUT(t)
	repo.topErr = errors.New("база недоступна")

	if _, err := svc.Top(context.Background(), 10); err == nil {
		t.Error("ошибка репозитория должна возвращаться наружу")
	}
}
