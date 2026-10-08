package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/adfnekc/ad_explorer/internal/rdp"
)

// Registry defaults.
const (
	// DefaultMaxSessions bounds how many remote desktops one gateway will hold
	// open at once. Each one is a TCP connection, a decode loop and a slice of
	// memory the size of the desktop, so this is a resource limit rather than a
	// policy.
	DefaultMaxSessions = 4
	// DefaultIdleTimeout ends a session nobody has touched. It applies to input
	// and clipboard traffic, not to the client's heartbeat: a tab left open
	// overnight must not hold a remote desktop hostage.
	DefaultIdleTimeout = 15 * time.Minute
)

// Options configures a Registry.
type Options struct {
	// Max is the number of concurrent sessions. Zero means the default.
	Max int
	// IdleTimeout ends a session with no input for that long. Zero means the
	// default; a negative value disables it.
	IdleTimeout time.Duration
	// Quality bounds passed to each session's encoder governor.
	QualityMin   int
	QualityMax   int
	QualityStart int
	// FrameInterval is the shortest gap between two encoded frames. Zero means
	// the default. It bounds what one session may spend on encoding, which is
	// the expensive part: a full 1280x720 frame costs about 32 ms.
	FrameInterval time.Duration
	// Demo replaces the remote with a synthetic desktop that draws the input
	// it receives. No network connection is made. It exists so the pipeline can
	// be exercised before grdp can supply a picture, and it must never be on in
	// a deployment that expects to reach real hosts.
	Demo bool
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// dialFunc opens a remote connection. It is a field rather than a direct call
// so the registry can be tested without a server.
type dialFunc func(context.Context, rdp.Config) (remote, error)

func dialWithGRDP(ctx context.Context, cfg rdp.Config) (remote, error) {
	s, err := rdp.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Registry owns every live session.
//
// It is safe for concurrent use.
type Registry struct {
	opts Options
	dial dialFunc

	mu           sync.Mutex
	sessions     map[string]*Session
	pending      int
	shuttingDown bool
}

// NewRegistry returns an empty registry.
func NewRegistry(opts Options) *Registry {
	if opts.Max <= 0 {
		opts.Max = DefaultMaxSessions
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Registry{
		opts:     opts,
		dial:     dialWithGRDP,
		sessions: make(map[string]*Session),
	}
}

// Create dials the remote and registers a session.
//
// The count of sessions in flight includes the ones still dialling, so a burst
// of requests cannot each pass the limit check and then all connect.
func (r *Registry) Create(ctx context.Context, cfg rdp.Config) (*Session, error) {
	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		return nil, ErrShuttingDown
	}
	if len(r.sessions)+r.pending >= r.opts.Max {
		r.mu.Unlock()
		return nil, ErrLimit
	}
	r.pending++
	r.mu.Unlock()

	// Whatever happens below, this dial stops counting against the limit.
	defer func() {
		r.mu.Lock()
		r.pending--
		r.mu.Unlock()
	}()

	var rs remote
	if r.opts.Demo {
		// The synthetic desktop has a fixed size, so the requested one is
		// replaced rather than left to disagree with what is drawn — and the
		// protocol is named, so the UI can say it is not talking to a host.
		cfg.Width, cfg.Height = demoWidth, demoHeight
		cfg.Protocol = demoProtocol
		rs = newDemoRemote(cfg, r.opts.Now)
	} else {
		var err error
		rs, err = r.dial(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}

	id, err := newID()
	if err != nil {
		rs.Close()
		return nil, err
	}

	s := newSession(id, cfg, rs, options{
		idleTimeout:  r.opts.IdleTimeout,
		qualityMin:   r.opts.QualityMin,
		qualityMax:   r.opts.QualityMax,
		qualityStart: r.opts.QualityStart,
		frameEvery:   r.opts.FrameInterval,
		now:          r.opts.Now,
		onDone:       r.forget,
	})

	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		rs.Close()
		return nil, ErrShuttingDown
	}
	r.sessions[id] = s
	r.mu.Unlock()

	return s, nil
}

// Get returns a session by id.
func (r *Registry) Get(id string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	return s, ok
}

// List describes every live session, oldest first.
func (r *Registry) List() []Info {
	r.mu.Lock()
	sessions := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	infos := make([]Info, 0, len(sessions))
	for _, s := range sessions {
		infos = append(infos, s.Info())
	}
	// The map iterates in a random order; sorting keeps the API stable so a
	// client polling the list does not see it shuffle.
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].StartedAt.Equal(infos[j].StartedAt) {
			return infos[i].StartedAt.Before(infos[j].StartedAt)
		}
		return infos[i].ID < infos[j].ID
	})
	return infos
}

// Count is the number of live sessions.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Close ends one session.
func (r *Registry) Close(id string) error {
	r.mu.Lock()
	s, ok := r.sessions[id]
	r.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	s.Close()
	return nil
}

// Shutdown ends every session and refuses new ones.
//
// Each session is closed outside the lock: closing one calls back into forget,
// and taking the lock here as well would deadlock.
func (r *Registry) Shutdown() {
	r.mu.Lock()
	r.shuttingDown = true
	sessions := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}

// forget removes a session that ended on its own, such as when the remote hung
// up. It is idempotent.
func (r *Registry) forget(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.sessions[s.id]; ok && cur == s {
		delete(r.sessions, s.id)
	}
}

// newID returns a random 64-bit identifier in hex.
//
// It is random rather than sequential so that knowing one session id does not
// let anyone name another. The cookie already gates access; this is the second
// lock on the same door, which costs nothing.
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
