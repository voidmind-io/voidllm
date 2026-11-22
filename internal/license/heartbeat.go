package license

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/api/health"
	dbtimestamp "github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
)

const (
	// DefaultServerURL is the production license verification endpoint.
	DefaultServerURL = "https://license.voidllm.ai"

	// TrialRequestPath is the license server path that accepts trial requests.
	TrialRequestPath = "/v1/trial/request"

	// DefaultInterval is the default heartbeat interval.
	DefaultInterval = 24 * time.Hour

	// refreshThreshold is how close to expiry the license must be before
	// the heartbeat will accept and store a refreshed JWT.
	refreshThreshold = 7 * 24 * time.Hour

	requestTimeout = 30 * time.Second

	// maxResponseSize limits the license server response body to prevent
	// memory exhaustion from a compromised or misconfigured server.
	maxResponseSize = 1 << 20 // 1 MB
)

// HeartbeatConfig configures the license heartbeat background goroutine.
type HeartbeatConfig struct {
	// ServerURL is the base URL of the license server (e.g. "https://license.voidllm.ai").
	ServerURL string
	// Interval is how often the heartbeat fires. Defaults to DefaultInterval.
	Interval time.Duration
	// Log is the structured logger for heartbeat events.
	Log *slog.Logger
	// DB persists refreshed license JWTs and tracks the last heartbeat timestamp.
	// When nil, refreshed JWTs are stored in memory only and per-pod deduplication
	// is skipped.
	DB SettingsReadWriter
	// InstanceID is the stable UUID for this deployment, included in heartbeat
	// requests so the license server can track instance count.
	InstanceID string
	// Trigger, when set, requests an extra heartbeat soon after a new license
	// key was activated, ahead of the regular schedule.
	Trigger <-chan struct{}
	// State, when set, is updated if the heartbeat ends a trial license.
	State *ExpiryState
	// OnDowngrade, when set, runs after the heartbeat swapped a trial license
	// to community, for example to re-gate the model registry.
	OnDowngrade func(ctx context.Context)
}

// initialDelay lets the application fully start before the first heartbeat.
// It is a variable so tests can shorten it.
var initialDelay = 1 * time.Minute

// activationDelay is how long after a key activation the extra heartbeat
// fires. It is a variable so tests can shorten it.
var activationDelay = 10 * time.Second

var heartbeatClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	},
}

type verifyRequest struct {
	Key        string `json:"key"`
	InstanceID string `json:"instance_id,omitempty"`
}

