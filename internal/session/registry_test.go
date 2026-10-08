package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/adfnekc/ad_explorer/internal/rdp"
)

// fakeDialer hands out fake remotes, and can be made to block so the tests can
// inspect the registry while a connection is still in flight.
type fakeDialer struct {
	mu       sync.Mutex
	dials    int
	err      error
	block    chan struct{}
	remotes  []*fakeRemote
	lastCfg  rdp.Config
	onDialIn func()
}

func (d *fakeDialer) dial(ctx context.Context, cfg rdp.Config) (remote, error) {
	d.mu.Lock()
	d.dials++
	d.lastCfg = cfg
	block := d.block
	if d.onDialIn != nil {
		d.onDialIn()
	}
	err := d.err
	d.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}

	r := newFakeRemote()
	d.mu.Lock()
	d.remotes = append(d.remotes, r)
	d.mu.Unlock()
	return r, nil
}

func (d *fakeDialer) DialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func newTestRegistry(t *testing.T, opts Options) (*Registry, *fakeDialer) {
	t.Helper()
	r := NewRegistry(opts)
	d := &fakeDialer{}
	r.dial = d.dial
	return r, d
}

func TestRegistryCreateAndList(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t, Options{Max: 4})

	s, err := r.Create(ctx, rdp.Config{Host: "10.0.0.1:3389", Name: "web-01", Width: 1280, Height: 720})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID() == "" {
		t.Error("the session has no id")
	}
	if r.Count() != 1 {
		t.Errorf("Count() = %d, want 1", r.Count())
	}

	got, ok := r.Get(s.ID())
	if !ok || got != s {
		t.Error("Get did not return the session that was just created")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get found a session that was never created")
	}

	infos := r.List()
	if len(infos) != 1 {
		t.Fatalf("List() returned %d entries, want 1", len(infos))
	}
	if infos[0].ID != s.ID() || infos[0].Name != "web-01" || infos[0].Host != "10.0.0.1:3389" {
		t.Errorf("List() = %+v", infos[0])
	}
	if infos[0].Attached {
		t.Error("a session that was never attached reports as attached")
	}
}

func TestRegistryIDsAreUnpredictable(t *testing.T) {
	// Knowing one id must not let anyone name another, even though the cookie
	// already gates access. The cost of the second lock is eight bytes.
	ctx := context.Background()
	r, _ := newTestRegistry(t, Options{Max: 32})

	seen := make(map[string]bool, 16)
	for i := 0; i < 16; i++ {
		s, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		if len(s.ID()) < 16 {
			t.Errorf("id %q is too short to be random", s.ID())
		}
		if seen[s.ID()] {
			t.Fatalf("id %q was reused", s.ID())
		}
		seen[s.ID()] = true
	}
}

func TestRegistryLimit(t *testing.T) {
	ctx := context.Background()
	r, d := newTestRegistry(t, Options{Max: 1})

	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); !errors.Is(err, ErrLimit) {
		t.Errorf("second Create = %v, want ErrLimit", err)
	}
	// The refused attempt must not have opened a connection.
	if got := d.DialCount(); got != 1 {
		t.Errorf("dialled %d times, want 1: a refused request must not reach the network", got)
	}
	if r.Count() != 1 {
		t.Errorf("Count() = %d, want 1", r.Count())
	}
}

func TestRegistryLimitCountsConnectionsInFlight(t *testing.T) {
	// Without counting the ones still dialling, a burst of requests would each
	// pass the check and then all connect, which is the failure the limit
	// exists to prevent.
	ctx := context.Background()
	release := make(chan struct{})
	dialing := make(chan struct{}, 1)

	r, _ := newTestRegistry(t, Options{Max: 1})
	d := &fakeDialer{block: release, onDialIn: func() {
		select {
		case dialing <- struct{}{}:
		default:
		}
	}}
	r.dial = d.dial

	done := make(chan error, 1)
	go func() {
		_, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480})
		done <- err
	}()

	// Wait until the first dial is under way, so the second check sees it.
	select {
	case <-dialing:
	case <-time.After(3 * time.Second):
		t.Fatal("the first dial never started")
	}

	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); !errors.Is(err, ErrLimit) {
		t.Errorf("a second Create while the first was dialling = %v, want ErrLimit", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first Create failed: %v", err)
	}
}

