// Package config читает конфигурацию из переменных окружения.
// Специально без сторонних библиотек (viper и т.п.) — стандартной библиотеки
// достаточно, а зависимостей в проекте становится меньше.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTP     HTTPConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	App      AppConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

type PostgresConfig struct {
	DSN         string
	MaxConns    int32
	MinConns    int32
	MaxConnLife time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	TTL      time.Duration
}

type AppConfig struct {
	BaseURL         string        // публичный адрес, из него собирается короткая ссылка
	ClickBufferSize int           // размер канала для асинхронной записи кликов
	ClickWorkers    int           // количество воркеров-потребителей
	ClickBatchSize  int           // сколько кликов копим перед вставкой
	ClickFlushEvery time.Duration // максимальная задержка сброса батча
	RateLimit       int           // запросов на IP в окне
	RateWindow      time.Duration
}

// Load собирает конфиг и валидирует обязательные поля.
func Load() (Config, error) {
	cfg := Config{
		HTTP: HTTPConfig{
			Addr:            env("HTTP_ADDR", ":8080"),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 5*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		Postgres: PostgresConfig{
			DSN:         env("POSTGRES_DSN", "postgres://shortener:shortener@localhost:5432/shortener?sslmode=disable"),
			MaxConns:    int32(envInt("POSTGRES_MAX_CONNS", 10)),
			MinConns:    int32(envInt("POSTGRES_MIN_CONNS", 2)),
			MaxConnLife: envDuration("POSTGRES_CONN_LIFETIME", time.Hour),
		},
		Redis: RedisConfig{
			Addr:     env("REDIS_ADDR", "localhost:6379"),
			Password: env("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0),
			TTL:      envDuration("REDIS_TTL", 24*time.Hour),
		},
		App: AppConfig{
			BaseURL:         env("BASE_URL", "http://localhost:8080"),
			ClickBufferSize: envInt("CLICK_BUFFER_SIZE", 1024),
			ClickWorkers:    envInt("CLICK_WORKERS", 4),
			ClickBatchSize:  envInt("CLICK_BATCH_SIZE", 50),
			ClickFlushEvery: envDuration("CLICK_FLUSH_EVERY", 2*time.Second),
			RateLimit:       envInt("RATE_LIMIT", 60),
			RateWindow:      envDuration("RATE_WINDOW", time.Minute),
		},
	}

	if cfg.Postgres.DSN == "" {
		return Config{}, fmt.Errorf("config: POSTGRES_DSN обязателен")
	}
	if cfg.App.ClickWorkers <= 0 {
		return Config{}, fmt.Errorf("config: CLICK_WORKERS должен быть > 0")
	}
	return cfg, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