type verifyResponse struct {
	Status         string `json:"status"`
	Plan           string `json:"plan,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	RevokedAt      string `json:"revoked_at,omitempty"`
	Key            string `json:"key,omitempty"`
	PaymentWarning string `json:"payment_warning,omitempty"`
	Message        string `json:"message,omitempty"`
}

// StartHeartbeat launches a background goroutine that periodically verifies
// the license key against the license server. If the current license is within
// refreshThreshold of expiry and the server returns a fresh JWT, the holder is
// updated in memory. No file persistence — in Docker, the filesystem is ephemeral.
//
// The returned function stops the goroutine, cancels any in-flight HTTP request,
// and blocks until the goroutine exits.
func StartHeartbeat(holder *Holder, rawKey string, cfg HeartbeatConfig) func() {
	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServerURL
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		log := cfg.Log.With(slog.String("component", "license.heartbeat"))
		holder.seedKey(rawKey)

		// beat sends the key that is active right now, which may differ from
		// the one this goroutine started with.
		beat := func(ctx context.Context) {
			key := holder.Key()
			if key == "" {
				return
			}
			before := holder.Load()
			runHeartbeat(ctx, holder, key, cfg.ServerURL, cfg.InstanceID, log, cfg.DB)
			ent, wasEnterprise := before.(*enterpriseLicense)
			if _, nowCommunity := holder.Load().(communityLicense); wasEnterprise && ent.trial && nowCommunity {
				cfg.State.record(ent)
				if cfg.OnDowngrade != nil {
					cfg.OnDowngrade(ctx)
				}
			}
		}

		// triggered waits briefly after an activation, then clears the
		// multi-pod dedup marker so the new key is verified right away.
		triggered := func() bool {
			select {
			case <-time.After(activationDelay):
			case <-done:
				return false
			}
			if cfg.DB != nil {
				if err := cfg.DB.SetSetting(ctx, "heartbeat_last_sent", ""); err != nil {
					log.LogAttrs(ctx, slog.LevelDebug, "failed to clear heartbeat timestamp",
						slog.String("error", err.Error()))
				}
			}
			return true
		}

		// Wait before first heartbeat to let the app fully start.
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-cfg.Trigger:
			if !triggered() {
				return
			}
		case <-done:
			return
		}

		// Run the first heartbeat immediately after the initial delay.
		beat(ctx)

		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				beat(ctx)
			case <-cfg.Trigger:
				if !triggered() {
					return
				}
				beat(ctx)
			case <-done:
				return
			}
		}
	}()

	return func() {
		close(done)
		cancel()
		wg.Wait()
	}
}

// runHeartbeat performs a single heartbeat check against the license server.
// It returns the (possibly updated) raw license key for use in the next heartbeat.
func runHeartbeat(ctx context.Context, holder *Holder, rawKey, serverURL, instanceID string, log *slog.Logger, db SettingsReadWriter) string {
	// Check if heartbeat was already sent recently (by this or another pod).
	if db != nil {
		lastSent, settErr := db.GetSetting(ctx, "heartbeat_last_sent")
		if settErr != nil {
			log.LogAttrs(ctx, slog.LevelDebug, "failed to read heartbeat timestamp, proceeding",
				slog.String("error", settErr.Error()),
			)
		}
		if lastSent != "" {
			if t, err := time.Parse(time.RFC3339, lastSent); err == nil && time.Since(t) < 23*time.Hour {
				log.LogAttrs(ctx, slog.LevelDebug, "heartbeat already sent recently, skipping",
					slog.String("last_sent", lastSent),
				)
				return rawKey
			}
		}
	}

	resp, err := sendVerifyRequest(ctx, serverURL, rawKey, instanceID)
	if err != nil {
		log.Warn("network error, continuing offline", slog.String("error", err.Error()))
		return rawKey
	}

	// Record that we sent a heartbeat (dedup for multi-pod).
	// Deferred so it fires on every return after this point, regardless of
	// response processing outcome.
	if db != nil {
		defer func() {
			_ = db.SetSetting(ctx, "heartbeat_last_sent", dbtimestamp.FormatTimestamp(time.Now()))
		}()
	}

	switch resp.Status {
	case "active":
		if resp.PaymentWarning != "" {
			log.Warn("subscription payment issue",
				slog.String("warning", resp.PaymentWarning),
			)
		}

		// Refresh the license if:
		// - the current license is not enterprise (e.g. expired key fell back to community), OR
		// - the current license expires within the refresh threshold (<7 days)
		if resp.Key != "" {
			lic := holder.Load()
			expiresAt := lic.ExpiresAt()
			needsRefresh := lic.Edition() != EditionEnterprise ||
				(!expiresAt.IsZero() && time.Until(expiresAt) < refreshThreshold)

			if needsRefresh {
				newLic, valErr := ValidateKeyForInstance(resp.Key, instanceID)
				if valErr != nil {
					log.Warn("received invalid refresh key from server",
						slog.String("error", valErr.Error()),
					)
					return rawKey
				}

				// Verify the refreshed JWT belongs to the same customer
				// (skip check when recovering from community fallback).
				if lic.Edition() == EditionEnterprise && newLic.CustomerID() != lic.CustomerID() {
					log.Warn("refreshed license has different customer ID, rejecting",
						slog.String("expected", lic.CustomerID()),
						slog.String("received", newLic.CustomerID()),
					)
					return rawKey
				}

				// Reject refresh JWTs that do not extend the expiry
				// (skip check when recovering from community fallback).
				if lic.Edition() == EditionEnterprise && !newLic.ExpiresAt().After(expiresAt) {
					log.Warn("refresh JWT does not extend expiry, rejecting",
						slog.Time("current_expires", expiresAt),
						slog.Time("refresh_expires", newLic.ExpiresAt()),
					)
					return rawKey
				}

				var persist func() error
				if db != nil {
					persist = func() error { return db.SetSetting(ctx, "license_jwt", resp.Key) }
				}
				applied, refreshErr := holder.refresh(rawKey, resp.Key, newLic, persist)
				if refreshErr != nil {
					log.Warn("failed to persist refreshed license to database",
						slog.String("error", refreshErr.Error()),
					)
				}
				if !applied {
					log.Info("refreshed license dropped, another key was activated meanwhile")
					return rawKey
				}
				log.Info("license refreshed",
					slog.String("plan", resp.Plan),
					slog.String("expires_at", resp.ExpiresAt),
				)
				return resp.Key
			}
			log.Info("license active, no refresh needed",
				slog.String("plan", resp.Plan),
				slog.Time("expires_at", expiresAt),
			)
		} else {
			log.Info("license active",
				slog.String("plan", resp.Plan),
			)
		}

	case "revoked":
		if endTrial(ctx, holder, rawKey, resp.Status, log, db) {
			return rawKey
		}
		log.Warn("license revoked by server — proxy will fall back to community edition when current license expires",
			slog.String("revoked_at", resp.RevokedAt),
		)

	case "expired":
		if endTrial(ctx, holder, rawKey, resp.Status, log, db) {
			return rawKey
		}
		log.Warn("license expired according to server",
			slog.String("plan", resp.Plan),
		)

	case "instance_mismatch":
		if endTrial(ctx, holder, rawKey, resp.Status, log, db) {
			return rawKey
		}
		log.Warn("license is bound to a different instance according to server")

	case "unknown":
		log.Warn("license not recognized by server")

	case "error":
		log.Warn("server returned error",
			slog.String("message", resp.Message),
		)

	default:
		log.Warn("unexpected status from license server",
			slog.String("status", resp.Status),
		)
	}

	return rawKey
}

// endTrial swaps a trial license to community when the license server reports
// that the trial ended or does not belong to this instance. It acts only when
// sentKey, the key the request was sent with, is still the active key; paid
// licenses are left alone. The ended trial's token ID is persisted so the key
// is not accepted again. It reports whether the swap happened.
func endTrial(ctx context.Context, holder *Holder, sentKey, status string, log *slog.Logger, db SettingsReadWriter) bool {
	ent, ok := holder.endTrialFor(sentKey)
	if !ok {
		return false
	}
	log.LogAttrs(ctx, slog.LevelWarn, "trial license ended according to server, reverting to community edition",
		slog.String("status", status))
	if err := RecordEndedTrial(ctx, db, ent); err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "failed to persist ended trial", slog.String("error", err.Error()))
	}
	return true
}

// sendVerifyRequest posts the license key to the server and returns the parsed response.
func sendVerifyRequest(ctx context.Context, serverURL, key, instanceID string) (*verifyResponse, error) {
	body, err := jsonx.Marshal(verifyRequest{Key: key, InstanceID: instanceID})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, serverURL+"/v1/verify", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VoidLLM/"+health.Version)

	resp, err := heartbeatClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from license server", resp.StatusCode)
	}

	var vr verifyResponse
	if decErr := jsonx.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&vr); decErr != nil {
		return nil, fmt.Errorf("decode response: %w", decErr)
	}
	return &vr, nil
}
