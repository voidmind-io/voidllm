package license

import "sync/atomic"

// licenseBox wraps a License interface in a concrete struct so that
// atomic.Value never sees different underlying types across stores.
type licenseBox struct {
	lic License
}

// Holder provides thread-safe read/write access to a License value.
// It is used by the application to allow the heartbeat goroutine to
// swap in refreshed licenses at runtime without restarting the proxy.
type Holder struct {
	v atomic.Value // stores licenseBox
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
