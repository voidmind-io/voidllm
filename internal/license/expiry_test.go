package license

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func newExpiringLicense(exp time.Time, trial bool) *enterpriseLicense {
	return &enterpriseLicense{
		features:    map[string]struct{}{FeatureAuditLogs: {}},
		featureList: []string{FeatureAuditLogs},
		expiresAt:   exp,
		maxOrgs:     -1,
		maxTeams:    -1,
		customerID:  "cust",
		trial:       trial,
	}
}

func TestEnterpriseLicense_ExpiryBehavesLikeCommunity(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		wantValid bool
	}{
		{"future", time.Now().Add(time.Hour), true},
		{"past", time.Now().Add(-time.Second), false},
		{"perpetual", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newExpiringLicense(tt.expiresAt, false)
			if got := l.HasFeature(FeatureAuditLogs); got != tt.wantValid {
				t.Errorf("HasFeature = %v, want %v", got, tt.wantValid)
			}
			if got := len(l.Features()) > 0; got != tt.wantValid {
				t.Errorf("Features non-empty = %v, want %v", got, tt.wantValid)
			}
			wantEdition := EditionEnterprise
			wantOrgs, wantTeams := -1, -1
			if !tt.wantValid {
				wantEdition, wantOrgs, wantTeams = EditionCommunity, CommunityMaxOrgs, CommunityMaxTeams
			}
			if l.Edition() != wantEdition || l.MaxOrgs() != wantOrgs || l.MaxTeams() != wantTeams {
				t.Errorf("edition/limits = %v/%d/%d, want %v/%d/%d",
					l.Edition(), l.MaxOrgs(), l.MaxTeams(), wantEdition, wantOrgs, wantTeams)
			}
		})
	}
}

func TestIsTrial(t *testing.T) {
	tests := []struct {
		name string
		lic  License
		want bool
	}{
		{"trial", newExpiringLicense(time.Now().Add(time.Hour), true), true},
		{"paid", newExpiringLicense(time.Now().Add(time.Hour), false), false},
		{"community", communityLicense{}, false},
		{"dev", devLicense{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTrial(tt.lic); got != tt.want {
				t.Errorf("IsTrial = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckExpiry(t *testing.T) {
	base := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		lic         License
		now         time.Time
		gated       bool
		wantSwap    bool
		wantRestart bool
	}{
		{"not yet expired", newExpiringLicense(base.Add(time.Minute), false), base, true, false, false},
		{"expired gated", newExpiringLicense(base.Add(-time.Minute), true), base, true, true, true},
		{"expired not gated", newExpiringLicense(base.Add(-time.Minute), false), base, false, true, false},
		{"community untouched", communityLicense{}, base, true, false, false},
		{"dev untouched", devLicense{}, base, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			holder := NewHolder(tt.lic)
			state := NewExpiryState(tt.gated)
			var calls atomic.Int32
			cfg := ExpiryWatcherConfig{
				Now:       func() time.Time { return tt.now },
				Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
				State:     state,
				OnExpired: func(context.Context) { calls.Add(1) },
			}
			if got := CheckExpiry(context.Background(), holder, cfg); got != tt.wantSwap {
				t.Fatalf("CheckExpiry = %v, want %v", got, tt.wantSwap)
			}
			if state.Expired() != tt.wantSwap || state.RestartRequired() != tt.wantRestart {
				t.Errorf("state expired=%v restart=%v, want %v/%v",
					state.Expired(), state.RestartRequired(), tt.wantSwap, tt.wantRestart)
			}
			if tt.wantSwap {
				if holder.Load().Edition() != EditionCommunity {
					t.Errorf("holder edition = %v, want community", holder.Load().Edition())
				}
				if calls.Load() != 1 {
					t.Errorf("OnExpired calls = %d, want 1", calls.Load())
				}
				// A second check is a no-op: logged and reloaded once.
				if CheckExpiry(context.Background(), holder, cfg) || calls.Load() != 1 {
					t.Error("second CheckExpiry swapped again")
				}
			}
		})
	}
}

func TestStartExpiryWatcher_SwapsOnTick(t *testing.T) {
	holder := NewHolder(newExpiringLicense(time.Now().Add(-time.Minute), false))
	state := NewExpiryState(false)
	stop := StartExpiryWatcher(holder, ExpiryWatcherConfig{
		Interval: 10 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		State:    state,
	})
	defer stop()

	deadline := time.After(2 * time.Second)
	for !state.Expired() {
		select {
		case <-deadline:
			t.Fatal("watcher did not detect expiry")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("edition = %v, want community", holder.Load().Edition())
	}
	if state.ExpiredAt().IsZero() {
		t.Error("ExpiredAt is zero")
	}
}

func TestHeartbeatManager_EnsureStartsOnce(t *testing.T) {
	tests := []struct {
		name       string
		keys       []string
		wantStarts int
	}{
		{"single", []string{"k"}, 1},
		{"double start guarded", []string{"k1", "k2"}, 1},
		{"empty key ignored", []string{""}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var starts, stops atomic.Int32
			m := NewHeartbeatManager(NewHolder(communityLicense{}), HeartbeatConfig{InstanceID: "id"})
			m.start = func(*Holder, string, HeartbeatConfig) func() {
				starts.Add(1)
				return func() { stops.Add(1) }
			}
			for _, k := range tt.keys {
				m.Ensure(context.Background(), k)
			}
			if int(starts.Load()) != tt.wantStarts {
				t.Errorf("starts = %d, want %d", starts.Load(), tt.wantStarts)
			}
			m.Stop()
			if int(stops.Load()) != tt.wantStarts {
				t.Errorf("stops = %d, want %d", stops.Load(), tt.wantStarts)
			}
		})
	}
}

func TestHeartbeatManager_NilSafe(t *testing.T) {
	var m *HeartbeatManager
	if m.Ensure(context.Background(), "k") {
		t.Error("nil manager started a heartbeat")
	}
	m.Stop()
}
