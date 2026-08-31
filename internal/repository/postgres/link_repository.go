// Package postgres — реализация портов domain.LinkRepository поверх pgx/v5.
//
// Здесь и только здесь знают про SQL. Сервисный слой работает с интерфейсом,
// поэтому замена Postgres на другое хранилище не затронет бизнес-логику.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hokagedno/urlshortener/internal/domain"
)

// uniqueViolation — код ошибки PostgreSQL при нарушении UNIQUE-ограничения.
const uniqueViolation = "23505"

type LinkRepository struct {
	pool *pgxpool.Pool
}

func NewLinkRepository(pool *pgxpool.Pool) *LinkRepository {
	return &LinkRepository{pool: pool}
}

// NextID берёт следующее значение последовательности.
// nextval атомарен и не блокируется другими транзакциями,
// поэтому параллельные запросы не получат одинаковый id.
func (r *LinkRepository) NextID(ctx context.Context) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT nextval('links_id_seq')`).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("nextval: %w", err)
	}
	return id, nil
}

func (r *LinkRepository) Create(ctx context.Context, l *domain.Link) error {
	const q = `
		INSERT INTO links (id, code, original_url, created_at, expires_at)
		VALUES (COALESCE(NULLIF($1, 0), nextval('links_id_seq')), $2, $3, $4, $5)
		RETURNING id`

	err := r.pool.QueryRow(ctx, q, l.ID, l.Code, l.OriginalURL, l.CreatedAt, l.ExpiresAt).Scan(&l.ID)
	if err != nil {
		// Отличаем «код занят» от прочих ошибок по SQLSTATE,
		// а не по тексту сообщения — текст зависит от локали и версии СУБД.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: %s", domain.ErrCodeTaken, l.Code)
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return nil
}

func (r *LinkRepository) GetByCode(ctx context.Context, code string) (domain.Link, error) {
	const q = `
		SELECT id, code, original_url, created_at, expires_at
		FROM links
		WHERE code = $1`

	var l domain.Link
	err := r.pool.QueryRow(ctx, q, code).
		Scan(&l.ID, &l.Code, &l.OriginalURL, &l.CreatedAt, &l.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Link{}, domain.ErrNotFound
		}
		return domain.Link{}, fmt.Errorf("select link: %w", err)
	}
	return l, nil
}

func (r *LinkRepository) DeleteByCode(ctx context.Context, code string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM links WHERE code = $1`, code)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// List — постраничный вывод. Индекс по created_at DESC делает сортировку
// без отдельного шага Sort в плане запроса.
func (r *LinkRepository) List(ctx context.Context, limit, offset int) ([]domain.Link, error) {
	const q = `
		SELECT id, code, original_url, created_at, expires_at
		FROM links
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2`

	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("select links: %w", err)
	}
	defer rows.Close()

	links := make([]domain.Link, 0, limit)
	for rows.Next() {
		var l domain.Link
		if err := rows.Scan(&l.ID, &l.Code, &l.OriginalURL, &l.CreatedAt, &l.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		links = append(links, l)
	}
	// rows.Err() обязателен: ошибка чтения может прийти уже после последнего Next().
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate links: %w", err)
	}
	return links, nil
}

// Top возвращает самые популярные ссылки по числу переходов.
//
// LEFT JOIN, а не INNER: ссылки без переходов тоже должны попадать в отчёт
// с нулём, иначе на свежей базе «топ» окажется пустым.
//
// COUNT(c.link_id), а не COUNT(*): при LEFT JOIN у ссылки без кликов
// появляется строка с NULL в колонках clicks, и COUNT(*) посчитал бы её
// за единицу. COUNT по конкретной колонке игнорирует NULL и даёт честный 0.
//
// Во втором ключе сортировки — created_at: без него порядок ссылок
// с одинаковым числом переходов не определён и может меняться от запроса
// к запросу, что ломает пагинацию и делает тесты нестабильными.
func (r *LinkRepository) Top(ctx context.Context, limit int) ([]domain.TopLink, error) {
	const q = `
		SELECT l.id, l.code, l.original_url, l.created_at, l.expires_at,
		       COUNT(c.link_id) AS clicks
		FROM links l
		LEFT JOIN clicks c ON c.link_id = l.id
		GROUP BY l.id
		ORDER BY clicks DESC, l.created_at DESC
		LIMIT $1`

	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("select top links: %w", err)
	}
	defer rows.Close()

	top := make([]domain.TopLink, 0, limit)
	for rows.Next() {
		var t domain.TopLink
		err := rows.Scan(
			&t.Link.ID, &t.Link.Code, &t.Link.OriginalURL,
			&t.Link.CreatedAt, &t.Link.ExpiresAt,
			&t.Clicks,
		)
		if err != nil {
			return nil, fmt.Errorf("scan top link: %w", err)
		}
		top = append(top, t)
	}
	// Ошибка чтения может прийти уже после последнего Next(),
	// поэтому проверять rows.Err() обязательно.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top links: %w", err)
	}
	return top, nil
}

// StatsByCode — пример агрегирующего запроса с LEFT JOIN.
//
// LEFT JOIN, а не INNER: у ссылки без переходов должна вернуться строка
// с нулевыми счётчиками, а не пустой результат.
// COUNT(c.link_id) считает только не-NULL, поэтому для ссылки без кликов даёт 0.
func (r *LinkRepository) StatsByCode(ctx context.Context, code string) (domain.LinkStats, error) {
	const q = `
		SELECT l.id, l.code, l.original_url, l.created_at, l.expires_at,
		       COUNT(c.link_id)              AS total_clicks,
		       COUNT(DISTINCT c.ip)          AS unique_ips,
		       MAX(c.created_at)             AS last_click_at
		FROM links l
		LEFT JOIN clicks c ON c.link_id = l.id
		WHERE l.code = $1
		GROUP BY l.id`

	var st domain.LinkStats
	err := r.pool.QueryRow(ctx, q, code).Scan(
		&st.Link.ID, &st.Link.Code, &st.Link.OriginalURL, &st.Link.CreatedAt, &st.Link.ExpiresAt,
		&st.TotalClicks, &st.UniqueIPs, &st.LastClickAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.LinkStats{}, domain.ErrNotFound
		}
		return domain.LinkStats{}, fmt.Errorf("select stats: %w", err)
	}
	return st, nil
}

// SaveClicks вставляет батч переходов одной транзакцией.
//
// pgx.Batch отправляет все команды одним сетевым пакетом — это на порядок
// быстрее, чем N отдельных INSERT с round-trip на каждый.
// Транзакция даёт атомарность: либо батч записан целиком, либо не записан вовсе.
func (r *LinkRepository) SaveClicks(ctx context.Context, clicks []domain.Click) error {
	if len(clicks) == 0 {
		return nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Rollback после успешного Commit — no-op, поэтому defer безопасен
	// и гарантирует откат на любом раннем return или панике.
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, c := range clicks {
		batch.Queue(
			`INSERT INTO clicks (link_id, ip, user_agent, referer, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			c.LinkID, c.IP, truncate(c.UserAgent, 512), truncate(c.Referer, 512), c.CreatedAt,
		)
	}

	br := tx.SendBatch(ctx, batch)
	for i := 0; i < len(clicks); i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("batch insert click #%d: %w", i, err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close batch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// NewPool создаёт пул соединений и проверяет доступность БД.
func NewPool(ctx context.Context, dsn string, maxConns, minConns int32, lifetime time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DSN: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = lifetime
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
