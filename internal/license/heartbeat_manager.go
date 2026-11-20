package license

import (
	"context"
	"log/slog"
	"sync"
)

// HeartbeatManager starts the license heartbeat at most once per process. It
// lets a license key activated at runtime begin heartbeating without a
// restart, while guarding against a second heartbeat goroutine.
type HeartbeatManager struct {
	holder *Holder
	cfg    HeartbeatConfig
	start  func(*Holder, string, HeartbeatConfig) func()

	mu      sync.Mutex
	stop    func()
	running bool
	key     string
}

// NewHeartbeatManager returns a manager that starts heartbeats against holder
// using cfg. When cfg.InstanceID is empty and cfg.DB is set, the instance ID is
// resolved lazily on first start.
func NewHeartbeatManager(holder *Holder, cfg HeartbeatConfig) *HeartbeatManager {
	return &HeartbeatManager{holder: holder, cfg: cfg, start: StartHeartbeat}
}

// currentKey returns the most recently activated license key.
func (m *HeartbeatManager) currentKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key
}

// keyRefreshed records a key issued by the license server, unless a different
// key was activated while the heartbeat request was in flight.
func (m *HeartbeatManager) keyRefreshed(oldKey, newKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == oldKey {
		m.key = newKey
	}
}

// Ensure records rawKey as the active license key and starts the heartbeat if
// none is running. A running heartbeat picks up the new key on its next tick.
// It reports whether a heartbeat was started by this call. A nil manager or
// empty key is a no-op.
func (m *HeartbeatManager) Ensure(ctx context.Context, rawKey string) bool {
	if m == nil || rawKey == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key = rawKey
	if m.running {
		return false
	}

	cfg := m.cfg
	cfg.KeyFunc = m.currentKey
	cfg.OnKeyRefreshed = m.keyRefreshed
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

	m.stop = m.start(m.holder, rawKey, cfg)
	m.running = true
	return true
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
