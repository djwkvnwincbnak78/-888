-- ============================================================================
-- 002: 租户 / RBAC / 商业化基座
-- ============================================================================

BEGIN;

-- 1. 租户
CREATE TABLE IF NOT EXISTS tenants (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL,
    plan       VARCHAR(32) NOT NULL DEFAULT 'free',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO tenants (id, name, plan) VALUES (1, 'default', 'free')
ON CONFLICT (id) DO NOTHING;

SELECT setval(pg_get_serial_sequence('tenants','id'),
              GREATEST((SELECT COALESCE(MAX(id), 1) FROM tenants), 1));

-- 2. users 归属租户与角色
ALTER TABLE users ADD COLUMN IF NOT EXISTS tenant_id BIGINT REFERENCES tenants(id);
ALTER TABLE users ADD COLUMN IF NOT EXISTS role VARCHAR(16) NOT NULL DEFAULT 'member';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_role_chk') THEN
        ALTER TABLE users ADD CONSTRAINT users_role_chk
            CHECK (role IN ('owner', 'admin', 'member', 'viewer'));
    END IF;
END $$;

UPDATE users SET tenant_id = 1 WHERE tenant_id IS NULL;

-- 3. 管理端密钥（只存 SHA-256）
CREATE TABLE IF NOT EXISTS admin_api_keys (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  BIGINT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_hash   CHAR(64)    NOT NULL,
    role       VARCHAR(16) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT admin_api_keys_hash_uniq UNIQUE (key_hash),
    CONSTRAINT admin_api_keys_role_chk
        CHECK (role IN ('owner', 'admin', 'member', 'viewer'))
);

CREATE INDEX IF NOT EXISTS idx_admin_api_keys_tenant ON admin_api_keys (tenant_id);

-- 4. 充值订单
CREATE TABLE IF NOT EXISTS orders (
    id         TEXT PRIMARY KEY,               -- ord_<hex>
    tenant_id  BIGINT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    amount     NUMERIC(20,10) NOT NULL CHECK (amount > 0),
    currency   CHAR(3)       NOT NULL DEFAULT 'USD',
    status     VARCHAR(16)   NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    paid_at    TIMESTAMPTZ,
    CONSTRAINT orders_status_chk CHECK (status IN ('pending', 'paid', 'cancelled')),
    CONSTRAINT orders_paid_chk CHECK (status <> 'paid' OR paid_at IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_orders_tenant_time ON orders (tenant_id, created_at DESC);

-- 5. 兑换码
CREATE TABLE IF NOT EXISTS redeem_codes (
    code       TEXT PRIMARY KEY,               -- rc_<hex>
    tenant_id  BIGINT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    amount     NUMERIC(20,10) NOT NULL CHECK (amount > 0),
    status     VARCHAR(16)   NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    claimed_by BIGINT        REFERENCES users(id) ON DELETE SET NULL,
    CONSTRAINT redeem_codes_status_chk CHECK (status IN ('active', 'claimed', 'revoked')),
    CONSTRAINT redeem_codes_claimed_chk
        CHECK (status <> 'claimed' OR (claimed_at IS NOT NULL AND claimed_by IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_redeem_codes_tenant ON redeem_codes (tenant_id, status);

-- 6. 流水允许 credit（充值 / 兑换入账）
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_kind_chk;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_chk
    CHECK (kind IN ('reserve', 'settle', 'refund', 'overrun', 'reconcile', 'credit'));

COMMIT;
