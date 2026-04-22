package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	defaultResponseTTL    = 24 * time.Hour
	defaultLockTTL        = 15 * time.Second
	defaultRequestTimeout = 60 * time.Second
	defaultWaitTimeout    = 8 * time.Second
	defaultPollEvery      = 150 * time.Millisecond
)

// Idempotency provides an HTTP middleware that:
// 1) uses Redis as the fast response cache,
// 2) uses Redis distributed lock to serialize duplicate in-flight requests,
// 3) falls back to PostgreSQL for response lookup and row-level lease when needed.
type Idempotency struct {
	redis *redis.Client
	db    *pgxpool.Pool

	responseTTL    time.Duration
	lockTTL        time.Duration
	requestTimeout time.Duration // PG idempotency lease (lock_expires_at); set via NewIdempotency default or SetRequestTimeout
	waitTimeout    time.Duration
	pollEvery      time.Duration
}

type cachedResponse struct {
	RequestPath       string              `json:"request_path"`
	RequestMethod     string              `json:"request_method"`
	RequestHash       string              `json:"request_hash"`
	ResponseStatus    int                 `json:"response_status"`
	ResponseHeaders   map[string][]string `json:"response_headers"`
	ResponseBody      []byte              `json:"response_body"`
	CreatedAtUnixNano int64               `json:"created_at_unix_nano"`
	ExpiresAtUnixNano int64               `json:"expires_at_unix_nano"`
}

type lockHandle struct {
	kind      string // "redis" | "pg"
	key       string
	token     string // request_hash / lease token; pg matches idempotency_keys.lock_token
	completed bool   // pg: set after successful storeCachedResponse (lease completion UPDATE); release skips DELETE when true
}

// NewIdempotency returns middleware state with safe defaults.
func NewIdempotency(redisClient *redis.Client, db *pgxpool.Pool) *Idempotency {
	return &Idempotency{
		redis:          redisClient,
		db:             db,
		responseTTL:    defaultResponseTTL,
		lockTTL:        defaultLockTTL,
		requestTimeout: defaultRequestTimeout,
		waitTimeout:    defaultWaitTimeout,
		pollEvery:      defaultPollEvery,
	}
}

// SetRequestTimeout sets the Postgres idempotency lease duration (lock_expires_at), used
// when the Redis lock is unavailable. Must cover the slowest handler run (e.g. PSP + DB work).
// Non-positive d leaves the current value unchanged.
func (m *Idempotency) SetRequestTimeout(d time.Duration) {
	if m == nil || d <= 0 {
		return
	}
	m.requestTimeout = d
}

// Middleware wraps handlers with idempotency behavior for mutating methods.
func (m *Idempotency) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || next == nil {
			http.Error(w, "server misconfiguration", http.StatusInternalServerError)
			return
		}
		if !isIdempotentCandidateMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			key = strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
		}
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 255 {
			writeIdempotencyError(w, http.StatusBadRequest, "idempotency key too long")
			return
		}

		reqBody, reqHash, err := readAndHashBody(r)
		if err != nil {
			writeIdempotencyError(w, http.StatusBadRequest, "unable to read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(reqBody))

		if cached, err := m.lookupCachedResponse(r.Context(), key); err == nil && cached != nil {
			if !m.matchesRequest(cached, r, reqHash) {
				writeIdempotencyError(w, http.StatusConflict, "idempotency key reused with different request")
				return
			}
			writeCachedResponse(w, cached)
			return
		}

		lockScope := buildLockScope(r.Method, r.URL.Path, key)
		lockToken := reqHash
		handle, lockErr := m.tryAcquireLock(r.Context(), lockScope, key, r.URL.Path, r.Method, lockToken)
		if lockErr != nil {
			writeIdempotencyError(w, http.StatusServiceUnavailable, "unable to acquire idempotency lock")
			return
		}
		if handle == nil {
			cached, waitErr := m.waitForCachedResponse(r.Context(), key)
			if waitErr == nil && cached != nil {
				if !m.matchesRequest(cached, r, reqHash) {
					writeIdempotencyError(w, http.StatusConflict, "idempotency key reused with different request")
					return
				}
				writeCachedResponse(w, cached)
				return
			}
			writeIdempotencyError(w, http.StatusConflict, "request with this idempotency key is already in progress")
			return
		}
		defer m.releaseLock(context.Background(), handle)

		// Re-check after lock acquisition; another process may have finished and committed
		// the response while lock ownership changed.
		if cached, err := m.lookupCachedResponse(r.Context(), key); err == nil && cached != nil {
			if !m.matchesRequest(cached, r, reqHash) {
				writeIdempotencyError(w, http.StatusConflict, "idempotency key reused with different request")
				return
			}
			writeCachedResponse(w, cached)
			return
		}

		rec := newResponseRecorder()
		next.ServeHTTP(rec, r)

		resp := &cachedResponse{
			RequestPath:       r.URL.Path,
			RequestMethod:     r.Method,
			RequestHash:       reqHash,
			ResponseStatus:    rec.statusCode(),
			ResponseHeaders:   cloneHeaders(rec.Header()),
			ResponseBody:      rec.body.Bytes(),
			CreatedAtUnixNano: time.Now().UTC().UnixNano(),
			ExpiresAtUnixNano: time.Now().UTC().Add(m.responseTTL).UnixNano(),
		}

		// Persist best-effort. If persistence fails, the original response still succeeds.
		_ = m.storeCachedResponse(r.Context(), key, resp, handle)
		writeCachedResponse(w, resp)
	})
}

func isIdempotentCandidateMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func readAndHashBody(r *http.Request) ([]byte, string, error) {
	if r == nil || r.Body == nil {
		sum := sha256.Sum256(nil)
		return nil, hex.EncodeToString(sum[:]), nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func buildLockScope(method, path, key string) string {
	return method + ":" + path + ":" + key
}

func redisResponseKey(key string) string {
	return "idem:resp:" + key
}

func redisLockKey(scope string) string {
	return "idem:lock:" + scope
}

func (m *Idempotency) lookupCachedResponse(ctx context.Context, key string) (*cachedResponse, error) {
	if m.redis != nil {
		if raw, err := m.redis.Get(ctx, redisResponseKey(key)).Result(); err == nil {
			var resp cachedResponse
			if err := json.Unmarshal([]byte(raw), &resp); err == nil {
				if resp.ExpiresAtUnixNano == 0 || time.Now().UTC().UnixNano() < resp.ExpiresAtUnixNano {
					return &resp, nil
				}
			}
		} else if !errors.Is(err, redis.Nil) {
			// Continue to PG fallback.
		}
	}

	if m.db == nil {
		return nil, nil
	}

	var (
		resp      cachedResponse
		headerRaw []byte
		body      []byte
		createdAt time.Time
		expiresAt time.Time
	)

	err := m.db.QueryRow(ctx, `
		SELECT request_path, request_method, request_hash,
		       response_status_code, response_headers, response_body, created_at, expires_at
		FROM idempotency_keys
		WHERE key = $1 AND status = 'completed' AND expires_at > NOW()`,
		key,
	).Scan(
		&resp.RequestPath,
		&resp.RequestMethod,
		&resp.RequestHash,
		&resp.ResponseStatus,
		&headerRaw,
		&body,
		&createdAt,
		&expiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	resp.CreatedAtUnixNano = createdAt.UTC().UnixNano()
	resp.ExpiresAtUnixNano = expiresAt.UTC().UnixNano()
	resp.ResponseBody = body
	if len(headerRaw) > 0 {
		_ = json.Unmarshal(headerRaw, &resp.ResponseHeaders)
	}
	if resp.ResponseHeaders == nil {
		resp.ResponseHeaders = make(map[string][]string)
	}

	// Warm Redis when available to reduce future DB lookups.
	if m.redis != nil {
		_ = m.writeRedisCache(ctx, key, &resp)
	}
	return &resp, nil
}

func (m *Idempotency) storeCachedResponse(ctx context.Context, key string, resp *cachedResponse, handle *lockHandle) error {
	if resp == nil {
		return nil
	}

	var firstErr error
	if m.redis != nil {
		if err := m.writeRedisCache(ctx, key, resp); err != nil {
			firstErr = err
		}
	}
	if m.db != nil {
		var err error
		if handle != nil && handle.kind == "pg" {
			err = m.writePGCache(ctx, key, resp, handle)
			if err == nil {
				handle.completed = true
			}
		} else {
			err = m.upsertPGCompleted(ctx, key, resp)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Idempotency) writeRedisCache(ctx context.Context, key string, resp *cachedResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	ttl := m.responseTTL
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	return m.redis.Set(ctx, redisResponseKey(key), payload, ttl).Err()
}

// writePGCache completes an in-progress row acquired via the Postgres lease path.
// It only succeeds if we still hold the lease (lock_token matches handle.token).
func (m *Idempotency) writePGCache(ctx context.Context, key string, resp *cachedResponse, handle *lockHandle) error {
	if handle == nil {
		return errors.New("idempotency: writePGCache: nil handle")
	}
	lockToken := handle.token
	headersJSON, err := json.Marshal(resp.ResponseHeaders)
	if err != nil {
		return err
	}
	expiresAt := time.Unix(0, resp.ExpiresAtUnixNano).UTC()
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(m.responseTTL)
	}

	tag, err := m.db.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed',
			response_status_code = $2,
			response_headers = $3::jsonb,
			response_body = $4,
			expires_at = $5,
			lock_token = NULL,
			lock_expires_at = NULL
		WHERE key = $1 AND status = 'in_progress' AND lock_token = $6
		`,
		key,
		resp.ResponseStatus,
		string(headersJSON),
		resp.ResponseBody,
		expiresAt,
		lockToken,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("idempotency: pg lease completion: no matching in-progress row or token mismatch")
	}
	return nil
}

// upsertPGCompleted writes a completed idempotency row when the lock was held in Redis
// (no Postgres in-progress lease on this request).
func (m *Idempotency) upsertPGCompleted(ctx context.Context, key string, resp *cachedResponse) error {
	headersJSON, err := json.Marshal(resp.ResponseHeaders)
	if err != nil {
		return err
	}
	expiresAt := time.Unix(0, resp.ExpiresAtUnixNano).UTC()
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(m.responseTTL)
	}

	_, err = m.db.Exec(ctx, `
		INSERT INTO idempotency_keys (
			key, request_path, request_method, request_hash,
			status, lock_token, lock_expires_at,
			response_status_code, response_headers, response_body, expires_at
		) VALUES ($1, $2, $3, $4, 'completed', NULL, NULL, $5, $6::jsonb, $7, $8)
		ON CONFLICT (key) DO UPDATE SET
			request_path = EXCLUDED.request_path,
			request_method = EXCLUDED.request_method,
			request_hash = EXCLUDED.request_hash,
			status = 'completed',
			lock_token = NULL,
			lock_expires_at = NULL,
			response_status_code = EXCLUDED.response_status_code,
			response_headers = EXCLUDED.response_headers,
			response_body = EXCLUDED.response_body,
			expires_at = EXCLUDED.expires_at
		`,
		key,
		resp.RequestPath,
		resp.RequestMethod,
		resp.RequestHash,
		resp.ResponseStatus,
		string(headersJSON),
		resp.ResponseBody,
		expiresAt,
	)
	return err
}

// tryAcquireLock takes scope (Redis key material), the client idempotency key, and request fields
// so PostgreSQL can record a row-level lease on idempotency_keys without session-scoped locks.
func (m *Idempotency) tryAcquireLock(ctx context.Context, scope, idemKey, requestPath, requestMethod, token string) (*lockHandle, error) {
	lockTTL := m.lockTTL
	if lockTTL <= 0 {
		lockTTL = defaultLockTTL
	}

	if m.redis != nil {
		res, err := m.redis.SetArgs(ctx, redisLockKey(scope), token, redis.SetArgs{
			Mode: "NX",
			TTL:  lockTTL,
		}).Result()
		if err == nil && res == "OK" {
			return &lockHandle{kind: "redis", key: scope, token: token}, nil
		}
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		// Redis unavailable: continue to PG fallback lock.
	}

	if m.db == nil {
		return nil, errors.New("no lock backend configured")
	}

	leaseDur := m.requestTimeout
	if leaseDur <= 0 {
		leaseDur = defaultRequestTimeout
	}
	lockExpiresAt := time.Now().UTC().Add(leaseDur)

	respTTL := m.responseTTL
	if respTTL <= 0 {
		respTTL = defaultResponseTTL
	}
	rowExpiresAt := time.Now().UTC().Add(respTTL)

	var returnedToken string
	err := m.db.QueryRow(ctx, `
		INSERT INTO idempotency_keys (
			key, request_path, request_method, request_hash,
			status, lock_token, lock_expires_at, expires_at,
			response_status_code, response_headers, response_body
		) VALUES ($1, $2, $3, $4, 'in_progress', $4, $5, $6, NULL, NULL, NULL)
		ON CONFLICT (key) DO UPDATE SET
			request_path     = EXCLUDED.request_path,
			request_method   = EXCLUDED.request_method,
			request_hash     = EXCLUDED.request_hash,
			status           = 'in_progress',
			lock_token       = EXCLUDED.lock_token,
			lock_expires_at  = EXCLUDED.lock_expires_at,
			expires_at       = EXCLUDED.expires_at
		WHERE idempotency_keys.status = 'in_progress'
			AND idempotency_keys.lock_expires_at < NOW()
		RETURNING lock_token
		`,
		idemKey, requestPath, requestMethod, token, lockExpiresAt, rowExpiresAt,
	).Scan(&returnedToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if returnedToken == "" {
		return nil, nil
	}
	return &lockHandle{kind: "pg", key: idemKey, token: returnedToken}, nil
}

func (m *Idempotency) releaseLock(ctx context.Context, h *lockHandle) {
	if h == nil {
		return
	}
	tok := h.token
	switch h.kind {
	case "redis":
		if m.redis == nil {
			return
		}
		const releaseLua = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`
		_ = m.redis.Eval(ctx, releaseLua, []string{redisLockKey(h.key)}, tok).Err()
	case "pg":
		if m.db == nil {
			return
		}
		if h.completed {
			return
		}
		_, _ = m.db.Exec(ctx, `
			DELETE FROM idempotency_keys
			WHERE key = $1 AND lock_token = $2 AND status = 'in_progress'
			`,
			h.key, tok,
		)
	}
}

func (m *Idempotency) waitForCachedResponse(ctx context.Context, key string) (*cachedResponse, error) {
	timeout := m.waitTimeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	pollEvery := m.pollEvery
	if pollEvery <= 0 {
		pollEvery = defaultPollEvery
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resp, err := m.lookupCachedResponse(ctx, key); err == nil && resp != nil {
			return resp, nil
		}

		timer := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, context.DeadlineExceeded
}

func (m *Idempotency) matchesRequest(resp *cachedResponse, r *http.Request, requestHash string) bool {
	if resp == nil || r == nil {
		return false
	}
	if resp.RequestMethod != r.Method {
		return false
	}
	if resp.RequestPath != r.URL.Path {
		return false
	}
	return resp.RequestHash == requestHash
}

func writeCachedResponse(w http.ResponseWriter, resp *cachedResponse) {
	if resp == nil {
		http.Error(w, "idempotency response missing", http.StatusInternalServerError)
		return
	}
	for k, vv := range resp.ResponseHeaders {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if resp.ResponseStatus == 0 {
		resp.ResponseStatus = http.StatusOK
	}
	w.WriteHeader(resp.ResponseStatus)
	if len(resp.ResponseBody) > 0 {
		_, _ = w.Write(resp.ResponseBody)
	}
}

func writeIdempotencyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"code":"IDEMPOTENCY_ERROR","message":%q}}`, msg))
}

func cloneHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vv := range h {
		copied := make([]string, len(vv))
		copy(copied, vv)
		out[k] = copied
	}
	return out
}

type responseRecorder struct {
	header        http.Header
	body          bytes.Buffer
	statusCodeVal int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header: make(http.Header),
	}
}

func (rr *responseRecorder) Header() http.Header {
	return rr.header
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if rr.statusCodeVal == 0 {
		rr.statusCodeVal = http.StatusOK
	}
	return rr.body.Write(b)
}

func (rr *responseRecorder) WriteHeader(statusCode int) {
	rr.statusCodeVal = statusCode
}

func (rr *responseRecorder) statusCode() int {
	if rr.statusCodeVal == 0 {
		return http.StatusOK
	}
	return rr.statusCodeVal
}
