package license

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultExpiryCheckInterval is how often the expiry watcher inspects the
// current license.
const DefaultExpiryCheckInterval = time.Hour

// ExpiryState records whether the running process observed its license
// expiring, and whether startup-gated features were active at boot.
// It is safe for concurrent use.
type ExpiryState struct {
	expired      atomic.Bool
	expiredAtNs  atomic.Int64
	wasTrial     atomic.Bool
	startupGated bool
}

// NewExpiryState returns an ExpiryState. startupGated must be true when a
// feature that is only initialized at process start (audit logs, SSO, OTel)
// was enabled by the license at startup, so that an expiry requires a restart
// to fully take effect.
func NewExpiryState(startupGated bool) *ExpiryState {
	return &ExpiryState{startupGated: startupGated}
}

// Expired reports whether the watcher has swapped an expired license out.
func (s *ExpiryState) Expired() bool {
	return s != nil && s.expired.Load()
}

// ExpiredAt returns the expiry time of the license that was swapped out, or the
// zero time when no expiry was observed.
func (s *ExpiryState) ExpiredAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	ns := s.expiredAtNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// WasTrial reports whether the license that was swapped out was a trial.
func (s *ExpiryState) WasTrial() bool {
	return s != nil && s.wasTrial.Load()
}

// record notes that ent ended while the process was running.
func (s *ExpiryState) record(ent *enterpriseLicense) {
	if s == nil {
		return
	}
	s.wasTrial.Store(ent.trial)
	s.expiredAtNs.Store(ent.expiresAt.UnixNano())
	s.expired.Store(true)
}

// RestartRequired reports whether the license expired while startup-gated
// features are still running and a restart is needed to disable them.
func (s *ExpiryState) RestartRequired() bool {
	return s != nil && s.startupGated && s.expired.Load()
}

// ExpiryWatcherConfig configures StartExpiryWatcher.
type ExpiryWatcherConfig struct {
	// Interval is the check period. Defaults to DefaultExpiryCheckInterval.
	Interval time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Log receives the one-time expiry message. Defaults to slog.Default.
	Log *slog.Logger
	// State is updated when an expiry is detected. May be nil.
	State *ExpiryState
	// OnExpired is invoked once after the holder was swapped to community, for
	// example to re-gate the model registry. May be nil.
	OnExpired func(ctx context.Context)
}

// StartExpiryWatcher launches a goroutine that periodically checks whether the
// holder's license has expired and, if so, swaps in a community license. It
// runs one check immediately. The returned function stops the goroutine and
// blocks until it exits.
func StartExpiryWatcher(holder *Holder, cfg ExpiryWatcherConfig) func() {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultExpiryCheckInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		CheckExpiry(ctx, holder, cfg)
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				CheckExpiry(ctx, holder, cfg)
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}
}

// CheckExpiry performs a single expiry check. When the holder contains an
// enterprise license whose expiry is at or before cfg.Now, the holder is
// swapped to community, the state is flagged, and the expiry is logged once.
// It reports whether a swap happened.
func CheckExpiry(ctx context.Context, holder *Holder, cfg ExpiryWatcherConfig) bool {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	cur := holder.Load()
	ent, ok := cur.(*enterpriseLicense)
	if !ok || ent.expiresAt.IsZero() || now().Before(ent.expiresAt) {
		return false
	}
	if !holder.compareAndStore(cur, communityLicense{}) {
		return false
	}
	cfg.State.record(ent)
	log.LogAttrs(ctx, slog.LevelWarn, "license expired, reverting to community edition",
		slog.Time("expired_at", ent.expiresAt),
		slog.Bool("restart_required", cfg.State.RestartRequired()),
	)
	if cfg.OnExpired != nil {
		cfg.OnExpired(ctx)
	}
	return true
}
