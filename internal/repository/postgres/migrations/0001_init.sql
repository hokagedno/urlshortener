-- Последовательность создаётся отдельно от таблицы: её значения нужны
-- приложению ДО вставки строки (nextval → base62 → code).
CREATE SEQUENCE IF NOT EXISTS links_id_seq;

CREATE TABLE IF NOT EXISTS links (
    id           BIGINT PRIMARY KEY DEFAULT nextval('links_id_seq'),
    code         VARCHAR(32)  NOT NULL,
    original_url VARCHAR(2048) NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ
);

ALTER SEQUENCE links_id_seq OWNED BY links.id;

-- Основной путь чтения — поиск по code (редирект). UNIQUE-индекс решает
-- сразу две задачи: гарантирует отсутствие дублей и обслуживает WHERE code = $1
-- за O(log n) вместо Seq Scan.
CREATE UNIQUE INDEX IF NOT EXISTS links_code_uidx ON links (code);

-- Индекс под ORDER BY created_at DESC в постраничном списке:
-- планировщик берёт готовый порядок из индекса и не делает отдельную сортировку.
CREATE INDEX IF NOT EXISTS links_created_at_idx ON links (created_at DESC);

-- Частичный индекс: строк с expires_at IS NOT NULL обычно меньшинство,
-- поэтому индекс получается компактнее полного и дешевле в обслуживании.
CREATE INDEX IF NOT EXISTS links_expires_at_idx
    ON links (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS clicks (
    id         BIGSERIAL PRIMARY KEY,
    link_id    BIGINT       NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    ip         INET,
    user_agent VARCHAR(512),
    referer    VARCHAR(512),
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Составной индекс (link_id, created_at DESC) обслуживает и JOIN по link_id,
-- и агрегат MAX(created_at) в запросе статистики.
-- Порядок колонок важен: сначала колонка равенства, потом колонка сортировки.
CREATE INDEX IF NOT EXISTS clicks_link_created_idx ON clicks (link_id, created_at DESC);
