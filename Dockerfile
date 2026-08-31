# --- Стадия сборки --------------------------------------------------------
# Многостадийная сборка: тяжёлый образ с компилятором нужен только здесь,
# в финальный образ попадает один статический бинарник (~15 МБ вместо ~800 МБ).
FROM golang:1.25-alpine AS builder

WORKDIR /src

# go.mod и go.sum копируются отдельно от исходников: слой с зависимостями
# кэшируется Docker'ом и не пересобирается, пока эти файлы не изменились.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 даёт статический бинарник — он запустится в scratch/alpine
# без libc. Флаги -s -w убирают отладочные символы и уменьшают размер.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/server ./cmd/server

# --- Финальная стадия -----------------------------------------------------
FROM alpine:3.20

# ca-certificates нужны, если сервис ходит по https;
# tzdata — чтобы time.LoadLocation работал внутри контейнера.
RUN apk add --no-cache ca-certificates tzdata wget

# Запуск не от root — базовое требование безопасности контейнеров.
RUN adduser -D -u 10001 appuser
USER appuser

COPY --from=builder /out/server /usr/local/bin/server

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/server"]