func TestRegistryDialFailureDoesNotConsumeASlot(t *testing.T) {
	ctx := context.Background()
	r, d := newTestRegistry(t, Options{Max: 1})
	d.err = errors.New("connection refused")

	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err == nil {
		t.Fatal("Create succeeded against a failing dialer")
	}
	if r.Count() != 0 {
		t.Errorf("Count() = %d after a failed dial, want 0", r.Count())
	}
	// The slot must be free again, or one bad host would block the gateway.
	d.err = nil
	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
		t.Errorf("Create after a failure: %v", err)
	}
}

func TestRegistryClose(t *testing.T) {
	ctx := context.Background()
	r, d := newTestRegistry(t, Options{Max: 4})

	s, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Close(s.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("Count() = %d after Close, want 0", r.Count())
	}
	if !d.remotes[0].isClosed() {
		t.Error("Close did not release the remote connection")
	}
	if err := r.Close(s.ID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Close of a closed session = %v, want ErrNotFound", err)
	}
	if err := r.Close("never-existed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Close of an unknown session = %v, want ErrNotFound", err)
	}
}

func TestRegistryForgetsASessionThatEndsOnItsOwn(t *testing.T) {
	// A session that is created and never attached still has to be reclaimable,
	// or the registry fills with connections nobody can reach.
	ctx := context.Background()
	r, _ := newTestRegistry(t, Options{Max: 1})

	s, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Close()

	waitFor(t, "the registry to forget the session", func() bool { return r.Count() == 0 })
	if _, ok := r.Get(s.ID()); ok {
		t.Error("Get still returns a closed session")
	}
	// And the freed slot is usable.
	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
		t.Errorf("Create after a session ended: %v", err)
	}
}

func TestRegistryShutdown(t *testing.T) {
	ctx := context.Background()
	r, d := newTestRegistry(t, Options{Max: 4})

	for i := 0; i < 3; i++ {
		if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	r.Shutdown()

	waitFor(t, "every remote to be released", func() bool {
		n := 0
		for _, rem := range d.remotes {
			if rem.isClosed() {
				n++
			}
		}
		return n == 3
	})
	if r.Count() != 0 {
		t.Errorf("Count() = %d after Shutdown, want 0", r.Count())
	}
	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Create after Shutdown = %v, want ErrShuttingDown", err)
	}
}

func TestRegistryShutdownIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t, Options{Max: 4})
	if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			r.Shutdown()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown blocked")
	}
}

func TestRegistryListIsSortedAndStable(t *testing.T) {
	ctx := context.Background()

	// A clock that moves forward on every read makes the creation order
	// explicit, so the sort has something to order by.
	var mu sync.Mutex
	tick := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Second)
		return tick
	}

	r, _ := newTestRegistry(t, Options{Max: 4, Now: now})
	for i := 0; i < 3; i++ {
		if _, err := r.Create(ctx, rdp.Config{Host: "h:3389", Name: string(rune('a' + i)), Width: 640, Height: 480}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	first := r.List()
	for i := 1; i < len(first); i++ {
		if first[i].StartedAt.Before(first[i-1].StartedAt) {
			t.Fatalf("List() is not ordered by start time: %+v", first)
		}
	}
	// The map iteration order is random; two calls must agree.
	for attempt := 0; attempt < 5; attempt++ {
		again := r.List()
		for i := range first {
			if again[i].ID != first[i].ID {
				t.Fatalf("List() is not stable across calls: %v then %v", ids(first), ids(again))
			}
		}
	}
}

func TestRegistryCreatePropagatesContextCancellation(t *testing.T) {
	// A client that gives up must not leave a dial running against a limit
	// that is now permanently consumed.
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	r, _ := newTestRegistry(t, Options{Max: 4})
	d := &fakeDialer{block: release}
	r.dial = d.dial

	done := make(chan error, 1)
	go func() {
		_, err := r.Create(ctx, rdp.Config{Host: "h:3389", Width: 640, Height: 480})
		done <- err
	}()

	waitFor(t, "the dial to start", func() bool { return d.DialCount() == 1 })
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Create = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Create ignored the cancelled context")
	}
	close(release)

	waitFor(t, "the slot to be released", func() bool { return r.Count() == 0 })
	// The registry must be usable again.
	if _, err := r.Create(context.Background(), rdp.Config{Host: "h:3389", Width: 640, Height: 480}); err != nil {
		t.Errorf("Create after a cancelled dial: %v", err)
	}
}

func ids(infos []Info) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.ID)
	}
	return out
}
