-- ============================================================================
-- Nexus — schema 001
-- PostgreSQL 15+
--
-- 修复要点：
--   1. 彻底移除 INHERITS 与 PARTITION OF 混用（PostgreSQL 直接报错
--      "cannot create table with both inheritance and partition"）。
--   2. 分区表的 PRIMARY KEY 必须包含分区键 -> PRIMARY KEY (id, request_time)。
--   3. 所有金额统一 NUMERIC(20,10)，并加非负约束；禁止 float/double。
--   4. 分区按 UTC 月边界建；提供 ensure_request_log_partition() 动态补区，
--      并保留 DEFAULT 分区兜底（防止漏建分区导致写入失败）。
--   5. 账务表（price_versions / ledger_accounts / ledger_reservations /
--      ledger_entries）支撑 ledger.go 的 price_version 钉死与只追加流水。
-- ============================================================================

BEGIN;

-- ---------------------------------------------------------------------------
-- 0. 通用函数
-- ---------------------------------------------------------------------------
-- gen_random_uuid() 自 PostgreSQL 13 起为内置函数，无需 pgcrypto 扩展
-- （Alpine 的 postgresql15 主包不含 contrib，加载扩展会直接失败）。

-- updated_at 统一维护
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- 1. 租户
--    只存 API key 的 SHA-256，明文永不落库。
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS users (
    id           BIGSERIAL PRIMARY KEY,
    username     VARCHAR(64)  NOT NULL,
    api_key_hash CHAR(64)     NOT NULL,              -- hex(sha256(key))
    rate_limit   INTEGER      NOT NULL DEFAULT 100 CHECK (rate_limit >= 0),
    model_whitelist TEXT[]    NOT NULL DEFAULT '{}', -- 空数组 = 不限制
    enabled      BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT users_username_uniq UNIQUE (username),
    CONSTRAINT users_api_key_hash_uniq UNIQUE (api_key_hash)
);

DROP TRIGGER IF EXISTS trg_users_updated_at ON users;
CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 2. 上游渠道
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS channels (
    id          BIGSERIAL PRIMARY KEY,
    name        VARCHAR(100) NOT NULL,
    provider    VARCHAR(32)  NOT NULL,
    api_key     TEXT         NOT NULL,               -- 上游密钥，建议由 KMS 加密后写入
    base_url    TEXT         NOT NULL,
    models      TEXT[]       NOT NULL DEFAULT '{}',
    rate_limit  INTEGER      NOT NULL DEFAULT 100 CHECK (rate_limit >= 0),
    weight      INTEGER      NOT NULL DEFAULT 1   CHECK (weight >= 0),
    healthy     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT channels_name_uniq UNIQUE (name),
    CONSTRAINT channels_provider_chk CHECK (provider IN ('openai', 'anthropic'))
);

DROP TRIGGER IF EXISTS trg_channels_updated_at ON channels;
CREATE TRIGGER trg_channels_updated_at
    BEFORE UPDATE ON channels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 3. 模型定价（金额一律 NUMERIC(20,10)）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS model_channels (
    id             BIGSERIAL PRIMARY KEY,
    model_name     VARCHAR(100)  NOT NULL,
    channel_id     BIGINT        NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    price_per_1m   NUMERIC(20,10) NOT NULL CHECK (price_per_1m >= 0),
    context_limit  INTEGER       NOT NULL DEFAULT 8192 CHECK (context_limit > 0),
    enabled        BOOLEAN       NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT model_channels_uniq UNIQUE (model_name, channel_id)
);

CREATE INDEX IF NOT EXISTS idx_model_channels_model
    ON model_channels (model_name) WHERE enabled;

