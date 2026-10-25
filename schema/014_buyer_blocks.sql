CREATE TABLE IF NOT EXISTS buyer_blocks (
    user_id    BIGINT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sellers_stripe_account ON sellers (stripe_account_id) WHERE stripe_account_id <> '';
