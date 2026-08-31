// Package redis — реализация domain.Cache поверх go-redis/v9.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hokagedno/urlshortener/internal/domain"
)

const keyPrefix = "url:"

type Cache struct {
	client *redis.Client
}

func NewCache(client *redis.Client) *Cache {
	return &Cache{client: client}
}

// NewClient создаёт клиент и проверяет соединение.
func NewClient(ctx context.Context, addr, password string, db int) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     20,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

// GetLink возвращает domain.ErrCacheMiss при отсутствии ключа.
// Промах кэша — штатная ситуация, поэтому она отделена от настоящих ошибок:
// сервис на неё реагирует походом в БД, а на остальные — логированием.
//
// Значение хранится в компактном виде "<id>|<url>", а не в JSON: разбор
// строки дешевле, а поле всего два. Разделитель '|' в URL не встречается
// (RFC 3986 требует его процентного кодирования).
func (c *Cache) GetLink(ctx context.Context, code string) (domain.CachedLink, error) {
	v, err := c.client.Get(ctx, keyPrefix+code).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return domain.CachedLink{}, domain.ErrCacheMiss
		}
		return domain.CachedLink{}, fmt.Errorf("redis get: %w", err)
	}

	sep := strings.IndexByte(v, '|')
	if sep < 0 {
		// Повреждённое или устаревшее по формату значение: считаем промахом,
		// сервис сходит в БД и перезапишет ключ.
		return domain.CachedLink{}, domain.ErrCacheMiss
	}
	linkID, err := strconv.ParseInt(v[:sep], 10, 64)
	if err != nil {
		return domain.CachedLink{}, domain.ErrCacheMiss
	}
	return domain.CachedLink{ID: linkID, URL: v[sep+1:]}, nil
}

func (c *Cache) SetLink(ctx context.Context, code string, l domain.CachedLink, ttl time.Duration) error {
	value := strconv.FormatInt(l.ID, 10) + "|" + l.URL
	if err := c.client.Set(ctx, keyPrefix+code, value, ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, code string) error {
	if err := c.client.Del(ctx, keyPrefix+code).Err(); err != nil {
		return fmt.Errorf("redis del: %w", err)
	}
	return nil
}

// Allow — счётчик запросов в фиксированном окне (fixed window rate limiting).
//
// INCR и EXPIRE отправляются одним pipeline: это один round-trip к Redis
// вместо двух. INCR атомарен, поэтому параллельные запросы к одному ключу
// не собьют счётчик.
//
// Используется ExpireNX (EXPIRE ... NX), то есть TTL ставится только если его
// ещё нет. Обычный EXPIRE на каждом запросе бесконечно продлевал бы окно,
// и лимит для активного клиента никогда бы не сбрасывался.
func (c *Cache) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	fullKey := "rl:" + key

	pipe := c.client.TxPipeline()
	incr := pipe.Incr(ctx, fullKey)
	pipe.ExpireNX(ctx, fullKey, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, fmt.Errorf("redis rate limit: %w", err)
	}

	return incr.Val() <= int64(limit), nil
}
