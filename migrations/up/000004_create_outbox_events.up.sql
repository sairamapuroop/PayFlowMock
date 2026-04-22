CREATE TABLE outbox_events (
    id UUID PRIMARY KEY,
    payment_id UUID NOT NULL REFERENCES payments(id),
    merchant_id UUID NOT NULL,
    event_type VARCHAR(50) NOT NULL,
    webhook_url TEXT NOT NULL,
    payload JSONB NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    attempt_count INT NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT,
    delivered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_outbox_due ON outbox_events (status, next_retry_at)
    WHERE status = 'PENDING';

CREATE INDEX idx_outbox_payment ON outbox_events (payment_id);
