// Package http — транспортный слой на Gin.
//
// Задача слоя: разобрать HTTP-запрос, вызвать сервис, превратить доменную
// ошибку в статус-код. Никакой бизнес-логики здесь быть не должно.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hokagedno/urlshortener/internal/domain"
	"github.com/hokagedno/urlshortener/internal/service"
)

// ShortenerService — порт, который нужен хендлеру.
// Интерфейс объявлен здесь, у потребителя: транспорт не тянет за собой
// весь сервис целиком и легко подменяется в тестах.
type ShortenerService interface {
	Create(ctx context.Context, in service.CreateInput) (domain.Link, error)
	Resolve(ctx context.Context, code string, click domain.Click) (string, error)
	Get(ctx context.Context, code string) (domain.Link, error)
	List(ctx context.Context, limit, offset int) ([]domain.Link, error)
	Stats(ctx context.Context, code string) (domain.LinkStats, error)
	Top(ctx context.Context, limit int) ([]domain.TopLink, error)
	Delete(ctx context.Context, code string) error
}

type Handler struct {
	svc     ShortenerService
	baseURL string
	log     *slog.Logger
}

func NewHandler(svc ShortenerService, baseURL string, log *slog.Logger) *Handler {
	return &Handler{svc: svc, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// createLink godoc: POST /api/v1/links
func (h *Handler) createLink(c *gin.Context) {
	var req createLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse{Error: "некорректное тело запроса", Details: err.Error()})
		return
	}

	link, err := h.svc.Create(c.Request.Context(), service.CreateInput{
		OriginalURL: req.URL,
		Alias:       req.Alias,
		ExpiresAt:   req.ExpiresAt,
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	c.Header("Location", h.shortURL(link.Code))
	c.JSON(http.StatusCreated, h.toResponse(link))
}

// redirect godoc: GET /:code — основной «горячий» путь.
func (h *Handler) redirect(c *gin.Context) {
	code := c.Param("code")

	click := domain.Click{
		IP:        c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
		Referer:   c.Request.Referer(),
	}

	target, err := h.svc.Resolve(c.Request.Context(), code, click)
	if err != nil {
		h.fail(c, err)
		return
	}

	// 302, а не 301: постоянный редирект браузер кэширует навсегда,
	// после чего переходы перестают доходить до сервиса и статистика ломается.
	c.Redirect(http.StatusFound, target)
}

// getLink godoc: GET /api/v1/links/:code
func (h *Handler) getLink(c *gin.Context) {
	link, err := h.svc.Get(c.Request.Context(), c.Param("code"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, h.toResponse(link))
}

// listLinks godoc: GET /api/v1/links?limit=&offset=
func (h *Handler) listLinks(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	links, err := h.svc.List(c.Request.Context(), limit, offset)
	if err != nil {
		h.fail(c, err)
		return
	}

	items := make([]linkResponse, 0, len(links))
	for _, l := range links {
		items = append(items, h.toResponse(l))
	}
	c.JSON(http.StatusOK, listResponse{Items: items, Limit: limit, Offset: offset})
}

// stats godoc: GET /api/v1/links/:code/stats
func (h *Handler) stats(c *gin.Context) {
	st, err := h.svc.Stats(c.Request.Context(), c.Param("code"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, statsResponse{
		Code:        st.Link.Code,
		OriginalURL: st.Link.OriginalURL,
		TotalClicks: st.TotalClicks,
		UniqueIPs:   st.UniqueIPs,
		LastClickAt: st.LastClickAt,
	})
}

// topLinks godoc: GET /api/v1/top?limit=10
func (h *Handler) topLinks(c *gin.Context) {
	// Ошибку Atoi намеренно игнорируем: при мусоре в query-параметре
	// Atoi вернёт 0, а сервис подставит значение по умолчанию.
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))

	top, err := h.svc.Top(c.Request.Context(), limit)
	if err != nil {
		h.fail(c, err)
		return
	}

	// Доменные модели превращаются в DTO: контракт API не должен
	// меняться сам собой вслед за структурами внутри приложения.
	items := make([]topLinkResponse, 0, len(top))
	for _, t := range top {
		items = append(items, topLinkResponse{
			Code:        t.Link.Code,
			ShortURL:    h.shortURL(t.Link.Code),
			OriginalURL: t.Link.OriginalURL,
			Clicks:      t.Clicks,
		})
	}

	c.JSON(http.StatusOK, topResponse{Items: items, Limit: limit})
}

// deleteLink godoc: DELETE /api/v1/links/:code
func (h *Handler) deleteLink(c *gin.Context) {
	if err := h.svc.Delete(c.Request.Context(), c.Param("code")); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) toResponse(l domain.Link) linkResponse {
	return linkResponse{
		Code:        l.Code,
		ShortURL:    h.shortURL(l.Code),
		OriginalURL: l.OriginalURL,
		CreatedAt:   l.CreatedAt,
		ExpiresAt:   l.ExpiresAt,
	}
}

func (h *Handler) shortURL(code string) string { return h.baseURL + "/" + code }

// fail — единая точка преобразования доменной ошибки в HTTP-ответ.
// errors.Is корректно разворачивает цепочку обёрток из fmt.Errorf("%w").
func (h *Handler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		c.JSON(http.StatusNotFound, errorResponse{Error: "ссылка не найдена"})
	case errors.Is(err, domain.ErrExpired):
		c.JSON(http.StatusGone, errorResponse{Error: "срок действия ссылки истёк"})
	case errors.Is(err, domain.ErrCodeTaken):
		c.JSON(http.StatusConflict, errorResponse{Error: "такой алиас уже занят"})
	case errors.Is(err, domain.ErrInvalidURL), errors.Is(err, domain.ErrInvalidAlias):
		c.JSON(http.StatusBadRequest, errorResponse{Error: err.Error()})
	default:
		// Внутренние ошибки наружу не отдаём — только в лог,
		// чтобы не раскрывать структуру БД и внутренние адреса.
		h.log.ErrorContext(c.Request.Context(), "внутренняя ошибка", "err", err, "path", c.FullPath())
		c.JSON(http.StatusInternalServerError, errorResponse{Error: "внутренняя ошибка сервера"})
	}
}
