package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// MustRedis returns a *redis.Client for integration tests using REDIS_ADDR (default 127.0.0.1:6379).
// Skips when -short is set; fails fast when ping fails.
func MustRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test (-short)")
	}
	loadTestEnvFromDotenv()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}

// FlushRedisIdempotency deletes keys used by idempotency middleware (idem:*) for test isolation.
func FlushRedisIdempotency(t *testing.T, rdb *redis.Client) {
	t.Helper()
	if rdb == nil {
		return
	}
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, "idem:*", 64).Iterator()
	for iter.Next(ctx) {
		if err := rdb.Del(ctx, iter.Val()).Err(); err != nil {
			t.Fatalf("redis del %q: %v", iter.Val(), err)
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("redis scan: %v", err)
	}
}
