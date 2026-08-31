package http

import "time"

// DTO транспортного слоя отделены от доменных моделей: контракт API можно
// менять, не трогая бизнес-логику, и наоборот. Теги binding — валидация Gin
// (под капотом go-playground/validator).

type createLinkRequest struct {
	URL       string     `json:"url" binding:"required,url,max=2048"`
	Alias     string     `json:"alias,omitempty" binding:"omitempty,min=3,max=32,alphanum|containsany=-_"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type linkResponse struct {
	Code        string     `json:"code"`
	ShortURL    string     `json:"short_url"`
	OriginalURL string     `json:"original_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type listResponse struct {
	Items  []linkResponse `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// topLinkResponse — одна строка отчёта «самые популярные ссылки».
// Поле ID ссылки наружу не отдаём: клиенту оно не нужно, а внутренние
// идентификаторы лишний раз светить незачем.
type topLinkResponse struct {
	Code        string `json:"code"`
	ShortURL    string `json:"short_url"`
	OriginalURL string `json:"original_url"`
	Clicks      int64  `json:"clicks"`
}

type topResponse struct {
	Items []topLinkResponse `json:"items"`
	Limit int               `json:"limit"`
}

type statsResponse struct {
	Code        string     `json:"code"`
	OriginalURL string     `json:"original_url"`
	TotalClicks int64      `json:"total_clicks"`
	UniqueIPs   int64      `json:"unique_ips"`
	LastClickAt *time.Time `json:"last_click_at,omitempty"`
}

// errorResponse — единый формат ошибки для всего API.
type errorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}
