// Package redis provides a Redis client wrapper for VoidLLM with key prefixing,
// distributed rate limiting, token budget tracking, and cache invalidation
// pub/sub. All operations accept a context.Context as the first parameter.
package redis

import (
	"context"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client wraps go-redis with VoidLLM key prefixing.
type Client struct {
	rdb       *goredis.Client
	keyPrefix string
}

// New creates a Redis client from url, verifies connectivity with PING, and
// returns a ready-to-use Client. keyPrefix is prepended to every Redis key and
// channel name. If keyPrefix is empty it defaults to "voidllm:"; a trailing
// colon is appended if absent.
func New(url, keyPrefix string) (*Client, error) {
	c, err := NewUnverified(url, keyPrefix)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		_ = c.Close() // best-effort cleanup on ping failure
		return nil, err
	}
	return c, nil
}

// NewUnverified creates a Redis client from url without contacting the
// server; it only fails when url cannot be parsed. The client connects on
// first use and reconnects on its own, so it is usable for callers that can
// work while Redis is down and want to pick it up once it is reachable.
// keyPrefix is handled as in New.
func NewUnverified(url, keyPrefix string) (*Client, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Let context deadlines bound the network I/O of every command, so a
	// publish under a timeout context cannot hang on an unresponsive server.
	opts.ContextTimeoutEnabled = true
	rdb := goredis.NewClient(opts)

	if keyPrefix == "" {
		keyPrefix = "voidllm:"
	}
	if !strings.HasSuffix(keyPrefix, ":") {
		keyPrefix += ":"
	}

	return &Client{rdb: rdb, keyPrefix: keyPrefix}, nil
}

// Ping checks that the Redis server answers.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

// Close closes the underlying Redis connection.
func (c *Client) Close() error { return c.rdb.Close() }

// key builds a prefixed Redis key by joining parts with ":" and prepending
// the configured key prefix.
func (c *Client) key(parts ...string) string {
	return c.keyPrefix + strings.Join(parts, ":")
}
