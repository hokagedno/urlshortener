// Package service содержит бизнес-логику. Он зависит только от domain
// (интерфейсов), но не от Gin, pgx или redis — Dependency Inversion.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/hokagedno/urlshortener/internal/domain"
	"github.com/hokagedno/urlshortener/pkg/base62"
)

const (
	minAliasLen = 3
	maxAliasLen = 32
	maxURLLen   = 2048
)

// Shortener — сервис коротких ссылок.
//
// Зависимости приходят снаружи через конструктор (Dependency Injection),
// поэтому в тестах вместо Postgres и Redis подставляются простые стабы.
type Shortener struct {
	repo     domain.LinkRepository
	cache    domain.Cache
	clicks   ClickRecorder
	log      *slog.Logger
	cacheTTL time.Duration
	now      func() time.Time // подменяется в тестах вместо time.Now
}

// ClickRecorder — асинхронный приёмник переходов.
// Отдельный маленький интерфейс вместо «толстого» — Interface Segregation.
type ClickRecorder interface {
	Record(c domain.Click)
}

// Option — функциональная опция (паттерн Functional Options).
// Позволяет расширять конструктор без ломающих изменений сигнатуры.
type Option func(*Shortener)

func WithCacheTTL(ttl time.Duration) Option {
	return func(s *Shortener) { s.cacheTTL = ttl }
}

func WithClock(now func() time.Time) Option {
	return func(s *Shortener) { s.now = now }
}

