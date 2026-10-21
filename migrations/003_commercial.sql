-- ============================================================================
-- 003: 商业运营层（用户登录 / 渠道加密 / 令牌 / 订单归属）
-- ============================================================================

BEGIN;

-- 1. 用户：邮箱 + 密码哈希 + 状态
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS status VARCHAR(16) NOT NULL DEFAULT 'active';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_status_chk') THEN
        ALTER TABLE users ADD CONSTRAINT users_status_chk
            CHECK (status IN ('active', 'disabled'));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users (lower(email)) WHERE email IS NOT NULL;

-- 2. 渠道：启用状态 + 加密后的 key + 备注
ALTER TABLE channels ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE channels ADD COLUMN IF NOT EXISTS api_key_cipher TEXT;
ALTER TABLE channels ADD COLUMN IF NOT EXISTS description TEXT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'channels_weight_chk') THEN
        ALTER TABLE channels ADD CONSTRAINT channels_weight_chk CHECK (weight >= 0);
    END IF;
END $$;

-- 3. 令牌：sk-nexus-xxx，只存哈希
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT          NOT NULL DEFAULT 'default',
    token_hash   CHAR(64)      NOT NULL,
    token_prefix VARCHAR(24)   NOT NULL,
    quota        NUMERIC(20,10) NOT NULL DEFAULT 0,   -- 0 = 不限
    used         NUMERIC(20,10) NOT NULL DEFAULT 0,
    status       VARCHAR(16)   NOT NULL DEFAULT 'active',
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT api_tokens_hash_uniq UNIQUE (token_hash),
    CONSTRAINT api_tokens_status_chk CHECK (status IN ('active', 'disabled')),
    CONSTRAINT api_tokens_quota_chk CHECK (quota >= 0 AND used >= 0)
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens (user_id, created_at DESC);

-- 4. 订单归属用户（租户账号即用户账号）
ALTER TABLE orders ADD COLUMN IF NOT EXISTS user_id BIGINT REFERENCES users(id) ON DELETE SET NULL;

-- 5. 审计表按用户/模型聚合的索引（dashboard 用）
CREATE INDEX IF NOT EXISTS idx_request_logs_user_time2
    ON request_logs (user_id, request_time DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_model_time2
    ON request_logs (model_name, request_time DESC);

COMMIT;
