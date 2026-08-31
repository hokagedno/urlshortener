package http

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hokagedno/urlshortener/internal/domain"
)

const requestIDHeader = "X-Request-ID"

// Middleware в Gin — это паттерн «Цепочка обязанностей» (Chain of Responsibility)
// в связке с «Декоратором»: каждый слой оборачивает следующий,
// добавляя своё поведение, и решает, передавать ли управление дальше.

// requestID проставляет идентификатор запроса и кладёт его в контекст,
// чтобы все логи одного запроса можно было связать между собой.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		c.Set("request_id", id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// structuredLogger пишет один структурированный лог на запрос через log/slog
// (стандартная библиотека, Go 1.21+) вместо форматированной строки gin.Logger.
func structuredLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next() // передаём управление дальше по цепочке

		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		log.Log(c.Request.Context(), level, "http request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
			"request_id", c.GetString("request_id"),
		)
	}
}

// rateLimit ограничивает частоту запросов по IP, используя счётчик в Redis.
// Хранилище общее, поэтому лимит работает корректно и при нескольких репликах
// сервиса — в отличие от лимитера в памяти процесса.
func rateLimit(cache domain.Cache, limit int, window time.Duration, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed, err := cache.Allow(c.Request.Context(), c.ClientIP(), limit, window)
		if err != nil {
			// Fail-open: недоступность Redis не должна ронять весь сервис.
			log.WarnContext(c.Request.Context(), "rate limiter недоступен", "err", err)
			c.Next()
			return
		}
		if !allowed {
			c.Header("Retry-After", window.String())
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				errorResponse{Error: "слишком много запросов"})
			return
		}
		c.Next()
	}
}

// recovery перехватывает панику в хендлере, чтобы процесс не падал целиком.
func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, err any) {
		log.ErrorContext(c.Request.Context(), "паника в хендлере",
			"err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError,
			errorResponse{Error: "внутренняя ошибка сервера"})
	})
}
