package license

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// licenseBox wraps a License interface in a concrete struct so that
// atomic.Value never sees different underlying types across stores.
type licenseBox struct {
	lic License
}

// Holder provides thread-safe read/write access to a License value.
// It is used by the application to allow the heartbeat goroutine to
// swap in refreshed licenses at runtime without restarting the proxy.
//
// Besides the parsed License the Holder tracks the raw license key the
// License was built from. Changes to the key (activation through the admin
// API, refresh by the heartbeat) go through Activate and refresh, which hold
// one mutex across the in-memory swap and the database write, so the License,
// the persisted key and the key the heartbeat sends never diverge.
type Holder struct {
	v atomic.Value // stores licenseBox

	keyMu sync.Mutex
	key   string
}

// NewHolder returns a Holder pre-loaded with the given License.
func NewHolder(lic License) *Holder {
	h := &Holder{}
	h.Store(lic)
	return h
}

// Load returns the current License. If no License has been stored,
// a communityLicense is returned as a safe default.
func (h *Holder) Load() License {
	v := h.v.Load()
	if v == nil {
		return communityLicense{}
	}
	return v.(licenseBox).lic
}

// Store atomically replaces the current License.
func (h *Holder) Store(lic License) {
	h.v.Store(licenseBox{lic: lic})
}

// compareAndStore replaces the current License with next only when the holder
// still contains old. It reports whether the replacement happened.
func (h *Holder) compareAndStore(old, next License) bool {
	return h.v.CompareAndSwap(licenseBox{lic: old}, licenseBox{lic: next})
}

// Key returns the raw license key the heartbeat should currently send. It is
// empty when no key was activated.
func (h *Holder) Key() string {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	return h.key
}

// seedKey records key as the active key unless one is already set.
func (h *Holder) seedKey(key string) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	if h.key == "" {
		h.key = key
	}
}

// Activate installs lic as the current License and rawKey as the active key.
// persist, if non-nil, runs first under the same lock; when it fails nothing
// is changed and its error is returned. Concurrent Activate calls are
// serialized, so the License, the persisted key and Key always belong to the
// same call.
func (h *Holder) Activate(lic License, rawKey string, persist func() error) error {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	if persist != nil {
		if err := persist(); err != nil {
			return fmt.Errorf("persist license key: %w", err)
		}
	}
	h.Store(lic)
	h.key = rawKey
	return nil
}

// refresh installs a license issued by the license server for oldKey. It
// applies only while oldKey is still the active key (or no key is recorded
// yet), so a refresh that raced with an activation of another key is dropped.
// persist runs under the lock. A persist failure does not block the swap (the
// previously persisted key stays valid until it expires); its error is
// returned alongside applied=true. It reports whether the refresh was applied.
func (h *Holder) refresh(oldKey, newKey string, lic License, persist func() error) (bool, error) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	if h.key != "" && h.key != oldKey {
		return false, nil
	}
	var persistErr error
	if persist != nil {
		if err := persist(); err != nil {
			persistErr = fmt.Errorf("persist refreshed license key: %w", err)
		}
	}
	h.Store(lic)
	h.key = newKey
	return true, persistErr
}
