-- Row-level lease for idempotency (replaces connection-scoped advisory locks).

ALTER TABLE idempotency_keys
    ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'
        CHECK (status IN ('in_progress', 'completed')),
    ADD COLUMN lock_token VARCHAR(64),
    ADD COLUMN lock_expires_at TIMESTAMPTZ;

ALTER TABLE idempotency_keys
    ALTER COLUMN response_status_code DROP NOT NULL;

-- response_headers and response_body are already nullable in 000003

CREATE INDEX idx_idempotency_keys_lock_expires_at
    ON idempotency_keys (lock_expires_at)
    WHERE status = 'in_progress';
