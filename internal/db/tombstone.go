package db

import "strings"

// tombstoneMarker is appended to a unique column's value at soft-delete time,
// suffixed with the row's own id, so the original value becomes immediately
// available for re-use while the deleted row still occupies its unique
// constraint slot. See #172.
const tombstoneMarker = ":deleted:"

// StripTombstone removes a tombstone suffix previously applied at soft-delete
// time, returning the original value. It is display-only: callers that render
// soft-deleted rows to admins (e.g. include_deleted listings) should run the
// stored value through StripTombstone before returning it, so the response
// shows the value the row had before deletion rather than the mangled one. If
// value does not end with exactly tombstoneMarker+id, value is returned
// unchanged.
func StripTombstone(value, id string) string {
	suffix := tombstoneMarker + id
	if strings.HasSuffix(value, suffix) {
		return strings.TrimSuffix(value, suffix)
	}
	return value
}

// ContainsTombstoneMarker reports whether s contains the tombstone marker
// anywhere. Input validation on unique columns (model/deployment names, user
// emails) must reject values containing the marker so a live row can never
// collide with, or be mistaken for, a tombstoned one.
func ContainsTombstoneMarker(s string) bool {
	return strings.Contains(s, tombstoneMarker)
}
