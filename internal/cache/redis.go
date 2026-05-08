package cache

import (
	"context"
	"os"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// Client wraps go-redis for connection lifecycle; use Redis() for standard commands.
type Client struct {
	rdb *redis.Client
}

// New returns a Redis client for addr (host:port).
func New(addr string) *Client {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		log.Warn().Err(err).Msg("redis tracing instrumentation failed")
	}
	if err := redisotel.InstrumentMetrics(rdb); err != nil {
		log.Warn().Err(err).Msg("redis metrics instrumentation failed")
	}
	return &Client{
		rdb: rdb,
	}
}

// NewFromEnv uses REDIS_ADDR, defaulting to 127.0.0.1:6379 when unset.
func NewFromEnv() *Client {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	return New(addr)
}

// Redis returns the underlying go-redis client.
func (c *Client) Redis() *redis.Client {
	return c.rdb
}

// Ping verifies connectivity.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases connections.
func (c *Client) Close() error {
	return c.rdb.Close()
}