DROP TRIGGER IF EXISTS trg_model_channels_updated_at ON model_channels;
CREATE TRIGGER trg_model_channels_updated_at
    BEFORE UPDATE ON model_channels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 4. 价格版本（billing 预留时钉死 version，结算沿用同一版本）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS price_versions (
    id            BIGSERIAL PRIMARY KEY,
    model_name    VARCHAR(100)  NOT NULL,
    version       BIGINT        NOT NULL,
    price_per_1m  NUMERIC(20,10) NOT NULL CHECK (price_per_1m >= 0),
    effective_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT price_versions_uniq UNIQUE (model_name, version)
);

CREATE INDEX IF NOT EXISTS idx_price_versions_lookup
    ON price_versions (model_name, effective_at DESC);

-- ---------------------------------------------------------------------------
-- 5. 请求日志（RANGE 分区，按月；分区键 request_time）
--    注意：分区表的唯一约束必须包含分区键。
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS request_logs (
    id                BIGSERIAL,
    request_id        UUID          NOT NULL DEFAULT gen_random_uuid(),
    user_id           BIGINT        REFERENCES users(id) ON DELETE SET NULL,
    channel_id        BIGINT        REFERENCES channels(id) ON DELETE SET NULL,
    model_name        VARCHAR(100)  NOT NULL,
    provider          VARCHAR(32)   NOT NULL,
    route_reason      VARCHAR(64)   NOT NULL,        -- 对应 router 的 reason 枚举
    prompt_tokens     INTEGER       NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER       NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    total_tokens      INTEGER       GENERATED ALWAYS AS (prompt_tokens + completion_tokens) STORED,
    cost              NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK (cost >= 0),
    price_version     BIGINT        NOT NULL DEFAULT 0,
    status            VARCHAR(16)   NOT NULL DEFAULT 'success',
    http_status       INTEGER       NOT NULL DEFAULT 200,
    error_message     TEXT,
    latency_ms        INTEGER       NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
    cached            BOOLEAN       NOT NULL DEFAULT FALSE,
    metadata          JSONB         NOT NULL DEFAULT '{}'::jsonb,
    request_time      TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (id, request_time),
    CONSTRAINT request_logs_status_chk
        CHECK (status IN ('success', 'error', 'timeout', 'rate_limited', 'aborted', 'cache_hit'))
) PARTITION BY RANGE (request_time);

