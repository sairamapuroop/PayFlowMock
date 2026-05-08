package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

func idemTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)
	return pool
}

func decodeIdemError(t *testing.T, body io.Reader) string {
	t.Helper()
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out.Error.Message
}

func TestIdempotency_Integration_CachedResponseFromRedis(t *testing.T) {
	pool := idemTestPool(t)
	rdb := testutil.MustRedis(t)
	testutil.FlushRedisIdempotency(t, rdb)

	idem := NewIdempotency(rdb, pool)
	var calls atomic.Int32
	h := idem.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pay_1"}`))
	}))

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	key := "idem-int-cached-1"
	body := `{"amount":100,"currency":"USD"}`
	do := func() (int, string) {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/payments", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(b)
	}

	code1, b1 := do()
	if code1 != http.StatusCreated || b1 != `{"id":"pay_1"}` {
		t.Fatalf("first response: status=%d body=%q", code1, b1)
	}
	code2, b2 := do()
	if code2 != http.StatusCreated || b2 != `{"id":"pay_1"}` {
		t.Fatalf("second response: status=%d body=%q", code2, b2)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls: got %d, want 1", calls.Load())
	}
}

func TestIdempotency_Integration_RequestHashConflict(t *testing.T) {
	pool := idemTestPool(t)
	rdb := testutil.MustRedis(t)
	testutil.FlushRedisIdempotency(t, rdb)

	idem := NewIdempotency(rdb, pool)
	var calls atomic.Int32
	h := idem.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	key := "idem-int-conflict-1"
	req1, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/payments", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req1.Header.Set("Idempotency-Key", key)
	rec1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rec1.Body)
	rec1.Body.Close()
	if rec1.StatusCode != http.StatusCreated {
		t.Fatalf("first status: %d", rec1.StatusCode)
	}

	req2, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/payments", strings.NewReader(`{"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Idempotency-Key", key)
	rec2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Body.Close()
	if rec2.StatusCode != http.StatusConflict {
		t.Fatalf("second status: got %d, want 409", rec2.StatusCode)
	}
	if msg := decodeIdemError(t, rec2.Body); !strings.Contains(msg, "different request") {
		t.Fatalf("error message: %q", msg)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls: got %d, want 1", calls.Load())
	}
}

func TestIdempotency_Integration_ConcurrentDuplicatesRedisLock(t *testing.T) {
	pool := idemTestPool(t)
	rdb := testutil.MustRedis(t)
	testutil.FlushRedisIdempotency(t, rdb)

	idem := NewIdempotency(rdb, pool)
	idem.waitTimeout = 4 * time.Second
	idem.pollEvery = 25 * time.Millisecond
	idem.lockTTL = 30 * time.Second

	var calls atomic.Int32
	h := idem.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			time.Sleep(150 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"n":42}`))
	}))

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	key := "idem-int-concurrent-1"
	body := `{"x":1}`
	path := srv.URL + "/v1/payments"

	start := make(chan struct{})
	var wg sync.WaitGroup
	status := make([]int, 2)
	respBody := make([]string, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Idempotency-Key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Error(err)
				return
			}
			status[i] = resp.StatusCode
			respBody[i] = string(b)
		}()
	}
	close(start)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("handler calls: got %d, want 1", calls.Load())
	}
	for i := 0; i < 2; i++ {
		if status[i] != http.StatusOK {
			t.Fatalf("goroutine %d status: got %d body %q", i, status[i], respBody[i])
		}
		if respBody[i] != `{"n":42}` {
			t.Fatalf("goroutine %d body: got %q", i, respBody[i])
		}
	}
}

func TestIdempotency_Integration_PostgresLeaseWithoutRedis(t *testing.T) {
	pool := idemTestPool(t)

	idem := NewIdempotency(nil, pool)
	idem.requestTimeout = 10 * time.Second

	var calls atomic.Int32
	h := idem.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"via":"postgres"}`))
	}))

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	key := "idem-int-pg-lease-1"
	body := `{"lease":true}`
	path := srv.URL + "/v1/refunds"

	req1, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req1.Header.Set("Idempotency-Key", key)
	rec1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(rec1.Body)
	rec1.Body.Close()
	if rec1.StatusCode != http.StatusAccepted || string(b1) != `{"via":"postgres"}` {
		t.Fatalf("first: status=%d body=%q", rec1.StatusCode, string(b1))
	}

	req2, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Idempotency-Key", key)
	rec2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Body.Close()
	b2, _ := io.ReadAll(rec2.Body)
	if rec2.StatusCode != http.StatusAccepted || string(b2) != `{"via":"postgres"}` {
		t.Fatalf("second: status=%d body=%q", rec2.StatusCode, string(b2))
	}

	if calls.Load() != 1 {
		t.Fatalf("handler calls: got %d, want 1", calls.Load())
	}
}

func TestIdempotency_Integration_PostgresLeaseConcurrentWithoutRedis(t *testing.T) {
	pool := idemTestPool(t)

	idem := NewIdempotency(nil, pool)
	idem.requestTimeout = 15 * time.Second
	idem.waitTimeout = 5 * time.Second
	idem.pollEvery = 25 * time.Millisecond

	var calls atomic.Int32
	h := idem.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			time.Sleep(120 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	key := "idem-int-pg-concurrent-1"
	body := `{"k":9}`
	path := srv.URL + "/v1/payments"

	var wg sync.WaitGroup
	status := make([]int, 2)
	wg.Add(2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Idempotency-Key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			status[i] = resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("handler calls: got %d, want 1", calls.Load())
	}
	for i := 0; i < 2; i++ {
		if status[i] != http.StatusNoContent {
			t.Fatalf("goroutine %d: status %d", i, status[i])
		}
	}
}
