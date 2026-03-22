package redis

import (
	"context"
	"log/slog"
	"time"
)

// Channel name constants for cache invalidation pub/sub. These are the
// unprefixed channel names; the Client methods apply the configured key prefix
// automatically before publishing or subscribing.
const (
	// ChannelKeys is published when an API key is deleted. The payload is the
	// key hash, allowing subscribers to evict exactly that entry from the cache.
	ChannelKeys = "invalidate:keys"

	// ChannelModels is published when the model registry changes.
	ChannelModels = "invalidate:models"

	// ChannelAccess is published when any model access allowlist changes.
	// Subscribers should reload the full access cache from the database.
	ChannelAccess = "invalidate:access"

	// ChannelAliases is published when any model alias changes.
	// Subscribers should reload the full alias cache from the database.
	ChannelAliases = "invalidate:aliases"
)

// PublishInvalidation sends a cache invalidation message to the given channel.
// channel should be one of the Channel* constants; payload is message-specific
// (e.g. a key hash for ChannelKeys, or "reload" for bulk invalidations).
func (c *Client) PublishInvalidation(ctx context.Context, channel, payload string) error {
	return c.rdb.Publish(ctx, c.keyPrefix+channel, payload).Err()
}

// SubscribeInvalidations blocks, receiving messages from all four invalidation
// channels, and calls handler for each message received. The channel argument
// passed to handler has the key prefix stripped so callers can compare against
// the Channel* constants directly. The function returns when ctx is canceled,
// making it safe to run as a long-lived goroutine.
func (c *Client) SubscribeInvalidations(ctx context.Context, log *slog.Logger, handler func(channel, payload string)) {
	channels := []string{
		c.keyPrefix + ChannelKeys,
		c.keyPrefix + ChannelModels,
		c.keyPrefix + ChannelAccess,
		c.keyPrefix + ChannelAliases,
	}

	sub := c.rdb.Subscribe(ctx, channels...)
	defer sub.Close()

	log.LogAttrs(ctx, slog.LevelInfo, "redis pub/sub subscribed",
		slog.Int("channels", len(channels)),
	)

	for {
		msg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // context canceled — clean shutdown
			}
			log.LogAttrs(ctx, slog.LevelError, "redis pub/sub receive error",
				slog.String("error", err.Error()),
			)
			// Backoff to avoid tight error loop when Redis is unavailable.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		// Strip the key prefix from the channel name so the handler can
		// compare against the unprefixed Channel* constants.
		ch := msg.Channel
		if len(ch) > len(c.keyPrefix) {
			ch = ch[len(c.keyPrefix):]
		}
		handler(ch, msg.Payload)
	}
}
