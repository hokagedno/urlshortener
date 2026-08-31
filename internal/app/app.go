// Package app — composition root: единственное место, где конкретные
// реализации связываются друг с другом. Всё остальное приложение работает
// с интерфейсами и не знает, что под ними Postgres, Redis и Gin.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hokagedno/urlshortener/internal/config"
	"github.com/hokagedno/urlshortener/internal/repository/postgres"
	redisrepo "github.com/hokagedno/urlshortener/internal/repository/redis"
	"github.com/hokagedno/urlshortener/internal/service"
	httptransport "github.com/hokagedno/urlshortener/internal/transport/http"
)

func Run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("конфигурация: %w", err)
	}

	// signal.NotifyContext отменяет контекст по SIGINT/SIGTERM.
	// Docker при `docker stop` шлёт именно SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.Postgres.DSN,
		cfg.Postgres.MaxConns, cfg.Postgres.MinConns, cfg.Postgres.MaxConnLife)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	log.Info("миграции применены")

	redisClient, err := redisrepo.NewClient(ctx, cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer func() { _ = redisClient.Close() }()

	repo := postgres.NewLinkRepository(pool)
	cache := redisrepo.NewCache(redisClient)

	collector := service.NewClickCollector(repo, log, service.CollectorConfig{
		BufferSize: cfg.App.ClickBufferSize,
		Workers:    cfg.App.ClickWorkers,
		BatchSize:  cfg.App.ClickBatchSize,
		FlushEvery: cfg.App.ClickFlushEvery,
	})

	shortener := service.NewShortener(repo, cache, collector, log,
		service.WithCacheTTL(cfg.Redis.TTL))

	handler := httptransport.NewHandler(shortener, cfg.App.BaseURL, log)
	router := httptransport.NewRouter(handler, cache, log, httptransport.RouterConfig{
		BaseURL:    cfg.App.BaseURL,
		RateLimit:  cfg.App.RateLimit,
		RateWindow: cfg.App.RateWindow,
	})

	srv := &nethttp.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      router,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  60 * time.Second,
	}

	// Сервер слушает в отдельной горутине, а основная ждёт сигнала остановки.
	// Ошибка старта передаётся через канал: паниковать в горутине нельзя,
	// её никто не перехватит.
	errCh := make(chan error, 1)
	go func() {
		log.Info("http-сервер запущен", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http-сервер: %w", err)
	case <-ctx.Done():
		log.Info("получен сигнал остановки, завершаем работу")
	}

	// Graceful shutdown, порядок важен:
	//  1) сервер перестаёт принимать новые запросы и доигрывает текущие;
	//  2) сборщик кликов дописывает накопленный батч в БД;
	//  3) defer'ы закрывают пулы соединений.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http-сервер не остановился штатно", "err", err)
	}
	collector.Stop()
	log.Info("остановлено", "dropped_clicks", collector.Dropped())
	return nil
}
