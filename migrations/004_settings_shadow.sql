-- 004: dynamic settings center, shadow testing, replayable request bodies.
BEGIN;

CREATE TABLE IF NOT EXISTS system_settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO system_settings (key, value, description) VALUES
    ('enable_compress',         'true',  'Prompt 压缩器（确定性去冗余）'),
    ('enable_semantic_cache',   'true',  '语义缓存（需配置 EMBEDDING_API_URL）'),
    ('enable_shadow_test',      'false', '影子测试：主请求返回后异步复制到影子模型'),
    ('shadow_model',            '',      '影子模型名（必须在渠道目录中存在）'),
    ('shadow_sample_rate',      '100',   '影子采样率，0-100'),
    ('replay_enabled',          'true',  '允许控制台一键重放历史请求'),
    ('shadow_timeout_seconds',  '60',    '影子请求超时（秒）'),
    ('replay_timeout_seconds',  '120',   '重放请求超时（秒）')
ON CONFLICT (key) DO NOTHING;

-- Request bodies are stored so the console can replay them verbatim.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS request_body JSONB;

CREATE TABLE IF NOT EXISTS shadow_logs (
    id             BIGSERIAL PRIMARY KEY,
    trace_id       TEXT NOT NULL DEFAULT '',
    request_id     UUID,
    main_model     VARCHAR(100) NOT NULL,
    shadow_model   VARCHAR(100) NOT NULL,
    main_channel   BIGINT,
    shadow_channel BIGINT,
    main_latency   INTEGER NOT NULL DEFAULT 0,
    shadow_latency INTEGER NOT NULL DEFAULT 0,
    main_cost      NUMERIC(20,10) NOT NULL DEFAULT 0,
    shadow_cost    NUMERIC(20,10) NOT NULL DEFAULT 0,
    main_tokens    INTEGER NOT NULL DEFAULT 0,
    shadow_tokens  INTEGER NOT NULL DEFAULT 0,
    status         VARCHAR(16) NOT NULL DEFAULT 'ok',
    error_message  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_shadow_logs_created ON shadow_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_logs_models  ON shadow_logs (main_model, shadow_model);

COMMIT;
