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
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := goredis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close() // best-effort cleanup on ping failure
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	if keyPrefix == "" {
		keyPrefix = "voidllm:"
	}
	if !strings.HasSuffix(keyPrefix, ":") {
		keyPrefix += ":"
	}

	return &Client{rdb: rdb, keyPrefix: keyPrefix}, nil
}

// Close closes the underlying Redis connection.
func (c *Client) Close() error { return c.rdb.Close() }

// key builds a prefixed Redis key by joining parts with ":" and prepending
// the configured key prefix.
func (c *Client) key(parts ...string) string {
	return c.keyPrefix + strings.Join(parts, ":")
}
