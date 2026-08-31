package http

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hokagedno/urlshortener/internal/domain"
)

type RouterConfig struct {
	BaseURL    string
	RateLimit  int
	RateWindow time.Duration
	Debug      bool
}

// NewRouter собирает маршруты и цепочку middleware.
func NewRouter(h *Handler, cache domain.Cache, log *slog.Logger, cfg RouterConfig) *gin.Engine {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(recovery(log), requestID(), structuredLogger(log))
	r.RedirectTrailingSlash = false

	r.GET("/healthz", h.health)

	// Версионирование API в пути — простой способ выпускать несовместимые
	// изменения, не ломая существующих клиентов.
	api := r.Group("/api/v1")
	api.Use(rateLimit(cache, cfg.RateLimit, cfg.RateWindow, log))
	{
		api.POST("/links", h.createLink)
		api.GET("/links", h.listLinks)
		api.GET("/links/:code", h.getLink)
		api.GET("/links/:code/stats", h.stats)
		api.DELETE("/links/:code", h.deleteLink)
		// Отдельная ветка /top, а не /links/top: в роутере Gin статический
		// сегмент конфликтует с уже занятым параметром /links/:code.
		api.GET("/top", h.topLinks)
	}

	// Редирект регистрируется последним: маршрут "/:code" самый широкий
	// и не должен перехватывать /api/... и /healthz.
	r.GET("/:code", h.redirect)

	return r
}
