package license

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultServerURL is the production license verification endpoint.
	DefaultServerURL = "https://license.voidllm.ai"

	// DefaultInterval is the default heartbeat interval.
	DefaultInterval = 24 * time.Hour

	// refreshThreshold is how close to expiry the license must be before
	// the heartbeat will accept and store a refreshed JWT.
	refreshThreshold = 7 * 24 * time.Hour

	// initialDelay lets the application fully start before the first heartbeat.
	initialDelay   = 1 * time.Minute
	requestTimeout = 30 * time.Second

	// maxResponseSize limits the license server response body to prevent
	// memory exhaustion from a compromised or misconfigured server.
	maxResponseSize = 1 << 20 // 1 MB
)

// SettingsWriter is the subset of the database interface needed by the heartbeat
// to persist refreshed license JWTs. Using an interface avoids circular imports
// with the db package.
type SettingsWriter interface {
	SetSetting(ctx context.Context, key, value string) error
}

// HeartbeatConfig configures the license heartbeat background goroutine.
type HeartbeatConfig struct {
	// ServerURL is the base URL of the license server (e.g. "https://license.voidllm.ai").
	ServerURL string
	// Interval is how often the heartbeat fires. Defaults to DefaultInterval.
	Interval time.Duration
	// Log is the structured logger for heartbeat events.
	Log *slog.Logger
	// DB persists refreshed license JWTs across container restarts.
	// When nil, refreshed JWTs are stored in memory only.
	DB SettingsWriter
}

var heartbeatClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	},
}

type verifyRequest struct {
	Key string `json:"key"`
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
		currentKey := rawKey

		// Wait before first heartbeat to let the app fully start.
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-done:
			return
		}

		// Run the first heartbeat immediately after the initial delay.
		currentKey = runHeartbeat(ctx, holder, currentKey, cfg.ServerURL, log, cfg.DB)

		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				currentKey = runHeartbeat(ctx, holder, currentKey, cfg.ServerURL, log, cfg.DB)
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
func runHeartbeat(ctx context.Context, holder *Holder, rawKey, serverURL string, log *slog.Logger, db SettingsWriter) string {
	resp, err := sendVerifyRequest(ctx, serverURL, rawKey)
	if err != nil {
		log.Warn("network error, continuing offline", slog.String("error", err.Error()))
		return rawKey
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
				newLic, valErr := ValidateKey(resp.Key)
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

				holder.Store(newLic)
				if db != nil {
					if dbErr := db.SetSetting(context.Background(), "license_jwt", resp.Key); dbErr != nil {
						log.Warn("failed to persist refreshed license to database",
							slog.String("error", dbErr.Error()),
						)
					}
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
		log.Warn("license revoked by server — proxy will fall back to community edition when current license expires",
			slog.String("revoked_at", resp.RevokedAt),
		)

	case "expired":
		log.Warn("license expired according to server",
			slog.String("plan", resp.Plan),
		)

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

// sendVerifyRequest posts the license key to the server and returns the parsed response.
func sendVerifyRequest(ctx context.Context, serverURL, key string) (*verifyResponse, error) {
	body, err := jsonx.Marshal(verifyRequest{Key: key})
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
