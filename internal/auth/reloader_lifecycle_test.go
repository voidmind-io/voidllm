package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// lifecycleTimeout bounds every wait in these tests; it only elapses on failure.
const lifecycleTimeout = 10 * time.Second

// waitQueued blocks until a trailing load is queued behind the load in flight.
func waitQueued(t *testing.T, r *Reloader) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTimeout)
	for {
		r.mu.Lock()
		queued := r.next != nil
		r.mu.Unlock()
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no trailing load was queued")
		}
		time.Sleep(time.Millisecond)
	}
}

// recv waits for a value on ch and fails the test when none arrives.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(lifecycleTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// stillPending fails the test when ch already has a value.
func stillPending[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s finished too early", what)
	default:
	}
}

// TestReloader_ReloadWaitsForTrailingLoad verifies, step by step, that a Reload
// arriving while a load is in flight is served by a second load that starts
// only after the first has finished, and not by the load already running.
func TestReloader_ReloadWaitsForTrailingLoad(t *testing.T) {
	t.Parallel()

	var runs atomic.Int32
	started := make(chan int32)
	release := make(chan struct{})
	r := newReloader(func(ctx context.Context) error {
		n := runs.Add(1)
		started <- n
		<-release
		return nil
	}, discardLogger())
	t.Cleanup(r.Stop)

	r.Trigger()
	if n := recv(t, started, "first load"); n != 1 {
		t.Fatalf("first load number = %d, want 1", n)
	}

	done := make(chan error, 1)
	go func() { done <- r.Reload(context.Background()) }()
	waitQueued(t, r) // the Reload is now queued behind load 1
	stillPending(t, done, "Reload (first load still running)")
	if got := runs.Load(); got != 1 {
		t.Fatalf("loads while the first is in flight = %d, want 1", got)
	}

	release <- struct{}{} // finish load 1
	if n := recv(t, started, "trailing load"); n != 2 {
		t.Fatalf("trailing load number = %d, want 2", n)
	}
	// Load 1 is finished, load 2 is running: the Reload must still wait.
	stillPending(t, done, "Reload (trailing load still running)")

	release <- struct{}{} // finish load 2
	if err := recv(t, done, "Reload"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Errorf("loads = %d, want 2", got)
	}
}

// TestReloader_LoadFailure verifies that a failed load is reported to exactly
// the callers it served and that later loads still run.
func TestReloader_LoadFailure(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("first load failed")
	errSecond := errors.New("second load failed")
	results := []error{errFirst, errSecond, nil}

	var runs atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	r := newReloader(func(ctx context.Context) error {
		n := runs.Add(1)
		started <- struct{}{}
		<-release
		return results[n-1]
	}, discardLogger())
	t.Cleanup(r.Stop)

	first := make(chan error, 1)
	go func() { first <- r.Reload(context.Background()) }()
	recv(t, started, "first load")

	second := make(chan error, 1)
	go func() { second <- r.Reload(context.Background()) }()
	waitQueued(t, r)

	release <- struct{}{} // load 1 fails
	if err := recv(t, first, "first Reload"); !errors.Is(err, errFirst) {
		t.Errorf("first Reload error = %v, want %v", err, errFirst)
	}

	// The queued load runs although its predecessor failed.
	recv(t, started, "second load")
	stillPending(t, second, "second Reload")
	release <- struct{}{} // load 2 fails
	if err := recv(t, second, "second Reload"); !errors.Is(err, errSecond) {
		t.Errorf("second Reload error = %v, want %v", err, errSecond)
	}
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// A fresh Reload after two failures starts a new load and succeeds.
	third := make(chan error, 1)
	go func() { third <- r.Reload(context.Background()) }()
	recv(t, started, "third load")
	release <- struct{}{}
	if err := recv(t, third, "third Reload"); err != nil {
		t.Errorf("third Reload error = %v, want nil", err)
	}
	if got := runs.Load(); got != 3 {
		t.Errorf("loads = %d, want 3", got)
	}
}

