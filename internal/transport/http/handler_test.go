package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hokagedno/urlshortener/internal/domain"
	"github.com/hokagedno/urlshortener/internal/service"
	httptransport "github.com/hokagedno/urlshortener/internal/transport/http"
)

// fakeService реализует порт httptransport.ShortenerService.
// Хендлеры тестируются изолированно — без БД, кэша и реального сервиса.
type fakeService struct {
	createErr  error
	resolveErr error
	resolved   string
}

func (f *fakeService) Create(_ context.Context, in service.CreateInput) (domain.Link, error) {
	if f.createErr != nil {
		return domain.Link{}, f.createErr
	}
	return domain.Link{Code: "abc123", OriginalURL: in.OriginalURL, CreatedAt: time.Now()}, nil
}

func (f *fakeService) Resolve(context.Context, string, domain.Click) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return f.resolved, nil
}

func (f *fakeService) Get(context.Context, string) (domain.Link, error) {
	return domain.Link{Code: "abc123", OriginalURL: "https://example.com"}, nil
}
func (f *fakeService) List(context.Context, int, int) ([]domain.Link, error) { return nil, nil }
func (f *fakeService) Stats(context.Context, string) (domain.LinkStats, error) {
	return domain.LinkStats{}, nil
}
func (f *fakeService) Delete(context.Context, string) error { return nil }

// noopCache отключает rate limiting в тестах.
type noopCache struct{}

func (noopCache) GetLink(context.Context, string) (domain.CachedLink, error) {
	return domain.CachedLink{}, domain.ErrCacheMiss
}

func (noopCache) SetLink(context.Context, string, domain.CachedLink, time.Duration) error {
	return nil
}
func (noopCache) Delete(context.Context, string) error { return nil }
func (noopCache) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return true, nil
}

func newTestRouter(svc httptransport.ShortenerService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := httptransport.NewHandler(svc, "http://short.test", log)
	return httptransport.NewRouter(h, noopCache{}, log, httptransport.RouterConfig{
		BaseURL: "http://short.test", RateLimit: 1000, RateWindow: time.Minute,
	})
}

func TestCreateLink_Created(t *testing.T) {
	r := newTestRouter(&fakeService{})

	body, _ := json.Marshal(map[string]string{"url": "https://example.com/very/long/path"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("ожидали 201, получили %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code     string `json:"code"`
		ShortURL string `json:"short_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("невалидный JSON: %v", err)
	}
	if resp.ShortURL != "http://short.test/abc123" {
		t.Errorf("short_url = %q", resp.ShortURL)
	}
}

func TestCreateLink_ValidationError(t *testing.T) {
	r := newTestRouter(&fakeService{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links",
		bytes.NewReader([]byte(`{"url":"не-ссылка"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("ожидали 400, получили %d", w.Code)
	}
}

func TestRedirect_Found(t *testing.T) {
	r := newTestRouter(&fakeService{resolved: "https://example.com/target"})

	req := httptest.NewRequest(http.MethodGet, "/abc123", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("ожидали 302, получили %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com/target" {
		t.Errorf("Location = %q", loc)
	}
}

// Проверяем сопоставление доменных ошибок и HTTP-статусов.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"не найдено", domain.ErrNotFound, http.StatusNotFound},
		{"истекла", domain.ErrExpired, http.StatusGone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRouter(&fakeService{resolveErr: tc.err})
			req := httptest.NewRequest(http.MethodGet, "/abc123", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Errorf("ожидали %d, получили %d", tc.want, w.Code)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	r := newTestRouter(&fakeService{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ожидали 200, получили %d", w.Code)
	}
}
