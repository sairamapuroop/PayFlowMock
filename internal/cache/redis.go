package cache

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"
)

// Client wraps go-redis for connection lifecycle; use Redis() for standard commands.
type Client struct {
	rdb *redis.Client
}

// New returns a Redis client for addr (host:port).
func New(addr string) *Client {
	return &Client{
		rdb: redis.NewClient(&redis.Options{Addr: addr}),
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