// TestReloader_StopCancelsBlockedLoad verifies that Stop cancels the load in
// flight, releases every waiter, waits for the worker goroutine and leaves the
// Reloader inert.
func TestReloader_StopCancelsBlockedLoad(t *testing.T) {
	t.Parallel()

	var runs atomic.Int32
	started := make(chan struct{})
	loadReturned := make(chan struct{})
	r := newReloader(func(ctx context.Context) error {
		runs.Add(1)
		close(started)
		<-ctx.Done() // blocks until the Reloader cancels the load
		close(loadReturned)
		return ctx.Err()
	}, discardLogger())

	inFlight := make(chan error, 1)
	go func() { inFlight <- r.Reload(context.Background()) }()
	recv(t, started, "load start")

	queued := make(chan error, 1)
	go func() { queued <- r.Reload(context.Background()) }()
	waitQueued(t, r)

	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	recv(t, stopped, "Stop")

	// Stop waits for the worker goroutine, so the load has returned and no
	// load is registered any more.
	select {
	case <-loadReturned:
	default:
		t.Fatal("Stop returned before the load in flight returned")
	}
	r.mu.Lock()
	current, next := r.current, r.next
	r.mu.Unlock()
	if current != nil || next != nil {
		t.Errorf("after Stop: current=%v next=%v, want both nil", current, next)
	}

	if err := recv(t, inFlight, "Reload served by the cancelled load"); !errors.Is(err, context.Canceled) {
		t.Errorf("Reload on the cancelled load = %v, want context.Canceled", err)
	}
	if err := recv(t, queued, "queued Reload"); !errors.Is(err, ErrReloaderStopped) {
		t.Errorf("queued Reload = %v, want ErrReloaderStopped", err)
	}

	// After Stop nothing is loaded any more and nothing blocks.
	after := make(chan error, 1)
	go func() { after <- r.Reload(context.Background()) }()
	if err := recv(t, after, "Reload after Stop"); !errors.Is(err, ErrReloaderStopped) {
		t.Errorf("Reload after Stop = %v, want ErrReloaderStopped", err)
	}
	r.Trigger()
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after Stop: %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("loads = %d, want 1 (the queued load and later triggers must not run)", got)
	}

	// Stop is idempotent.
	again := make(chan struct{})
	go func() {
		r.Stop()
		close(again)
	}()
	recv(t, again, "second Stop")
}

func TestReloader_StopWhenIdle(t *testing.T) {
	t.Parallel()

	r, runs, _ := newCountingReloader(nil)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	r.Stop()
	if err := r.Reload(context.Background()); !errors.Is(err, ErrReloaderStopped) {
		t.Errorf("Reload after Stop = %v, want ErrReloaderStopped", err)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("loads = %d, want 1", got)
	}
}

// TestReloader_StoppedDoesNotUseClosedDatabase verifies the shutdown order used
// in production: once stopped, the Reloader returns at once and never queries
// the database, which may be closed by then.
func TestReloader_StoppedDoesNotUseClosedDatabase(t *testing.T) {
	t.Parallel()

	d := openLoaderDB(t)
	kc := cache.New[string, KeyInfo]()
	r := NewReloader(d, kc, discardLogger())

	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	r.Stop()
	if err := d.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	if err := r.Reload(context.Background()); !errors.Is(err, ErrReloaderStopped) {
		t.Errorf("Reload after Stop = %v, want ErrReloaderStopped", err)
	}
	r.Trigger()
}

