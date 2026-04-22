DROP INDEX IF EXISTS idx_idempotency_keys_lock_expires_at;

ALTER TABLE idempotency_keys
    DROP COLUMN IF EXISTS lock_token,
    DROP COLUMN IF EXISTS lock_expires_at,
    DROP COLUMN IF EXISTS status;

UPDATE idempotency_keys
SET response_status_code = 0
WHERE response_status_code IS NULL;

ALTER TABLE idempotency_keys
    ALTER COLUMN response_status_code SET NOT NULL;