func NewShortener(
	repo domain.LinkRepository,
	cache domain.Cache,
	clicks ClickRecorder,
	log *slog.Logger,
	opts ...Option,
) *Shortener {
	s := &Shortener{
		repo:     repo,
		cache:    cache,
		clicks:   clicks,
		log:      log,
		cacheTTL: 24 * time.Hour,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateInput — входные данные для создания ссылки.
type CreateInput struct {
	OriginalURL string
	Alias       string // необязательный пользовательский код
	ExpiresAt   *time.Time
}

// Create создаёт короткую ссылку.
//
// Код генерируется из id, выданного последовательностью Postgres:
// sequence атомарен, поэтому коллизий кодов не бывает и не нужен
// цикл «сгенерировать случайный код → проверить занятость → повторить».
func (s *Shortener) Create(ctx context.Context, in CreateInput) (domain.Link, error) {
	normalized, err := normalizeURL(in.OriginalURL)
	if err != nil {
		return domain.Link{}, err
	}

	link := domain.Link{
		OriginalURL: normalized,
		CreatedAt:   s.now().UTC(),
		ExpiresAt:   in.ExpiresAt,
	}

	if in.Alias != "" {
		if err := validateAlias(in.Alias); err != nil {
			return domain.Link{}, err
		}
		link.Code = in.Alias
	} else {
		id, err := s.repo.NextID(ctx)
		if err != nil {
			return domain.Link{}, fmt.Errorf("получить id: %w", err)
		}
		link.ID = id
		// Сдвиг делает первые коды не однобуквенными («0», «1»),
		// иначе короткие коды пересекались бы с пользовательскими алиасами.
		link.Code = base62.Encode(id + 100_000)
	}

	if err := s.repo.Create(ctx, &link); err != nil {
		return domain.Link{}, err
	}

	s.log.InfoContext(ctx, "ссылка создана", "code", link.Code, "url", link.OriginalURL)
	return link, nil
}

// Resolve возвращает оригинальный URL по коду и регистрирует переход.
//
// Стратегия кэширования — cache-aside (lazy loading):
//  1. читаем из Redis;
//  2. если промах — читаем из Postgres;
//  3. кладём в Redis с TTL.
//
// Ошибка Redis не должна ронять запрос: кэш — это оптимизация,
// а не источник истины, поэтому она только логируется.
func (s *Shortener) Resolve(ctx context.Context, code string, click domain.Click) (string, error) {
	if !base62.IsValid(code) && !isValidAlias(code) {
		return "", domain.ErrNotFound
	}

	if cached, err := s.cache.GetLink(ctx, code); err == nil {
		// id берётся из кэша, поэтому клик долетает до статистики
		// и на «горячем» пути, без похода в БД.
		click.LinkID = cached.ID
		s.recordClick(click)
		return cached.URL, nil
	} else if !errors.Is(err, domain.ErrCacheMiss) {
		s.log.WarnContext(ctx, "ошибка чтения кэша", "code", code, "err", err)
	}

	link, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return "", err
	}
	if link.IsExpired(s.now()) {
		return "", domain.ErrExpired
	}

	ttl := s.cacheTTL
	if link.ExpiresAt != nil {
		// Кэш не должен жить дольше самой ссылки.
		if left := time.Until(*link.ExpiresAt); left < ttl {
			ttl = left
		}
	}
	if ttl > 0 {
		cached := domain.CachedLink{ID: link.ID, URL: link.OriginalURL}
		if err := s.cache.SetLink(ctx, code, cached, ttl); err != nil {
			s.log.WarnContext(ctx, "ошибка записи в кэш", "code", code, "err", err)
		}
	}

	click.LinkID = link.ID
	s.recordClick(click)
	return link.OriginalURL, nil
}

func (s *Shortener) Get(ctx context.Context, code string) (domain.Link, error) {
	return s.repo.GetByCode(ctx, code)
}

func (s *Shortener) List(ctx context.Context, limit, offset int) ([]domain.Link, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.List(ctx, limit, offset)
}

func (s *Shortener) Stats(ctx context.Context, code string) (domain.LinkStats, error) {
	return s.repo.StatsByCode(ctx, code)
}

// Top возвращает самые популярные ссылки — отчёт по числу переходов.
//
// limit нормализуется здесь, а не в SQL: значение приходит от клиента через
// query-параметр, а защита от запроса «отдай миллион строк» — это правило
// бизнес-логики, которому не место ни в хендлере, ни в тексте запроса.
func (s *Shortener) Top(ctx context.Context, limit int) ([]domain.TopLink, error) {
	const (
		defaultLimit = 10
		maxLimit     = 100
	)

	// Два случая обрабатываются по-разному. Не указан лимит — подставляем
	// разумное значение по умолчанию. Запрошено слишком много — отдаём
	// максимум разрешённого, а не откатываемся к дефолту: молча подменять
	// «дай 500» на «дай 10» значит обманывать клиента API.
	switch {
	case limit <= 0:
		limit = defaultLimit
	case limit > maxLimit:
		limit = maxLimit
	}

	return s.repo.Top(ctx, limit)
}

// Delete удаляет ссылку и обязательно инвалидирует кэш,
// иначе редирект продолжит работать из Redis до истечения TTL.
func (s *Shortener) Delete(ctx context.Context, code string) error {
	if err := s.repo.DeleteByCode(ctx, code); err != nil {
		return err
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		s.log.WarnContext(ctx, "не удалось инвалидировать кэш", "code", code, "err", err)
	}
	return nil
}

func (s *Shortener) recordClick(c domain.Click) {
	if s.clicks == nil || c.LinkID == 0 {
		return
	}
	c.CreatedAt = s.now().UTC()
	s.clicks.Record(c)
}

// --- валидация ------------------------------------------------------------

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return "", domain.ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s", domain.ErrInvalidURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: поддерживаются только http и https", domain.ErrInvalidURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: пустой хост", domain.ErrInvalidURL)
	}
	return u.String(), nil
}

func validateAlias(alias string) error {
	if !isValidAlias(alias) {
		return fmt.Errorf("%w: %d–%d символов [0-9a-zA-Z_-]", domain.ErrInvalidAlias, minAliasLen, maxAliasLen)
	}
	return nil
}

func isValidAlias(alias string) bool {
	if len(alias) < minAliasLen || len(alias) > maxAliasLen {
		return false
	}
	for _, r := range alias {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
