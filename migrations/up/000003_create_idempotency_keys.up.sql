CREATE TABLE idempotency_keys (
    key VARCHAR(255) PRIMARY KEY,
    request_path VARCHAR(255) NOT NULL,
    request_method VARCHAR(10) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    response_status_code INT NOT NULL,
    response_headers JSONB,
    response_body BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys(expires_at);
