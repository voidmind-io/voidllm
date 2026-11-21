package license

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

const (
	// endedTrialsSetting is the settings key holding the JSON list of token IDs
	// of trial licenses that have ended.
	endedTrialsSetting = "ended_trial_ids"

	// maxEndedTrials bounds the stored list; the oldest entries are dropped.
	maxEndedTrials = 100
)

// ErrTrialEnded is wrapped by ErrInvalidKey errors returned for a trial license
// that already ended on this installation.
var ErrTrialEnded = errors.New("this trial has ended")

// endedMu serializes read-modify-write cycles on the ended-trials setting
// within this process.
var endedMu sync.Mutex

// LicenseID returns the unique token ID of lic, or an empty string for
// licenses without one.
func LicenseID(lic License) string {
	i, ok := lic.(interface{ ID() string })
	if !ok {
		return ""
	}
	return i.ID()
}

func loadEndedTrials(ctx context.Context, db SettingsReadWriter) ([]string, error) {
	raw, err := db.GetSetting(ctx, endedTrialsSetting)
	if err != nil {
		return nil, fmt.Errorf("read ended trials: %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	var ids []string
	if err := jsonx.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("parse ended trials: %w", err)
	}
	return ids, nil
}

// RecordEndedTrial remembers that the trial license lic ended, so the same key
// is not accepted again after a restart or re-activation. Licenses that are not
// trials, or have no token ID, are ignored.
func RecordEndedTrial(ctx context.Context, db SettingsReadWriter, lic License) error {
	id := LicenseID(lic)
	if db == nil || id == "" || !IsTrial(lic) {
		return nil
	}
	endedMu.Lock()
	defer endedMu.Unlock()

	ids, err := loadEndedTrials(ctx, db)
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	ids = append(ids, id)
	if len(ids) > maxEndedTrials {
		ids = ids[len(ids)-maxEndedTrials:]
	}
	raw, err := jsonx.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode ended trials: %w", err)
	}
	if err := db.SetSetting(ctx, endedTrialsSetting, string(raw)); err != nil {
		return fmt.Errorf("store ended trials: %w", err)
	}
	return nil
}

// CheckTrialNotEnded returns an error wrapping ErrInvalidKey and ErrTrialEnded
// when lic is a trial whose token ID was recorded as ended. Paid licenses and
// trials without a recorded end pass.
func CheckTrialNotEnded(ctx context.Context, db SettingsReadWriter, lic License) error {
	id := LicenseID(lic)
	if db == nil || id == "" || !IsTrial(lic) {
		return nil
	}
	ids, err := loadEndedTrials(ctx, db)
	if err != nil {
		return err
	}
	for _, ended := range ids {
		if ended == id {
			return fmt.Errorf("%w: %w", ErrInvalidKey, ErrTrialEnded)
		}
	}
	return nil
}

// LoadForStartup verifies key like VerifyForInstance and additionally falls
// back to community when the key is a trial that already ended on this
// installation. A failure to read the ended-trials list is returned so the
// caller can log it; the license is then accepted, because a storage hiccup
// must not lock out a legitimate key.
func LoadForStartup(ctx context.Context, db SettingsReadWriter, key string, enterpriseDev bool, instanceID string) (License, error) {
	lic := VerifyForInstance(key, enterpriseDev, instanceID)
	err := CheckTrialNotEnded(ctx, db, lic)
	if err == nil {
		return lic, nil
	}
	if errors.Is(err, ErrTrialEnded) {
		return communityLicense{}, err
	}
	return lic, err
}