-- 分区表上的索引（会自动下推到每个分区）
CREATE INDEX IF NOT EXISTS idx_request_logs_user_time
    ON request_logs (user_id, request_time DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_model_time
    ON request_logs (model_name, request_time DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_status
    ON request_logs (status) WHERE status <> 'success';

-- 5.1 动态补区：只做 CREATE TABLE ... PARTITION OF，绝不使用 INHERITS
CREATE OR REPLACE FUNCTION ensure_request_log_partition(p_month DATE)
RETURNS TEXT AS $$
DECLARE
    v_start TIMESTAMPTZ := date_trunc('month', p_month)::timestamptz;
    v_end   TIMESTAMPTZ := (date_trunc('month', p_month) + INTERVAL '1 month')::timestamptz;
    v_name  TEXT := format('request_logs_p%s', to_char(v_start, 'YYYY_MM'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF request_logs
           FOR VALUES FROM (%L) TO (%L)',
        v_name, v_start, v_end
    );
    RETURN v_name;
END;
$$ LANGUAGE plpgsql;

-- 5.2 预建当前月 + 未来 11 个月
--     注意：timestamptz -> date 是 assignment cast，函数解析不会自动应用，
--     必须显式写 ::date，否则报 "function ... (timestamp with time zone) does not exist"。
SELECT ensure_request_log_partition(d::date) AS created_partition
FROM generate_series(
        date_trunc('month', now()),
        date_trunc('month', now()) + INTERVAL '11 months',
        INTERVAL '1 month'
     ) AS d;

-- 5.3 兜底分区：漏建分区的写入会落到这里，而不是直接报错
CREATE TABLE IF NOT EXISTS request_logs_pdefault
    PARTITION OF request_logs DEFAULT;

-- ---------------------------------------------------------------------------
-- 6. 账务：账户 / 预留 / 只追加流水
-- ---------------------------------------------------------------------------

-- balance 允许为负：上游实际用量超过预留（overrun）时，结算不能因为
-- CHECK(balance >= 0) 而失败——那会导致「响应已发出、钱收不到、hold 悬挂」。
-- 透支由代码层在事务内判定并写入 overrun 流水（ledger_entries.note 标注）。
CREATE TABLE IF NOT EXISTS ledger_accounts (
    user_id     BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    balance     NUMERIC(20,10) NOT NULL DEFAULT 0,   -- may go negative on overrun
    reserved    NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    spent       NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK (spent >= 0),
    updated_at  TIMESTAMPTZ    NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS trg_ledger_accounts_updated_at ON ledger_accounts;
CREATE TRIGGER trg_ledger_accounts_updated_at
    BEFORE UPDATE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS ledger_reservations (
    id             TEXT PRIMARY KEY,                     -- res_<user>_<seq>
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    model_name     VARCHAR(100) NOT NULL,
    price_version  BIGINT NOT NULL,
    price_per_1m   NUMERIC(20,10) NOT NULL CHECK (price_per_1m >= 0),
    held           NUMERIC(20,10) NOT NULL CHECK (held >= 0),
    actual         NUMERIC(20,10),
    prompt_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    output_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    status         VARCHAR(16) NOT NULL DEFAULT 'open',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at     TIMESTAMPTZ,
    CONSTRAINT ledger_reservations_status_chk
        CHECK (status IN ('open', 'settled', 'reconciled')),
    CONSTRAINT ledger_reservations_open_chk
        CHECK (status <> 'settled' OR (actual IS NOT NULL AND settled_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_ledger_reservations_open
    ON ledger_reservations (created_at) WHERE status = 'open';

CREATE TABLE IF NOT EXISTS ledger_entries (
    seq            BIGSERIAL PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reservation_id TEXT REFERENCES ledger_reservations(id) ON DELETE SET NULL,
    kind           VARCHAR(16) NOT NULL,
    amount         NUMERIC(20,10) NOT NULL,
    price_version  BIGINT NOT NULL DEFAULT 0,
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_kind_chk
        CHECK (kind IN ('reserve', 'settle', 'refund', 'overrun', 'reconcile'))
);

-- 流水只追加：禁止 UPDATE / DELETE
CREATE OR REPLACE FUNCTION forbid_ledger_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (attempted %)', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ledger_entries_append_only ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

CREATE INDEX IF NOT EXISTS idx_ledger_entries_user_seq
    ON ledger_entries (user_id, seq);

-- ---------------------------------------------------------------------------
-- 7. 语义缓存元数据（向量本身放外部存储，这里只存键与引用）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS semantic_cache_entries (
    id           BIGSERIAL PRIMARY KEY,
    cache_key    CHAR(64) NOT NULL,                  -- sha256(exact normalized prompt)
    embedding_ref TEXT    NOT NULL,                  -- 向量库中的 id
    model_name   VARCHAR(100) NOT NULL,
    response     JSONB   NOT NULL,
    tokens_saved INTEGER NOT NULL DEFAULT 0 CHECK (tokens_saved >= 0),
    hits         BIGINT  NOT NULL DEFAULT 0 CHECK (hits >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT semantic_cache_entries_key_uniq UNIQUE (cache_key, model_name)
);

CREATE INDEX IF NOT EXISTS idx_semantic_cache_expiry
    ON semantic_cache_entries (expires_at);

COMMIT;

-- ============================================================================
-- 后续维护示例（运维手动执行，或由应用启动时调用）：
--   SELECT ensure_request_log_partition((now() + INTERVAL '12 months')::date);
-- 注意：DEFAULT 分区已含有数据时，再为其覆盖区间挂载具名分区会失败；
--       因此建议提前 12 个月建区，并在迁移 DEFAULT 数据后再挂载。
-- ============================================================================