// TestLoadKeysIntoCache_StaleSnapshot blocks a full load between its database
// read and the cache swap, revokes one key and stores a new one in that
// window, and verifies that the stale snapshot neither brings the revoked key
// back nor drops the new one.
func TestLoadKeysIntoCache_StaleSnapshot(t *testing.T) {
	t.Parallel()

	d := openLoaderDB(t)
	kc := cache.New[string, KeyInfo]()
	log := discardLogger()
	ctx := context.Background()

	orgID := createOrg(t, d, "OrgStale", "org-stale", db.CreateOrgParams{})
	userID := createUser(t, d, "stale@example.com", false)
	createMembership(t, d, orgID, userID, RoleMember)
	newKey := func(name string) string {
		return keygen.Hash(insertKey(t, d, db.CreateAPIKeyParams{
			KeyType: keygen.KeyTypeUser, Name: name, OrgID: orgID, UserID: ptrStr(userID), CreatedBy: userID,
		}), loaderHMACSecret)
	}
	revokedHash := newKey("revoked")
	keptHash := newKey("kept")

	if err := LoadKeysIntoCache(ctx, d, kc, log); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if kc.Len() != 2 {
		t.Fatalf("cache.Len() after initial load = %d, want 2", kc.Len())
	}

	readDone := make(chan struct{})
	release := make(chan struct{})
	loadErr := make(chan error, 1)
	go func() {
		loadErr <- loadKeysIntoCache(ctx, func(ctx context.Context) ([]db.KeyRecord, []error, error) {
			records, skipped, err := d.LoadAllActiveKeys(ctx)
			close(readDone) // the snapshot is taken: it contains the revoked key
			<-release
			return records, skipped, err
		}, kc, log)
	}()
	recv(t, readDone, "database read")

	// Revoke one key and create another, the way the admin handlers do:
	// database first, then the cache.
	if _, err := d.SQL().ExecContext(ctx,
		"UPDATE api_keys SET deleted_at = CURRENT_TIMESTAMP WHERE key_hash = ?", revokedHash); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	kc.Delete(revokedHash)
	createdHash := newKey("created")
	kc.Set(createdHash, KeyInfo{ID: "created-during-load", OrgID: orgID})

	close(release)
	if err := recv(t, loadErr, "blocked load"); err != nil {
		t.Fatalf("blocked load: %v", err)
	}

	if _, ok := kc.Get(revokedHash); ok {
		t.Error("revoked key is back in the cache after the stale snapshot was installed")
	}
	if ki, ok := kc.Get(createdHash); !ok {
		t.Error("key stored during the load was dropped by the stale snapshot")
	} else if ki.ID != "created-during-load" {
		t.Errorf("key stored during the load has ID %q, want the stored value", ki.ID)
	}
	if _, ok := kc.Get(keptHash); !ok {
		t.Error("untouched key missing after the load")
	}

	// A later load reflects the database again.
	if err := LoadKeysIntoCache(ctx, d, kc, log); err != nil {
		t.Fatalf("final load: %v", err)
	}
	if _, ok := kc.Get(revokedHash); ok {
		t.Error("revoked key present after the final load")
	}
	if ki, ok := kc.Get(createdHash); !ok || ki.ID == "created-during-load" {
		t.Errorf("created key after the final load: ok=%v id=%q, want the database record", ok, ki.ID)
	}
}

// TestLoadKeysIntoCache_FailedReadLeavesCacheAndEndsLoad verifies that a failed
// database read keeps the cache contents and does not leave the load open.
func TestLoadKeysIntoCache_FailedReadLeavesCacheAndEndsLoad(t *testing.T) {
	t.Parallel()

	kc := cache.New[string, KeyInfo]()
	kc.Set("existing", KeyInfo{ID: "k"})
	errRead := errors.New("read failed")

	err := loadKeysIntoCache(context.Background(), func(context.Context) ([]db.KeyRecord, []error, error) {
		return nil, nil, errRead
	}, kc, discardLogger())
	if !errors.Is(err, errRead) {
		t.Fatalf("error = %v, want %v", err, errRead)
	}
	if _, ok := kc.Get("existing"); !ok {
		t.Error("failed load removed an existing entry")
	}

	// The aborted load must not affect a later one.
	token := kc.BeginLoad()
	if !kc.CommitLoad(token, map[string]KeyInfo{"fresh": {ID: "f"}}) {
		t.Error("CommitLoad after a failed load = false")
	}
}
