# URL Shortener

[![CI](https://github.com/hokagedno/urlshortener/actions/workflows/ci.yml/badge.svg)](https://github.com/hokagedno/urlshortener/actions/workflows/ci.yml)

Сервис коротких ссылок на Go: REST API на **Gin**, хранилище — **PostgreSQL**,
кэш и rate limiting — **Redis**, асинхронный сбор статистики переходов на
горутинах и каналах. Всё поднимается одной командой в **Docker**.

```
POST /api/v1/links  {"url": "https://very-long-url..."}  ->  http://localhost:8080/1Ml2
GET  /1Ml2                                               ->  302 Location: https://very-long-url...
GET  /api/v1/links/1Ml2/stats                            ->  {"total_clicks": 42, "unique_ips": 17}
```

## Быстрый старт

```bash
docker compose up --build      # postgres + redis + приложение
curl -X POST localhost:8080/api/v1/links \
     -H 'Content-Type: application/json' \
     -d '{"url":"https://go.dev/doc/effective_go"}'
```

Локально без Docker:

```bash
cp .env.example .env           # поправить DSN при необходимости
make up                        # или поднять postgres/redis самостоятельно
make run
make test-race                 # тесты с детектором гонок
```

## Архитектура

Слоистая архитектура с инверсией зависимостей: стрелки зависимостей всегда
направлены внутрь, к `domain`.

```
cmd/server            точка входа (main)
  └── internal/app    composition root: сборка зависимостей, graceful shutdown
        ├── internal/transport/http     Gin: роутер, хендлеры, middleware, DTO
        ├── internal/service            бизнес-логика, сборщик кликов
        ├── internal/repository/postgres  SQL, миграции (embed)
        ├── internal/repository/redis     кэш + rate limiter
        └── internal/domain             сущности, доменные ошибки, ПОРТЫ (интерфейсы)
pkg/base62            алгоритм кодирования id -> короткий код
```

`domain` не импортирует ничего, кроме стандартной библиотеки. `service`
работает с интерфейсами `LinkRepository` и `Cache`, а не с pgx и redis
напрямую, поэтому:

* бизнес-логика тестируется без БД — тесты идут ~1 секунду;
* заменить Postgres на что-то другое можно, не трогая сервис;
* каждая зависимость подставляется через конструктор (DI), глобальных
  переменных и `init()`-магии в проекте нет.

### Как это ложится на SOLID

| Принцип | Где в коде |
|---|---|
| **S** — единственная ответственность | хендлер только разбирает HTTP, сервис только считает бизнес-правила, репозиторий только ходит в БД |
| **O** — открытость/закрытость | новое поведение сервиса добавляется через `service.Option` (functional options), а не правкой конструктора |
| **L** — подстановка Лисков | стабы в тестах подставляются вместо `LinkRepository` без изменения кода сервиса |
| **I** — разделение интерфейсов | `ClickRecorder` — один метод; сервису не нужен «толстый» интерфейс сборщика целиком |
| **D** — инверсия зависимостей | интерфейсы объявлены в `domain` и в транспорте (у потребителя), реализации — во внешних слоях |

### Паттерны

* **Repository** — `domain.LinkRepository` и реализация на pgx.
* **Cache-Aside (Lazy Loading)** — `Shortener.Resolve`: Redis → Postgres → прогрев Redis.
* **Functional Options** — `service.WithCacheTTL`, `service.WithClock`.
* **Worker Pool + Batching** — `service.ClickCollector`.
* **Chain of Responsibility / Decorator** — цепочка middleware Gin.
* **Graceful Shutdown** — `internal/app`.
* **DTO** — `internal/transport/http/dto.go`, контракт API отделён от домена.

## API

| Метод | Путь | Описание |
|---|---|---|
| `POST` | `/api/v1/links` | создать ссылку; тело `{"url": "...", "alias": "...", "expires_at": "..."}` |
| `GET` | `/api/v1/links` | список с пагинацией `?limit=20&offset=0` |
| `GET` | `/api/v1/links/:code` | информация о ссылке |
| `GET` | `/api/v1/links/:code/stats` | агрегированная статистика переходов |
| `DELETE` | `/api/v1/links/:code` | удалить ссылку (кэш инвалидируется) |
| `GET` | `/:code` | редирект `302` на оригинальный URL |
| `GET` | `/healthz` | health check для Docker/Kubernetes |

Коды ответов: `201`, `200`, `204`, `400` (валидация), `404` (нет ссылки),
`409` (алиас занят), `410` (срок истёк), `429` (rate limit), `500`.

Соответствие доменных ошибок и HTTP-статусов собрано в одном месте —
`Handler.fail`, через `errors.Is`, поэтому статус-коды не «размазаны» по хендлерам.

## Как генерируется короткий код

Наивный подход — сгенерировать случайную строку и проверить, не занята ли она,
— требует дополнительного запроса в БД и цикла повторов при коллизии.

Здесь id берётся из последовательности Postgres (`nextval` атомарен и не
блокируется), а затем кодируется в base62:

```
id = 1        -> "0"      (после сдвига на 100000 -> "q0U")
id = 1000000  -> "4c92"
```

Сложность `O(log₆₂ n)`, коллизии невозможны по построению, код обратим
(`base62.Decode`). Пользовательские алиасы (`alias`) проверяются на
уникальность самой БД через `UNIQUE`-индекс — приложение отличает эту ошибку
по SQLSTATE `23505`, а не по тексту сообщения.

## PostgreSQL: схема, транзакции, индексы

Миграции лежат в `internal/repository/postgres/migrations` и вшиты в бинарник
через `//go:embed`, применяются на старте. Каждая — в своей транзакции вместе
с записью в `schema_migrations`, так что упавшая миграция не отметится
применённой.

**Индексы и зачем они:**

```sql
CREATE UNIQUE INDEX links_code_uidx ON links (code);
```
Основной горячий путь — `WHERE code = $1` при редиректе. Без индекса это
`Seq Scan` по всей таблице; с индексом — `Index Scan` за `O(log n)`.
UNIQUE заодно обеспечивает целостность.

```sql
CREATE INDEX links_created_at_idx ON links (created_at DESC);
```
Под `ORDER BY created_at DESC LIMIT/OFFSET`: планировщик берёт готовый порядок
из индекса и не выполняет отдельный шаг `Sort`.

```sql
CREATE INDEX links_expires_at_idx ON links (expires_at) WHERE expires_at IS NOT NULL;
```
Частичный индекс: строк со сроком жизни обычно меньшинство, индекс получается
существенно компактнее полного.

```sql
CREATE INDEX clicks_link_created_idx ON clicks (link_id, created_at DESC);
```
Составной индекс под `JOIN ... ON c.link_id = l.id` и `MAX(c.created_at)`.
Порядок колонок принципиален: сначала колонка равенства, потом сортировки.

**JOIN в запросе статистики:**

```sql
SELECT l.id, l.code, l.original_url, l.created_at, l.expires_at,
       COUNT(c.link_id)     AS total_clicks,
       COUNT(DISTINCT c.ip) AS unique_ips,
       MAX(c.created_at)    AS last_click_at
FROM links l
LEFT JOIN clicks c ON c.link_id = l.id
WHERE l.code = $1
GROUP BY l.id;
```

`LEFT JOIN`, а не `INNER`: у ссылки без переходов должна вернуться строка с
нулями, а не пустой результат. `COUNT(c.link_id)` считает только не-NULL,
поэтому для такой ссылки корректно даёт `0` (а `COUNT(*)` дал бы `1`).

Посмотреть план запроса:

```bash
make psql
EXPLAIN (ANALYZE, BUFFERS) SELECT ... ;
```

**Транзакция:** пакетная запись кликов (`SaveClicks`) идёт в одной транзакции
через `pgx.Batch` — все `INSERT` уходят одним сетевым пакетом вместо N
round-trip'ов, и записываются атомарно. `defer tx.Rollback(ctx)` безопасен:
после успешного `Commit` откат — no-op.

## Redis

* **Кэш редиректов** (cache-aside) с TTL; TTL не может пережить саму ссылку.
* **Инвалидация** при удалении — иначе редирект продолжал бы работать из кэша.
* **Rate limiting** — `INCR` + `EXPIRE NX` в одном pipeline (один round-trip).
  `EXPIRE NX` вместо обычного `EXPIRE` важен: иначе окно продлевалось бы на
  каждом запросе и лимит для активного клиента никогда не сбрасывался.
* Ошибка Redis не роняет запрос: кэш — оптимизация, а не источник истины
  (fail-open в rate limiter, поход в БД при ошибке кэша).

## Конкурентность

Запись клика не должна задерживать редирект. Хендлер кладёт событие в
буферизованный канал и сразу отвечает `302`; пул воркеров в фоне копит батч и
пишет его в Postgres одной транзакцией.

```
HTTP handler ──> chan domain.Click (буфер 1024) ──> worker × N ──> Batch INSERT
                        │
                        └─ select/default: очередь полна -> клик отбрасывается,
                           счётчик dropped растёт, хендлер не блокируется
```

Что здесь используется:

* буферизованный канал как очередь;
* `select` с `default` — неблокирующая отправка;
* `time.Ticker` — сброс неполного батча по таймауту;
* `sync.WaitGroup` + закрытие канала — корректное завершение без потери данных;
* `sync.Once` — идемпотентный `Stop()`;
* отдельный `context.WithTimeout` для записи в БД: контекст HTTP-запроса к
  этому моменту уже отменён.

Тесты покрывают все четыре гарантии: сброс по размеру батча, сброс остатка при
остановке, идемпотентность `Stop`, отсутствие блокировки при переполнении.
`go test -race ./...` проходит чисто.

## Graceful shutdown

`signal.NotifyContext` ловит `SIGINT`/`SIGTERM` (именно `SIGTERM` шлёт
`docker stop`). Порядок остановки:

1. `srv.Shutdown(ctx)` — сервер перестаёт принимать новые соединения и
   доигрывает текущие запросы;
2. `collector.Stop()` — воркеры дописывают накопленные клики;
3. `defer` закрывает пулы Postgres и Redis.

## Тесты

```bash
make test        # unit-тесты
make test-race   # то же с детектором гонок
make bench       # бенчмарки base62
```

* `pkg/base62` — табличные тесты round-trip, проверка уникальности, бенчмарк.
* `internal/service` — бизнес-логика на стабах: генерация кода, валидация URL,
  конфликт алиасов, попадание в кэш, инвалидация, истёкшая ссылка,
  параллельные вызовы.
* `internal/transport/http` — хендлеры через `httptest`: статусы, JSON,
  заголовок `Location`, маппинг ошибок.

Подмена времени (`service.WithClock`) позволяет тестировать истечение срока
ссылки без `time.Sleep`.

## Стек

Go 1.24 · Gin · pgx/v5 · go-redis/v9 · log/slog · Docker · PostgreSQL 16 · Redis 7
