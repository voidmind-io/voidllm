package license

import (
	"context"
	"log/slog"
	"sync"
)

// HeartbeatManager starts the license heartbeat at most once per process. It
// lets a license key activated at runtime begin heartbeating without a
// restart, while guarding against a second heartbeat goroutine. The key the
// heartbeat sends is always the one the Holder currently carries.
type HeartbeatManager struct {
	holder  *Holder
	cfg     HeartbeatConfig
	start   func(*Holder, string, HeartbeatConfig) func()
	trigger chan struct{}

	mu      sync.Mutex
	stop    func()
	running bool
}

// NewHeartbeatManager returns a manager that starts heartbeats against holder
// using cfg. When cfg.InstanceID is empty and cfg.DB is set, the instance ID is
// resolved lazily on first start.
func NewHeartbeatManager(holder *Holder, cfg HeartbeatConfig) *HeartbeatManager {
	return &HeartbeatManager{
		holder:  holder,
		cfg:     cfg,
		start:   StartHeartbeat,
		trigger: make(chan struct{}, 1),
	}
}

// Ensure starts the heartbeat if none is running. rawKey seeds the holder's
// active key when it has none; an already recorded key is never replaced, so a
// concurrent activation wins. It reports whether a heartbeat was started by
// this call. A nil manager or empty key is a no-op.
func (m *HeartbeatManager) Ensure(ctx context.Context, rawKey string) bool {
	if m == nil || rawKey == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holder.seedKey(rawKey)
	if m.running {
		return false
	}

	cfg := m.cfg
	cfg.Trigger = m.trigger
	if cfg.InstanceID == "" && cfg.DB != nil {
		id, err := GetOrCreateInstanceID(ctx, cfg.DB)
		if err != nil {
			log := cfg.Log
			if log == nil {
				log = slog.Default()
			}
			log.LogAttrs(ctx, slog.LevelWarn, "failed to resolve instance ID for heartbeat",
				slog.String("error", err.Error()))
		} else {
			cfg.InstanceID = id
		}
	}

	m.stop = m.start(m.holder, m.holder.Key(), cfg)
	m.running = true
	return true
}

// Activated must be called after a new key was installed with Holder.Activate.
// It starts the heartbeat if needed and requests one extra heartbeat soon, so
// the new key is verified (and refreshed if due) without waiting for the next
// daily tick. A nil manager is a no-op.
func (m *HeartbeatManager) Activated(ctx context.Context, rawKey string) {
	if m == nil {
		return
	}
	m.Ensure(ctx, rawKey)
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Stop halts the heartbeat if one is running and allows a later Ensure to
// start a new one.
func (m *HeartbeatManager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	stop := m.stop
	m.stop = nil
	m.running = false
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
}
