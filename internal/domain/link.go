// Package domain содержит модели предметной области и порты (интерфейсы),
// от которых зависит бизнес-логика. Здесь нет ни одного импорта фреймворка
// или драйвера БД — это и есть Dependency Inversion из SOLID:
// внутренние слои не знают о внешних.
package domain

import (
	"context"
	"time"
)

// Link — короткая ссылка. Агрегат предметной области.
type Link struct {
	ID          int64      `json:"-"`
	Code        string     `json:"code"` // короткий код: "aX9zQ"
	OriginalURL string     `json:"original_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // nil = бессрочная
}

// IsExpired сообщает, истёк ли срок жизни ссылки.
// Метод на доменной модели, а не в сервисе: правило принадлежит самой сущности.
func (l Link) IsExpired(now time.Time) bool {
	return l.ExpiresAt != nil && now.After(*l.ExpiresAt)
}

// Click — факт перехода по ссылке. Пишется асинхронно (см. internal/service/clicks.go).
type Click struct {
	LinkID    int64
	IP        string
	UserAgent string
	Referer   string
	CreatedAt time.Time
}

// LinkStats — результат агрегирующего запроса с JOIN (links + clicks).
type LinkStats struct {
	Link        Link       `json:"link"`
	TotalClicks int64      `json:"total_clicks"`
	UniqueIPs   int64      `json:"unique_ips"`
	LastClickAt *time.Time `json:"last_click_at,omitempty"`
}

// TopLink — строка отчёта «самые популярные ссылки».
type TopLink struct {
	Link   Link  `json:"link"`
	Clicks int64 `json:"clicks"`
}

// --- Порты (интерфейсы), которые реализуют внешние слои -------------------
//
// Интерфейс объявлен на стороне ПОТРЕБИТЕЛЯ (Go-идиома "accept interfaces,
// return structs"). Благодаря этому сервис можно тестировать со стабами,
// а Postgres при желании заменить на любое другое хранилище.

// LinkRepository — доступ к постоянному хранилищу ссылок.
type LinkRepository interface {
	// Create сохраняет ссылку. Возвращает ErrCodeTaken, если код занят.
	Create(ctx context.Context, l *Link) error
	// NextID резервирует новый числовой идентификатор (Postgres sequence).
	NextID(ctx context.Context) (int64, error)
	GetByCode(ctx context.Context, code string) (Link, error)
	DeleteByCode(ctx context.Context, code string) error
	List(ctx context.Context, limit, offset int) ([]Link, error)
	StatsByCode(ctx context.Context, code string) (LinkStats, error)
	// Top возвращает limit самых популярных ссылок по числу переходов.
	Top(ctx context.Context, limit int) ([]TopLink, error)
	// SaveClicks — пакетная вставка переходов одной транзакцией.
	SaveClicks(ctx context.Context, clicks []Click) error
}

// CachedLink — то, что кладётся в кэш.
//
// В кэше хранится не только URL, но и id ссылки: иначе при попадании в кэш
// у клика не было бы link_id и статистика теряла бы все переходы,
// обслуженные из Redis (а это подавляющее большинство).
type CachedLink struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

// Cache — кэш «код → ссылка» (реализация на Redis).
type Cache interface {
	GetLink(ctx context.Context, code string) (CachedLink, error)
	SetLink(ctx context.Context, code string, l CachedLink, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
	// Allow — счётчик для rate limiting (INCR + EXPIRE).
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}
